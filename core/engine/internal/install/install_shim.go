package install

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gluestick-sh/core/engine/internal/runtime"
	"github.com/gluestick-sh/core/manifest"
	"github.com/gluestick-sh/core/shim"
	"github.com/gluestick-sh/core/verbose"
)

// ShimNameForBin returns the PATH command name for a manifest bin entry.
func ShimNameForBin(exePath, alias string) string {
	return shimNameForBin(exePath, alias)
}

// RemoveShimsForPackage removes shims belonging to pkgName.
func RemoveShimsForPackage(shimMgr *shim.Manager, shimsMetaDir, appsDir, pkgName string) ([]string, error) {
	return removeShimsForPackage(shimMgr, shimsMetaDir, appsDir, pkgName)
}

// CreatePackageShims registers PATH shims for m. Names already provided by another
// installed package are skipped because the shims directory is a flat namespace.
func CreatePackageShims(shimMgr *shim.Manager, shimsMetaDir, pkgName, installDir, shimDir string, m *manifest.Manifest) error {
	return createPackageShims(shimMgr, shimsMetaDir, pkgName, installDir, shimDir, m)
}

func ensureExtractor7zWithProf(e *runtime.Engine, ctx context.Context, prof *installPhaseProfile, pkgName string) error {
	runBootstrap := func(fn func() error) error {
		if prof != nil {
			return prof.runBootstrap(fn)
		}
		return fn()
	}
	// Installing the 7zip package itself may use the minimal 7za bootstrap.
	// Every other package that extracts archives (including NSIS #/dl.7z) needs
	// the full 7-Zip build with 7z.dll codecs.
	needFull := pkgName != "7zip"
	if needFull {
		if SetFullSevenZipFromLocal(e) {
			cleanupSevenZipSeeds(e)
			return nil
		}
	} else if SetSevenZipFromLocal(e) {
		cleanupSevenZipSeeds(e)
		return nil
	}
	err := runBootstrap(func() error {
		var (
			path string
			bErr error
		)
		if needFull {
			path, bErr = e.Bootstrap.Ensure7zip(ctx)
		} else {
			path, bErr = e.Bootstrap.Ensure7z(ctx)
		}
		if bErr != nil {
			return bErr
		}
		e.Extractor.Set7zPath(path)
		return nil
	})
	if err == nil {
		cleanupSevenZipSeeds(e)
	}
	return err
}

func cleanupSevenZipSeeds(e *runtime.Engine) {
	if e != nil && e.Bootstrap != nil {
		e.Bootstrap.CleanupSevenZipSeeds()
	}
}

func createPackageShims(shimMgr *shim.Manager, shimsMetaDir, pkgName, installDir, shimDir string, m *manifest.Manifest) error {
	if shimDir == "" {
		shimDir = installDir
	}
	shimEnv := ShimEnvForPackage(m, installDir, "")
	var firstErr error
	recordErr := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	tryCreateShim := func(shimName, shimTarget, label, fromDir string, extraArgs []string) {
		if owner, conflict := shimOwnedByOtherPackage(shimsMetaDir, shimName, pkgName); conflict {
			verbose.Progressf("    skipped shim %s (already provided by %s)\n", shimName, owner)
			return
		}
		opts := shim.CreateOpts{Args: extraArgs, Env: shimEnv}
		if err := shimMgr.Create(shimName, shimTarget, opts); err != nil {
			verbose.Progressf("    %s failed to create shim for %s: %v\n", failedMark(), label, err)
			recordErr(fmt.Errorf("shim %s: %w", shimName, err))
			return
		}
		if fromDir != "" {
			verbose.Progressf("    %s %s (from %s)\n", successMark(), shimName, fromDir)
		} else {
			verbose.Progressf("    %s %s\n", successMark(), shimName)
		}
	}

	if bins := m.Binaries(); len(bins) > 0 {
		verbose.Progressf("  Creating shims...\n")
		for _, binPattern := range bins {
			binName, binAlias, extraArgs := ParseBinPatternParts(binPattern)
			if binName == "" {
				continue
			}
			shimName := shimNameForBin(binName, binAlias)
			relBin := binName
			exePath := filepath.Join(installDir, binName)
			if resolved, rel, ok := resolveInstalledBinPath(installDir, binName, ""); ok {
				exePath = resolved
				relBin = rel
			} else if _, err := os.Stat(exePath); os.IsNotExist(err) {
				altPath := filepath.Join(installDir, "_"+binName)
				if _, statErr := os.Stat(altPath); statErr == nil {
					exePath = altPath
					relBin = "_" + binName
				}
			}
			shimTarget := filepath.Join(shimDir, relBin)
			_ = exePath
			tryCreateShim(shimName, shimTarget, binName, "", extraArgs)
		}
	}

	if envPaths := m.EnvAddPaths(); len(envPaths) > 0 {
		if len(m.Binaries()) == 0 {
			verbose.Progressf("  Creating shims (from env_add_path)...\n")
		}
		for _, dirPath := range envPaths {
			scanDir := installDir
			if dirPath != "." {
				scanDir = filepath.Join(installDir, dirPath)
			}
			entries, err := os.ReadDir(scanDir)
			if err != nil {
				if os.IsNotExist(err) {
					continue
				}
				verbose.Progressf("    cannot read directory %s: %v\n", scanDir, err)
				recordErr(fmt.Errorf("read %s: %w", scanDir, err))
				continue
			}
			for _, entry := range entries {
				if entry.IsDir() {
					continue
				}
				name := entry.Name()
				lower := strings.ToLower(name)
				if !strings.HasSuffix(lower, ".exe") && !strings.HasSuffix(lower, ".cmd") {
					continue
				}
				shimName := strings.TrimSuffix(strings.TrimSuffix(name, ".exe"), ".cmd")
				shimName = strings.TrimSuffix(strings.TrimSuffix(shimName, ".EXE"), ".CMD")
				exePath := filepath.Join(scanDir, name)
				rel, err := filepath.Rel(installDir, exePath)
				if err != nil {
					rel = name
				}
				shimTarget := filepath.Join(shimDir, rel)
				tryCreateShim(shimName, shimTarget, name, dirPath, nil)
			}
		}
	}

	return firstErr
}

