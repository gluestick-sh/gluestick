package install

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gluestick-sh/core/manifest"
	"github.com/gluestick-sh/core/store"
)

// isPortableExeInstall reports Scoop-style single-file .exe downloads.
func isPortableExeInstall(m *manifest.Manifest, downloadName, installArch string) bool {
	if m == nil || !strings.EqualFold(filepath.Ext(downloadName), ".exe") {
		return false
	}
	if m.HasInstallerScriptForInstall(installArch) || len(m.Installer.Args) > 0 {
		return false
	}
	if m.InnoSetup {
		return false
	}
	if m.GetExtractDirForInstall(installArch) != "" {
		return false
	}
	if preInstallExtractsDownloadBlob(m, installArch) {
		return false
	}
	dlBase := strings.ToLower(filepath.Base(downloadName))
	bins := m.Binaries()
	if len(bins) == 0 {
		return true
	}
	for _, binPattern := range bins {
		binName, _ := ParseBinPattern(binPattern)
		if binName == "" {
			continue
		}
		if strings.EqualFold(filepath.Base(binName), dlBase) {
			return true
		}
	}
	// Architecture-specific URL with a different bin name (e.g. comet on arm64).
	return len(bins) == 1
}

// isPreInstall7zHookInstall reports Scoop-style SFX .exe installs that link the installer
// blob and extract via Expand-7zipArchive in pre_install (e.g. wpsoffice).
func isPreInstall7zHookInstall(m *manifest.Manifest, downloadName, installArch, pkgName string) bool {
	if m == nil || !strings.EqualFold(filepath.Ext(downloadName), ".exe") {
		return false
	}
	hooks := m.PreInstallHooksForInstall(installArch)
	if len(hooks) == 0 {
		return false
	}
	if isSevenZipArm64SFXPreInstall(pkgName, installArch, hooks) {
		return false
	}
	body := strings.ToLower(strings.Join(hooks, "\n"))
	if !strings.Contains(body, "expand-7ziparchive") {
		return false
	}
	for _, hook := range hooks {
		h := strings.ToLower(hook)
		if strings.Contains(h, "expand-7ziparchive") && strings.Contains(h, "$fname") {
			return true
		}
	}
	return false
}

// preInstallExtractsDownloadBlob reports whether pre_install hooks unpack the linked
// installer blob (7z/MSI/Inno/dark). Rename-only hooks (e.g. bitwarden) still link the exe.
func preInstallExtractsDownloadBlob(m *manifest.Manifest, installArch string) bool {
	hooks := m.PreInstallHooksForInstall(installArch)
	if len(hooks) == 0 {
		return false
	}
	body := strings.ToLower(strings.Join(hooks, "\n"))
	for _, marker := range []string{
		"expand-7ziparchive",
		"expand-msiarchive",
		"expand-innoarchive",
		"expand-darkarchive",
		"msiextract",
	} {
		if strings.Contains(body, marker) {
			return true
		}
	}
	return false
}

// skipArchiveBlobOnLink reports whether the download blob should be omitted when linking from cache.
func skipArchiveBlobOnLink(fileExt, downloadName string, m *manifest.Manifest, installArch, pkgName string) bool {
	if isPlainFileExt(fileExt) {
		return false
	}
	if manifest.IsScoopMsiHookInstall(downloadName, fileExt) {
		return false
	}
	if isPreInstall7zHookInstall(m, downloadName, installArch, pkgName) {
		return false
	}
	return true
}

// resolveInstalledBinPath locates an installed binary when manifest bin names differ from the downloaded exe.
func resolveInstalledBinPath(installDir, binName, downloadName string) (exePath, relBin string, ok bool) {
	for _, candidate := range ResolveBinCandidates(installDir, binName, downloadName) {
		if _, err := os.Stat(candidate); err == nil {
			rel, err := filepath.Rel(installDir, candidate)
			if err != nil {
				rel = filepath.Base(candidate)
			}
			return candidate, rel, true
		}
	}
	entries, err := os.ReadDir(installDir)
	if err != nil {
		return "", "", false
	}
	var exes []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if strings.EqualFold(filepath.Ext(entry.Name()), ".exe") {
			exes = append(exes, entry.Name())
		}
	}
	if len(exes) == 1 {
		rel := exes[0]
		return filepath.Join(installDir, rel), rel, true
	}
	return "", "", false
}

func shouldSkipPortableScanDir(name string) bool {
	lower := strings.ToLower(name)
	switch lower {
	case "node_modules", ".git", "__pycache__":
		return true
	default:
		return strings.HasPrefix(name, ".")
	}
}

