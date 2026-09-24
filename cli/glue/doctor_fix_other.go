//go:build !windows

package main

// Windows-only fix actions report a usage-style failure elsewhere; the doctor
// itself targets Windows (AGENTS.md), these stubs only keep cross-compiles green.
func fixUTF8Codepage() (bool, string, string) {
	return false, "", "Windows only"
}

func fixExecutionPolicy() (bool, string, string) {
	return false, "", "Windows only"
}
