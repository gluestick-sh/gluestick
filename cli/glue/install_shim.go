package main

// Thin CLI wrappers around engine shim helpers (reset and tests).

import (
	"github.com/gluestick-sh/core/engine"
	"github.com/gluestick-sh/core/manifest"
	"github.com/gluestick-sh/core/shim"
)

func removeShimsForPackage(shimMgr *shim.Manager, shimsMetaDir, appsDir, pkgName string) ([]string, error) {
	return engine.RemoveShimsForPackage(shimMgr, shimsMetaDir, appsDir, pkgName)
}

func createPackageShims(shimMgr *shim.Manager, shimsMetaDir, pkgName, installDir, shimDir string, m *manifest.Manifest) error {
	return engine.CreatePackageShims(shimMgr, shimsMetaDir, pkgName, installDir, shimDir, m)
}
