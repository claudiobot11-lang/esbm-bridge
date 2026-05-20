//go:build !windows

// Elevation stubs for non-Windows. Dev builds run with whatever
// privileges the shell already has.
package main

// isElevated is always true off-Windows (no UAC model here).
func isElevated() bool { return true }

// relaunchElevated is a no-op off-Windows.
func relaunchElevated(_ ...string) error { return nil }
