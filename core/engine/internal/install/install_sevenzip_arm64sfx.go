package install

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gluestick-sh/core/engine/internal/runtime"
	"github.com/gluestick-sh/core/manifest"
)

func isSevenZipArm64SFXPreInstall(pkgName, installArch string, hooks []string) bool {
	if pkgName != "7zip" || installArch != "arm64" || len(hooks) == 0 {
		return false
	}
	body := strings.ToLower(strings.Join(hooks, "\n"))
	return strings.Contains(body, "7zr") && strings.Contains(body, "invoke-externalcommand")
}

func cleanupSevenZipSFXArtifacts(installDir, downloadName string) {
	_ = os.Remove(filepath.Join(installDir, "Uninstall.exe"))
	_ = os.Remove(filepath.Join(installDir, downloadName))
	entries, err := os.ReadDir(installDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasSuffix(strings.ToLower(name), "-arm64.exe") {
			_ = os.Remove(filepath.Join(installDir, name))
		}
	}
}

// applySevenZipArm64SFXInstall handles Scoop's arm64 7zip SFX without PowerShell pre_install.
// Extraction uses the same Go/7z path as other packages (~/.glue/bin/7z.exe when present).
func applySevenZipArm64SFXInstall(e *runtime.Engine, ctx context.Context, installDir, downloadName, archiveHash string, m *manifest.Manifest) error {
	if manifestBinExistsAtRoot(installDir, m) {
		cleanupSevenZipSFXArtifacts(installDir, downloadName)
		return nil
	}

	sfxPath := filepath.Join(installDir, downloadName)
	if _, err := os.Stat(sfxPath); err != nil {
		if archiveHash == "" {
			return fmt.Errorf("7zip arm64 installer not found: %w", err)
		}
		if err := materializeInstallerFile(e.Store, installDir, downloadName, archiveHash); err != nil {
			return fmt.Errorf("stage 7zip arm64 sfx: %w", err)
		}
	}

	if err := ensureExtractor7zWithProf(e, ctx, nil, "7zip"); err != nil {
		return fmt.Errorf("7zip arm64 sfx: %w", err)
	}
	if err := e.Extractor.ExtractToDir(sfxPath, installDir, downloadName); err != nil {
		return fmt.Errorf("extract 7zip arm64 sfx: %w", err)
	}

	cleanupSevenZipSFXArtifacts(installDir, downloadName)
	if err := normalizeSevenZipArm64Names(installDir); err != nil {
		return err
	}
	if !manifestBinExistsAtRoot(installDir, m) {
		return fmt.Errorf("7zip arm64 install incomplete after sfx extract")
	}
	return nil
}

// normalizeSevenZipArm64Names renames 7-Zip installer outputs (_7z.exe, _7zip.dll, …)
// to the standard names expected by the manifest, shortcuts, and install-context.reg.
// Both arm64 SFX and x64/x86 MSI ship underscore-prefixed files inside the archive.
func normalizeSevenZipArm64Names(installDir string) error {
	renames := map[string]string{
		"_7z.exe":     "7z.exe",
		"_7zG.exe":    "7zG.exe",
		"_7zFM.exe":   "7zFM.exe",
		"_7z.dll":     "7z.dll",
		"_7zip.dll":   "7-zip.dll",
		"_7zip32.dll": "7zip32.dll",
		"_7zip.chm":   "7-zip.chm",
		"_7-zip.chm":  "7-zip.chm",
	}
	for src, dst := range renames {
		srcPath := filepath.Join(installDir, src)
		if _, err := os.Stat(srcPath); err != nil {
			continue
		}
		dstPath := filepath.Join(installDir, dst)
		if srcPath == dstPath {
			continue
		}
		if _, err := os.Stat(dstPath); err == nil {
			if err := os.Remove(dstPath); err != nil {
				return fmt.Errorf("replace 7zip %s: %w", dst, err)
			}
		}
		if err := os.Rename(srcPath, dstPath); err != nil {
			return fmt.Errorf("rename 7zip %s to %s: %w", src, dst, err)
		}
	}
	return nil
}

func repairSevenZipArm64Layout(installDir string) error {
	if _, err := os.Stat(filepath.Join(installDir, "_7zip.dll")); err != nil {
		return nil
	}
	return normalizeSevenZipArm64Names(installDir)
}

func archiveHashForDownload(installedFiles map[string]string, downloadName string) string {
	for hash, name := range installedFiles {
		if filepath.Base(name) == downloadName {
			return hash
		}
	}
	return ""
}
