//go:build windows

package uninstall

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/gluestick-sh/core/verbose"
	"github.com/gluestick-sh/core/procutil"
)

// releaseInstallDirFileLocks restarts File Explorer when shell integration was registered
// and Explorer may still hold extension DLLs (e.g. 7-zip.dll) after uninstall-context.reg.
func releaseInstallDirFileLocks(installDir string) {
	if shouldRestartExplorerForInstallDir(installDir) {
		restartExplorerForUninstall()
		return
	}
	tryRestartExplorerForLoadedModules(installDir)
}

func shouldRestartExplorerForInstallDir(installDir string) bool {
	regPath := filepath.Join(installDir, "install-context.reg")
	if _, err := os.Stat(regPath); err == nil {
		return true
	}
	return false
}

func tryRestartExplorerForLoadedModules(installDir string) {
	procs, err := procutil.ProcessesWithModulesFrom(installDir)
	if err != nil || len(procs) == 0 {
		return
	}
	for _, p := range procs {
		if !strings.EqualFold(p.Name, "explorer.exe") {
			return
		}
	}
	restartExplorerForUninstall()
}

func restartExplorerForUninstall() {
	fmt.Println("  Restarting File Explorer to release shell extension locks...")
	if err := restartExplorer(); err != nil {
		verbose.Progressf("  Warning: failed to restart File Explorer: %v\n", err)
		return
	}
	time.Sleep(500 * time.Millisecond)
}

func restartExplorer() error {
	cmd := exec.Command(
		"powershell.exe",
		"-NoProfile",
		"-NonInteractive",
		"-Command",
		"Get-Process explorer -ErrorAction SilentlyContinue | Stop-Process -Force; Start-Sleep -Milliseconds 300; Start-Process explorer",
	)
	procutil.HideWindow(cmd)
	return cmd.Run()
}
