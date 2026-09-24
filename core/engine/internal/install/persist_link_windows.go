//go:build windows

package install

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/gluestick-sh/core/procutil"
)

func linkDirectoryJunction(linkPath, targetPath string) error {
	linkPath, err := filepath.Abs(linkPath)
	if err != nil {
		return err
	}
	targetPath, err = filepath.Abs(targetPath)
	if err != nil {
		return err
	}
	if target, linked, err := directoryLinkTarget(linkPath); err != nil {
		return err
	} else if linked {
		absTarget, absErr := filepath.Abs(target)
		if absErr == nil && strings.EqualFold(absTarget, targetPath) {
			return nil
		}
		if err := removeDirectoryLink(linkPath); err != nil {
			return err
		}
	} else if _, statErr := os.Lstat(linkPath); statErr == nil {
		if err := removeInstallPersistPath(linkPath); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(linkPath), 0755); err != nil {
		return err
	}
	cmd := exec.Command("cmd", "/C", "mklink", "/J", linkPath, targetPath)
	procutil.HideWindow(cmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("mklink /J: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func removeDirectoryLink(linkPath string) error {
	if _, err := os.Lstat(linkPath); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	cmd := exec.Command("cmd", "/C", "rmdir", linkPath)
	procutil.HideWindow(cmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("rmdir junction: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
