package cache

import (
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/gluestick-sh/core/apps"
)

// listPurgeScanPackages scans the apps directory and returns package names that match
// the limit/exclude filters, with priorityPkg sorted first.
func listPurgeScanPackages(
	appsDir, priorityPkg string,
	limitPkgNames, excludePkgNames map[string]struct{},
) ([]string, error) {
	pkgEntries, err := os.ReadDir(appsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var pkgNames []string
	for _, entry := range pkgEntries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if limitPkgNames != nil {
			if _, ok := limitPkgNames[name]; !ok {
				continue
			}
		}
		if excludePkgNames != nil {
			if _, ok := excludePkgNames[name]; ok {
				continue
			}
		}
		pkgNames = append(pkgNames, name)
	}
	sortPkgNames(pkgNames, priorityPkg)
	return pkgNames, nil
}

// sortPkgNames sorts package names with priorityPkg first, then alphabetically.
func sortPkgNames(pkgNames []string, priorityPkg string) {
	sort.Slice(pkgNames, func(i, j int) bool {
		if pkgNames[i] == priorityPkg {
			return true
		}
		if pkgNames[j] == priorityPkg {
			return false
		}
		return pkgNames[i] < pkgNames[j]
	})
}

// enumeratePurgeScanTasksParallel enumerates app scan tasks for multiple packages
// in parallel, respecting worker limits for GC operations.
func enumeratePurgeScanTasksParallel(pkgNames []string, appsDir string, prog *gcProgress) ([]appScanTask, error) {
	if len(pkgNames) == 0 {
		return nil, nil
	}
	workers := gcWorkers(len(pkgNames))
	if workers < maxGCWorkers {
		workers = maxGCWorkers
	}
	if workers > len(pkgNames) {
		workers = len(pkgNames)
	}

	var all []appScanTask
	var mu sync.Mutex
	var wg sync.WaitGroup
	var firstErr error
	var errOnce sync.Once
	sem := make(chan struct{}, workers)

	for _, pkgName := range pkgNames {
		pkgName := pkgName
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			pkgDir := filepath.Join(appsDir, pkgName)
			installDir, _, ok := apps.ActiveInstallDir(pkgDir)
			if !ok {
				return
			}
			tasks, _, err := enumeratePackageDirs(pkgName, installDir)
			if err != nil {
				errOnce.Do(func() { firstErr = err })
				return
			}
			if prog != nil {
				prog.complete(1)
			}
			mu.Lock()
			all = append(all, tasks...)
			mu.Unlock()
		}()
	}
	wg.Wait()
	return all, firstErr
}

// scanAppsForCandidatesParallel scans app directories for purge candidates using
// multiple workers. Returns the number of files scanned and any error encountered.
func scanAppsForCandidatesParallel(
	tasks []appScanTask,
	keyToHashes map[fileRefKey][]string,
	remaining map[string]struct{},
	refs map[string]bool,
	prog *gcProgress,
) (int, error) {
	if len(tasks) == 0 || len(remaining) == 0 {
		return 0, nil
	}

	workers := gcWorkers(len(tasks))
	if workers < maxGCWorkers {
		workers = maxGCWorkers
	}
	if workers > len(tasks) {
		workers = len(tasks)
	}

	var remainingMu sync.Mutex
	var refsMu sync.Mutex
	var filesScanned atomic.Int64
	var pendingHashes atomic.Int32
	var taskIdx atomic.Int64
	var wg sync.WaitGroup
	var firstErr error
	var errOnce sync.Once

	pendingHashes.Store(int32(len(remaining)))

	processDir := func(task appScanTask) {
		if pendingHashes.Load() == 0 {
			return
		}
		entries, readErr := os.ReadDir(task.scanDir)
		if readErr != nil {
			errOnce.Do(func() { firstErr = readErr })
			return
		}

		for _, entry := range entries {
			if pendingHashes.Load() == 0 {
				return
			}
			if entry.IsDir() {
				continue
			}
			path := filepath.Join(task.scanDir, entry.Name())
			relPath, relErr := filepath.Rel(task.installDir, path)
			if relErr != nil || IsHiddenInstallPath(relPath) {
				continue
			}

			n := filesScanned.Add(1)
			if prog != nil {
				prog.complete(1)
				if n == 1 || n%100 == 0 {
					prog.reportPurgeFiles(task.pkgName, int(n))
				}
			}

			if matchPurgeCandidateFile(path, keyToHashes, remaining, refs, &remainingMu, &refsMu, &pendingHashes) {
				return
			}
		}
	}

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				if pendingHashes.Load() == 0 {
					return
				}
				idx := taskIdx.Add(1) - 1
				if int(idx) >= len(tasks) {
					return
				}
				processDir(tasks[idx])
			}
		}()
	}
	wg.Wait()
	return int(filesScanned.Load()), firstErr
}

// matchPurgeCandidateFile returns true when every pending candidate hash was resolved.
func matchPurgeCandidateFile(
	path string,
	keyToHashes map[fileRefKey][]string,
	remaining map[string]struct{},
	refs map[string]bool,
	remainingMu, refsMu *sync.Mutex,
	pendingHashes *atomic.Int32,
) bool {
	k, ok := fileRefKeyForPath(path)
	if !ok {
		return false
	}
	hashes, known := keyToHashes[k]
	if !known {
		return false
	}
	remainingMu.Lock()
	for _, hash := range hashes {
		if _, pending := remaining[hash]; pending {
			refsMu.Lock()
			refs[hash] = true
			refsMu.Unlock()
			delete(remaining, hash)
			pendingHashes.Add(-1)
		}
	}
	done := len(remaining) == 0
	remainingMu.Unlock()
	return done
}
