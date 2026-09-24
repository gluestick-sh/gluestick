package procutil

import (
	"path/filepath"
	"strings"
)

// RunningProcess is a process whose executable lives under an install directory.
type RunningProcess struct {
	PID  int
	Name string
	Path string
}

// ProcessesBlockingRemove reports processes running from any of the given directories.
func ProcessesBlockingRemove(dirs ...string) ([]RunningProcess, error) {
	normalized, err := normalizeBlockDirs(dirs...)
	if err != nil {
		return nil, err
	}
	if len(normalized) == 0 {
		return nil, nil
	}
	return findProcessesRunningFrom(normalized)
}

// ProcessesWithModulesFrom reports processes that loaded a module from any of the given directories.
func ProcessesWithModulesFrom(dirs ...string) ([]RunningProcess, error) {
	normalized, err := normalizeBlockDirs(dirs...)
	if err != nil {
		return nil, err
	}
	if len(normalized) == 0 {
		return nil, nil
	}
	return findProcessesWithModulesFrom(normalized)
}

// ProcessesBlockingUninstall merges executable and loaded-module blockers for an install directory.
func ProcessesBlockingUninstall(dirs ...string) ([]RunningProcess, error) {
	byExe, err := ProcessesBlockingRemove(dirs...)
	if err != nil {
		return nil, err
	}
	byMod, err := ProcessesWithModulesFrom(dirs...)
	if err != nil {
		return nil, err
	}
	return mergeRunningProcesses(byExe, byMod), nil
}

func normalizeBlockDirs(dirs ...string) ([]string, error) {
	normalized := make([]string, 0, len(dirs))
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		abs, err := filepath.Abs(dir)
		if err != nil {
			return nil, err
		}
		normalized = append(normalized, filepath.Clean(abs))
	}
	return normalized, nil
}

func mergeRunningProcesses(groups ...[]RunningProcess) []RunningProcess {
	seen := make(map[int]RunningProcess)
	for _, group := range groups {
		for _, p := range group {
			if _, ok := seen[p.PID]; !ok {
				seen[p.PID] = p
			}
		}
	}
	out := make([]RunningProcess, 0, len(seen))
	for _, p := range seen {
		out = append(out, p)
	}
	return out
}

func normalizePath(path string) string {
	return strings.ToLower(filepath.ToSlash(filepath.Clean(path)))
}

func pathUnderDir(exePath, dir string) bool {
	if exePath == "" || dir == "" {
		return false
	}
	exeLower := normalizePath(exePath)
	dirLower := normalizePath(dir)
	if exeLower == dirLower {
		return true
	}
	return strings.HasPrefix(exeLower, dirLower+"/")
}
