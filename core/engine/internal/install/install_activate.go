package install

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/gluestick-sh/core/apps"
	"github.com/gluestick-sh/core/cache"
	"github.com/gluestick-sh/core/engine/internal/runtime"
	etypes "github.com/gluestick-sh/core/engine/types"
	"github.com/gluestick-sh/core/manifest"
	"github.com/gluestick-sh/core/message"
	"github.com/gluestick-sh/core/verbose"
)

// ActiveInstallVersion returns the on-disk active version without creating current.
func ActiveInstallVersion(root, pkgName string) string {
	pkgRoot := apps.PkgRoot(root, pkgName)
	if ver, err := apps.ReadCurrent(pkgRoot); err == nil && ver != "" {
		if runtime.VersionDirInstalled(root, pkgName, ver) {
			return ver
		}
	}
	if ver, ok := runtime.InstalledPackage(root, pkgName); ok {
		return ver
	}
	return ""
}

// LocalVersionReadyToActivate reports whether a non-active version dir is complete enough to activate.
func LocalVersionReadyToActivate(root, pkgName, version string, m *manifest.Manifest) bool {
	if !runtime.VersionDirInstalled(root, pkgName, version) {
		return false
	}
	installDir := filepath.Join(apps.PkgRoot(root, pkgName), version)
	if _, err := apps.LoadInstallRecord(installDir); err == nil {
		return true
	}
	return orphanInstallRepairable(root, pkgName, version, m)
}

// activateLocalVersion switches to an already-installed version instead of downloading again.
func activateLocalVersion(e *runtime.Engine, 
	ctx context.Context,
	pkgRef, pkgName, version string,
	m *manifest.Manifest,
	req *etypes.InstallRequest,
	reporter etypes.ProgressReporter,
	prof *installPhaseProfile,
) error {
	report := func(phase etypes.Phase, status etypes.Status, pct float64, key string, args map[string]interface{}, bytes, total int64) {
		runtime.ReportProgress(reporter, phase, pkgRef, status, pct, key, args, bytes, total)
	}

	root := e.Config.RootDir
	installDir := filepath.Join(apps.PkgRoot(root, pkgName), version)

	if _, err := apps.LoadInstallRecord(installDir); err != nil {
		if orphanInstallRepairable(root, pkgName, version, m) {
			return repairOrphanInstall(e, ctx, pkgRef, pkgName, version, m, req, reporter, prof)
		}
		return fmt.Errorf("local version %s@%s is incomplete", pkgName, version)
	}

	verbose.Progressf("  Switching to locally installed %s@%s (skipping download)...\n", pkgName, version)
	report(PhaseLink, StatusRunning, 0, message.ProgressLinkingFiles, nil, 0, 0)

	files, totalSize, err := cache.ScanInstallDir(installDir, e.Store)
	if err != nil {
		return fmt.Errorf("scan local install %s@%s: %w", pkgName, version, err)
	}
	if len(files) == 0 {
		return fmt.Errorf("local install %s@%s has no indexed files", pkgName, version)
	}

	if err := e.Cache.Add(pkgName, version, files, totalSize); err != nil {
		return fmt.Errorf("update cache for %s@%s: %w", pkgName, version, err)
	}
	metadata := preserveInstalledMetadata(e.Cache, pkgName)
	if err := e.Cache.AddInstalled(pkgName, version, installDir, totalSize, metadata); err != nil {
		return fmt.Errorf("update installed record for %s@%s: %w", pkgName, version, err)
	}

	appsDir := filepath.Join(root, "apps")
	shimsMetaDir := filepath.Join(root, "shims-meta")
	if err := ResetPackage(e, appsDir, shimsMetaDir, pkgName+"@"+version); err != nil {
		return err
	}

	verbose.Progressf("  %s %s@%s activated\n", successMark(), pkgName, version)
	report(PhaseComplete, StatusSuccess, 100, message.ProgressPackageInstallComplete, map[string]interface{}{
		"package": pkgName,
	}, 0, 0)
	return nil
}
