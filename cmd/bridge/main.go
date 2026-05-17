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
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/claudiobot11-lang/esbm-bridge/internal/api"
	"github.com/claudiobot11-lang/esbm-bridge/internal/config"
	"github.com/claudiobot11-lang/esbm-bridge/internal/proxy"
	"github.com/claudiobot11-lang/esbm-bridge/internal/tunnel"
)

// version is stamped at build time via -ldflags "-X main.version=…"
// Heartbeats include it so the admin dashboard can flag stale Bridges.
var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	switch cmd {
	case "pair":
		os.Exit(cmdPair(log, args))
	case "run":
		os.Exit(cmdRun(log, args))
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
  esbm-bridge pair   --code CODE [--server URL]   Pair with esbm-app once
  esbm-bridge run                                  Start the bridge service
  esbm-bridge status                               Show paired state
  esbm-bridge version                              Print version

Pairing codes come from the esbm-app /esl/stores page.
`, version)
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
	cfg, err := config.Load()
	if err != nil {
		log.Error("not paired", "err", err)
		fmt.Fprintln(os.Stderr, "Run `esbm-bridge pair --code …` first.")
		return 1
	}
	cfg.Defaults()

	ctx, cancel := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Bring up Tailscale first — the proxy needs the Dialer.
	t := &tunnel.Tunnel{
		Hostname: cfg.Hostname,
		AuthKey:  cfg.TailscaleAuthKey,
		StateDir: cfg.TailscaleStateDir,
		Logger:   log,
	}
	if err := t.Start(ctx); err != nil {
		log.Error("tailscale failed to start", "err", err)
		return 1
	}
	defer t.Close()

	// Local listener for the gateway. The eRetail MQTT broker on the
	// cloud listens on 9071, so we mirror the port locally — gateway
	// can keep its default config.
	srv := &proxy.Server{
		Listen:   "0.0.0.0:9071",
		Upstream: cfg.ServerAddr,
		Dialer:   t,
		Logger:   log,
	}

	// Heartbeats in parallel with the proxy. Both share ctx so
	// Ctrl-C cleanly shuts everything down.
	cli := api.New(cfg.EsbmAppURL, cfg.BridgeJWT)
	go heartbeatLoop(ctx, log, cli, srv)

	log.Info("bridge running", "shop_code", cfg.ShopCode, "version", version)
	if err := srv.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("proxy stopped with error", "err", err)
		return 1
	}
	log.Info("bridge stopped cleanly")
	return 0
}

// heartbeatLoop fires once at startup so the dashboard flips to
// green immediately, then every 60s. Failures are logged but never
// kill the proxy — the gateway → cloud path is what matters.
func heartbeatLoop(ctx context.Context, log *slog.Logger, cli *api.Client, srv *proxy.Server) {
	start := time.Now()
	tick := time.NewTicker(60 * time.Second)
	defer tick.Stop()

	send := func() {
		c, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		err := cli.SendHeartbeat(c, api.Heartbeat{
			UptimeSec:       int64(time.Since(start).Seconds()),
			Version:         version,
			MQTTMsgsLastMin: srv.MsgsLastMin(),
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
