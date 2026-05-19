// Command esbm-bridge is the Windows-first agent that bridges an
// in-store BLE gateway to the ESBM cloud eRetail. It runs in two
// modes:
//
//	esbm-bridge pair --code <6-digit>   one-shot: trade pairing code
//	                                     for JWT + Tailscale auth key
//	esbm-bridge run                      foreground service: connect
//	                                     Tailscale, listen on 9071,
//	                                     forward to cloud, heartbeat
//	esbm-bridge status                   read config, print current
//	                                     pairing state
//
// Windows service wrapper, tray UI, MSI installer all build on top
// of these three subcommands — they don't change what the binary does,
// only how it's launched.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/claudiobot11-lang/esbm-bridge/internal/api"
	"github.com/claudiobot11-lang/esbm-bridge/internal/config"
	"github.com/claudiobot11-lang/esbm-bridge/internal/dashboard"
	"github.com/claudiobot11-lang/esbm-bridge/internal/proxy"
	"github.com/claudiobot11-lang/esbm-bridge/internal/supervisor"
	"github.com/claudiobot11-lang/esbm-bridge/internal/tray"
	"github.com/claudiobot11-lang/esbm-bridge/internal/tunnel"
	"github.com/claudiobot11-lang/esbm-bridge/internal/winconsole"
	"github.com/claudiobot11-lang/esbm-bridge/internal/winservice"
	"io"
	"path/filepath"
)

// version is stamped at build time via -ldflags "-X main.version=…"
// Heartbeats include it so the admin dashboard can flag stale Bridges.
var version = "dev"

func main() {
	// The .exe is linked with -H windowsgui so double-clicking from
	// Explorer doesn't pop a cmd.exe window. When the operator DID
	// launch us from a terminal (running `esbm-bridge install` etc.)
	// AttachToParent hijacks the parent's stdout/stderr so CLI output
	// still appears as expected. Result: tray mode is GUI-only; CLI
	// mode looks like every other command-line tool.
	hasConsole := winconsole.AttachToParent()

	// Logs always go to a file in %ProgramData% so we can debug
	// crashes from a user session where the tray icon vanished
	// silently. When attached to a terminal, mirror them there too.
	logSinks := []io.Writer{openLogFile()}
	if hasConsole {
		logSinks = append(logSinks, os.Stderr)
	}
	log := slog.New(slog.NewTextHandler(io.MultiWriter(logSinks...),
		&slog.HandlerOptions{Level: slog.LevelInfo}))

	// Windows-Service mode: when launched by SCM there's no controlling
	// terminal, IsWindowsService() returns true, and we MUST call
	// svc.Run before anything else or SCM gives up after 30s and
	// marks the service as failed. The svc handler calls cmdRunCtx
	// which is the same work cmdRun does interactively — only the
	// shutdown signal source differs (SCM Stop vs Ctrl-C).
	if winservice.IsWindowsService() {
		if err := winservice.Run(log, cmdRunCtx); err != nil {
			log.Error("windows service exited with error", "err", err)
			os.Exit(1)
		}
		return
	}

	// Default command for double-click from Windows Explorer:
	//   - If we have a paired config already → start in tray mode
	//     (the operator probably wants to monitor the running bridge).
	//   - Otherwise fall back to `setup`, the interactive pairing
	//     wizard, so a first-time user can finish setup without
	//     touching cmd.exe.
	// From a real terminal we still want help() on bare invocation.
	if len(os.Args) < 2 {
		if launchedFromExplorer() {
			if _, err := config.Load(); err == nil {
				os.Exit(cmdTray(log, nil))
			}
			os.Exit(cmdSetup(log, nil))
		}
		usage()
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]

	switch cmd {
	case "setup":
		os.Exit(cmdSetup(log, args))
	case "pair":
		os.Exit(cmdPair(log, args))
	case "run":
		os.Exit(cmdRun(log, args))
	case "tray":
		os.Exit(cmdTray(log, args))
	case "install":
		os.Exit(cmdInstall(log, args))
	case "uninstall":
		os.Exit(cmdUninstall(log, args))
	case "status":
		os.Exit(cmdStatus(log, args))
	case "version", "-v", "--version":
		fmt.Println("esbm-bridge", version)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintln(os.Stderr, "unknown command:", cmd)
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `esbm-bridge %s

Usage:
  esbm-bridge setup                                Pair + start, interactive (the easy way)
  esbm-bridge pair      --code CODE [--server URL] Pair with esbm-app once (scripting)
  esbm-bridge tray                                 Run bridge + system tray icon + http://localhost:9099 dashboard
  esbm-bridge run                                  Start the bridge in the foreground (no tray)
  esbm-bridge install                              Install as a Windows service (admin)
  esbm-bridge uninstall                            Remove the Windows service (admin)
  esbm-bridge status                               Show paired state
  esbm-bridge version                              Print version

Pairing codes come from the esbm-app /esl/stores page.
Recommended flow on a store PC:
  1. esbm-bridge pair --code CODE
  2. (run cmd.exe as Administrator)  esbm-bridge install
  → service starts now, auto-starts on every boot, restarts on crash,
    and the Windows Firewall is opened for 9071 + 9080 automatically.
`, version)
}

