// Stubs for `install` and `uninstall` on non-Windows builds — the
// commands are visible in help but fail cleanly when invoked.
//
//go:build !windows

package main

import (
	"fmt"
	"log/slog"
	"os"
)

func cmdInstall(_ *slog.Logger, _ []string) int {
	fmt.Fprintln(os.Stderr, "install is Windows-only.")
	return 2
}

func cmdUninstall(_ *slog.Logger, _ []string) int {
	fmt.Fprintln(os.Stderr, "uninstall is Windows-only.")
	return 2
}
