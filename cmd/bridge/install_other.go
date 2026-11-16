// Stub for `uninstall` on non-Windows builds — the command is visible
// in help but fails cleanly when invoked. (`install` is cmdFix, stubbed
// in heal_other.go.)
//
//go:build !windows

package main

import (
	"fmt"
	"log/slog"
	"os"
)

func cmdUninstall(_ *slog.Logger, _ []string) int {
	fmt.Fprintln(os.Stderr, "uninstall is Windows-only.")
	return 2
}
