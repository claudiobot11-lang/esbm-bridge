//go:build windows

// cmdFix — the one-click repair + install path.
//
// This is what a store operator gets when they double-click the .exe,
// and what `esbm-bridge install` runs. It exists because of a real
// 22-hour outage on 2026-11-15: the operator ran a freshly downloaded
// binary by hand, it worked, and when the console window closed the
// gateway went dark. Meanwhile an OLD Bridge from months earlier was
// still sending heartbeats (so the cloud dashboard looked green) and
// forwarding nothing.
//
// So "install" can no longer mean only "register a service". It means:
//
//  1. stop whatever service is registered, and disarm its auto-restart
//  2. kill every other Bridge process still running on this PC
//  3. remove whatever used to relaunch them (Run keys, Startup
//     shortcuts, scheduled tasks) so none of them comes back
//  4. wait for 9071/9080/9099 to actually be free
//  5. copy ourselves to a stable path under %ProgramFiles% — the
//     service must not point at the operator's Downloads folder
//  6. point the service at THAT copy (reconfigured in place — never
//     delete+create, which Windows blocks while services.msc is open),
//     auto-start, auto-restart, firewall
//  7. prove it: the Bridge answering on the local dashboard is the
//     process we just started, both gateway ports are bound by it, and
//     the tunnel has actually reached the cloud
//
// Step 7 decides what the operator is told. There are three outcomes,
// not two: installed and working; installed but something still needs
// attention (shown as a warning, never as a plain success); failed.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/claudiobot11-lang/esbm-bridge/internal/api"
	"github.com/claudiobot11-lang/esbm-bridge/internal/config"
	"github.com/claudiobot11-lang/esbm-bridge/internal/winservice"
	"github.com/ncruces/zenity"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc/mgr"
)

// bridgePorts are every port a running Bridge binds. All three must be
// free before we start the new service, or the fresh copy comes up
// half-crippled: a stale process holding 9071 is exactly the failure
// that looks like "installed fine, tags still frozen".
var bridgePorts = []struct {
	addr string
	what string
}{
	{"127.0.0.1:9071", "gateway Cronus (9071)"},
	{"127.0.0.1:9080", "gateway eStation (9080)"},
	{"127.0.0.1:9099", "painel local (9099)"},
}

// maxPathBuf is Windows' long-path ceiling. QueryFullProcessImageName
// needs a buffer, and MAX_PATH (260) silently truncates paths from
// deep user folders — which is where downloaded binaries live.
const maxPathBuf = 32768

// installDirName is the stable home for the shipped binary. Registering
// the service against the download location was the latent bug behind
// "it worked yesterday": clearing Downloads would have broken the
// service on the next boot even if it had been installed properly.
const installDirName = "ESBM Bridge"

// fixMutexName serialises repairs. Two elevated copies running at once
// (an impatient double double-click) would each kill "every other
// Bridge" — including one another — and race to register the service.
const fixMutexName = `Global\ESBMBridgeFix`

// launcherMutexName serialises the UN-elevated launchers of one desktop
// session. Two quick double-clicks would otherwise each raise a UAC
// prompt, and the first repair would then kill the second launcher and
// list it in the summary as a "stale Bridge" that never existed.
const launcherMutexName = `Local\ESBMBridgeLauncher`

// statusMutexName marks a process that is only showing the "installed
// and running" status window, so another launch does not mistake it
// for a stale Bridge and reinstall a healthy service.
const statusMutexName = `Local\ESBMBridgeStatusUI`

// procInfo is one running Bridge we are about to close.
type procInfo struct {
	pid    uint32
	parent uint32
	name   string
	path   string
}

// report accumulates what actually happened so the operator sees one
// honest summary instead of a flashing console.
type report struct {
	lines []string
	warns []string
}

func (r *report) add(format string, a ...any) {
	r.lines = append(r.lines, fmt.Sprintf(format, a...))
}

func (r *report) warn(format string, a ...any) {
	r.warns = append(r.warns, fmt.Sprintf(format, a...))
}

func (r *report) text() string {
	out := strings.Join(r.lines, "\n")
	if len(r.warns) > 0 {
		out += "\n\nATENÇÃO:\n• " + strings.Join(r.warns, "\n• ")
	}
	return out
}

