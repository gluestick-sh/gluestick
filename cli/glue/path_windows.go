//go:build windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gluestick-sh/core/engine"
	"golang.org/x/sys/windows/registry"
)

const userEnvKey = `Environment`

// ensureUserPathFront puts dir first on the User PATH in the registry.
// If it is already present later in the list, it is moved to the front so
// Microsoft Store App Execution Aliases (WindowsApps) do not shadow shims.
func ensureUserPathFront(dir string) (moved bool, err error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return false, fmt.Errorf("empty directory")
	}

	key, err := registry.OpenKey(registry.CURRENT_USER, userEnvKey, registry.SET_VALUE|registry.QUERY_VALUE)
	if err != nil {
		return false, fmt.Errorf("open user Environment key: %w", err)
	}
	defer key.Close()

	current, _, err := key.GetStringValue("Path")
	if err != nil && err != registry.ErrNotExist {
		return false, fmt.Errorf("read user Path: %w", err)
	}

	newPath, changed := ensureDirFirstInPathList(current, dir)
	if !changed {
		return false, nil
	}
	if err := key.SetStringValue("Path", newPath); err != nil {
		return false, fmt.Errorf("set user Path: %w", err)
	}
	return true, nil
}

func ensureDirFirstInPathList(current, dir string) (string, bool) {
	dir = strings.TrimRight(strings.TrimSpace(dir), `\`)
	if dir == "" {
		return current, false
	}
	parts := splitPathList(current)
	out := make([]string, 0, len(parts)+1)
	out = append(out, dir)
	for _, p := range parts {
		if strings.EqualFold(strings.TrimRight(p, `\`), dir) {
			continue
		}
		out = append(out, p)
	}
	newPath := strings.Join(out, ";")
	if len(parts) > 0 && strings.EqualFold(strings.TrimRight(parts[0], `\`), dir) && newPath == strings.Join(parts, ";") {
		return current, false
	}
	if current == newPath {
		return current, false
	}
	return newPath, true
}

func splitPathList(path string) []string {
	if path == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(path, ";") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func prependDirToProcessPath(dir string) {
	dir = strings.TrimRight(strings.TrimSpace(dir), `\`)
	if dir == "" {
		return
	}
	cur := os.Getenv("PATH")
	newPath, _ := ensureDirFirstInPathList(cur, dir)
	_ = os.Setenv("PATH", newPath)
}

// disableWindowsPythonAliases turns off Store python.exe / python3.exe stubs
// so Glue shims are used. Those files are 0-byte App Execution Aliases.
func disableWindowsPythonAliases() {
	localAppData := os.Getenv("LOCALAPPDATA")
	if localAppData == "" {
		return
	}
	apps := filepath.Join(localAppData, "Microsoft", "WindowsApps")
	for _, name := range []string{"python.exe", "python3.exe", "pythonw.exe"} {
		p := filepath.Join(apps, name)
		fi, err := os.Lstat(p)
		if err != nil {
			continue
		}
		if fi.IsDir() || fi.Size() > 1024 {
			continue
		}
		_ = os.Remove(p)
	}
}

// windowsAppsDir delegates to core so PATH diagnostics share one implementation.
func windowsAppsDir() string {
	return engine.WindowsAppsDir()
}

// pathDirPrecedes delegates to core so PATH ordering checks share one implementation.
func pathDirPrecedes(pathList, earlier, later string) bool {
	return engine.PathDirPrecedes(pathList, earlier, later)
}
