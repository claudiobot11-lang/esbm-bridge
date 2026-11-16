// registerService / cmdUninstall — the Windows service plumbing: the
// Bridge auto-starts on boot, restarts on crash, and survives user
// logouts. Also installs Windows Firewall rules so the in-store gateway
// can reach 9071/9080 from the LAN.
//
// `esbm-bridge install` no longer lands here directly: it runs cmdFix
// (heal_windows.go), which closes every stale Bridge first and then
// calls registerService. All of it needs admin; cmdFix self-elevates.
//
//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/claudiobot11-lang/esbm-bridge/internal/winservice"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// firewallRules are the inbound TCP allow rules the Bridge needs so
// the in-store gateway can connect from the LAN. We name them with a
// "ESBM Bridge — " prefix so uninstall finds them all reliably.
var firewallRules = []struct {
	name string
	port int
}{
	{"ESBM Bridge — Cronus (TCP 9071)", 9071},
	{"ESBM Bridge — eStation (TCP 9080)", 9080},
}

// registerService points the ESBMBridge service at exePath and starts
// it. Callers must already be elevated and must already have stopped
// any previous instance (see cmdFix, the only entry point).
//
// An existing registration is RECONFIGURED in place rather than deleted
// and recreated. DeleteService merely marks the entry; Windows keeps it
// until every open handle to it closes — an operator with services.msc
// open is enough — and CreateService fails with ERROR_SERVICE_MARKED_
// FOR_DELETE in the meantime. ChangeServiceConfig has no such trap.
//
// warns carries the non-fatal shortfalls (no restart policy, no
// firewall rule) so the installer's summary cannot promise something
// that was not actually set up.
func registerService(log *slog.Logger, exePath string) (warns []string, err error) {
	m, err := mgr.Connect()
	if err != nil {
		return nil, fmt.Errorf("connect to service manager (needs admin): %w", err)
	}
	defer m.Disconnect()

	// The binary takes one argument: `run`, the same command an operator
	// would invoke interactively. main() flips into SCM mode when it is
	// launched without a controlling terminal. CreateService builds the
	// same `"<path>" run` string itself from exePath + args.
	cfg := mgr.Config{
		ServiceType:      windows.SERVICE_WIN32_OWN_PROCESS,
		StartType:        mgr.StartAutomatic,
		ErrorControl:     mgr.ErrorNormal,
		DisplayName:      winservice.DisplayName,
		Description:      winservice.Description,
		BinaryPathName:   fmt.Sprintf(`"%s" run`, exePath),
		DelayedAutoStart: false, // Boot fast — the gateway is waiting.
	}

	// A registration that an earlier uninstall left "marked for
	// deletion" rejects every call until its last handle closes. That
	// usually takes a moment, so retry briefly before giving up with
	// the specific error (the caller turns it into a plain instruction).
	var s *mgr.Service
	deadline := time.Now().Add(12 * time.Second)
	for {
		s, err = openOrCreate(log, m, cfg, exePath)
		if err == nil || !errors.Is(err, windows.ERROR_SERVICE_MARKED_FOR_DELETE) ||
			time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Second)
	}
	if err != nil {
		return nil, err
	}
	defer s.Close()

	// Auto-restart policy: if the process dies, SCM relaunches it after
	// 5s, 30s, 5min for the first three failures, then keeps retrying
	// every 5min. Reset the failure counter daily. (A worker that merely
	// RETURNS is re-run inside the service — see winservice.Execute.)
	if err := s.SetRecoveryActions([]mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 30 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 5 * time.Minute},
	}, 86400); err != nil {
		log.Warn("could not set recovery actions", "err", err)
		warns = append(warns, "não consegui ligar o reinício automático em caso de queda")
	}

	// Firewall rules. The gateway connects FROM the LAN, so we need
	// inbound TCP allow on 9071 + 9080. Scoped to this exe to keep the
	// rule narrow even if someone else listens on the same ports.
	if failed := installFirewallRules(log, exePath); len(failed) > 0 {
		warns = append(warns, fmt.Sprintf("não consegui liberar no firewall "+
			"do Windows a(s) porta(s) %s — se o Windows perguntar se permite "+
			"o \"esbm-bridge\", responda Permitir", strings.Join(failed, ", ")))
	}

	// Start now so the gateway reconnects immediately — no reboot, no
	// manual "Start" click in services.msc.
	if err := s.Start(); err != nil && !errors.Is(err, windows.ERROR_SERVICE_ALREADY_RUNNING) {
		return warns, fmt.Errorf("start service: %w", err)
	}
	log.Info("Bridge registered as a Windows service",
		"name", winservice.ServiceName, "exe", exePath)
	return warns, nil
}

