// Non-Windows stub so cmd/bridge/main.go imports cleanly on Linux/macOS dev builds.
//
//go:build !windows

package winconsole

// AttachToParent is a no-op outside Windows. Callers should treat the
// false return as "you're headless, don't print anything fancy".
func AttachToParent() bool { return false }
