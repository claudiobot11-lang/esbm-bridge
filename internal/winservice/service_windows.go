// Package winservice wraps cmdRun so the Bridge can run as a native
// Windows service (SCM-managed, auto-start on boot, restart on crash).
//
// This file is Windows-only. The non-Windows build pulls in
// service_stub.go which provides no-op fallbacks so the cross-compile
// from macOS dev machines keeps working.
//
//go:build windows

package winservice

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"golang.org/x/sys/windows/svc"
)

// ServiceName is what shows up in services.msc and is used by
// `sc.exe query`. Keep it stable across versions so install/uninstall
// always target the right entry.
const ServiceName = "ESBMBridge"

// DisplayName is the human-readable name in services.msc.
const DisplayName = "ESBM Bridge"

// Description is shown in the service properties dialog.
const Description = "Bridges the in-store BLE gateway to the ESBM cloud " +
	"eRetail server over Tailscale."

// IsWindowsService reports whether the current process was launched by
// the Service Control Manager. Callers use this to decide between the
// CLI dispatcher and svc.Run.
func IsWindowsService() bool {
	is, err := svc.IsWindowsService()
	if err != nil {
		// Shouldn't happen on Windows — treat any error as "not a service".
		return false
	}
	return is
}

// Runner is the work the service performs. It must respect ctx — when
// the SCM sends Stop we cancel ctx and expect Runner to return.
// Returns 0 on clean shutdown, non-zero on error.
type Runner func(ctx context.Context, log *slog.Logger) int

// Run starts the service handler loop. Blocks until the SCM tells us
// to stop. Call this from main() when IsWindowsService() is true.
func Run(log *slog.Logger, runner Runner) error {
	h := &handler{log: log, runner: runner}
	if err := svc.Run(ServiceName, h); err != nil {
		return fmt.Errorf("svc.Run: %w", err)
	}
	return nil
}

type handler struct {
	log    *slog.Logger
	runner Runner
}

// Execute is the SCM callback. Pattern from
// https://pkg.go.dev/golang.org/x/sys/windows/svc#Handler. We start
// the worker in a goroutine, then loop on the change-request channel
// until we get Stop or Shutdown.
func (h *handler) Execute(args []string, r <-chan svc.ChangeRequest, status chan<- svc.Status) (svcSpecificEC bool, exitCode uint32) {
	const acceptedCmds = svc.AcceptStop | svc.AcceptShutdown
	status <- svc.Status{State: svc.StartPending}

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	var workerEC uint32

	wg.Add(1)
	go func() {
		defer wg.Done()
		// Re-run the worker if it ever returns on its own. It used to
		// run exactly once: a worker that bailed out early (unreadable
		// config at boot is the realistic case) left the service
		// reporting RUNNING to Windows forever with nothing listening —
		// and since the process never exits, the SCM recovery policy
		// never fires either. Looping here is the only restart that can
		// actually happen in that state.
		for {
			workerEC = uint32(h.runner(ctx, h.log))
			if ctx.Err() != nil {
				return
			}
			h.log.Error("bridge worker exited on its own; retrying in 30s",
				"code", workerEC)
			select {
			case <-ctx.Done():
				return
			case <-time.After(30 * time.Second):
			}
		}
	}()

	status <- svc.Status{State: svc.Running, Accepts: acceptedCmds}

loop:
	for {
		select {
		case c := <-r:
			switch c.Cmd {
			case svc.Interrogate:
				status <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				h.log.Info("service received stop signal", "cmd", c.Cmd)
				break loop
			default:
				h.log.Warn("service got unexpected control request", "cmd", c.Cmd)
			}
		}
	}

	status <- svc.Status{State: svc.StopPending}
	cancel()
	wg.Wait()

	// A stop we were asked for is a clean stop whatever the worker
	// returned while unwinding; svc reports Stopped for us on return.
	return false, 0
}
