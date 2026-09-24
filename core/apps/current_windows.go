//go:build windows

package apps

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/gluestick-sh/core/procutil"
)

// LinkCurrent points apps/<pkg>/current at apps/<pkg>/<version> via a directory junction.
func LinkCurrent(pkgRoot, version string) error {
	target, err := filepath.Abs(filepath.Join(pkgRoot, version))
	if err != nil {
		return err
	}
	if st, err := os.Stat(target); err != nil || !st.IsDir() {
		return fmt.Errorf("version directory not found: %s", target)
	}

	current, err := filepath.Abs(filepath.Join(pkgRoot, CurrentLinkName))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(pkgRoot, 0755); err != nil {
		return err
	}

	_ = removeCurrentLink(current)

	cmd := exec.Command("cmd", "/C", "mklink", "/J", current, target)
	procutil.HideWindow(cmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("mklink /J: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ReadCurrent returns the active version name from the current junction.
func ReadCurrent(pkgRoot string) (string, error) {
	current := filepath.Join(pkgRoot, CurrentLinkName)
	target, err := os.Readlink(current)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(pkgRoot, target)
	}
	target, err = filepath.Abs(target)
	if err != nil {
		return "", err
	}
	return filepath.Base(target), nil
}

// removeCurrentLink removes the current junction on Windows without deleting the target directory.
func removeCurrentLink(current string) error {
	if _, err := os.Lstat(current); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	// rmdir removes a junction without deleting the target directory.
	cmd := exec.Command("cmd", "/C", "rmdir", current)
	procutil.HideWindow(cmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("rmdir current: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
