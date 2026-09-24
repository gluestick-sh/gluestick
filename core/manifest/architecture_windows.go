//go:build windows

package manifest

import (
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/windows/registry"
)

func hostArchitecture() string {
	// PROCESSOR_ARCHITEW6432 reflects the native OS when this process runs under
	// WoW64/x64 emulation on ARM64 Windows (PROCESSOR_ARCHITECTURE would be AMD64).
	if native := strings.ToUpper(strings.TrimSpace(os.Getenv("PROCESSOR_ARCHITEW6432"))); native == "ARM64" {
		return ArchARM64
	}

	switch strings.ToUpper(strings.TrimSpace(os.Getenv("PROCESSOR_ARCHITECTURE"))) {
	case "ARM64":
		return ArchARM64
	case "AMD64":
		return Arch64bit
	case "X86":
		return Arch32bit
	default:
		return Arch64bit
	}
}

func hostWindowsBuild() int {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows NT\CurrentVersion`, registry.QUERY_VALUE)
	if err != nil {
		return 0
	}
	defer key.Close()

	buildStr, _, err := key.GetStringValue("CurrentBuildNumber")
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(buildStr)
	return n
}
