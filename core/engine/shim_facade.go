package engine

import (
	"github.com/gluestick-sh/core/engine/internal/install"
	"github.com/gluestick-sh/core/manifest"
	"github.com/gluestick-sh/core/shim"
)

// RemoveShimsForPackage removes shims belonging to pkgName.
func RemoveShimsForPackage(shimMgr *shim.Manager, shimsMetaDir, appsDir, pkgName string) ([]string, error) {
	return install.RemoveShimsForPackage(shimMgr, shimsMetaDir, appsDir, pkgName)
}

// CreatePackageShims registers PATH shims for m.
func CreatePackageShims(shimMgr *shim.Manager, shimsMetaDir, pkgName, installDir, shimDir string, m *manifest.Manifest) error {
	return install.CreatePackageShims(shimMgr, shimsMetaDir, pkgName, installDir, shimDir, m)
}
