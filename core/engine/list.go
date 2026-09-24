package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/gluestick-sh/core/apps"
	"github.com/gluestick-sh/core/cache"
	"github.com/gluestick-sh/core/engine/internal/install"
	"github.com/gluestick-sh/core/manifest"
	"github.com/gluestick-sh/core/shim"
)

// ListOptions contains options for listing packages
type ListOptions struct {
	Detailed bool
	ShowAll  bool
	Filter   []string
}

// activeListedInstall returns install dir/version only for completed installs.
func activeListedInstall(root, pkgName string, cacheIdx *cache.Index) (installDir, version string, ok bool) {
	pkgRoot := apps.PkgRoot(root, pkgName)

	if inst, registered := cacheIdx.GetInstalled(pkgName); registered {
		ver := inst.Version
		if versionDirInstalled(root, pkgName, ver) {
			dir := inst.InstallDir
			if dir == "" {
				dir = filepath.Join(pkgRoot, ver)
			}
			return dir, ver, true
		}
	}

	if ver, err := apps.ReadCurrent(pkgRoot); err == nil && ver != "" {
		if versionDirInstalled(root, pkgName, ver) {
			dir := filepath.Join(pkgRoot, ver)
			if _, err := apps.LoadInstallRecord(dir); err == nil {
				return dir, ver, true
			}
		}
	}

	versions, err := apps.ListVersions(pkgRoot)
	if err != nil {
		return "", "", false
	}
	for i := len(versions) - 1; i >= 0; i-- {
		ver := versions[i]
		dir := filepath.Join(pkgRoot, ver)
		if _, err := apps.LoadInstallRecord(dir); err == nil {
			return dir, ver, true
		}
	}
	return "", "", false
}

// listInstalledPackages lists packages with a completed install under apps/.
func (e *Engine) listInstalledPackages(ctx context.Context,
	opts ListOptions, reporter ProgressReporter,
) ([]*Package, error) {
	appsDir := filepath.Join(e.Config.RootDir, "apps")
	pkgEntries, err := os.ReadDir(appsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read apps dir: %w", err)
	}

	cacheEntries := e.Cache.ListPackages()
	var packages []*Package

	filterSet := make(map[string]bool, len(opts.Filter))
	for _, f := range opts.Filter {
		filterSet[f] = true
	}

	for _, dirEntry := range pkgEntries {
		if !dirEntry.IsDir() {
			continue
		}
		name := dirEntry.Name()
		if len(opts.Filter) > 0 && !filterSet[name] {
			continue
		}

		_, diskVersion, ok := activeListedInstall(e.Config.RootDir, name, e.Cache)
		if !ok {
			continue
		}

		version := diskVersion
		var installedSize int64
		var installedAt string
		if entry, ok := cacheEntries[name]; ok {
			version = e.resolveInstalledVersion(name, entry.Version)
			installedSize = entry.Size
			installedAt = entry.Installed
		}

		pkg := &Package{
			Name:          name,
			Version:       version,
			InstalledSize: installedSize,
			InstalledAt:   installedAt,
		}

		if bucket, manifestInfo := e.getInstalledPackageDetails(name, version); manifestInfo != nil {
			pkg.Manifest = manifestInfo
			pkg.Description = manifestInfo.Description
			pkg.Homepage = manifestInfo.Homepage
			pkg.Bucket = bucket
		} else if bucket != "" {
			pkg.Bucket = bucket
		}

		detected := e.detectPackageVersion(name, version)
		pkg.DetectedVersion = detected.DetectedVersion
		pkg.VersionDetectSrc = detected.Source
		pkg.ExternallyUpdated = detected.ExternallyUpdated

		if opts.Detailed {
			shims, err := install.GetShimsForPackage(e.Engine, name, version)
			if err == nil {
				if pkg.Manifest == nil {
					pkg.Manifest = &ManifestInfo{}
				}
				for _, shim := range shims {
					pkg.Manifest.Binaries = append(pkg.Manifest.Binaries, BinaryInfo{
						Name: shim,
					})
				}
			}
		}

		packages = append(packages, pkg)
	}

	// Sort by name
	sort.Slice(packages, func(i, j int) bool {
		return packages[i].Name < packages[j].Name
	})

	return packages, nil
}

