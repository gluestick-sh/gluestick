//go:build windows

package main

import (
	"fmt"
	"strings"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// editNlsCodePage writes the system codepage values behind "Beta: Use Unicode
// UTF-8 for worldwide language support" (ACP/OEMCP/MACCP = 65001). A seam so
// tests never touch HKLM.
var editNlsCodePage = func(value string) error {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Control\Nls\CodePage`, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	for _, name := range []string{"ACP", "OEMCP", "MACCP"} {
		if cur, _, err := k.GetStringValue(name); err == nil && cur == value {
			continue
		}
		if err := k.SetStringValue(name, value); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	return nil
}

// Test seams: the console and HKLM writes never run inside unit tests.
var (
	doctorConsoleOutputCP = func() uint32 {
		cp, _ := windows.GetConsoleOutputCP()
		return cp
	}
	doctorSetConsoleUTF8 = func() error {
		if err := windows.SetConsoleCP(65001); err != nil {
			return err
		}
		return windows.SetConsoleOutputCP(65001)
	}
)

// fixUTF8Codepage flips the attached console to UTF-8 immediately (session
// scope) and writes the system-wide UTF-8 codepages (HKLM: needs an elevated
// shell; fails soft without one). The chcp line shell_profile writes into
// PowerShell profiles keeps new terminals UTF-8 either way.
func fixUTF8Codepage() (bool, string, string) {
	parts := []string{}
	errs := []string{}
	applied := false
	if cp := doctorConsoleOutputCP(); cp != 0 {
		if err := doctorSetConsoleUTF8(); err != nil {
			errs = append(errs, "console: "+err.Error())
		} else {
			parts = append(parts, "console → 65001")
			applied = true
		}
	} else {
		parts = append(parts, "no console attached")
	}
	if err := editNlsCodePage("65001"); err != nil {
		errs = append(errs, "system ACP/OEMCP needs an elevated shell ("+err.Error()+")")
	} else {
		parts = append(parts, "system ACP/OEMCP → 65001 (re-login to apply everywhere)")
		applied = true
	}
	detail := strings.Join(parts, "; ")
	if applied {
		if len(errs) > 0 {
			detail += "; " + strings.Join(errs, "; ")
		}
		return true, detail, ""
	}
	return false, detail, strings.Join(errs, "; ")
}

// fixExecutionPolicy sets the per-user PowerShell execution policy to
// RemoteSigned (HKCU only — Machine and GroupPolicy scopes stay untouched).
func fixExecutionPolicy() (bool, string, string) {
	k, _, err := registry.CreateKey(registry.CURRENT_USER,
		`Software\Microsoft\PowerShell\1\ShellIds\Microsoft.PowerShell`, registry.SET_VALUE)
	if err != nil {
		return false, "", err.Error()
	}
	defer k.Close()
	if cur, _, err := k.GetStringValue("ExecutionPolicy"); err == nil && strings.EqualFold(cur, "RemoteSigned") {
		return true, "RemoteSigned (already set)", ""
	}
	if err := k.SetStringValue("ExecutionPolicy", "RemoteSigned"); err != nil {
		return false, "", err.Error()
	}
	return true, "CurrentUser → RemoteSigned", ""
}