func removeShimsForPackage(shimMgr *shim.Manager, shimsMetaDir, appsDir, pkgName string) ([]string, error) {
	entries, err := os.ReadDir(shimsMetaDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var removed []string
	for _, ent := range entries {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".json") {
			continue
		}

		configPath := filepath.Join(shimsMetaDir, ent.Name())
		data, err := os.ReadFile(configPath)
		if err != nil {
			continue
		}

		var cfg shim.Config
		if err := json.Unmarshal(data, &cfg); err != nil {
			continue
		}

		if shimMetaBelongsToPackage(cfg.Path, pkgName) {
			shimName := strings.TrimSuffix(ent.Name(), ".json")
			if err := shimMgr.Remove(shimName); err != nil {
				verbose.Progressf("    %s Failed to remove shim %s: %v\n", failedMark(), shimName, err)
				continue
			}
			removed = append(removed, shimName)
		}
	}

	return removed, nil
}

func shimMetaBelongsToPackage(cfgPath, pkgName string) bool {
	return packageNameFromAppsPath(cfgPath) == pkgName
}

func shimOwnerPackage(shimsMetaDir, shimName string) string {
	configPath := filepath.Join(shimsMetaDir, shimName+".json")
	data, err := os.ReadFile(configPath)
	if err != nil {
		return ""
	}
	var cfg shim.Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return ""
	}
	return packageNameFromAppsPath(cfg.Path)
}

func shimOwnedByOtherPackage(shimsMetaDir, shimName, pkgName string) (owner string, conflict bool) {
	owner = shimOwnerPackage(shimsMetaDir, shimName)
	if owner == "" || owner == pkgName {
		return "", false
	}
	return owner, true
}

func packageNameFromAppsPath(cfgPath string) string {
	normalized := filepath.ToSlash(cfgPath)
	const marker = "/apps/"
	idx := strings.Index(normalized, marker)
	if idx < 0 {
		return ""
	}
	rest := normalized[idx+len(marker):]
	if end := strings.Index(rest, "/"); end >= 0 {
		return rest[:end]
	}
	return ""
}

// HasShimsForPackage reports whether shims-meta still references the package.
func HasShimsForPackage(shimsMetaDir, pkgName string) bool {
	entries, err := os.ReadDir(shimsMetaDir)
	if err != nil {
		return false
	}
	for _, ent := range entries {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(shimsMetaDir, ent.Name()))
		if err != nil {
			continue
		}
		var cfg shim.Config
		if err := json.Unmarshal(data, &cfg); err != nil {
			continue
		}
		if shimMetaBelongsToPackage(cfg.Path, pkgName) {
			return true
		}
	}
	return false
}

func shimNameForBin(exePath, alias string) string {
	if name := normalizeShimAlias(alias); name != "" {
		return name
	}
	name := filepath.Base(exePath)
	for _, ext := range []string{".exe", ".bat", ".cmd", ".sh", ".EXE", ".BAT", ".CMD", ".SH"} {
		name = strings.TrimSuffix(name, ext)
	}
	return name
}

func normalizeShimAlias(alias string) string {
	name := strings.TrimSpace(alias)
	if name == "" {
		return ""
	}
	for _, ext := range []string{".exe", ".bat", ".cmd", ".sh", ".EXE", ".BAT", ".CMD", ".SH"} {
		name = strings.TrimSuffix(name, ext)
	}
	return name
}

// GetShimsForPackage lists shim names whose prefix matches pkgName for the given version.
func GetShimsForPackage(e *runtime.Engine, pkgName, version string) ([]string, error) {
	shims, err := e.ShimMgr.List()
	if err != nil {
		return nil, err
	}
	var result []string
	for _, s := range shims {
		if strings.HasPrefix(s.Name, pkgName) {
			result = append(result, s.Name)
		}
	}
	return result, nil
}