// cmdFix is the whole repair. hasConsole reports whether we are attached
// to a terminal; without one (double-click, or the elevated copy that
// UAC spawned) everything has to be a window, because nothing else is
// visible to the operator.
func cmdFix(log *slog.Logger, args []string, hasConsole bool) int {
	gui := !hasConsole
	elevatedChild := false
	for _, a := range args {
		switch a {
		case "--dialog":
			gui = true
		case "--elevated":
			elevatedChild = true
		}
	}

	if !isElevated() {
		// A "runas" child that is STILL not elevated means UAC is off
		// for a non-admin account. Relaunching again would loop forever.
		if elevatedChild {
			log.Error("still not elevated after the UAC relaunch; giving up")
			tell(gui, true, "Este usuário do Windows não tem permissão de "+
				"administrador, então não consigo instalar o Bridge.\n\n"+
				"Entre no Windows com a conta de administrador e abra o "+
				"programa de novo.")
			return 1
		}
		// A repeat double-click while a repair is running (or while its
		// result window is still open) gets an answer instead of a
		// second UAC prompt followed by nothing.
		if fixInProgress() {
			tell(gui, false, "A instalação do Bridge já está em andamento "+
				"(ou acabou de terminar).\n\nEla pode levar até 5 minutos. "+
				"No fim aparece uma janela \"ESBM Bridge\" com o resultado — "+
				"ela pode estar atrás de outra janela, na barra de tarefas.")
			return 0
		}
		// Hold the launcher mutex across the (blocking) UAC request: a
		// second double-click made meanwhile exits quietly instead of
		// queueing a second prompt.
		relL, dup := acquireMutex(launcherMutexName)
		if dup {
			log.Info("another launcher is already asking for elevation; exiting")
			return 0
		}
		defer relL()
		log.Info("not elevated — requesting admin via UAC")
		// --dialog: the elevated copy cannot write to this terminal, so
		// it must report through a window or the operator sees nothing.
		if err := relaunchElevated("fix", "--dialog", "--elevated"); err != nil {
			log.Error("UAC elevation declined or failed", "err", err)
			tell(gui, true, "Preciso de permissão de administrador para "+
				"instalar o Bridge como serviço.\n\nAbra o programa de novo e "+
				"clique em \"Sim\" quando o Windows perguntar.\n\n"+
				"Nada foi alterado neste computador.")
			return 1
		}
		// Only NOW — after the operator said yes — remove this user's
		// own autostart entries. HKCU and the user Startup folder belong
		// to whoever double-clicked; the elevated copy may be running as
		// a different (admin) account and would clean the wrong profile.
		// Doing it before consent would strip the old launcher even when
		// the install is then declined.
		for _, c := range cleanUserAutostart(log) {
			log.Info("user autostart entry removed", "entry", c)
		}
		if !gui {
			fmt.Println("Confirme o pedido de administrador do Windows.")
			fmt.Println("O andamento e o resultado aparecem numa janela separada.")
		}
		return 0
	}

	// ---- elevated from here on ---------------------------------------

	// One repair at a time. The mutex stays held until this function
	// returns, i.e. until the result window is dismissed, so a repeat
	// launch cannot tear down the service we just installed.
	release, busy := acquireMutex(fixMutexName)
	if busy {
		log.Warn("another repair is already running; exiting")
		tell(gui, false, "A instalação do Bridge já está em andamento "+
			"(ou acabou de terminar).\n\nEla pode levar até 5 minutos. No "+
			"fim aparece uma janela \"ESBM Bridge\" com o resultado — ela "+
			"pode estar atrás de outra janela.")
		return 0
	}
	defer release()

	// The repair takes from a few seconds to a couple of minutes. With
	// no window the operator sees "UAC, Yes, nothing" and clicks again.
	ui := newProgress(gui, log)
	defer ui.close()

	r := &report{}

	// When the operator launched us already elevated there was no
	// un-elevated parent to do this, and HKCU is then the right hive.
	ui.step("Preparando…")
	for _, c := range cleanUserAutostart(log) {
		r.add("• Removido do início automático: %s", c)
	}

	// A service with no config would start, find itself "not paired"
	// and sit there forever. Refuse early; pairing comes first.
	cfg, cfgErr := config.Load()
	if cfgErr != nil {
		log.Error("cannot install: bridge is not paired", "err", cfgErr)
		ui.done()
		tell(gui, true, "Este PC ainda não foi pareado com a loja.\n\n"+
			"Abra o programa de novo e informe o código de pareamento de "+
			"6 dígitos antes de instalar.")
		return 1
	}

	// Stage the new binary BEFORE touching anything that works. Copying
	// ~30 MB into Program Files is the one step that depends on disk
	// space and on what the antivirus feels like doing to a fresh .exe;
	// if it fails here, nothing has been stopped and the PC is exactly
	// as it was.
	ui.step("Copiando o programa…")
	staged, exePath, err := stageSelf(log)
	if err != nil {
		log.Error("could not stage the binary in Program Files", "err", err)
		r.warn("não consegui copiar o programa para a pasta definitiva (%v). "+
			"Nada foi alterado neste computador", err)
		return finish(ui, r, gui, outcomeFailed, log)
	}

	// Killing a Bridge that runs under another account (LocalSystem,
	// another user's session) needs SeDebugPrivilege. An elevated admin
	// HOLDS it but it is disabled by default.
	if err := enableDebugPrivilege(); err != nil {
		log.Warn("could not enable SeDebugPrivilege; killing other accounts' processes may fail", "err", err)
	}

	// ---- 1. Stop the existing service and disarm its auto-restart -----
	// Before killing processes: a service killed while its recovery
	// policy is armed gets relaunched by SCM 5 seconds later, and we'd
	// be racing the OLD binary for port 9071.
	ui.step("Parando o Bridge antigo…")
	found, oldPID, err := stopExistingService(log)
	switch {
	case err != nil:
		r.warn("não consegui parar o serviço antigo: %v", err)
	case found:
		r.add("• Serviço antigo parado")
	default:
		r.add("• Nenhum serviço antigo registrado")
	}

	// ---- 2. Close every other Bridge still running -------------------
	self := windows.GetCurrentProcessId()
	stale, err := listBridgeProcesses(self)
	if err != nil {
		r.warn("não consegui listar os processos em execução: %v", err)
	}
	// The service's own process, by PID: catches an old binary that was
	// renamed and so does not match the image-name prefix.
	if oldPID != 0 && oldPID != self {
		seen := false
		for _, p := range stale {
			if p.pid == oldPID {
				seen = true
			}
		}
		if !seen && processAlive(oldPID) {
			stale = append(stale, procInfo{pid: oldPID, name: "serviço antigo", path: processPath(oldPID)})
		}
	}
	// Our own un-elevated launcher is already on its way out; it is not
	// a "stale Bridge" and must not show up in the summary as one.
	myParent := parentOf(self)
	var closed []procInfo
	for _, p := range stale {
		if p.pid == myParent && p.pid != oldPID {
			continue
		}
		where := p.path
		if where == "" {
			where = p.name
		}
		gone, err := killPID(p.pid)
		switch {
		case err != nil:
			r.warn("não consegui fechar um Bridge antigo (PID %d, %s): %v", p.pid, where, err)
			log.Warn("kill failed", "pid", p.pid, "path", where, "err", err)
		case gone:
			log.Info("stale bridge had already exited", "pid", p.pid, "path", where)
		default:
			closed = append(closed, p)
			r.add("• Fechado Bridge antigo: %s", where)
			log.Info("killed stale bridge", "pid", p.pid, "path", where)
		}
	}
	if len(closed) == 0 && !found {
		r.add("• Nenhum Bridge antigo rodando")
	}

	// ---- 3. Machine-wide autostart leftovers -------------------------
	for _, c := range cleanMachineAutostart(log) {
		r.add("• Removido do início automático: %s", c)
	}

	// ---- 4. Wait for the ports to come free --------------------------
	// A port still held by something we could NOT kill is said out loud
	// here and, more importantly, decides the verdict in step 7. We still
	// install: the service retries its bind forever and takes over the
	// moment the squatter goes away.
	ui.step("Liberando as portas…")
	stuck := waitPortsFree(log, 20*time.Second)
	for _, s := range stuck {
		r.warn("a porta do %s continua ocupada por outro programa", s)
	}

	// ---- 5. Swap the staged binary into place ------------------------
	ui.step("Instalando o programa…")
	if err := commitSelf(log, staged, exePath); err != nil {
		log.Error("could not place the binary in Program Files", "err", err)
		r.warn("não consegui instalar o programa na pasta definitiva (%v)", err)
		// Running the service from Downloads is the fragile setup this
		// installer exists to replace. Stop here, loudly — but put back
		// whatever was running before we started.
		restoreOldService(log, r, found)
		return finish(ui, r, gui, outcomeFailed, log)
	}
	r.add("• Programa instalado em %s", exePath)

	// ---- 6. Register and start ---------------------------------------
	ui.step("Instalando o serviço…")
	warns, err := registerService(log, exePath)
	for _, w := range warns {
		r.warn("%s", w)
	}
	if err != nil {
		log.Error("service registration failed", "err", err)
		if errors.Is(err, windows.ERROR_SERVICE_MARKED_FOR_DELETE) {
			r.warn("o Windows ainda está apagando o serviço antigo. Feche a " +
				"janela \"Serviços\" se estiver aberta, reinicie o computador " +
				"e abra este programa de novo")
		} else {
			r.warn("falha ao registrar o serviço: %v", err)
		}
		restoreOldService(log, r, found)
		return finish(ui, r, gui, outcomeFailed, log)
	}
	r.add("• Serviço instalado: inicia junto com o Windows")

	ui.step("Iniciando o Bridge…")
	svcPID, err := waitServiceRunning(log, 45*time.Second)
	if err != nil {
		r.warn("o serviço foi instalado mas não iniciou: %v", err)
		return finish(ui, r, gui, outcomeFailed, log)
	}

	// ---- 7. Prove it --------------------------------------------------
	// "Service is Running" proves nothing: Windows is told Running
	// before the worker has bound a single port. Ask the Bridge itself,
	// and make sure the one answering is the process we just started.
	ui.step("Conferindo o gateway…")
	if ok, why := waitOwnBridgeUp(svcPID, 75*time.Second); !ok {
		r.warn("o Bridge novo não assumiu as portas do gateway: %s", why)
		return finish(ui, r, gui, outcomeFailed, log)
	}
	r.add("• Bridge rodando e escutando o gateway (9071 e 9080)")

	// The tunnel is the part that broke in two of the three past
	// outages. Never fail the install on it — the service keeps
	// retrying on its own — but never call it a success either.
	ui.step("Conectando à nuvem…")
	tunnelUp, tunnelErr := waitCloudReachable(svcPID, 90*time.Second)
	if tunnelUp {
		r.add("• Conexão com a nuvem: funcionando")
	} else {
		msg := "o Bridge está instalado, mas AINDA NÃO conseguiu falar com a " +
			"nuvem — as etiquetas só atualizam depois que conectar. Ele " +
			"continua tentando sozinho"
		if tunnelErr != "" {
			msg += " (" + tunnelErr + ")"
		}
		r.warn("%s", msg)
	}

	// Best-effort breadcrumb to the cloud. This also overwrites the
	// `note` column, which is never cleared on its own and has misled
	// every past diagnosis of this exact failure.
	sendFixEvent(log, cfg, exePath, closed, tunnelUp)

	if len(r.warns) > 0 {
		return finish(ui, r, gui, outcomeWarnings, log)
	}
	return finish(ui, r, gui, outcomeOK, log)
}

