package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gluestick-sh/core/apps"
	"github.com/gluestick-sh/core/cache"
)

// ParsePkgRef splits "pkg@version" or "bucket/pkg@version" into name and version.
func ParsePkgRef(pkgRef string) (pkgName, version string) {
	ref := strings.TrimSpace(pkgRef)
	if at := strings.LastIndex(ref, "@"); at >= 0 {
		version = ref[at+1:]
		ref = ref[:at]
	}
	return PackageBaseName(ref), version
}

// ManifestLookupRef returns the bucket/package path without @version.
func ManifestLookupRef(pkgRef string) string {
	ref := strings.TrimSpace(pkgRef)
	if at := strings.LastIndex(ref, "@"); at >= 0 {
		ref = ref[:at]
	}
	if strings.ContainsAny(ref, `/\`) {
		return ref
	}
	return PackageBaseName(ref)
}

// PackageBaseName returns the app directory name from a package reference.
func PackageBaseName(pkgRef string) string {
	ref := pkgRef
	if at := strings.LastIndex(ref, "@"); at >= 0 {
		ref = ref[:at]
	}
	if i := strings.LastIndexAny(ref, `/\`); i >= 0 {
		return ref[i+1:]
	}
	return ref
}

// PackageBucketName returns the bucket from a package reference (default "main").
func PackageBucketName(pkgRef string) string {
	ref := pkgRef
	if at := strings.LastIndex(ref, "@"); at >= 0 {
		ref = ref[:at]
	}
	if i := strings.IndexAny(ref, `/\`); i >= 0 {
		return ref[:i]
	}
	return "main"
}

// InstallDirIsEmpty reports whether a directory has no entries.
func InstallDirIsEmpty(dir string) bool {
	entries, err := os.ReadDir(dir)
	return err != nil || len(entries) == 0
}

// VersionDirInstalled reports whether apps/<pkg>/<version> exists and is non-empty.
func VersionDirInstalled(root, pkgName, version string) bool {
	if version == "" {
		return false
	}
	dir := filepath.Join(apps.PkgRoot(root, pkgName), version)
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return false
	}
	return !InstallDirIsEmpty(dir)
}

// InstalledPackage reports whether pkgName is installed under root/apps.
func InstalledPackage(root, pkgName string) (version string, ok bool) {
	pkgDir := apps.PkgRoot(root, pkgName)
	if ver, err := apps.ReadCurrent(pkgDir); err == nil && ver != "" {
		if VersionDirInstalled(root, pkgName, ver) {
			return ver, true
		}
	}
	if ver, ok := apps.PickDefaultVersion(pkgDir); ok {
		return ver, true
	}
	return "", false
}

// IsHiddenInstallPath reports cache/install paths that must not be linked into apps/.
func IsHiddenInstallPath(relPath string) bool {
	return cache.IsHiddenInstallPath(relPath)
}

// FormatAlreadyInstalled formats the standard already-installed message.
func FormatAlreadyInstalled(pkgName, version string) string {
	return fmt.Sprintf("%s@%s is already installed (use --force to reinstall)", pkgName, version)
}

// FormatNotInstalled formats the standard not-installed message.
func FormatNotInstalled(pkgName string) string {
	return fmt.Sprintf("package %q is not installed (check with glue list)", pkgName)
}
