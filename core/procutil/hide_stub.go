//go:build !windows

package procutil

import "os/exec"

func hideWindow(cmd *exec.Cmd) {}

func newConsoleWindow(cmd *exec.Cmd) {}