// openOrCreate returns a handle to the service configured for exePath:
// the existing registration, stopped and reconfigured, or a new one.
func openOrCreate(log *slog.Logger, m *mgr.Mgr, cfg mgr.Config, exePath string) (*mgr.Service, error) {
	s, err := m.OpenService(winservice.ServiceName)
	if err != nil {
		log.Info("creating the service", "exe", exePath)
		s, err = m.CreateService(winservice.ServiceName, exePath, cfg, "run")
		if err != nil {
			return nil, fmt.Errorf("create service: %w", err)
		}
		return s, nil
	}
	log.Info("reconfiguring the existing service", "exe", exePath)
	// It must be fully STOPPED before we start it again: StartService
	// on a service that is still running or stopping either fails or —
	// worse — is satisfied by the OLD image, and the new path we just
	// configured would only take effect at the next boot.
	if err := stopService(s); err != nil {
		log.Warn("existing service not confirmed stopped before reconfigure", "err", err)
	}
	if err := s.UpdateConfig(cfg); err != nil {
		s.Close()
		return nil, fmt.Errorf("update service config: %w", err)
	}
	return s, nil
}

func cmdUninstall(log *slog.Logger, _ []string) int {
	m, err := mgr.Connect()
	if err != nil {
		log.Error("connect to service manager failed (run as Administrator)", "err", err)
		return 1
	}
	defer m.Disconnect()

	s, err := m.OpenService(winservice.ServiceName)
	if err != nil {
		log.Warn("service not registered — nothing to uninstall", "err", err)
	} else {
		log.Info("stopping service")
		if err := stopService(s); err != nil {
			log.Warn("stop service failed (will try delete anyway)", "err", err)
		}
		if err := s.Delete(); err != nil {
			log.Error("delete service failed", "err", err)
			s.Close()
			return 1
		}
		s.Close()
		log.Info("service removed")
	}

	// Firewall cleanup runs unconditionally — leftover rules from a
	// half-failed install would otherwise stay forever.
	removeFirewallRules(log)
	fmt.Println("✓ ESBM Bridge service + firewall rules removed.")
	return 0
}

// stopService asks SCM to stop the service and waits for it to report
// Stopped. Returns nil only when the service IS stopped.
//
// It used to treat every ControlService error as "already stopped",
// which is true for exactly one of them (ERROR_SERVICE_NOT_ACTIVE). A
// service still in START_PENDING, or one whose handler is wedged,
// rejects the Stop with a different error and is very much alive.
func stopService(s *mgr.Service) error {
	st, err := s.Query()
	if err != nil {
		return err
	}
	if st.State == svc.Stopped {
		return nil
	}
	// The Control error is deliberately not returned. If Stop cannot be
	// delivered (a pending state, a hung handler) we still poll: the
	// caller may have killed the process, in which case SCM flips the
	// service to Stopped on its own — and if it never does, the timeout
	// below reports that honestly.
	_, _ = s.Control(svc.Stop)
	deadline := time.Now().Add(15 * time.Second)
	for {
		st, err = s.Query()
		if err != nil {
			return err
		}
		if st.State == svc.Stopped {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("stop timeout (current state %d)", st.State)
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// installFirewallRules adds the inbound allow rules and returns the
// ports it could NOT open, so the caller can say so.
func installFirewallRules(log *slog.Logger, exePath string) (failed []string) {
	// Best-effort: a failure does not abort the install. If the firewall
	// is disabled or the rule add fails, Windows asks the operator the
	// first time the service listens.
	for _, r := range firewallRules {
		// Remove any prior rule with the same name first so re-install
		// doesn't end up with two identical rules (or one that still
		// points at the old download path).
		_ = runQuiet("netsh", "advfirewall", "firewall", "delete", "rule",
			"name="+r.name)
		args := []string{
			"advfirewall", "firewall", "add", "rule",
			"name=" + r.name,
			"dir=in", "action=allow",
			"protocol=TCP",
			fmt.Sprintf("localport=%d", r.port),
			"program=" + exePath,
			"profile=any", "enable=yes",
			"description=ESBM Bridge — allow in-store BLE gateway",
		}
		if err := runQuiet("netsh", args...); err != nil {
			log.Warn("firewall rule add failed (non-fatal)",
				"port", r.port, "err", err)
			failed = append(failed, fmt.Sprint(r.port))
		} else {
			log.Info("firewall rule added", "port", r.port, "name", r.name)
		}
	}
	return failed
}

func removeFirewallRules(log *slog.Logger) {
	for _, r := range firewallRules {
		if err := runQuiet("netsh", "advfirewall", "firewall", "delete",
			"rule", "name="+r.name); err != nil {
			// Silent — rule may not have been installed.
			log.Debug("firewall rule remove (probably already gone)",
				"port", r.port, "err", err)
		}
	}
}

// runQuiet executes a helper (netsh, schtasks) and returns its error,
// discarding output. Two things matter now that the caller is a GUI
// process with no console: without CREATE_NO_WINDOW every helper pops
// its own black console window over the progress dialog, and without a
// timeout a wedged firewall service would hang the install forever at a
// point where the old Bridge is already stopped.
func runQuiet(name string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: windows.CREATE_NO_WINDOW,
	}
	return cmd.Run()
}
