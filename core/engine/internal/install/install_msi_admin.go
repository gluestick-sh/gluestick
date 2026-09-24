package install

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/gluestick-sh/core/manifest"
	"github.com/gluestick-sh/core/procutil"
)

func msiNeedsAdministrativeExtract(m *manifest.Manifest, installArch string) bool {
	if m == nil {
		return false
	}
	return m.GetExtractDirForInstall(installArch) != ""
}

// extractMSIAdministrative runs msiexec /a (Scoop Expand-MsiArchive) and flattens extract_dir.
// 7-Zip MSI extraction yields internal CAB stream names (Service, ClientCmd) without .exe paths;
// administrative install produces SourceDir/PFiles64/... layouts expected by Scoop manifests.
func extractMSIAdministrative(installDir, msiPath, extractDir string) error {
	if runtime.GOOS != "windows" {
		return fmt.Errorf("MSI with extract_dir requires Windows administrative extract")
	}
	stage, err := os.MkdirTemp("", "glue-msi-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)

	sourceDir := filepath.Join(stage, "SourceDir")
	if err := os.MkdirAll(sourceDir, 0755); err != nil {
		return err
	}

	targetDir := sourceDir + `\`
	msiexec, err := msiexecPath()
	if err != nil {
		return err
	}
	cmd := exec.Command(msiexec, "/a", msiPath, "/qn", "TARGETDIR="+targetDir)
	procutil.HideWindow(cmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("msiexec administrative extract: %w\n%s", err, out)
	}

	_ = os.Remove(filepath.Join(sourceDir, filepath.Base(msiPath)))

	applied, err := applyExtractDirLayout(sourceDir, extractDir)
	if err != nil {
		return err
	}
	if !applied {
		return fmt.Errorf("MSI extract_dir %q not found after administrative extract", extractDir)
	}

	return moveInstallTreeContents(sourceDir, installDir)
}

func moveInstallTreeContents(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		from := filepath.Join(src, entry.Name())
		to := filepath.Join(dst, entry.Name())
		if _, err := os.Stat(to); err == nil {
			if err := os.RemoveAll(to); err != nil {
				return fmt.Errorf("replace %s: %w", entry.Name(), err)
			}
		}
		if err := os.Rename(from, to); err != nil {
			return fmt.Errorf("move %s: %w", entry.Name(), err)
		}
	}
	return nil
}

func msiexecPath() (string, error) {
	if windir := os.Getenv("WINDIR"); windir != "" {
		for _, sub := range []string{`System32\msiexec.exe`, `Sysnative\msiexec.exe`} {
			p := filepath.Join(windir, sub)
			if _, err := os.Stat(p); err == nil {
				return p, nil
			}
		}
	}
	if p, err := exec.LookPath("msiexec.exe"); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("msiexec.exe not found")
}