type outcome int

const (
	outcomeOK outcome = iota
	outcomeWarnings
	outcomeFailed
)

// finish renders the one and only summary the operator sees.
func finish(ui *progressUI, r *report, gui bool, o outcome, log *slog.Logger) int {
	logPath := filepath.Join(os.Getenv("ProgramData"), "esbm-bridge", "bridge.log")
	var head, tail, title string
	switch o {
	case outcomeOK:
		title = "ESBM Bridge — Instalado"
		head = "Bridge instalado e funcionando.\n\n"
		tail = "\n\nPode fechar esta janela. O Bridge agora roda sozinho em " +
			"segundo plano, inicia junto com o Windows e volta sozinho se cair.\n" +
			"Painel: http://127.0.0.1:9099"
	case outcomeWarnings:
		title = "ESBM Bridge — Instalado, com avisos"
		head = "Bridge instalado, MAS falta conferir um ponto.\n\n"
		tail = "\n\nTire uma foto desta tela e me mande.\n" +
			"Painel: http://127.0.0.1:9099"
	default:
		title = "ESBM Bridge — A instalação falhou"
		head = "A instalação NÃO terminou.\n\n"
		tail = "\n\nTire uma foto desta tela e me mande, junto com o arquivo:\n" + logPath
	}
	body := head + r.text() + tail

	fmt.Println()
	fmt.Println(body)
	log.Info("fix finished", "outcome", int(o), "steps", len(r.lines), "warnings", len(r.warns))

	if gui {
		// Leave the progress window up (showing "done") while the result
		// box is open: it is the only top-level window this process
		// owns, and it is what lets the box come to the foreground.
		ui.done()
		// Bounded: a result box forgotten behind another window must not
		// hold the repair mutex forever.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		opts := []zenity.Option{zenity.Title(title), zenity.Context(ctx)}
		switch o {
		case outcomeOK:
			_ = zenity.Info(body, opts...)
		case outcomeWarnings:
			_ = zenity.Warning(body, opts...)
		default:
			_ = zenity.Error(body, opts...)
		}
	}
	if o == outcomeFailed {
		return 1
	}
	return 0
}

