package install

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/gluestick-sh/core/apps"
	"github.com/gluestick-sh/core/engine/internal/runtime"
	"github.com/gluestick-sh/core/manifest"
	"github.com/gluestick-sh/core/verbose"
)

// packageNeedsCurrentRepair reports a failed uninstall that removed current but left the version directory.
func packageNeedsCurrentRepair(root, pkgName string) (version string, ok bool) {
	pkgRoot := apps.PkgRoot(root, pkgName)
	current := filepath.Join(pkgRoot, apps.CurrentLinkName)
	if _, err := os.Lstat(current); err == nil {
		return "", false
	}
	ver, ok := apps.PickDefaultVersion(pkgRoot)
	return ver, ok
}

// repairBrokenCurrentInstall restores current and shims after a partial uninstall.
func repairBrokenCurrentInstall(e *runtime.Engine, pkgRef, pkgName, version string, m *manifest.Manifest) error {
	root := e.Config.RootDir
	pkgRoot := apps.PkgRoot(root, pkgName)
	if !runtime.VersionDirInstalled(root, pkgName, version) {
		return fmt.Errorf("repair %s@%s: install directory missing", pkgName, version)
	}
	installDir := filepath.Join(pkgRoot, version)
	verbose.Progressf("  Restoring broken install for %s@%s (current link missing)...\n", pkgName, version)
	if err := apps.LinkCurrent(pkgRoot, version); err != nil {
		return fmt.Errorf("restore current link: %w", err)
	}
	shimsMetaDir := filepath.Join(root, "shims-meta")
	appsDir := filepath.Join(root, "apps")
	if _, err := removeShimsForPackage(e.ShimMgr, shimsMetaDir, appsDir, pkgName); err != nil {
		return fmt.Errorf("remove broken shims: %w", err)
	}
	currentDir := filepath.Join(pkgRoot, apps.CurrentLinkName)
	if err := createPackageShims(e.ShimMgr, shimsMetaDir, pkgName, installDir, currentDir, m); err != nil {
		return fmt.Errorf("recreate shims: %w", err)
	}
	verbose.Progressf("  %s Restored %s@%s\n", successMark(), pkgName, version)
	return nil
}
