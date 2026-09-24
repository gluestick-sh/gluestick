package cache

import (
	"path/filepath"
	"sync"

	"github.com/gluestick-sh/core/apps"
	"github.com/gluestick-sh/core/store"
)

// Rebuild index scan walks ~/.glue/apps on disk and resolves cache store hashes before SQLite is rewritten.
// Index.Rebuild calls scanPackagesForRebuild first (parallel, no DB lock), then writes rows in one
// transaction. Shares list/walk helpers with GC in app_scan_parallel.go and refs.go.

// rebuildPackage is one package's on-disk file map ready for packages/package_files insert.
type rebuildPackage struct {
	name      string
	version   string
	files     map[string]string // cache store hash → install-relative path
	totalSize int64
}

// scanPackagesForRebuild discovers active installs under appsDir and maps each hardlinked file
// to its cache store hash via the store index. Work runs in parallel (gcWorkers) so Rebuild does not hold
// idx.mu while walking large install trees.
func scanPackagesForRebuild(appsDir string, store *store.Store, onPackage RebuildPackageFunc) ([]rebuildPackage, error) {
	// In-memory (device, inode) → hash map; built once and shared across workers.
	keyIndex, err := buildStoreFileRefIndex(store)
	if err != nil {
		return nil, err
	}

	targets, err := listAppScanTargets(appsDir)
	if err != nil {
		return nil, err
	}
	if len(targets) == 0 {
		return nil, nil
	}

	workers := gcWorkers(len(targets))
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var out []rebuildPackage
	var firstErr error
	var errOnce sync.Once // retain first walk error; skip packages with no linked files

	for _, target := range targets {
		target := target
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			pkgDir := filepath.Join(appsDir, target.pkgName)
			_, version, ok := apps.ActiveInstallDir(pkgDir)
			if !ok {
				return // no current/ version on disk
			}

			files, totalSize, walkErr := scanInstallDir(target.installDir, store, keyIndex)
			if walkErr != nil {
				errOnce.Do(func() { firstErr = walkErr })
				return
			}
			if len(files) == 0 {
				return // empty or non-cache store install dir
			}

			pkg := rebuildPackage{
				name:      target.pkgName,
				version:   version,
				files:     files,
				totalSize: totalSize,
			}
			mu.Lock()
			out = append(out, pkg)
			mu.Unlock()
			if onPackage != nil {
				onPackage(target.pkgName, version, len(files)) // progress callback for UI clients
			}
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}
