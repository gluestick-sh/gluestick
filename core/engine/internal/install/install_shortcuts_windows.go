//go:build windows

package install

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/gluestick-sh/core/verbose"
	"github.com/gluestick-sh/core/manifest"
	"github.com/gluestick-sh/core/procutil"
)

var (
	cachedUserShortcutsFolder string
	cachedUserShortcutsErr  error
	cachedUserShortcutsOnce sync.Once
)

func startMenuBaseFolder(global bool) (string, error) {
	startMenu := "StartMenu"
	if global {
		startMenu = "CommonStartMenu"
	}
	script := fmt.Sprintf(
		"[Environment]::GetFolderPath('%s')",
		startMenu,
	)
	out, err := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script).Output()
	if err != nil {
		return "", fmt.Errorf("resolve start menu folder: %w", err)
	}
	base := strings.TrimSpace(string(out))
	if base == "" {
		return "", fmt.Errorf("empty start menu folder")
	}
	return base, nil
}

func scoopShortcutsFolder(global bool) (string, error) {
	if !global {
		cachedUserShortcutsOnce.Do(func() {
			cachedUserShortcutsFolder, cachedUserShortcutsErr = scoopShortcutsFolderUncached(false)
		})
		return cachedUserShortcutsFolder, cachedUserShortcutsErr
	}
	return scoopShortcutsFolderUncached(global)
}

func scoopShortcutsFolderUncached(global bool) (string, error) {
	base, err := startMenuBaseFolder(global)
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "Programs", "Glue Apps"), nil
}

func createPackageShortcuts(installDir string, m *manifest.Manifest, installArch string) error {
	entries := m.ShortcutEntriesForInstall(installArch)
	if len(entries) == 0 {
		return nil
	}
	folder, err := scoopShortcutsFolder(false)
	if err != nil {
		return err
	}
	for _, sc := range entries {
		target := filepath.Join(installDir, filepath.FromSlash(sc.Target))
		if _, err := os.Stat(target); err != nil {
			verbose.Progressf("  Skipping shortcut %s (target missing: %s)\n", sc.Label, sc.Target)
			continue
		}
		if err := createStartMenuShortcut(folder, installDir, target, sc); err != nil {
			return err
		}
		verbose.Progressf("  Shortcut: %s\n", sc.Label)
	}
	return nil
}

// RemovePackageShortcuts deletes the start-menu shortcuts declared by the manifest.
func RemovePackageShortcuts(m *manifest.Manifest, installArch string) error {
	entries := m.ShortcutEntriesForInstall(installArch)
	if len(entries) == 0 {
		return nil
	}
	folder, err := scoopShortcutsFolder(false)
	if err != nil {
		return err
	}
	for _, sc := range entries {
		if sc.Label == "" {
			continue
		}
		if err := removeShortcutLink(folder, sc); err != nil {
			return err
		}
	}
	return nil
}

func removeShortcutLink(folder string, sc manifest.ShortcutEntry) error {
	lnk := shortcutLinkPath(folder, sc.Label)
	if err := os.Remove(lnk); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove shortcut %s: %w", sc.Label, err)
	}
	return nil
}

func shortcutLinkPath(folder, label string) string {
	return filepath.Join(folder, filepath.FromSlash(label+".lnk"))
}

func createStartMenuShortcut(folder, installDir, target string, sc manifest.ShortcutEntry) error {
	if sc.Label == "" {
		return nil
	}
	subDir := filepath.Dir(filepath.FromSlash(sc.Label))
	linkDir := folder
	if subDir != "." {
		linkDir = filepath.Join(folder, subDir)
	}
	if err := os.MkdirAll(linkDir, 0755); err != nil {
		return fmt.Errorf("create shortcut dir: %w", err)
	}
	lnkPath := filepath.Join(linkDir, filepath.Base(sc.Label)+".lnk")

	iconPath := ""
	if sc.Icon != "" {
		iconPath = filepath.Join(installDir, filepath.FromSlash(sc.Icon))
	}

	args := strings.ReplaceAll(sc.Args, "$dir", installDir)
	args = strings.ReplaceAll(args, "$original_dir", installDir)

	script := fmt.Sprintf(`$ErrorActionPreference = 'Stop'
$ws = New-Object -ComObject WScript.Shell
$path = %s
if (Test-Path -LiteralPath $path) { Remove-Item -LiteralPath $path -Force }
$link = $ws.CreateShortcut($path)
$link.TargetPath = %s
$link.WorkingDirectory = %s
if (%s -ne '') { $link.Arguments = %s }
if (%s -ne '' -and (Test-Path -LiteralPath %s)) { $link.IconLocation = %s }
$link.Save()`,
		psSingleQuoted(lnkPath),
		psSingleQuoted(target),
		psSingleQuoted(filepath.Dir(target)),
		psSingleQuoted(args),
		psSingleQuoted(args),
		psSingleQuoted(iconPath),
		psSingleQuoted(iconPath),
		psSingleQuoted(iconPath),
	)
	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", script)
	procutil.HideWindow(cmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := decodePowerShellOutput(out)
		if msg != "" {
			return fmt.Errorf("create shortcut %s: %w\n%s", sc.Label, err, msg)
		}
		return fmt.Errorf("create shortcut %s: %w", sc.Label, err)
	}
	return nil
}
