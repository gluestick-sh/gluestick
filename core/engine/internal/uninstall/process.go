package uninstall

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/gluestick-sh/core/apps"
	"github.com/gluestick-sh/core/message"
	"github.com/gluestick-sh/core/procutil"
)

func processesBlockingUninstallError(pkgName, version string, procs []procutil.RunningProcess) error {
	var b strings.Builder
	fmt.Fprintln(&b, message.FormatEN(message.ErrUninstallProcessesOpen, map[string]interface{}{
		"package": pkgName,
		"version": version,
	}))
	b.WriteString("The following processes are still running:\n")
	for _, p := range procs {
		fmt.Fprintf(&b, "  %s (PID %d)\n", p.Name, p.PID)
	}
	b.WriteString("Close them in Task Manager, then retry uninstall.")
	if hasExplorerProcess(procs) {
		b.WriteString("\nIf the package registered a shell extension (e.g. 7-Zip), restart File Explorer:\n")
		b.WriteString("  Stop-Process -Name explorer -Force; Start-Process explorer")
	}
	return fmt.Errorf("%s", b.String())
}

func hasExplorerProcess(procs []procutil.RunningProcess) bool {
	for _, p := range procs {
		if strings.EqualFold(p.Name, "explorer.exe") {
			return true
		}
	}
	return false
}

func checkProcessesBlockingUninstall(pkgRoot, verDir, targetVer, pkgName string) error {
	blockDirs := []string{verDir}
	if cur, err := apps.ReadCurrent(pkgRoot); err == nil && cur == targetVer {
		blockDirs = append(blockDirs, filepath.Join(pkgRoot, apps.CurrentLinkName))
	}
	procs, err := procutil.ProcessesBlockingRemove(blockDirs...)
	if err != nil {
		return fmt.Errorf("check running processes: %w", err)
	}
	if len(procs) > 0 {
		return processesBlockingUninstallError(pkgName, targetVer, procs)
	}
	return nil
}

func isAccessDeniedRemoveErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "access is denied") || strings.Contains(msg, "used by another process")
}

func accessDeniedUninstallHint(pkgName string) string {
	var b strings.Builder
	b.WriteString("Files are still locked. Common causes:\n")
	b.WriteString("  - A background service or daemon is still running (e.g. Tailscale: run Glue as Administrator)\n")
	b.WriteString("  - File Explorer holds a shell extension DLL; restart Explorer:\n")
	b.WriteString("    Stop-Process -Name explorer -Force; Start-Process explorer\n")
	if pkgName != "" {
		b.WriteString(fmt.Sprintf("  - Retry after closing all %s processes in Task Manager", pkgName))
	}
	return b.String()
}
