package engine

import (
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/gluestick-sh/core/engine/internal/runtime"
)

// PackageUpdate describes an installed package with a newer manifest version.
type PackageUpdate struct {
	Name             string `json:"name"`
	InstalledVersion string `json:"installedVersion"`
	LatestVersion    string `json:"latestVersion"`
	Bucket           string `json:"bucket"`
}

func manifestLookupForBucket(pkgName, bucket string) string {
	if bucket != "" && bucket != "main" {
		return bucket + "/" + pkgName
	}
	return pkgName
}

func (e *Engine) latestManifestVersion(pkgName, bucket string) (string, bool) {
	ref := manifestLookupForBucket(pkgName, bucket)
	_, m, err := e.BucketRegistry.GetManifestPath(ref)
	if err != nil || m == nil || m.Version == "" {
		return "", false
	}
	return m.Version, true
}

// installedPackagesForUpdateCheck lists packages to compare against buckets.
// Installed apps on disk are authoritative; cache index versions are hints only
// (cache clear must not hide available updates).
func (e *Engine) installedPackagesForUpdateCheck() (map[string]string, error) {
	if e == nil || e.Config == nil {
		return nil, nil
	}

	cacheVersions, cacheErr := e.Cache.ListPackageVersions()
	if cacheErr != nil {
		cacheVersions = map[string]string{}
	}

	appsDir := filepath.Join(e.Config.RootDir, "apps")
	pkgEntries, err := os.ReadDir(appsDir)
	if err != nil {
		if os.IsNotExist(err) {
			if len(cacheVersions) == 0 {
				return nil, nil
			}
			return cacheVersions, nil
		}
		return nil, err
	}

	out := make(map[string]string)
	for _, dirEntry := range pkgEntries {
		if !dirEntry.IsDir() {
			continue
		}
		name := dirEntry.Name()
		_, diskVersion, ok := activeListedInstall(e.Config.RootDir, name, e.Cache)
		if !ok || diskVersion == "" {
			continue
		}
		indexed := diskVersion
		if v, ok := cacheVersions[name]; ok && v != "" {
			indexed = v
		}
		out[name] = indexed
	}
	return out, nil
}

// CheckPackageUpdates compares installed packages against bucket manifests.
func (e *Engine) CheckPackageUpdates() ([]PackageUpdate, error) {
	versions, err := e.installedPackagesForUpdateCheck()
	if err != nil || len(versions) == 0 {
		return nil, err
	}

	var (
		mu      sync.Mutex
		updates []PackageUpdate
		wg      sync.WaitGroup
		sem     = make(chan struct{}, 8)
	)

	for name, indexed := range versions {
		if indexed == "" {
			continue
		}
		wg.Add(1)
		go func(name, indexed string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			if e.IsPackageVersionLocked(name) {
				return
			}

			installed := e.resolveInstalledVersion(name, indexed)
			bucket, _ := e.getInstalledPackageDetails(name, installed)
			latest, ok := e.latestManifestVersion(name, bucket)
			if !ok || !UpdateAvailable(installed, latest) {
				return
			}
			u := PackageUpdate{
				Name:             name,
				InstalledVersion: installed,
				LatestVersion:    latest,
				Bucket:           bucket,
			}
			mu.Lock()
			updates = append(updates, u)
			mu.Unlock()
		}(name, indexed)
	}
	wg.Wait()

	sort.Slice(updates, func(i, j int) bool {
		return updates[i].Name < updates[j].Name
	})
	return updates, nil
}

// CountPackageUpdates returns how many installed packages have updates in buckets.
func (e *Engine) CountPackageUpdates() int {
	updates, err := e.CheckPackageUpdates()
	if err != nil {
		return 0
	}
	return len(updates)
}

// ReloadBuckets rescans ~/.glue/buckets so manifest lookups see new pulls.
// refreshExisting controls whether already-indexed buckets are rescanned (e.g. after git pull).
func (e *Engine) ReloadBuckets(refreshExisting bool) {
	if e.BucketRegistry == nil {
		return
	}
	e.BucketRegistry.ReloadFromDisk()
	runtime.SyncSearchIndex(e.Engine, refreshExisting)
}
