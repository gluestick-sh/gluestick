package install

import (
	"os"
	"path/filepath"

	"github.com/gluestick-sh/core/apps"
	"github.com/gluestick-sh/core/engine/internal/runtime"
)

// cleanupIncompleteInstall removes artifacts from a failed install attempt for one version.
func cleanupIncompleteInstall(e *runtime.Engine, root, pkgName, version string) {
	if pkgName == "" || version == "" {
		return
	}
	if e != nil && e.Cache != nil {
		if inst, ok := e.Cache.GetInstalled(pkgName); ok && inst.Version == version {
			return
		}
	}

	pkgRoot := apps.PkgRoot(root, pkgName)
	installDir := filepath.Join(pkgRoot, version)

	if cur, err := apps.ReadCurrent(pkgRoot); err == nil && cur == version {
		_ = apps.RemoveCurrent(pkgRoot)
	}

	if e != nil && e.ShimMgr != nil {
		shimsMetaDir := filepath.Join(root, "shims-meta")
		appsDir := filepath.Join(root, "apps")
		_, _ = removeShimsForPackage(e.ShimMgr, shimsMetaDir, appsDir, pkgName)
	}

	if e != nil && e.Cache != nil {
		if entry, ok := e.Cache.Get(pkgName); ok && entry.Version == version {
			_ = e.Cache.Remove(pkgName)
		}
	}

	if runtime.VersionDirInstalled(root, pkgName, version) {
		_ = RemoveAll(installDir)
	}

	if entries, err := os.ReadDir(pkgRoot); err == nil && len(entries) == 0 {
		_ = os.Remove(pkgRoot)
	}
}
