package install

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/gluestick-sh/core/bootstrap"
	"github.com/gluestick-sh/core/git"
)

// ToolAvailability is the result of probing for a runtime tool without downloading.
type ToolAvailability struct {
	OK            bool   // tool was found and is runnable
	Path          string // resolved executable path
	InSystemPath  bool   // found on the system PATH
	FromBootstrap bool   // found under the glue-managed bin directory
}

// BootstrappedGitPath returns the expected MinGit executable path under root.
func BootstrappedGitPath(root string) string {
	return filepath.Join(root, "bin", "git", "mingw64", "bin", "git.exe")
}

// ProbeGit reports whether git is on PATH or bootstrapped under glue root.
func ProbeGit(root string) ToolAvailability {
	r := git.NewRunner()
	if err := r.Check(); err == nil {
		path := "git"
		if p, err := exec.LookPath("git"); err == nil {
			path = p
		} else if p, err := exec.LookPath("git.exe"); err == nil {
			path = p
		}
		return ToolAvailability{OK: true, Path: path, InSystemPath: true}
	}
	if root != "" {
		boot := BootstrappedGitPath(root)
		if bootstrap.GitExecutableReady(boot) {
			return ToolAvailability{OK: true, Path: boot, FromBootstrap: true}
		}
	}
	return ToolAvailability{}
}

// ProbeSevenZip reports whether 7-Zip exists under glue root or on PATH.
func ProbeSevenZip(root string) ToolAvailability {
	path := ResolveLocalSevenZip(root)
	if path == "" {
		return ToolAvailability{}
	}
	avail := ToolAvailability{OK: true, Path: path}
	if root != "" && isUnderGlueBin(root, path) {
		avail.FromBootstrap = true
	}
	return avail
}

// ProbeDark reports whether WiX dark/wix is available (bootstrap or glue install).
func ProbeDark(root string) ToolAvailability {
	path, err := resolveDark(root)
	if err != nil {
		return ToolAvailability{}
	}
	avail := ToolAvailability{OK: true, Path: path}
	if root != "" && isUnderGlueBin(root, path) {
		avail.FromBootstrap = true
	}
	return avail
}

// ProbeInnounp reports whether innounp is available under glue root.
func ProbeInnounp(root string) ToolAvailability {
	path, err := resolveInnounp(root)
	if err != nil {
		return ToolAvailability{}
	}
	avail := ToolAvailability{OK: true, Path: path}
	if root != "" && isUnderGlueBin(root, path) {
		avail.FromBootstrap = true
	}
	return avail
}

func isUnderGlueBin(root, toolPath string) bool {
	bin := filepath.Clean(filepath.Join(root, "bin"))
	clean := filepath.Clean(toolPath)
	return clean == bin || strings.HasPrefix(clean, bin+string(os.PathSeparator))
}
