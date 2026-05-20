//go:build !windows

// GUI stubs for non-Windows builds. The store operator experience is
// Windows-only; on Linux/macOS dev builds we fall back to the console
// setup wizard.
package main

import "log/slog"

// cmdSetupGUI falls back to the console setup on non-Windows.
func cmdSetupGUI(log *slog.Logger) int {
	return cmdSetup(log, nil)
}