// cmdSetup is the operator-friendly path: pair (if needed) then run.
// Designed so a non-technical user can double-click the .exe, type
// the pairing code, and end up with a running bridge — without ever
// touching cmd.exe themselves.
func cmdSetup(log *slog.Logger, args []string) int {
	fs := flag.NewFlagSet("setup", flag.ExitOnError)
	codeFlag := fs.String("code", "", "skip the prompt and use this pairing code")
	server := fs.String("server", "https://esbm-app-production.up.railway.app",
		"esbm-app base URL (override only for staging/dev)")
	_ = fs.Parse(args)

	reader := bufio.NewReader(os.Stdin)

	fmt.Println()
	fmt.Println("============================================================")
	fmt.Println("  ESBM Bridge", version, "— setup")
	fmt.Println("============================================================")

	// Step 1: pair if we don't already have a config.json.
	cfg, err := config.Load()
	if err != nil {
		code := strings.TrimSpace(*codeFlag)
		if code == "" {
			fmt.Println()
			fmt.Println("  Open the ESBM web app → /esl/stores → 'Add Store' to get")
			fmt.Println("  a 6-digit pairing code. Codes expire in 10 minutes.")
			fmt.Println()
			fmt.Print("  Pairing code: ")
			line, _ := reader.ReadString('\n')
			code = strings.TrimSpace(line)
		}
		if code == "" {
			fmt.Println("  No code entered — aborting.")
			holdForEnter(reader)
			return 2
		}
		fmt.Println("  Pairing with", *server, "…")
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		pr, perr := api.New(*server, "").Pair(ctx, code)
		cancel()
		if perr != nil {
			fmt.Println("  PAIR FAILED:", perr)
			holdForEnter(reader)
			return 1
		}
		cfg = &config.Config{
			BridgeJWT:        pr.JWT,
			TailscaleAuthKey: pr.TailscaleAuthKey,
			ShopCode:         pr.ShopCode,
			ServerAddr:       pr.ServerAddr,
			EsbmAppURL:       *server,
		}
		cfg.Defaults()
		if err := cfg.Save(); err != nil {
			fmt.Println("  SAVE FAILED:", err)
			holdForEnter(reader)
			return 1
		}
		fmt.Println("  Paired as", cfg.ShopCode)
	} else {
		fmt.Println()
		fmt.Println("  Already paired as", cfg.ShopCode, "(server", cfg.ServerAddr+")")
	}

	// Step 2: ask whether to run now. Default Yes for the obvious
	// happy path. Operator can answer "n" to just close — useful
	// when they're checking pairing status.
	fmt.Println()
	fmt.Print("  Start the bridge now? [Y/n]: ")
	answer, _ := reader.ReadString('\n')
	answer = strings.TrimSpace(strings.ToLower(answer))
	if answer == "n" || answer == "no" {
		fmt.Println()
		fmt.Println("  Not starting. Run `esbm-bridge run` later, or double-click again.")
		holdForEnter(reader)
		return 0
	}

	fmt.Println()
	fmt.Println("  Starting bridge — Ctrl+C to stop, or close this window.")
	fmt.Println("============================================================")
	fmt.Println()
	// After a fresh pair, drop into tray mode so the operator sees a
	// status icon + dashboard right away (the actual proxy work
	// happens in the goroutine cmdTray spawns).
	return cmdTray(log, nil)
}

// holdForEnter blocks until the operator presses Enter, used by the
// Explorer double-click flow so the console window doesn't slam shut
// before they can read the error.
func holdForEnter(r *bufio.Reader) {
	fmt.Println()
	fmt.Println("  Press Enter to close.")
	r.ReadString('\n')
}

