package main

// Installed-package helpers: parse refs and check apps/ install state.

import (
	"github.com/gluestick-sh/core/engine"
)

// packageBaseName returns the app directory name from a package reference.
func packageBaseName(pkgRef string) string {
	name, _ := engine.ParsePkgRef(pkgRef)
	return name
}

// installedPackage reports whether pkgName is installed under root/apps.
// Returns the active version (via current junction when present).
func installedPackage(root, pkgName string) (version string, ok bool) {
	return engine.EnsureInstalledVersion(root, pkgName)
}
