// Package tray puts an ESBM Bridge icon in the Windows system tray.
// Green when everything is healthy, yellow when at least one worker
// is restarting, red when the gateway is offline.
//
// Right-click menu:
//   - Open dashboard      → spawns the default browser at :9099
//   - Restart proxies     → calls dashboard's onRestart hook
//   - Quit                → cancels the parent ctx
//
//go:build windows

package tray

import (
	"context"
	"log/slog"
	"os/exec"
	"time"

	"fyne.io/systray"
	"github.com/claudiobot11-lang/esbm-bridge/internal/dashboard"
)

// Status is what the tray icon renders. Pull from the same source the
// dashboard uses so the two never disagree.
type Status struct {
	GatewayOnline bool
	WorkerIssue   bool // any worker is stopped or in backoff
}

// StatusFn is called on every tick so the tray icon can color itself
// without the caller having to push updates.
type StatusFn func() Status

// RestartFn is invoked when the operator clicks "Restart proxies".
type RestartFn func()

// Options configures Run.
type Options struct {
	Logger        *slog.Logger
	DashboardAddr string // shown in tooltip, used by "Open dashboard"
	Version       string // shown in tooltip + About menu
	StatusFn      StatusFn
	OnRestart     RestartFn
	OnQuit        func()
}

// Run blocks the calling goroutine. Must be invoked from main() with
// the GUI thread reservation that systray needs on Windows.
func Run(opts Options) {
	systray.Run(
		func() { onReady(opts) },
		func() {
			if opts.OnQuit != nil {
				opts.OnQuit()
			}
		},
	)
}

// Quit asks the systray loop to exit. Pair with Run when the parent
// ctx is cancelled.
func Quit() { systray.Quit() }

func onReady(opts Options) {
	systray.SetTitle("ESBM Bridge")
	systray.SetTooltip("ESBM Bridge " + opts.Version)
	systray.SetIcon(iconGreen)

	mOpen := systray.AddMenuItem("Open dashboard", "http://"+opts.DashboardAddr)
	mRestart := systray.AddMenuItem("Restart proxies", "Bounce the proxy + tunnel")
	systray.AddSeparator()
	mStatus := systray.AddMenuItem("Status: starting…", "")
	mStatus.Disable()
	systray.AddSeparator()
	mQuit := systray.AddMenuItem("Quit", "Stop the Bridge service")

	go func() {
		for {
			select {
			case <-mOpen.ClickedCh:
				_ = openURL("http://" + opts.DashboardAddr)
			case <-mRestart.ClickedCh:
				if opts.OnRestart != nil {
					opts.OnRestart()
				}
			case <-mQuit.ClickedCh:
				systray.Quit()
				return
			}
		}
	}()

	go func() {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for range t.C {
			if opts.StatusFn == nil {
				continue
			}
			s := opts.StatusFn()
			switch {
			case !s.GatewayOnline:
				systray.SetIcon(iconRed)
				mStatus.SetTitle("Status: gateway offline")
			case s.WorkerIssue:
				systray.SetIcon(iconYellow)
				mStatus.SetTitle("Status: degraded (restarting…)")
			default:
				systray.SetIcon(iconGreen)
				mStatus.SetTitle("Status: healthy")
			}
		}
	}()
}

func openURL(url string) error {
	// `rundll32 url.dll,FileProtocolHandler` is the most reliable way
	// to open the default browser on Windows without forking cmd.exe.
	return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
}

// RunBridge wires the tray to the dashboard so the menu reflects the
// same status the web UI sees. Convenience for main().
func RunBridge(ctx context.Context, log *slog.Logger, version string, srv *dashboard.Server, onRestart func()) {
	addr := dashboard.DefaultAddr
	go func() {
		<-ctx.Done()
		systray.Quit()
	}()
	Run(Options{
		Logger:        log,
		DashboardAddr: addr,
		Version:       version,
		StatusFn:      func() Status { return statusFromDashboard(srv) },
		OnRestart:     onRestart,
	})
}

func statusFromDashboard(srv *dashboard.Server) Status {
	// Use the dashboard's atomic snapshot — no extra plumbing needed.
	st := pullStatus(srv)
	gwOnline := !st.GatewayLastSeen.IsZero() && time.Since(st.GatewayLastSeen) < 90*time.Second
	worker := false
	for _, w := range st.Workers {
		if w.State == "backoff" || w.State == "stopped" {
			worker = true
			break
		}
	}
	return Status{GatewayOnline: gwOnline, WorkerIssue: worker}
}

// pullStatus reads the dashboard's current snapshot. We define it
// here so the dashboard package doesn't need to export its mu.
func pullStatus(srv *dashboard.Server) dashboard.Status {
	type snapshotter interface{ Snapshot() dashboard.Status }
	if s, ok := any(srv).(snapshotter); ok {
		return s.Snapshot()
	}
	return dashboard.Status{}
}