func cmdPair(log *slog.Logger, args []string) int {
	fs := flag.NewFlagSet("pair", flag.ExitOnError)
	code := fs.String("code", "", "6-digit pairing code from /esl/stores (required)")
	server := fs.String("server", "https://esbm-app-production.up.railway.app",
		"esbm-app base URL (override only for staging/dev)")
	_ = fs.Parse(args)

	if *code == "" {
		fmt.Fprintln(os.Stderr, "pair requires --code")
		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cli := api.New(*server, "")
	pr, err := cli.Pair(ctx, *code)
	if err != nil {
		log.Error("pair failed", "err", err)
		return 1
	}

	cfg := &config.Config{
		BridgeJWT:        pr.JWT,
		TailscaleAuthKey: pr.TailscaleAuthKey,
		ShopCode:         pr.ShopCode,
		ServerAddr:       pr.ServerAddr,
		EsbmAppURL:       *server,
	}
	cfg.Defaults()
	if err := cfg.Save(); err != nil {
		log.Error("save config failed", "err", err)
		return 1
	}

	log.Info("paired", "shop_code", cfg.ShopCode, "server", cfg.ServerAddr,
		"hostname", cfg.Hostname)
	fmt.Println("OK — run `esbm-bridge run` to start.")
	return 0
}

func cmdStatus(_ *slog.Logger, _ []string) int {
	cfg, err := config.Load()
	if errors.Is(err, os.ErrNotExist) {
		fmt.Println("status: NOT PAIRED — run `esbm-bridge pair --code …` first")
		return 0
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "status: error reading config:", err)
		return 1
	}
	fmt.Println("status: PAIRED")
	fmt.Println("  shop_code   :", cfg.ShopCode)
	fmt.Println("  server      :", cfg.ServerAddr)
	fmt.Println("  hostname    :", cfg.Hostname)
	fmt.Println("  esbm_app    :", cfg.EsbmAppURL)
	return 0
}

func cmdRun(log *slog.Logger, _ []string) int {
	ctx, cancel := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	return cmdRunCtx(ctx, log)
}

// cmdRunCtx is the cancellable variant of cmdRun — same logic, but the
// caller supplies the lifetime context. Used by the Windows Service
// handler, which gets its Stop signal from the SCM rather than SIGINT.
func cmdRunCtx(ctx context.Context, log *slog.Logger) int {
	cfg, err := config.Load()
	if err != nil {
		log.Error("not paired", "err", err)
		fmt.Fprintln(os.Stderr, "Run `esbm-bridge pair --code …` first.")
		return 1
	}
	cfg.Defaults()

	// Dashboard fronts the status JSON + HTML on localhost. We expose
	// it before anything else so even if Tailscale fails to start the
	// operator can still see why.
	dash := dashboard.New(dashboard.DefaultAddr, log)
	dash.SetStatus(dashboard.Status{
		Version:   version,
		ShopCode:  cfg.ShopCode,
		ServerAddr: cfg.ServerAddr,
		StartedAt: time.Now(),
	})

	// Supervisor wraps each long-running goroutine with panic recovery
	// and exponential-backoff restart. A failure in any one component
	// no longer takes the whole Bridge down — the operator sees the
	// "restarting" state in the tray + dashboard and the worker comes
	// back on its own.
	supDash := supervisor.Run(ctx, log, "dashboard", dash.Run)

	// Bring up Tailscale next — the proxies need the Dialer.
	t := &tunnel.Tunnel{
		Hostname: cfg.Hostname,
		AuthKey:  cfg.TailscaleAuthKey,
		StateDir: cfg.TailscaleStateDir,
		Logger:   log,
	}
	if err := t.Start(ctx); err != nil {
		log.Error("tailscale failed to start", "err", err)
		// Don't return — keep the dashboard up so the operator can see
		// the failure. The supervisor for the proxies will sit in
		// "starting" forever, surfacing the issue in the UI.
		dash.SetStatus(dashboard.Status{
			Version: version, ShopCode: cfg.ShopCode, ServerAddr: cfg.ServerAddr,
			TailscaleReady: false,
		})
	}
	defer t.Close()

	// Local listeners for the gateway. Two ports because eRetail's
	// hardware speaks one of two protocols depending on generation:
	//   9071 — old Cronus (AP03) TCP, used by eRetail 3.1/3.2 SendServer
	//   9080 — newer eStation (AP04) MQTT, per D20 dev manual default
	// Both upstreams are the same cloud server addr — the gateway
	// picks whichever protocol it speaks.
	srv9080 := &proxy.Server{
		Listen:   "0.0.0.0:9080",
		Upstream: dialAddr(cfg.ServerAddr, 9080),
		Dialer:   t,
		Logger:   log,
	}
	srv9071 := &proxy.Server{
		Listen:   "0.0.0.0:9071",
		Upstream: dialAddr(cfg.ServerAddr, 9071),
		Dialer:   t,
		Logger:   log,
	}
	sup9071 := supervisor.Run(ctx, log, "proxy:9071", srv9071.Run)
	sup9080 := supervisor.Run(ctx, log, "proxy:9080", srv9080.Run)

	// Heartbeats in parallel with the proxies. Same supervisor
	// treatment — a transient network error during heartbeat shouldn't
	// take the bridge process down.
	cli := api.New(cfg.EsbmAppURL, cfg.BridgeJWT)
	supHB := supervisor.Run(ctx, log, "heartbeat", func(ctx context.Context) error {
		heartbeatLoop(ctx, log, cli, srv9080, srv9071)
		return nil
	})

	// Status pump: every 2s, refresh the dashboard with current
	// supervisor state. Cheap and gives the tray icon something live.
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				dash.SetStatus(dashboard.Status{
					Version:   version,
					ShopCode:  cfg.ShopCode,
					ServerAddr: cfg.ServerAddr,
					StartedAt: dash.Snapshot().StartedAt,
					TailscaleHostname: cfg.Hostname,
					TailscaleReady: t != nil,
					Workers: []supervisor.Status{
						supDash.Snapshot(),
						sup9071.Snapshot(),
						sup9080.Snapshot(),
						supHB.Snapshot(),
					},
				})
			}
		}
	}()

	log.Info("bridge running", "shop_code", cfg.ShopCode, "version", version,
		"listen", []string{"0.0.0.0:9071", "0.0.0.0:9080"},
		"dashboard", "http://"+dashboard.DefaultAddr)

	<-ctx.Done()
	log.Info("bridge stopped cleanly")
	return 0
}

