//go:build !windows

package main

// initConsoleColor is a no-op on platforms without Windows console VT APIs.

func initConsoleColor() {}
