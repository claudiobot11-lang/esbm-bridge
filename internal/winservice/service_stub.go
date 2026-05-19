// Stub for non-Windows builds — keeps the cross-compile and macOS
// dev workflow building. None of these are reachable from a real
// install; they only exist so the package imports cleanly on Linux
// and Darwin.
//
//go:build !windows

package winservice

import (
	"context"
	"errors"
	"log/slog"
)

const (
	ServiceName = "ESBMBridge"
	DisplayName = "ESBM Bridge"
	Description = "ESBM Bridge"
)

// IsWindowsService always returns false off-Windows.
func IsWindowsService() bool { return false }

// Runner mirrors the Windows signature so callers stay portable.
type Runner func(ctx context.Context, log *slog.Logger) int

// Run is a no-op on non-Windows builds.
func Run(_ *slog.Logger, _ Runner) error {
	return errors.New("windows service mode is Windows-only")
}
