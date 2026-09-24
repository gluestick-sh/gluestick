//go:build windows

package main

import (
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

func initConsoleColor() {
	enableVT(windows.STD_OUTPUT_HANDLE)
	enableVT(windows.STD_ERROR_HANDLE)
	ensureVirtualTerminalRegistry()
}

func enableVT(handle windows.Handle) {
	var mode uint32
	if err := windows.GetConsoleMode(handle, &mode); err != nil {
		return
	}
	_ = windows.SetConsoleMode(handle, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING)
}

// ensureVirtualTerminalRegistry enables ANSI colors in classic cmd.exe for future sessions.
func ensureVirtualTerminalRegistry() {
	key, err := registry.OpenKey(registry.CURRENT_USER, `Console`, registry.SET_VALUE|registry.QUERY_VALUE)
	if err != nil {
		return
	}
	defer key.Close()

	val, _, err := key.GetIntegerValue("VirtualTerminalLevel")
	if err == registry.ErrNotExist || val == 0 {
		_ = key.SetDWordValue("VirtualTerminalLevel", 1)
	}
}
