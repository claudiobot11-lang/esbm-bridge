// Package winconsole hands a Windows-GUI .exe (linked with
// -H windowsgui, so no console window pops up on double-click) a
// way to still print to the operator's cmd.exe when they DID
// launch it from a terminal.
//
// Pattern:
//   - AttachConsole(ATTACH_PARENT_PROCESS) — if launched from cmd,
//     gives us back the parent's stdout/stderr handles.
//   - When no parent console exists (Explorer double-click), the
//     call fails silently and we stay headless. The tray icon +
//     localhost dashboard are the user-facing surface for that case.
//
//go:build windows

package winconsole

import (
	"os"
	"syscall"
	"unsafe"
)

var (
	kernel32          = syscall.NewLazyDLL("kernel32.dll")
	procAttachConsole = kernel32.NewProc("AttachConsole")
	procGetStdHandle  = kernel32.NewProc("GetStdHandle")
)

const (
	attachParentProcess = ^uintptr(0) // -1: parent process console
	stdOutputHandle     = ^uintptr(11) + 1
	stdErrorHandle      = ^uintptr(12) + 1
)

// AttachToParent connects this process's stdout/stderr to the parent
// cmd.exe window when one exists. Returns true when we successfully
// hijacked the parent console (so callers can choose to print rich
// output), false when running headless (e.g. Explorer double-click).
func AttachToParent() bool {
	ret, _, _ := procAttachConsole.Call(attachParentProcess)
	if ret == 0 {
		return false
	}
	// Re-bind os.Stdout / os.Stderr to the now-attached console
	// handles. Go's default os.Stdout points to a NUL handle in
	// /-H windowsgui/ builds; without this redirection the operator
	// sees no output even though we're attached.
	if h, ok := getStd(stdOutputHandle); ok {
		os.Stdout = os.NewFile(h, "stdout")
	}
	if h, ok := getStd(stdErrorHandle); ok {
		os.Stderr = os.NewFile(h, "stderr")
	}
	return true
}

func getStd(n uintptr) (uintptr, bool) {
	r, _, _ := procGetStdHandle.Call(n)
	// 0xFFFFFFFF means invalid; 0 means no console attached
	if r == 0 || r == ^uintptr(0) {
		return 0, false
	}
	_ = unsafe.Sizeof(r) // keep the import; some toolchains drop unused-imports
	return r, true
}