// tell shows one message the way this launch can show it.
func tell(gui, isErr bool, msg string) {
	if !gui {
		if isErr {
			fmt.Fprintln(os.Stderr, msg)
		} else {
			fmt.Println(msg)
		}
		return
	}
	if isErr {
		_ = zenity.Error(msg, zenity.Title("ESBM Bridge"))
	} else {
		_ = zenity.Info(msg, zenity.Title("ESBM Bridge"))
	}
}

// ---------------------------------------------------------------------
// Progress window
// ---------------------------------------------------------------------

// progressUI is a button-less "working…" window. Nil-safe throughout:
// in console mode, or if the window cannot be created, every method is
// a no-op and the repair carries on.
type progressUI struct {
	dlg zenity.ProgressDialog
	log *slog.Logger
}

func newProgress(gui bool, log *slog.Logger) *progressUI {
	ui := &progressUI{log: log}
	if !gui {
		return ui
	}
	// Pulsate + NoCancel + AutoClose = a marquee bar and no buttons: the
	// operator can watch it but cannot abort a half-done repair.
	dlg, err := zenity.Progress(
		zenity.Title("ESBM Bridge — instalando"),
		zenity.Pulsate(), zenity.NoCancel(), zenity.AutoClose())
	if err != nil {
		log.Warn("could not open the progress window (non-fatal)", "err", err)
		return ui
	}
	ui.dlg = dlg
	return ui
}

func (ui *progressUI) step(text string) {
	ui.log.Info("fix step", "step", text)
	if ui.dlg != nil {
		_ = ui.dlg.Text(text)
	}
}

func (ui *progressUI) done() {
	if ui.dlg != nil {
		_ = ui.dlg.Text("Concluído.")
		_ = ui.dlg.Complete()
	}
}

func (ui *progressUI) close() {
	if ui.dlg != nil {
		_ = ui.dlg.Close()
		ui.dlg = nil
	}
}

// ---------------------------------------------------------------------
// Single-instance guard
// ---------------------------------------------------------------------

// acquireMutex takes a named mutex. busy is true when another process
// already holds it.
func acquireMutex(mutexName string) (release func(), busy bool) {
	name, err := windows.UTF16PtrFromString(mutexName)
	if err != nil {
		return func() {}, false
	}
	// CreateMutex returns a VALID handle together with
	// ERROR_ALREADY_EXISTS when the mutex is already there.
	h, err := windows.CreateMutex(nil, false, name)
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		if h != 0 {
			windows.CloseHandle(h)
		}
		return func() {}, true
	}
	if h == 0 {
		return func() {}, false // could not create — run unguarded
	}
	return func() { windows.CloseHandle(h) }, false
}

