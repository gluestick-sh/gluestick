package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/gluestick-sh/core/apps"
)

// Layout constants for ~/.glue/apps (re-exported for path construction).
const (
	AppsCurrentLinkName   = apps.CurrentLinkName
	AppsInstallRecordFile = apps.InstallRecordFile
)

// ParseInstallFilePath extracts package name and version from a path under apps/<pkg>/<version>/.
func ParseInstallFilePath(path string) (pkgName, version string) {
	return apps.ParseInstallFilePath(path)
}

// EnsureInstalledVersion returns the active version, creating current from the newest
// on-disk version when the link is missing (lazy migration).
func EnsureInstalledVersion(rootDir, pkgName string) (version string, ok bool) {
	ver, err := apps.EnsureCurrent(apps.PkgRoot(rootDir, pkgName))
	if err != nil || ver == "" {
		return "", false
	}
	return ver, true
}

// InstalledVersion reports the active version without mutating the current link.
func InstalledVersion(rootDir, pkgName string) (version string, ok bool) {
	return installedPackage(rootDir, pkgName)
}

// InstalledPackageVersions lists on-disk version directories for one package.
type InstalledPackageVersions struct {
	Name     string   `json:"name"`
	Versions []string `json:"versions"`
	Current  string   `json:"current"`
}

// ListInstalledAllVersions walks apps/ and returns each package with non-empty version dirs.
// filter limits to those package names; nil or empty means all directories under apps/.
func (e *Engine) ListInstalledAllVersions(filter []string) ([]InstalledPackageVersions, error) {
	if e == nil || e.Config == nil {
		return nil, fmt.Errorf("engine not configured")
	}
	root := e.Config.RootDir

	var pkgNames []string
	if len(filter) > 0 {
		pkgNames = append([]string(nil), filter...)
	} else {
		appsDir := filepath.Join(root, "apps")
		entries, err := os.ReadDir(appsDir)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, nil
			}
			return nil, fmt.Errorf("read apps dir: %w", err)
		}
		for _, entry := range entries {
			if entry.IsDir() {
				pkgNames = append(pkgNames, entry.Name())
			}
		}
	}
	sort.Strings(pkgNames)

	out := make([]InstalledPackageVersions, 0, len(pkgNames))
	for _, name := range pkgNames {
		pkgRoot := apps.PkgRoot(root, name)
		versions, err := apps.ListVersions(pkgRoot)
		if err != nil {
			return nil, fmt.Errorf("list versions for %s: %w", name, err)
		}
		if len(versions) == 0 {
			continue
		}
		sort.Strings(versions)
		current, _ := apps.ReadCurrent(pkgRoot)
		out = append(out, InstalledPackageVersions{
			Name:     name,
			Versions: versions,
			Current:  current,
		})
	}
	return out, nil
}
