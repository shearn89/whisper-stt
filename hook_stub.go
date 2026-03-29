//go:build noX11

// hook_stub.go provides no-op implementations of the hotkey listeners for
// builds where gohook is excluded (e.g. headless CI, unit tests).
// Build with:  go build -tags noX11
// Test with:   go test  -tags noX11

package main

import "fmt"

func runHoldListener(_ *recorder, _ *config, _ []string) {
	fmt.Println("hotkey listener not available: built without X11 support (-tags noX11)")
}

func runToggleListener(_ *recorder, _ *config, _ []string) {
	fmt.Println("hotkey listener not available: built without X11 support (-tags noX11)")
}