// mutexExists reports whether some process holds the named mutex,
// without taking it.
func mutexExists(mutexName string) bool {
	name, err := windows.UTF16PtrFromString(mutexName)
	if err != nil {
		return false
	}
	h, err := windows.OpenMutex(windows.SYNCHRONIZE, false, name)
	if err == nil {
		windows.CloseHandle(h)
		return true
	}
	// Exists, but owned by an elevated copy and closed to us.
	return errors.Is(err, windows.ERROR_ACCESS_DENIED)
}

// fixInProgress is the un-elevated probe for the repair mutex.
func fixInProgress() bool { return mutexExists(fixMutexName) }

// ---------------------------------------------------------------------
// Service control
// ---------------------------------------------------------------------

// stopExistingService stops whatever is registered under our service
// name and clears its failure actions. It deliberately does NOT delete
// the registration: DeleteService only marks the entry, and Windows
// keeps it alive (CreateService then fails with "marked for deletion")
// for as long as anything holds a handle to it — an open services.msc
// window is enough. registerService reconfigures the entry in place.
//
// pid is the service's process at the time we looked (0 if none), so
// the caller can kill exactly that process even if its image name does
// not look like a Bridge.
func stopExistingService(log *slog.Logger) (found bool, pid uint32, err error) {
	m, err := mgr.Connect()
	if err != nil {
		return false, 0, fmt.Errorf("connect to service manager: %w", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService(winservice.ServiceName)
	if err != nil {
		return false, 0, nil // not registered — nothing to do
	}
	defer s.Close()

	if st, e := s.Query(); e == nil {
		pid = st.ProcessId
	}
	// ResetRecoveryActions, not SetRecoveryActions(nil): the latter
	// returns an error and changes nothing.
	if e := s.ResetRecoveryActions(); e != nil {
		log.Warn("could not clear the old recovery policy", "err", e)
	}
	if e := stopService(s); e != nil {
		// Not fatal: step 2 kills the process outright, and with the
		// recovery policy cleared SCM will not bring it back.
		log.Warn("service did not stop cleanly; its process will be killed", "err", e)
	}
	return true, pid, nil
}

// waitServiceRunning polls SCM until the service reports Running and
// returns the PID of its process.
func waitServiceRunning(log *slog.Logger, d time.Duration) (uint32, error) {
	m, err := mgr.Connect()
	if err != nil {
		return 0, err
	}
	defer m.Disconnect()
	s, err := m.OpenService(winservice.ServiceName)
	if err != nil {
		return 0, err
	}
	defer s.Close()

	deadline := time.Now().Add(d)
	for {
		st, err := s.Query()
		if err != nil {
			return 0, err
		}
		if uint32(st.State) == windows.SERVICE_RUNNING && st.ProcessId != 0 {
			return st.ProcessId, nil
		}
		if time.Now().After(deadline) {
			return 0, fmt.Errorf("continua no estado %d depois de %s", st.State, d)
		}
		log.Debug("waiting for service", "state", st.State)
		time.Sleep(500 * time.Millisecond)
	}
}

// ---------------------------------------------------------------------
// Proof
// ---------------------------------------------------------------------

// waitOwnBridgeUp asks the local dashboard who is running and accepts
// only OUR service: same version, same PID that SCM reports, and both
// gateway proxies bound. A bare "something accepts on 9071" check is
// satisfied by whichever stale Bridge still holds the port — which is
// the one situation this must catch.
func waitOwnBridgeUp(svcPID uint32, d time.Duration) (bool, string) {
	deadline := time.Now().Add(d)
	for {
		why := "o painel local (porta 9099) não respondeu"
		if lb, ok := queryLocalBridge(); ok {
			switch {
			case lb.Version != version:
				why = "quem responde na porta 9099 é outro Bridge, versão " + lb.Version
			case lb.PID != 0 && uint32(lb.PID) != svcPID:
				why = fmt.Sprintf("quem responde na porta 9099 é outro processo (PID %d)", lb.PID)
			default:
				bound := 0
				why = "as portas 9071/9080 ainda não abriram"
				for _, w := range lb.Workers {
					if w.Name != "proxy:9071" && w.Name != "proxy:9080" {
						continue
					}
					// The supervisor marks a worker "running" BEFORE its
					// bind attempt; a failed bind flips it to "backoff"
					// within milliseconds. So require the state to have
					// held for a few seconds.
					if w.State == "running" && !w.StartedAt.IsZero() &&
						time.Since(w.StartedAt) > 3*time.Second {
						bound++
					} else if w.State != "running" && w.LastError != "" {
						why = w.Name + ": " + w.LastError
					}
				}
				if bound == 2 {
					return true, ""
				}
			}
		}
		if time.Now().After(deadline) {
			return false, why
		}
		time.Sleep(time.Second)
	}
}

// waitCloudReachable waits for the new service's tunnel to prove it can
// reach eRetail (a health probe through the VPN succeeded). Returns the
// tunnel worker's last error when it does not.
func waitCloudReachable(svcPID uint32, d time.Duration) (bool, string) {
	deadline := time.Now().Add(d)
	lastErr := ""
	for {
		if lb, ok := queryLocalBridge(); ok && lb.Version == version &&
			(lb.PID == 0 || uint32(lb.PID) == svcPID) {
			if lb.CloudReachable {
				return true, ""
			}
			for _, w := range lb.Workers {
				if w.Name == "tunnel" && w.LastError != "" {
					lastErr = w.LastError
				}
			}
		}
		if time.Now().After(deadline) {
			return false, lastErr
		}
		time.Sleep(time.Second)
	}
}

// ---------------------------------------------------------------------
// Stale processes
// ---------------------------------------------------------------------

// enableDebugPrivilege turns on SeDebugPrivilege in our token so that
// OpenProcess(PROCESS_TERMINATE) works on processes owned by other
// accounts — an old Bridge running as LocalSystem, or one started in
// another user's session.
func enableDebugPrivilege() error {
	var tok windows.Token
	p, err := windows.GetCurrentProcess()
	if err != nil {
		return err
	}
	if err := windows.OpenProcessToken(p,
		windows.TOKEN_ADJUST_PRIVILEGES|windows.TOKEN_QUERY, &tok); err != nil {
		return err
	}
	defer tok.Close()

	name, err := windows.UTF16PtrFromString("SeDebugPrivilege")
	if err != nil {
		return err
	}
	var luid windows.LUID
	if err := windows.LookupPrivilegeValue(nil, name, &luid); err != nil {
		return err
	}
	tp := windows.Tokenprivileges{PrivilegeCount: 1}
	tp.Privileges[0] = windows.LUIDAndAttributes{
		Luid: luid, Attributes: windows.SE_PRIVILEGE_ENABLED,
	}
	return windows.AdjustTokenPrivileges(tok, false, &tp, 0, nil, nil)
}

// snapshotProcesses walks the process table once.
func snapshotProcesses() ([]procInfo, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(snap)

	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	if err := windows.Process32First(snap, &e); err != nil {
		return nil, err
	}
	var out []procInfo
	for {
		out = append(out, procInfo{
			pid:    e.ProcessID,
			parent: e.ParentProcessID,
			name:   windows.UTF16ToString(e.ExeFile[:]),
		})
		if err := windows.Process32Next(snap, &e); err != nil {
			break // ERROR_NO_MORE_FILES
		}
	}
	return out, nil
}

// listBridgeProcesses returns every other running Bridge binary.
// Matching is on the image name prefix so every shape we have ever
// shipped is caught: esbm-bridge.exe, esbm-bridge-windows-amd64.exe,
// "esbm-bridge-windows-amd64 (1).exe" out of a browser download.
func listBridgeProcesses(self uint32) ([]procInfo, error) {
	all, err := snapshotProcesses()
	if err != nil {
		return nil, err
	}
	var out []procInfo
	for _, p := range all {
		if p.pid == self || !strings.HasPrefix(strings.ToLower(p.name), "esbm-bridge") {
			continue
		}
		p.path = processPath(p.pid)
		out = append(out, p)
	}
	return out, nil
}

// parentOf returns pid's parent PID, or 0.
func parentOf(pid uint32) uint32 {
	all, err := snapshotProcesses()
	if err != nil {
		return 0
	}
	for _, p := range all {
		if p.pid == pid {
			return p.parent
		}
	}
	return 0
}

// processPath resolves a PID's full image path. Best effort: a process
// we cannot open still gets closed by PID, we just can't name the file.
func processPath(pid uint32) string {
	h, err := windows.OpenProcess(
		windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, maxPathBuf)
	n := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &n); err != nil {
		return ""
	}
	return windows.UTF16ToString(buf[:n])
}

// processAlive reports whether pid is a live process we can see.
func processAlive(pid uint32) bool {
	h, err := windows.OpenProcess(
		windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, pid)
	if err != nil {
		// Access denied means it exists; anything else means it is gone.
		return errors.Is(err, windows.ERROR_ACCESS_DENIED)
	}
	defer windows.CloseHandle(h)
	ev, _ := windows.WaitForSingleObject(h, 0)
	return ev == uint32(windows.WAIT_TIMEOUT)
}

// killPID terminates one process and waits for the kernel to report it
// gone, so the caller knows its listening sockets are released before
// it checks the ports. alreadyGone is true when the process had exited
// by the time we got to it — not an error, and not something we closed.
func killPID(pid uint32) (alreadyGone bool, err error) {
	h, err := windows.OpenProcess(
		windows.PROCESS_TERMINATE|windows.SYNCHRONIZE, false, pid)
	if err != nil {
		// A PID that no longer exists fails with INVALID_PARAMETER.
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return true, nil
		}
		return false, err
	}
	defer windows.CloseHandle(h)

	if ev, _ := windows.WaitForSingleObject(h, 0); ev == windows.WAIT_OBJECT_0 {
		return true, nil // signalled already: exited between snapshot and now
	}
	if err := windows.TerminateProcess(h, 1); err != nil {
		// Lost the race with its own exit.
		if ev, _ := windows.WaitForSingleObject(h, 0); ev == windows.WAIT_OBJECT_0 {
			return true, nil
		}
		return false, err
	}
	// TerminateProcess is asynchronous; 10s is generous.
	if ev, _ := windows.WaitForSingleObject(h, 10_000); ev != windows.WAIT_OBJECT_0 {
		return false, fmt.Errorf("o processo não encerrou em 10s")
	}
	return false, nil
}

