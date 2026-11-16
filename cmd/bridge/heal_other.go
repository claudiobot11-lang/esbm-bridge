// Stub for `install` / `fix` on non-Windows builds. The repair flow is
// all Windows service, registry and process-table work; on Linux/macOS
// dev builds the command exists only so help text and the dispatcher
// stay identical across platforms.
//
//go:build !windows

package main

import (
	"fmt"
	"log/slog"
	"os"
)

func cmdFix(_ *slog.Logger, _ []string, _ bool) int {
	fmt.Fprintln(os.Stderr, "install is Windows-only.")
	return 2
}