// ResolveBinCandidates returns possible on-disk locations for a manifest bin, checking the
// install root and one level of subdirectories (including Scoop's "_"-prefixed variants).
func ResolveBinCandidates(installDir, binName, downloadName string) []string {
	out := installRootBinCandidates(installDir, binName, downloadName)
	seen := make(map[string]struct{}, len(out))
	for _, p := range out {
		seen[p] = struct{}{}
	}
	if binName != "" {
		entries, err := os.ReadDir(installDir)
		if err == nil {
			for _, entry := range entries {
				if !entry.IsDir() || shouldSkipPortableScanDir(entry.Name()) {
					continue
				}
				for _, rel := range []string{binName, "_" + binName} {
					path := filepath.Join(installDir, entry.Name(), rel)
					if _, ok := seen[path]; ok {
						continue
					}
					if _, err := os.Stat(path); err != nil {
						continue
					}
					seen[path] = struct{}{}
					out = append(out, path)
				}
			}
		}
	}
	return out
}

func installRootBinCandidates(installDir, binName, downloadName string) []string {
	seen := make(map[string]struct{})
	var out []string
	add := func(path string) {
		if path == "" {
			return
		}
		if _, ok := seen[path]; ok {
			return
		}
		if _, err := os.Stat(path); err != nil {
			return
		}
		seen[path] = struct{}{}
		out = append(out, path)
	}
	if binName != "" {
		add(filepath.Join(installDir, binName))
		add(filepath.Join(installDir, "_"+binName))
		add(filepath.Join(installDir, "SourceDir", binName))
	}
	if downloadName != "" {
		add(filepath.Join(installDir, downloadName))
	}
	return out
}

func linkPortableExe(store *store.Store, installDir, downloadName, hash string, recordFile func(hash, rel string)) error {
	targetPath := filepath.Join(installDir, downloadName)
	if err := store.Link(hash, targetPath); err != nil {
		return fmt.Errorf("link %s: %w", downloadName, err)
	}
	if recordFile != nil {
		recordFile(hash, downloadName)
	}
	return nil
}

func manifestBinExistsAtRoot(installDir string, m *manifest.Manifest) bool {
	return installBinLayoutReady(installDir, m)
}

func installBinLayoutReady(installDir string, m *manifest.Manifest) bool {
	if m == nil {
		return true
	}
	for _, binPattern := range m.Binaries() {
		binName, _ := ParseBinPattern(binPattern)
		if binName == "" {
			continue
		}
		found := false
		for _, candidate := range installRootBinCandidates(installDir, binName, "") {
			if _, err := os.Stat(candidate); err == nil {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func nestedDirWithManifestBinLayout(installDir string, m *manifest.Manifest) (string, bool) {
	entries, err := os.ReadDir(installDir)
	if err != nil {
		return "", false
	}
	var matches []string
	for _, entry := range entries {
		if !entry.IsDir() || shouldSkipPortableScanDir(entry.Name()) {
			continue
		}
		sub := filepath.Join(installDir, entry.Name())
		if installBinLayoutReady(sub, m) {
			matches = append(matches, entry.Name())
		}
	}
	if len(matches) != 1 {
		return "", false
	}
	return matches[0], true
}

// ensureExtractDirFlattened flattens installDir/extract_dir when bins are still nested
// (e.g. zip linked without prefix strip, or backslash extract_dir mismatch).
func ensureExtractDirFlattened(installDir string, m *manifest.Manifest, installArch string) (bool, error) {
	if m == nil {
		return false, nil
	}
	if installBinLayoutReady(installDir, m) {
		return false, nil
	}
	var flattened bool

	extractDir := m.GetExtractDirForInstall(installArch)
	if extractDir != "" {
		applied, err := applyExtractDirLayout(installDir, extractDir)
		if err != nil {
			return flattened, err
		}
		if applied {
			flattened = true
			if installBinLayoutReady(installDir, m) {
				return flattened, nil
			}
		}
	}

	for depth := 0; depth < 4; depth++ {
		if installBinLayoutReady(installDir, m) {
			return flattened, nil
		}
		nested, ok := nestedDirWithManifestBinLayout(installDir, m)
		if !ok {
			return flattened, nil
		}
		if err := flattenExtractDir(installDir, nested); err != nil {
			return flattened, err
		}
		flattened = true
	}
	return flattened, nil
}

func validateManifestBins(installDir string, m *manifest.Manifest) error {
	for _, binPattern := range m.Binaries() {
		binName, _ := ParseBinPattern(binPattern)
		if binName == "" {
			continue
		}
		if _, _, ok := resolveInstalledBinPath(installDir, binName, ""); !ok {
			return fmt.Errorf("expected binary missing after install: %s (installer may be incomplete; try force reinstall)", binName)
		}
	}
	return nil
}