// ---------------------------------------------------------------------
// Ports
// ---------------------------------------------------------------------

// waitPortsFree blocks until nothing is listening on any Bridge port,
// returning the ones that never came free.
func waitPortsFree(log *slog.Logger, d time.Duration) []string {
	deadline := time.Now().Add(d)
	for {
		var stuck []string
		for _, p := range bridgePorts {
			if portBusy(p.addr) {
				stuck = append(stuck, p.what)
			}
		}
		if len(stuck) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			log.Warn("ports still in use", "ports", stuck)
			return stuck
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// portBusy reports whether something accepts connections on addr.
//
// It DIALS rather than binds. Binding 0.0.0.0 from a freshly downloaded
// .exe makes Windows Defender Firewall pop a "blocked some features of
// this app" prompt in the middle of the install; a loopback dial opens
// no listening socket at all. A wildcard listener (which is how every
// Bridge binds) accepts on loopback, so this sees it.
func portBusy(addr string) bool {
	c, err := net.DialTimeout("tcp", addr, 1500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// ---------------------------------------------------------------------
// Binary placement
// ---------------------------------------------------------------------

// installedPath is where the shipped binary lives once installed.
func installedPath() string {
	base := os.Getenv("ProgramFiles")
	if base == "" {
		base = `C:\Program Files`
	}
	return filepath.Join(base, installDirName, "esbm-bridge.exe")
}

// stageSelf copies the running .exe next to its final location as
// esbm-bridge.exe.new, flushed to disk. It touches nothing that is in
// use, so it is safe to run before any service is stopped. tmp is ""
// when we are already running from the install location (nothing to
// swap in).
func stageSelf(log *slog.Logger) (tmp, dst string, err error) {
	src, err := os.Executable()
	if err != nil {
		return "", "", err
	}
	src, _ = filepath.Abs(src)
	dst = installedPath()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", "", fmt.Errorf("mkdir %s: %w", filepath.Dir(dst), err)
	}

	// Running the installed copy again is a no-op, not a copy onto
	// itself. os.SameFile compares file identity, so 8.3 short names
	// and differently-spelled paths to the same file are caught too.
	if si, e1 := os.Stat(src); e1 == nil {
		if di, e2 := os.Stat(dst); e2 == nil && os.SameFile(si, di) {
			log.Info("already running from the install location", "path", dst)
			return "", dst, nil
		}
	}

	tmp = dst + ".new"
	_ = os.Remove(tmp)
	if err := copyFileSync(src, tmp); err != nil {
		_ = os.Remove(tmp)
		return "", "", err
	}
	log.Info("binary staged", "from", src, "to", tmp)
	return tmp, dst, nil
}

// commitSelf swaps the staged copy into place. Called once every old
// Bridge has been stopped.
func commitSelf(log *slog.Logger, tmp, dst string) error {
	if tmp == "" {
		return nil
	}
	// Straight replace first: os.Rename replaces an existing file
	// atomically, so the service's binary is never missing. It only
	// fails when dst is still locked as a running image.
	if err := renameRetry(tmp, dst, 3*time.Second); err == nil {
		log.Info("binary installed", "path", dst)
		return nil
	}
	// Windows refuses to overwrite a running image but allows renaming
	// it, so an old copy still held by a process we could not kill is
	// moved aside rather than blocking the install.
	aside := dst + ".old"
	_ = os.Remove(aside)
	if err := renameRetry(dst, aside, 5*time.Second); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("replace %s: %w", dst, err)
	}
	if err := renameRetry(tmp, dst, 5*time.Second); err != nil {
		_ = os.Rename(aside, dst) // put the previous binary back
		_ = os.Remove(tmp)
		return fmt.Errorf("install %s: %w", dst, err)
	}
	_ = os.Remove(aside) // best effort; fails harmlessly if still locked
	log.Info("binary installed (previous copy moved aside)", "path", dst)
	return nil
}

// renameRetry is os.Rename that rides out the short sharing violations
// an antivirus or the indexer causes while scanning a just-written .exe.
func renameRetry(from, to string, d time.Duration) error {
	deadline := time.Now().Add(d)
	for {
		err := os.Rename(from, to)
		if err == nil {
			return nil
		}
		var en syscall.Errno
		transient := errors.As(err, &en) &&
			(en == windows.ERROR_ACCESS_DENIED || en == windows.ERROR_SHARING_VIOLATION)
		if !transient || time.Now().After(deadline) {
			return err
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// restoreOldService is the rollback for a repair that failed after it
// had already stopped a registered service: re-arm the restart policy
// we cleared and start it again, so a failed upgrade does not leave a
// PC that was working without any Bridge at all.
func restoreOldService(log *slog.Logger, r *report, hadService bool) {
	if !hadService {
		return
	}
	m, err := mgr.Connect()
	if err != nil {
		return
	}
	defer m.Disconnect()
	s, err := m.OpenService(winservice.ServiceName)
	if err != nil {
		return
	}
	defer s.Close()
	_ = s.SetRecoveryActions([]mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 30 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 5 * time.Minute},
	}, 86400)
	if err := s.Start(); err != nil && !errors.Is(err, windows.ERROR_SERVICE_ALREADY_RUNNING) {
		log.Warn("could not restart the previously installed service", "err", err)
		return
	}
	log.Info("previously installed service restarted after a failed repair")
	r.warn("o Bridge que já estava instalado antes foi religado")
}

// copyFileSync copies src to dst and flushes it to disk before
// returning, so a power cut cannot leave a truncated executable behind
// a service registration.
func copyFileSync(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// ---------------------------------------------------------------------
// Autostart leftovers
// ---------------------------------------------------------------------

// cleanUserAutostart removes per-user launchers for the Bridge.
func cleanUserAutostart(log *slog.Logger) []string {
	var removed []string
	removed = append(removed, cleanRunKey(log, registry.CURRENT_USER, "HKCU")...)
	if appdata := os.Getenv("APPDATA"); appdata != "" {
		removed = append(removed, cleanStartupDir(log, filepath.Join(appdata,
			`Microsoft\Windows\Start Menu\Programs\Startup`))...)
	}
	return removed
}

// cleanMachineAutostart removes machine-wide launchers. Needs admin.
func cleanMachineAutostart(log *slog.Logger) []string {
	var removed []string
	removed = append(removed, cleanRunKey(log, registry.LOCAL_MACHINE, "HKLM")...)
	if pd := os.Getenv("ProgramData"); pd != "" {
		removed = append(removed, cleanStartupDir(log, filepath.Join(pd,
			`Microsoft\Windows\Start Menu\Programs\Startup`))...)
	}
	// Scheduled tasks: only the two exact names we have ever created, by
	// name, so a typo can never delete somebody else's task.
	for _, tn := range []string{"ESBM Bridge", "ESBMBridge"} {
		if err := runQuiet("schtasks", "/Delete", "/TN", tn, "/F"); err == nil {
			removed = append(removed, "tarefa agendada \""+tn+"\"")
			log.Info("scheduled task removed", "name", tn)
		}
	}
	return removed
}

// isBridgeLauncherName reports whether a file name is a Bridge launcher.
// "ESBM" alone is the store's own name — the ESBM app, the POS and
// anything else of theirs may sit in the same Startup folder — so the
// name has to say "bridge" as well.
func isBridgeLauncherName(name string) bool {
	n := strings.ToLower(name)
	return strings.Contains(n, "esbm") && strings.Contains(n, "bridge")
}

// cleanRunKey deletes Run values that point at a Bridge binary. Scoped
// hard: the value's data must mention esbm-bridge.
func cleanRunKey(log *slog.Logger, root registry.Key, label string) []string {
	const path = `Software\Microsoft\Windows\CurrentVersion\Run`
	k, err := registry.OpenKey(root, path, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		log.Debug("run key not accessible", "hive", label, "err", err)
		return nil
	}
	defer k.Close()

	names, err := k.ReadValueNames(0)
	if err != nil {
		return nil
	}
	var removed []string
	for _, n := range names {
		v, _, err := k.GetStringValue(n)
		if err != nil || !strings.Contains(strings.ToLower(v), "esbm-bridge") {
			continue
		}
		if err := k.DeleteValue(n); err != nil {
			log.Warn("could not delete run value", "hive", label, "name", n, "err", err)
			continue
		}
		removed = append(removed, label+`\Run\`+n)
		log.Info("run value removed", "hive", label, "name", n, "data", v)
	}
	return removed
}

// cleanStartupDir removes Startup-folder shortcuts for the Bridge.
func cleanStartupDir(log *slog.Logger, dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var removed []string
	for _, e := range entries {
		if e.IsDir() || !isBridgeLauncherName(e.Name()) {
			continue
		}
		switch strings.ToLower(filepath.Ext(e.Name())) {
		case ".lnk", ".url", ".bat", ".cmd", ".exe":
		default:
			continue
		}
		full := filepath.Join(dir, e.Name())
		if err := os.Remove(full); err != nil {
			log.Warn("could not remove startup entry", "path", full, "err", err)
			continue
		}
		removed = append(removed, full)
		log.Info("startup entry removed", "path", full)
	}
	return removed
}

// ---------------------------------------------------------------------
// Cloud breadcrumb
// ---------------------------------------------------------------------

// sendFixEvent tells esbm-app which machine was repaired and what was
// closed. Best effort and deliberately last: a cloud hiccup must never
// turn a successful install into a reported failure.
func sendFixEvent(log *slog.Logger, cfg *config.Config, exePath string, killed []procInfo, tunnelUp bool) {
	host, _ := os.Hostname()
	var closed []string
	for _, p := range killed {
		where := p.path
		if where == "" {
			where = p.name
		}
		closed = append(closed, where)
	}
	msg := fmt.Sprintf("installed as a Windows service on %s (%s)", host, version)
	if len(closed) > 0 {
		msg += fmt.Sprintf("; closed %d stale bridge(s)", len(closed))
	}
	sev := "info"
	if !tunnelUp {
		msg += "; tunnel had NOT reached the cloud when the installer finished"
		sev = "warn"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	err := api.New(cfg.EsbmAppURL, cfg.BridgeJWT).SendEvent(ctx, api.Event{
		Type:     "bridge_installed",
		Severity: sev,
		Message:  msg,
		Context: map[string]any{
			"hostname":        host,
			"version":         version,
			"exe":             exePath,
			"closed":          closed,
			"cloud_reachable": tunnelUp,
		},
	})
	if err != nil {
		log.Warn("could not report the install to esbm-app (non-fatal)", "err", err)
		return
	}
	log.Info("install reported to esbm-app", "hostname", host)
}
