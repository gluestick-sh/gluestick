package install

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/gluestick-sh/core/procutil"
)

// mergeInstallerSourceDir folds leftover MSI SourceDir/ payloads into installDir.
// Python keeps Lib/idlelib under SourceDir when persist pre-seeded Lib/site-packages blocks movedir.
func mergeInstallerSourceDir(installDir string) error {
	if runtime.GOOS != "windows" {
		return nil
	}
	sourceDir := filepath.Join(installDir, "SourceDir")
	entries, err := os.ReadDir(sourceDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read SourceDir: %w", err)
	}
	if len(entries) == 0 {
		return os.Remove(sourceDir)
	}
	for _, ent := range entries {
		src := filepath.Join(sourceDir, ent.Name())
		dst := filepath.Join(installDir, ent.Name())
		if err := robocopyMergeTree(src, dst); err != nil {
			return fmt.Errorf("merge %s: %w", ent.Name(), err)
		}
	}
	return os.RemoveAll(sourceDir)
}

func robocopyMergeTree(from, to string) error {
	if _, err := os.Stat(from); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if err := os.MkdirAll(to, 0755); err != nil {
		return err
	}
	cmd := exec.Command(
		"robocopy.exe", from, to,
		"/E", "/COPY:DAT", "/R:1", "/W:1",
		"/NFL", "/NDL", "/NJH", "/NJS", "/NC", "/NS",
	)
	procutil.HideWindow(cmd)
	err := cmd.Run()
	if err == nil {
		return nil
	}
	exit, ok := err.(*exec.ExitError)
	if !ok {
		return err
	}
	// Robocopy: 0-7 indicate success or benign outcomes.
	if code := exit.ExitCode(); code < 8 {
		return nil
	}
	return fmt.Errorf("robocopy exit %d", exit.ExitCode())
}