// extractPackageFromPath extracts package name and version from a shim target path.
func (e *Engine) extractPackageFromPath(path string) (string, string) {
	return ParseInstallFilePath(path)
}

// getInstalledPackageDetails resolves bucket and manifest metadata for an installed package.
func (e *Engine) getInstalledPackageDetails(name, version string) (bucket string, info *ManifestInfo) {
	pkgRoot := apps.PkgRoot(e.Config.RootDir, name)
	ver := version
	if ver == "" {
		if v, err := apps.ReadCurrent(pkgRoot); err == nil {
			ver = v
		}
	}
	if ver != "" {
		installDir := filepath.Join(pkgRoot, ver)
		if rec, err := apps.LoadInstallRecord(installDir); err == nil {
			if rec.Bucket != "" {
				bucket = rec.Bucket
			}
			if rec.Manifest != nil {
				info = e.createManifestInfo(rec.Manifest)
			}
			if bucket != "" {
				return bucket, info
			}
		}
	}

	bucketsDir := filepath.Join(e.Config.RootDir, "buckets")
	entries, err := os.ReadDir(bucketsDir)
	if err != nil {
		return "", info
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		bucketName := entry.Name()
		manifestPath := filepath.Join(bucketsDir, bucketName, "bucket", name+".json")
		if _, err := os.Stat(manifestPath); err != nil {
			continue
		}
		m, err := manifest.ParseFile(manifestPath)
		if err != nil {
			continue
		}
		return bucketName, e.createManifestInfo(m)
	}
	return "", info
}

// groupByPackage groups shims by their package (legacy support)
func (e *Engine) groupByPackage(configs []shim.Config) map[string]*Package {
	packages := make(map[string]*Package)

	for _, cfg := range configs {
		// Extract package info from path
		pkgName, version := e.extractPackageFromPath(cfg.Path)

		if pkgName == "" {
			// Skip shims that don't follow the pattern
			continue
		}

		if _, exists := packages[pkgName]; !exists {
			packages[pkgName] = &Package{
				Name:    pkgName,
				Version: version,
			}
		}

		// Add shim to manifest binaries
		if packages[pkgName].Manifest == nil {
			packages[pkgName].Manifest = &ManifestInfo{}
		}
		packages[pkgName].Manifest.Binaries = append(packages[pkgName].Manifest.Binaries, BinaryInfo{
			Name: cfg.Name,
		})
	}

	return packages
}

// ListLegacy lists packages using the shim-based approach (legacy)
func (e *Engine) ListLegacy(ctx context.Context,
	opts ListOptions,
	reporter ProgressReporter,
) ([]*Package, error) {
	// Get shim configs
	configs, err := e.ShimMgr.List()
	if err != nil {
		return nil, fmt.Errorf("list shims: %w", err)
	}

	// Group by package
	packages := e.groupByPackage(configs)

	// Apply filter if provided
	if len(opts.Filter) > 0 {
		filtered := make(map[string]*Package)
		filterSet := make(map[string]bool)
		for _, f := range opts.Filter {
			filterSet[f] = true
		}

		for name, pkg := range packages {
			if filterSet[name] {
				filtered[name] = pkg
			}
		}
		packages = filtered
	}

	// Convert to slice and sort
	var result []*Package
	for _, pkg := range packages {
		result = append(result, pkg)
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].Name < result[j].Name
	})

	return result, nil
}

// GetPackageStats returns statistics about installed packages
func (e *Engine) GetPackageStats() (*PackageStats, error) {
	entries := e.Cache.ListPackages()

	stats := &PackageStats{
		TotalPackages: int64(len(entries)),
		TotalSize:     0,
	}

	for _, entry := range entries {
		stats.TotalSize += entry.Size
	}

	return stats, nil
}

// PackageStats contains statistics about installed packages
type PackageStats struct {
	TotalPackages int64
	TotalSize     int64
}
