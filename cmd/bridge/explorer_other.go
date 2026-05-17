//go:build !windows

// On non-Windows the "double-click from Explorer" problem doesn't
// exist — CLI runs from a terminal in dev workflows. These are
// no-ops so the main() control flow stays identical.
package main

func launchedFromExplorer() bool   { return false }
func holdConsoleAndReport() bool   { return false }
