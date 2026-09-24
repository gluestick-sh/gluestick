//go:build windows

package install

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/gluestick-sh/core/procutil"
	"github.com/gluestick-sh/core/verbose"
)

func applyInstallContextRegistry(installDir string) {
	regPath := filepath.Join(installDir, "install-context.reg")
	if _, err := os.Stat(regPath); err != nil {
		return
	}
	cmd := exec.Command("reg.exe", "import", regPath)
	procutil.HideWindow(cmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		verbose.Progressf("  Warning: failed to register shell integration: %v\n", err)
		if msg := strings.TrimSpace(string(out)); msg != "" {
			verbose.Progressf("    %s\n", msg)
		}
		return
	}
	verbose.Progressf("  Registered shell integration\n")
}
