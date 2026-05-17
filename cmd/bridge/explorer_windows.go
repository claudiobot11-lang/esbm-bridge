//go:build windows

// Detect double-click-from-Explorer launches and keep the console
// window open with a friendly message + status snapshot so the
// operator doesn't see a console flash and assume the install is
// broken.
package main

import (
	"bufio"
	"fmt"
	"os"
	"syscall"
	"unsafe"

	"github.com/claudiobot11-lang/esbm-bridge/internal/config"
)

var (
	kernel32                  = syscall.NewLazyDLL("kernel32.dll")
	procGetConsoleProcessList = kernel32.NewProc("GetConsoleProcessList")
)

// launchedFromExplorer returns true when our process is the ONLY one
// attached to this console — which happens when Windows Explorer
// spawned a fresh `conhost.exe` just for us (double-click). When run
// from an existing `cmd.exe` or `pwsh`, that shell process is also
// attached, so the count is ≥ 2.
//
// Done via raw syscall because golang.org/x/sys/windows doesn't
// expose GetConsoleProcessList yet (as of x/sys v0.36). The Win32
// API itself is stable since XP.
func launchedFromExplorer() bool {
	var pids [4]uint32
	r1, _, _ := procGetConsoleProcessList.Call(
		uintptr(unsafe.Pointer(&pids[0])),
		uintptr(len(pids)),
	)
	if r1 == 0 {
		return false
	}
	return r1 <= 1
}

// holdConsoleAndReport prints a self-contained "what is this thing"
// summary then blocks on Enter. Returns true so the caller can exit
// cleanly without falling through to `usage()` + immediate close.
func holdConsoleAndReport() bool {
	fmt.Println()
	fmt.Println("============================================================")
	fmt.Println("  ESBM Bridge", version)
	fmt.Println("============================================================")
	fmt.Println()
	fmt.Println("  This is a command-line tool — double-clicking it just")
	fmt.Println("  prints this message. To actually use it, open a Command")
	fmt.Println("  Prompt (Win+R, type 'cmd', press Enter), navigate to the")
	fmt.Println("  folder where this .exe lives, and run one of:")
	fmt.Println()
	fmt.Println("    esbm-bridge-windows-amd64.exe pair --code 123456")
	fmt.Println("    esbm-bridge-windows-amd64.exe run")
	fmt.Println("    esbm-bridge-windows-amd64.exe status")
	fmt.Println()
	fmt.Println("  The pairing code comes from the ESBM web app — open")
	fmt.Println("  /esl/stores, click 'Add Store' to generate one.")
	fmt.Println()

	// Show the current state so a returning operator can confirm
	// "yes I already paired this PC" without opening cmd.exe.
	if cfg, err := config.Load(); err == nil {
		fmt.Println("  Current status: PAIRED")
		fmt.Println("    shop_code :", cfg.ShopCode)
		fmt.Println("    server    :", cfg.ServerAddr)
		fmt.Println("    hostname  :", cfg.Hostname)
	} else if os.IsNotExist(err) {
		fmt.Println("  Current status: NOT PAIRED")
	} else {
		fmt.Println("  Current status: ERROR —", err)
	}

	fmt.Println()
	fmt.Println("  Press Enter to close this window.")
	fmt.Println("============================================================")
	bufio.NewReader(os.Stdin).ReadString('\n')
	return true
}
