//go:build windows

// Self-elevation so the operator never needs an admin cmd.exe. When an
// action needs administrator rights (installing the Windows service),
// the Bridge re-launches itself with the "runas" verb, which makes
// Windows show the standard UAC consent prompt. The elevated copy does
// the privileged work and exits.
package main

import (
	"os"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
)

// isElevated reports whether the current process already has admin
// rights (so we don't pop a redundant UAC prompt).
func isElevated() bool {
	return windows.GetCurrentProcessToken().IsElevated()
}

// relaunchElevated re-runs THIS executable with the given args under a
// UAC elevation prompt (ShellExecute "runas"). Fire-and-forget: returns
// once Windows accepts the request; the elevated process runs
// independently. If the operator declines the UAC prompt, ShellExecute
// returns an error.
func relaunchElevated(args ...string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	verb, err := syscall.UTF16PtrFromString("runas")
	if err != nil {
		return err
	}
	exePtr, err := syscall.UTF16PtrFromString(exe)
	if err != nil {
		return err
	}
	var argPtr *uint16
	if len(args) > 0 {
		argPtr, err = syscall.UTF16PtrFromString(strings.Join(args, " "))
		if err != nil {
			return err
		}
	}
	var cwdPtr *uint16
	if cwd, e := os.Getwd(); e == nil {
		cwdPtr, _ = syscall.UTF16PtrFromString(cwd)
	}
	// SW_NORMAL = 1
	return windows.ShellExecute(0, verb, exePtr, argPtr, cwdPtr, windows.SW_NORMAL)
}
