//go:build windows

// GUI setup path — the "no cmd" experience for store operators. When
// the .exe is double-clicked from Explorer (no console attached), all
// interaction happens through native Windows dialogs (zenity): the
// operator types the pairing code in a popup and confirms install with
// a UAC prompt. They never see cmd.exe.
package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/claudiobot11-lang/esbm-bridge/internal/api"
	"github.com/claudiobot11-lang/esbm-bridge/internal/config"
	"github.com/claudiobot11-lang/esbm-bridge/internal/dashboard"
	"github.com/ncruces/zenity"
	"golang.org/x/sys/windows"
)

const guiServerDefault = "https://app.estacionsanbrunomarket.com"

// cmdSetupGUI runs the double-click-friendly setup with native dialogs.
// Returns a process exit code. Flow:
//  1. Already paired → repair + install (cmdOpenPaired), or just show
//     status if this exact version is already installed and running.
//  2. Not paired → popup for the 6-digit code → pair → save.
//  3. Offer "install as service" → self-elevate (UAC) → background service.
//     If declined, run in tray for this session.
func cmdSetupGUI(log *slog.Logger) int {
	if _, err := config.Load(); err == nil {
		return cmdOpenPaired(log)
	}

	code, err := zenity.Entry(
		"Cole o código de pareamento de 6 dígitos.\n\n"+
			"Pegue em app.estacionsanbrunomarket.com → /esl/stores → \"Add Store\".\n"+
			"O código expira em 10 minutos.",
		zenity.Title("ESBM Bridge — Configuração"),
	)
	if err != nil {
		// Operator cancelled the dialog — nothing to do.
		return 0
	}
	code = strings.TrimSpace(code)
	if code == "" {
		_ = zenity.Error("Nenhum código informado.", zenity.Title("ESBM Bridge"))
		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	pr, perr := api.New(guiServerDefault, "").Pair(ctx, code)
	cancel()
	if perr != nil {
		_ = zenity.Error("Falha no pareamento:\n\n"+perr.Error()+
			"\n\nConfira o código e a internet, e tente de novo.",
			zenity.Title("ESBM Bridge"))
		return 1
	}

	cfg := &config.Config{
		BridgeJWT:        pr.JWT,
		TailscaleAuthKey: pr.TailscaleAuthKey,
		ShopCode:         pr.ShopCode,
		ServerAddr:       pr.ServerAddr,
		EsbmAppURL:       guiServerDefault,
	}
	cfg.Defaults()
	if err := cfg.Save(); err != nil {
		_ = zenity.Error("Falha ao salvar a configuração:\n\n"+err.Error(),
			zenity.Title("ESBM Bridge"))
		return 1
	}
	log.Info("paired via GUI", "shop_code", cfg.ShopCode)

	// Offer the unattended install (recommended). zenity.Question
	// returns nil when the operator clicks the OK button.
	q := zenity.Question(
		"Pareado como \""+cfg.ShopCode+"\". \n\n"+
			"Instalar para rodar SEMPRE em segundo plano "+
			"(inicia no boot e reinicia sozinho)?\n\n"+
			"O Windows vai pedir permissão de administrador.",
		zenity.Title("ESBM Bridge"),
		zenity.OKLabel("Instalar"),
		zenity.CancelLabel("Agora não"),
	)
	if q == nil {
		// cmdFix elevates, installs, and shows the result itself.
		return cmdFix(log, []string{"--dialog"}, false)
	}

	// Declined install → run in tray so it works at least this session.
	_ = zenity.Info(
		"OK. O Bridge vai rodar enquanto esta janela/ícone estiver aberto.\n\n"+
			"Pra rodar sempre, abra o programa de novo e escolha Instalar.",
		zenity.Title("ESBM Bridge"))
	return cmdTray(log, nil)
}

// localBridge is the slice of the dashboard's /api/status we need to
// decide what the Bridge already running here is, and whether it works.
type localBridge struct {
	Version        string    `json:"version"`
	PID            int       `json:"pid"`
	StartedAt      time.Time `json:"started_at"`
	TailscaleReady bool      `json:"tailscale_ready"`
	CloudReachable bool      `json:"cloud_reachable"`
	Workers        []struct {
		Name      string    `json:"name"`
		State     string    `json:"state"`
		Restarts  int64     `json:"restarts"`
		LastError string    `json:"last_error"`
		StartedAt time.Time `json:"started_at"`
	} `json:"workers"`
}

// queryLocalBridge asks whatever Bridge is serving the local dashboard
// what it is. ok is false when nothing answers.
func queryLocalBridge() (lb localBridge, ok bool) {
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://" + dashboard.DefaultAddr + "/api/status")
	if err != nil {
		return lb, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return lb, false
	}
	if err := json.NewDecoder(resp.Body).Decode(&lb); err != nil {
		return lb, false
	}
	return lb, true
}

// samePath compares two Windows paths for the same file, textually.
func samePath(a, b string) bool {
	return a != "" && b != "" &&
		strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}

// cmdOpenPaired is what a double-click does on a PC that is already
// paired. It used to start a SECOND full Bridge in the tray, which
// fought the installed service for port 9071 and for the Tailscale
// identity — and it is how a store ended up with one Bridge running in
// a console window that died when the window closed.
//
// Now: unless this exact version is already installed and nothing else
// is running beside it, a double-click repairs and installs. So
// downloading a newer .exe and opening it is the whole upgrade.
func cmdOpenPaired(log *slog.Logger) int {
	// A status window is already up (from this file or another copy of
	// it). Its process is not a stale Bridge; point at it and leave.
	if mutexExists(statusMutexName) {
		_ = zenity.Info("O ESBM Bridge já está aberto.\n\n"+
			"Procure a janela \"ESBM Bridge\" na barra de tarefas.",
			zenity.Title("ESBM Bridge"))
		return 0
	}

	lb, answering := queryLocalBridge()
	others, _ := listBridgeProcesses(windows.GetCurrentProcessId())

	// Sort the other Bridge processes into: the installed service copy,
	// other launches of THIS very file (a second double-click, a status
	// window still open — not Bridges at all), and everything else,
	// which is by definition stale.
	me, _ := os.Executable()
	inst := installedPath()
	installed, stale := 0, 0
	for _, p := range others {
		switch {
		case samePath(p.path, inst):
			installed++
		case samePath(p.path, me):
			// a sibling launch of this installer; ignore
		case p.path == "":
			// Cannot read its path (it belongs to another account, as
			// the LocalSystem service does). If a Bridge of our version
			// answers on the dashboard, this is that service.
			installed++
		default:
			stale++
		}
	}
	healthy := answering && lb.Version == version && installed >= 1 && stale == 0
	if !healthy {
		log.Info("double-click on a paired PC: repairing + installing",
			"dashboard_answering", answering, "running_version", lb.Version,
			"installed_copies", installed, "stale", stale)
		return cmdFix(log, []string{"--dialog"}, false)
	}

	// Installed and current: say how it is actually doing — every link
	// of the chain, not just "the service exists".
	gateway := "escutando (9071 e 9080)"
	for _, w := range lb.Workers {
		if (w.Name == "proxy:9071" || w.Name == "proxy:9080") && w.State != "running" {
			gateway = "COM PROBLEMA na porta " + strings.TrimPrefix(w.Name, "proxy:")
			if w.LastError != "" {
				gateway += " (" + w.LastError + ")"
			}
		}
	}
	cloud := "funcionando"
	if !lb.CloudReachable {
		cloud = "SEM CONEXÃO"
		for _, w := range lb.Workers {
			if w.Name == "tunnel" && w.LastError != "" {
				cloud += " (" + w.LastError + ")"
			}
		}
	}
	head := "O Bridge está instalado e rodando neste PC."
	if !lb.CloudReachable || strings.HasPrefix(gateway, "COM PROBLEMA") {
		head = "O Bridge está instalado, MAS há um problema — me mande uma foto desta tela."
	}
	relS, _ := acquireMutex(statusMutexName)
	defer relS()
	// Bounded, so a window forgotten behind the browser does not make
	// every later launch answer "already open" forever.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	err := zenity.Question(
		head+"\n\n"+
			"Versão: "+lb.Version+"\n"+
			"Gateway: "+gateway+"\n"+
			"Conexão com a nuvem: "+cloud+"\n\n"+
			"Ele roda sozinho em segundo plano — não precisa deixar "+
			"nada aberto.\n"+
			"Painel completo: http://"+dashboard.DefaultAddr,
		zenity.Title("ESBM Bridge"),
		zenity.OKLabel("Reinstalar"),
		zenity.CancelLabel("Fechar"),
		zenity.DefaultCancel(),
		zenity.Context(ctx),
	)
	if err == nil {
		relS() // we are no longer just a status window
		return cmdFix(log, []string{"--dialog"}, false)
	}
	return 0
}
