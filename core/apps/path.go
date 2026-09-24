package apps

import (
	"path/filepath"
)

// ParseInstallFilePath extracts package name and version from a file path under
// .../apps/<pkg>/<version>/... . Uses the apps path segment (not substring search).
func ParseInstallFilePath(path string) (pkgName, version string) {
	path = filepath.Clean(filepath.FromSlash(path))
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}

	verDir := filepath.Dir(path)
	version = filepath.Base(verDir)
	pkgRoot := filepath.Dir(verDir)
	pkgName = filepath.Base(pkgRoot)
	if filepath.Base(filepath.Dir(pkgRoot)) != "apps" {
		return "", ""
	}
	if version == CurrentLinkName {
		if ver, err := ReadCurrent(pkgRoot); err == nil && ver != "" {
			version = ver
		}
	}
	if pkgName == "" || version == "" {
		return "", ""
	}
	return pkgName, version
}