// cmdTray runs the bridge with a system tray icon + local dashboard.
// Designed for the "I want to see what's happening" case — the
// service install handles the unattended-restart case. Tray must
// run on the GUI thread so we call it from main(); the actual work
// runs in cmdRunCtx via a goroutine.
func cmdTray(log *slog.Logger, _ []string) int {
	ctx, cancel := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Run the bridge in a goroutine; the tray loop owns the main thread.
	bridgeCtx, cancelBridge := context.WithCancel(ctx)
	done := make(chan int, 1)
	go func() { done <- cmdRunCtx(bridgeCtx, log) }()

	// Tray must run synchronously on main. RunBridge returns when the
	// user clicks Quit or systray itself receives a shutdown signal.
	dash := dashboard.New(dashboard.DefaultAddr, log)
	tray.RunBridge(ctx, log, version, dash, func() {
		cancelBridge()
		bridgeCtx, cancelBridge = context.WithCancel(ctx)
		go func() { done <- cmdRunCtx(bridgeCtx, log) }()
	})

	cancelBridge()
	select {
	case rc := <-done:
		return rc
	case <-time.After(5 * time.Second):
		log.Warn("bridge didn't shut down in 5s — forcing exit")
		return 0
	}
}

// openLogFile returns an append-mode handle to %ProgramData%\esbm-bridge\bridge.log
// (or $HOME/.local/share/esbm-bridge/bridge.log on dev builds). Falls
// back to io.Discard when neither path is writable so the process
// can't fail to start just because of a logging issue. Rotation is
// intentionally absent — the log volume is tiny (heartbeats + bind
// events) and operators are expected to delete the file occasionally
// if it grows past their comfort level.
func openLogFile() io.Writer {
	dir := ""
	if pd := os.Getenv("ProgramData"); pd != "" {
		dir = filepath.Join(pd, "esbm-bridge")
	} else if hd, _ := os.UserHomeDir(); hd != "" {
		dir = filepath.Join(hd, ".local", "share", "esbm-bridge")
	}
	if dir == "" {
		return io.Discard
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return io.Discard
	}
	f, err := os.OpenFile(filepath.Join(dir, "bridge.log"),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return io.Discard
	}
	return f
}

// dialAddr rewrites the upstream port — config.ServerAddr stores ONE
// "host:port" but we want different upstream ports per listener.
// Returns "<host>:<port>" with the host preserved.
func dialAddr(serverAddr string, port int) string {
	host := serverAddr
	if i := indexLastColon(serverAddr); i >= 0 {
		host = serverAddr[:i]
	}
	return fmt.Sprintf("%s:%d", host, port)
}

func indexLastColon(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == ':' {
			return i
		}
	}
	return -1
}

// heartbeatLoop fires once at startup so the dashboard flips to
// green immediately, then every 60s. Failures are logged but never
// kill the proxy — the gateway → cloud path is what matters.
func heartbeatLoop(ctx context.Context, log *slog.Logger, cli *api.Client, servers ...*proxy.Server) {
	start := time.Now()
	tick := time.NewTicker(60 * time.Second)
	defer tick.Stop()

	send := func() {
		c, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		// Sum MQTT byte counters across every listener — the
		// dashboard cares about traffic regardless of which port
		// the gateway happened to dial.
		var msgs int64
		for _, s := range servers {
			msgs += s.MsgsLastMin()
		}
		err := cli.SendHeartbeat(c, api.Heartbeat{
			UptimeSec:       int64(time.Since(start).Seconds()),
			Version:         version,
			MQTTMsgsLastMin: msgs,
			TailscaleOK:     true,
		})
		if err != nil {
			log.Warn("heartbeat failed", "err", err)
		}
	}
	send() // first beat — populate the dashboard ASAP
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			send()
		}
	}
}
