//go:build windows

package engine

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unsafe"

	"github.com/gluestick-sh/core/apps"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// bootstrapPossible reports whether missing install helpers (git, 7-Zip) can be
// downloaded automatically. Glue bootstraps them on Windows only.
func bootstrapPossible() bool { return true }

// WindowsAppsDir returns the Microsoft Store app-execution-alias directory.
// Its python.exe stubs are zero-byte aliases that can shadow Glue shims on PATH.
func WindowsAppsDir() string {
	localAppData := os.Getenv("LOCALAPPDATA")
	if localAppData == "" {
		return ""
	}
	return filepath.Join(localAppData, "Microsoft", "WindowsApps")
}

// StoreAliasShadowsShims reports whether the Microsoft Store aliases directory
// precedes the Glue shims directory on PATH, which would shadow installed shims.
func StoreAliasShadowsShims(binDir string) bool {
	apps := WindowsAppsDir()
	if apps == "" {
		return false
	}
	return PathDirPrecedes(os.Getenv("PATH"), apps, binDir)
}

// agentKernel32 holds the kernel32 procs used by the readiness checks.
var (
	agentKernel32           = windows.NewLazySystemDLL("kernel32.dll")
	agentGetConsoleOutputCP = agentKernel32.NewProc("GetConsoleOutputCP")
	agentGetACP             = agentKernel32.NewProc("GetACP")
	agentGetVolumeInfoW     = agentKernel32.NewProc("GetVolumeInformationW")
	agentGetDriveTypeW      = agentKernel32.NewProc("GetDriveTypeW")
)

// agentOSInfo reads the real build number from the registry. ProductName alone
// still says "Windows 10" on upgraded Windows 11 machines, so build >= 22000
// corrects the name. ("", 0) means the query failed.
func agentOSInfo() (string, int) {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows NT\CurrentVersion`, registry.QUERY_VALUE)
	if err != nil {
		return "", 0
	}
	defer k.Close()
	product, _, _ := k.GetStringValue("ProductName")
	buildStr, _, err := k.GetStringValue("CurrentBuild")
	if err != nil {
		return product, 0
	}
	build, convErr := strconv.Atoi(buildStr)
	if convErr != nil {
		return product, 0
	}
	if build >= 22000 && strings.Contains(product, "Windows 10") {
		product = strings.Replace(product, "Windows 10", "Windows 11", 1)
	}
	return product, build
}

// agentOutputCodepage prefers the attached console's output codepage and falls
// back to the system ANSI codepage (the common case when glue runs piped under
// an agent host). ok=false when neither can be queried.
func agentOutputCodepage() (int, bool) {
	cp, _, _ := agentGetConsoleOutputCP.Call()
	if cp == 0 {
		cp, _, _ = agentGetACP.Call()
	}
	if cp == 0 {
		return 0, false
	}
	return int(cp), true
}

// agentPowerShellExecutionPolicy reads the per-user execution policy value.
// A missing key/value means "never configured" (Restricted by default) and is
// reported as "" with known=true; Machine policy overrides are not consulted
// (best-effort v1).
func agentPowerShellExecutionPolicy() (string, bool) {
	k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\PowerShell\1\ShellIds\Microsoft.PowerShell`, registry.QUERY_VALUE)
	if err == registry.ErrNotExist {
		return "", true
	}
	if err != nil {
		return "", false
	}
	defer k.Close()
	value, _, err := k.GetStringValue("ExecutionPolicy")
	if err != nil {
		return "", true
	}
	return value, true
}

// agentVolumeFileSystem returns the filesystem name (NTFS, ReFS, ...) of the
// volume rooted at root, e.g. "C:\".
func agentVolumeFileSystem(root string) (string, bool) {
	rootPtr, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return "", false
	}
	var buf [260]uint16
	ok, _, _ := agentGetVolumeInfoW.Call(
		uintptr(unsafe.Pointer(rootPtr)),
		0, 0, 0, 0, 0,
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(len(buf)),
	)
	if ok == 0 {
		return "", false
	}
	return windows.UTF16ToString(buf[:]), true
}

// agentDriveIsRemote reports whether path's volume is a mapped/network drive
// (GetDriveTypeW == DRIVE_REMOTE), which silently breaks shims and git clones.
func agentDriveIsRemote(path string) bool {
	volume := filepath.VolumeName(path)
	if volume == "" {
		return false
	}
	rootPtr, err := windows.UTF16PtrFromString(volume + `\`)
	if err != nil {
		return false
	}
	kind, _, _ := agentGetDriveTypeW.Call(uintptr(unsafe.Pointer(rootPtr)))
	const driveRemote = 4
	return kind == driveRemote
}

// agentPWSHVersion trims the PE file version of pwsh.exe to major.minor.patch
// (its file version tracks the PowerShell version), avoiding a process spawn.
func agentPWSHVersion(path string) string {
	fv, err := apps.ReadFileVersion(path)
	if err != nil {
		return ""
	}
	parts := strings.Split(fv, ".")
	if len(parts) >= 3 {
		return strings.Join(parts[:3], ".")
	}
	return fv
}
