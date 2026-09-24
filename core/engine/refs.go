package engine

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/gluestick-sh/core/apps"
)

// ParsePkgRef splits "pkg@version" or "bucket/pkg@version" into name and version.
func ParsePkgRef(pkgRef string) (pkgName, version string) {
	return parsePkgRef(pkgRef)
}

// parsePkgRef splits "pkg@version" or "bucket/pkg@version" into name and version.
func parsePkgRef(pkgRef string) (pkgName, version string) {
	ref := strings.TrimSpace(pkgRef)
	if at := strings.LastIndex(ref, "@"); at >= 0 {
		version = ref[at+1:]
		ref = ref[:at]
	}
	return packageBaseName(ref), version
}

// packageBaseName returns the app directory name from a package reference.
func packageBaseName(pkgRef string) string {
	ref := pkgRef
	if at := strings.LastIndex(ref, "@"); at >= 0 {
		ref = ref[:at]
	}
	if i := strings.LastIndexAny(ref, `/\`); i >= 0 {
		return ref[i+1:]
	}
	return ref
}

func installDirIsEmpty(dir string) bool {
	entries, err := os.ReadDir(dir)
	return err != nil || len(entries) == 0
}

// versionDirInstalled reports whether apps/<pkg>/<version> exists and is non-empty.
func versionDirInstalled(root, pkgName, version string) bool {
	if version == "" {
		return false
	}
	dir := filepath.Join(apps.PkgRoot(root, pkgName), version)
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return false
	}
	return !installDirIsEmpty(dir)
}

// installedPackage reports whether pkgName is installed under root/apps.
// Does not mutate the current link (unlike apps.EnsureCurrent).
func installedPackage(root, pkgName string) (version string, ok bool) {
	pkgDir := apps.PkgRoot(root, pkgName)
	if ver, err := apps.ReadCurrent(pkgDir); err == nil && ver != "" {
		if versionDirInstalled(root, pkgName, ver) {
			return ver, true
		}
	}
	if ver, ok := apps.PickDefaultVersion(pkgDir); ok {
		return ver, true
	}
	return "", false
}
