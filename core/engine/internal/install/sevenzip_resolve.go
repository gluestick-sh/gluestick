package install

import (
	"os"
	"os/exec"
	"path/filepath"

	"github.com/gluestick-sh/core/apps"
	"github.com/gluestick-sh/core/engine/internal/runtime"
)

// ResolveLocalSevenZip returns an existing 7-Zip binary under glue root or on PATH.
// It never downloads; callers bootstrap only when this returns empty.
// Prefers the full codec build (7z.exe + 7z.dll) over the minimal 7za bootstrap,
// because NSIS/MSI/#/dl.7z extracts need codecs bundled in 7z.dll.
func ResolveLocalSevenZip(glueRoot string) string {
	if path := ResolveFullLocalSevenZip(glueRoot); path != "" {
		return path
	}
	for _, p := range minimalSevenZipCandidates(glueRoot) {
		if fileExists(p) {
			return p
		}
	}
	return ""
}

// ResolveFullLocalSevenZip returns a 7-Zip binary that has codecs (7z.dll beside it).
// Minimal bootstrap 7za (bin/7z.exe without 7z.dll) is skipped.
func ResolveFullLocalSevenZip(glueRoot string) string {
	for _, p := range fullSevenZipCandidates(glueRoot) {
		if fileExists(p) && isFullSevenZipBinary(p) {
			return p
		}
	}
	return ""
}

func fullSevenZipCandidates(glueRoot string) []string {
	var candidates []string
	if glueRoot != "" {
		if pkgRoot, version, err := apps.CurrentInstalledPath(glueRoot, "7zip"); err == nil {
			candidates = append(candidates, filepath.Join(pkgRoot, apps.CurrentLinkName, "7z.exe"))
			if version != "" {
				candidates = append(candidates, filepath.Join(pkgRoot, version, "7z.exe"))
			}
		}
		candidates = append(candidates, filepath.Join(glueRoot, "bin", "7z.exe"))
	}
	if p, err := exec.LookPath("7z"); err == nil {
		candidates = append(candidates, p)
	}
	if p, err := exec.LookPath("7z.exe"); err == nil {
		candidates = append(candidates, p)
	}
	return candidates
}

func minimalSevenZipCandidates(glueRoot string) []string {
	var candidates []string
	if glueRoot != "" {
		candidates = append(candidates,
			filepath.Join(glueRoot, "bin", "7z.exe"),
			filepath.Join(glueRoot, "bin", "7zr.exe"),
			filepath.Join(glueRoot, "bin", "7za.exe"),
		)
	}
	if p, err := exec.LookPath("7z"); err == nil {
		candidates = append(candidates, p)
	}
	if p, err := exec.LookPath("7z.exe"); err == nil {
		candidates = append(candidates, p)
	}
	return candidates
}

// isFullSevenZipBinary reports whether path is the full 7-Zip console build
// (7z.dll must sit next to the executable for NSIS/ISO/RAR codecs in 7-Zip 26+).
func isFullSevenZipBinary(path string) bool {
	if path == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(filepath.Dir(path), "7z.dll"))
	return err == nil
}

func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}

// SetSevenZipFromLocal configures the extractor from glue bin or PATH when available.
func SetSevenZipFromLocal(e *runtime.Engine) bool {
	if e == nil || e.Extractor == nil {
		return false
	}
	if path := e.Extractor.SevenZipPath(); path != "" {
		if fileExists(path) {
			return true
		}
	}
	if path := ResolveLocalSevenZip(e.Config.RootDir); path != "" {
		e.Extractor.Set7zPath(path)
		return true
	}
	return false
}

// SetFullSevenZipFromLocal configures the extractor only when a full codec 7-Zip is available.
func SetFullSevenZipFromLocal(e *runtime.Engine) bool {
	if e == nil || e.Extractor == nil {
		return false
	}
	if path := e.Extractor.SevenZipPath(); path != "" && fileExists(path) && isFullSevenZipBinary(path) {
		return true
	}
	root := ""
	if e.Config != nil {
		root = e.Config.RootDir
	}
	if path := ResolveFullLocalSevenZip(root); path != "" {
		e.Extractor.Set7zPath(path)
		return true
	}
	return false
}
