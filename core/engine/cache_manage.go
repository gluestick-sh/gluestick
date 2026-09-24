package engine

import (
	"path/filepath"
	"sort"

	"github.com/gluestick-sh/core/cache"
	"github.com/gluestick-sh/core/message"
)

// CachePackageInfo describes one package entry in the cache index.
type CachePackageInfo struct {
	Name      string `json:"name"`
	Version   string `json:"version"`
	Installed string `json:"installed"`
	Size      int64  `json:"size"`
	FileCount int    `json:"fileCount"`
}

// CacheSummary aggregates cache index totals.
type CacheSummary struct {
	PackageCount int   `json:"packageCount"`
	TotalSize    int64 `json:"totalSize"`
	TotalFiles   int   `json:"totalFiles"`
}

// CacheSpaceResult reports blobs removed and bytes freed.
type CacheSpaceResult struct {
	RemovedBlobs int   `json:"removedBlobs"`
	FreedBytes   int64 `json:"freedBytes"`
}

// ListCachePackages returns cache index entries sorted by name.
func (e *Engine) ListCachePackages() []CachePackageInfo {
	entries := e.Cache.ListPackages()
	fileCounts, err := e.Cache.PackageFileCounts()
	if err != nil {
		fileCounts = map[string]int{}
	}
	out := make([]CachePackageInfo, 0, len(entries))
	for name, entry := range entries {
		out = append(out, CachePackageInfo{
			Name:      name,
			Version:   entry.Version,
			Installed: entry.Installed,
			Size:      entry.Size,
			FileCount: fileCounts[name],
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Name < out[j].Name
	})
	return out
}

// CacheSummary returns aggregate cache index statistics.
func (e *Engine) CacheSummary() CacheSummary {
	packages := e.ListCachePackages()
	var totalSize int64
	var totalFiles int
	for _, p := range packages {
		totalSize += p.Size
		totalFiles += p.FileCount
	}
	return CacheSummary{
		PackageCount: len(packages),
		TotalSize:    totalSize,
		TotalFiles:   totalFiles,
	}
}

// PurgeCachePackage removes a package from the cache index and deletes unreferenced cache store blobs.
func (e *Engine) PurgeCachePackage(pkgName string) (*CacheSpaceResult, error) {
	return e.PurgeCachePackageWithProgress(pkgName, nil)
}

// PurgeCachePackageWithProgress runs purge with optional phased progress reporting.
func (e *Engine) PurgeCachePackageWithProgress(pkgName string, report CacheGCReporter) (*CacheSpaceResult, error) {
	appsDir := filepath.Join(e.Config.RootDir, "apps")
	removed, freed, err := cache.PurgePackageWithProgress(e.Cache, e.Store, appsDir, pkgName, report)
	if err != nil {
		return nil, err
	}
	return &CacheSpaceResult{RemovedBlobs: removed, FreedBytes: freed}, nil
}

// RunCacheGC removes unreferenced store blobs and abandoned download partials.
func (e *Engine) RunCacheGC() (*CacheSpaceResult, error) {
	return e.RunCacheGCWithProgress(nil)
}

// CacheGCReporter receives phased cache GC progress updates.
type CacheGCReporter = cache.GCProgressReporter

// RunCacheGCWithProgress runs GC and reports phased progress when reporter is set.
// It also deletes abandoned resumable downloads under store/.partial and stale
// sidecar indexes under store/.zip-index and store/.manifest-hash.
func (e *Engine) RunCacheGCWithProgress(report CacheGCReporter) (*CacheSpaceResult, error) {
	extraRemoved, extraFreed := 0, int64(0)
	if e.Downloader != nil {
		if report != nil {
			report(cache.GCProgressEvent{
				Phase:      cache.GCPhasePrepare,
				MessageKey: message.GCClearingPartials,
				Percent:    0,
			})
		}
		removed, freed, err := e.Downloader.ClearAllPartials()
		if err != nil {
			return nil, err
		}
		extraRemoved += removed
		extraFreed += freed
	}

	appsDir := filepath.Join(e.Config.RootDir, "apps")
	removed, freed, err := cache.PurgeOrphanBlobsWithProgress(e.Cache, e.Store, appsDir, report)
	if err != nil {
		return nil, err
	}

	if e.Downloader != nil {
		if report != nil {
			report(cache.GCProgressEvent{
				Phase:      cache.GCPhaseDelete,
				MessageKey: message.GCClearingSidecars,
				Percent:    99,
			})
		}
		sideRemoved, sideFreed, sideErr := e.Downloader.ClearStaleSidecarIndexes()
		if sideErr != nil {
			return nil, sideErr
		}
		extraRemoved += sideRemoved
		extraFreed += sideFreed
	}

	return &CacheSpaceResult{
		RemovedBlobs: removed + extraRemoved,
		FreedBytes:   freed + extraFreed,
	}, nil
}

// ClearCacheIndex removes package rows from the SQLite cache index (store blobs are kept).
func (e *Engine) ClearCacheIndex(names []string) (cleared int, err error) {
	for _, name := range names {
		if _, ok := e.Cache.Get(name); !ok {
			continue
		}
		if err := e.Cache.Remove(name); err != nil {
			return cleared, err
		}
		cleared++
	}
	return cleared, nil
}

// ClearAllCacheIndex removes every package row from the cache index and returns pre-clear totals.
func (e *Engine) ClearAllCacheIndex() (CacheSummary, error) {
	summary := e.CacheSummary()
	if summary.PackageCount == 0 {
		return summary, nil
	}
	if err := e.Cache.ClearAll(); err != nil {
		return CacheSummary{}, err
	}
	return summary, nil
}

// RebuildCacheReporter receives per-package progress during cache index rebuild.
type RebuildCacheReporter func(pkgName, version string, fileCount int)

// RebuildCacheIndex rescans installed apps and rebuilds the cache index from hardlinks.
func (e *Engine) RebuildCacheIndex(report RebuildCacheReporter) (int, error) {
	appsDir := filepath.Join(e.Config.RootDir, "apps")
	var onPackage func(string, string, int)
	if report != nil {
		onPackage = func(pkg, ver string, fc int) { report(pkg, ver, fc) }
	}
	return e.Cache.Rebuild(appsDir, e.Store, onPackage)
}
