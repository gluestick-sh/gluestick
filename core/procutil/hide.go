// Package procutil provides cross-platform process utilities.
package procutil

import "os/exec"

// HideWindow configures a command to run without creating a visible
// console window. On non-Windows platforms this is a no-op.
func HideWindow(cmd *exec.Cmd) {
	hideWindow(cmd)
}

// NewConsoleWindow configures a command to run in a new visible console.
// On non-Windows platforms this is a no-op.
func NewConsoleWindow(cmd *exec.Cmd) {
	newConsoleWindow(cmd)
}
