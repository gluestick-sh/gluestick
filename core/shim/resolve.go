package shim

import (
	"os"
	"path/filepath"
	"strings"
)

// resolveShimStubPath returns the path to the prebuilt shim runner (shim.exe), or "" if not found.
func resolveShimStubPath(glueRootDir string) string {
	var candidates []string

	if glueRootDir != "" {
		candidates = append(candidates, filepath.Join(glueRootDir, "shim.exe"))
	}

	if execPath, err := os.Executable(); err == nil {
		execPath, _ = filepath.EvalSymlinks(execPath)
		execDir := filepath.Dir(execPath)
		candidates = append(candidates, filepath.Join(execDir, "shim.exe"))
		candidates = append(candidates, walkUpShimStub(execDir)...)
	}

	if cwd, err := os.Getwd(); err == nil && cwd != "" {
		candidates = append(candidates, devShimStubCandidates(cwd)...)
	}

	if pd := os.Getenv("PROGRAMDATA"); pd != "" {
		candidates = append(candidates, filepath.Join(pd, "glue", "shim.exe"))
	}

	seen := make(map[string]struct{})
	for _, p := range candidates {
		if p == "" {
			continue
		}
		p = filepath.Clean(p)
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// devShimStubCandidates returns local dev paths for a prebuilt shim.exe under dir.
func devShimStubCandidates(dir string) []string {
	return []string{
		filepath.Join(dir, "shim", "shim.exe"),
		filepath.Join(dir, "shim.exe"),
	}
}

func walkUpShimStub(startDir string) []string {
	var out []string
	dir := startDir
	for range 8 {
		out = append(out, devShimStubCandidates(dir)...)
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return out
}

// cacheShimStub copies stub to glueRoot/shim.exe so later installs do not depend on cwd.
func cacheShimStub(glueRootDir, stubPath string) error {
	if glueRootDir == "" || stubPath == "" {
		return nil
	}
	dest := filepath.Join(glueRootDir, "shim.exe")
	if strings.EqualFold(filepath.Clean(dest), filepath.Clean(stubPath)) {
		return nil
	}
	data, err := os.ReadFile(stubPath)
	if err != nil {
		return err
	}
	return os.WriteFile(dest, data, 0755)
}
