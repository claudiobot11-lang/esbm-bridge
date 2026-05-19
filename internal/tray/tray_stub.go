// Stubs for non-Windows builds. The cross-compile on macOS/Linux is
// just for dev — the tray icon only matters on the operator's Windows
// box, so we keep this package importable everywhere but no-op
// outside Windows.
//
//go:build !windows

package tray

import (
	"context"
	"log/slog"

	"github.com/claudiobot11-lang/esbm-bridge/internal/dashboard"
)

type Status struct {
	GatewayOnline bool
	WorkerIssue   bool
}

type StatusFn func() Status

type RestartFn func()

type Options struct {
	Logger        *slog.Logger
	DashboardAddr string
	Version       string
	StatusFn      StatusFn
	OnRestart     RestartFn
	OnQuit        func()
}

// Run blocks until ctx (closed externally) or returns immediately on
// non-Windows. Lets cmd/bridge call tray.Run without build tags.
func Run(_ Options) {}

// Quit is a no-op outside Windows.
func Quit() {}

// RunBridge is the no-op variant for cross-compile builds.
func RunBridge(_ context.Context, _ *slog.Logger, _ string, _ *dashboard.Server, _ func()) {}
