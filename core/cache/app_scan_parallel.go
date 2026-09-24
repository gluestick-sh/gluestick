package cache

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/gluestick-sh/core/apps"
	"github.com/gluestick-sh/core/store"
	"github.com/gluestick-sh/core/message"
)

type appScanTask struct {
	pkgName    string
	installDir string
	scanDir    string
}

type appScanPlan struct {
	targets   []struct{ pkgName, installDir string }
	tasks     []appScanTask
	fileTotal int
	dirTotal  int
}

// listAppScanTargets scans the apps directory and returns a list of packages with their
// active install directories.
func listAppScanTargets(appsDir string) ([]struct{ pkgName, installDir string }, error) {
	if appsDir == "" {
		return nil, nil
	}
	pkgEntries, err := os.ReadDir(appsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var targets []struct{ pkgName, installDir string }
	for _, pkgEntry := range pkgEntries {
		if !pkgEntry.IsDir() {
			continue
		}
		pkgName := pkgEntry.Name()
		pkgDir := filepath.Join(appsDir, pkgName)
		installDir, _, ok := apps.ActiveInstallDir(pkgDir)
		if !ok {
			continue
		}
		targets = append(targets, struct {
			pkgName    string
			installDir string
		}{pkgName: pkgName, installDir: installDir})
	}
	return targets, nil
}

// buildAppScanPlan creates a scan plan without progress reporting.
func buildAppScanPlan(appsDir string, idx *Index) (appScanPlan, error) {
	return buildAppScanPlanWithProgress(appsDir, idx, nil, nil, 0)
}

// buildAppScanPlanWithProgress creates a scan plan with optional progress reporting and
// store shard counting for accurate progress estimation.
func buildAppScanPlanWithProgress(
	appsDir string,
	idx *Index,
	store *store.Store,
	prog *gcProgress,
	finalizeUnits int64,
) (appScanPlan, error) {
	var plan appScanPlan
	targets, err := listAppScanTargets(appsDir)
	if err != nil {
		return plan, err
	}
	plan.targets = targets
	if len(plan.targets) == 0 {
		return plan, nil
	}

	pkgNames := make([]string, len(plan.targets))
	for i, t := range plan.targets {
		pkgNames[i] = t.pkgName
	}
	sqliteFiles, _ := idx.IndexedFileCountForPackages(pkgNames)

	if prog != nil && store != nil {
		shards := countStoreShards(store)
		planned := int64(shards) + int64(sqliteFiles) + int64(len(plan.targets)) + finalizeUnits
		if planned <= 0 {
			planned = int64(len(plan.targets))
		}
		prog.addPlanned(planned)
		prog.reportInfo(GCPhaseScan, message.GCAppsPendingScan, map[string]interface{}{
			"count": len(plan.targets),
		})
		prog.reportInfo(GCPhaseScan, message.GCAppsEnumeratingDirs, map[string]interface{}{
			"packages": len(plan.targets),
		})
	}

	tasks, diskFiles, err := enumerateAppScanTasksParallel(plan.targets, prog)
	if err != nil {
		return plan, err
	}
	if prog != nil && diskFiles > sqliteFiles {
		prog.addPlanned(int64(diskFiles - sqliteFiles))
	}
	plan.tasks = tasks
	plan.fileTotal = sqliteFiles
	if diskFiles > plan.fileTotal {
		plan.fileTotal = diskFiles
	}
	if plan.fileTotal <= 0 {
		plan.fileTotal = len(plan.targets)
	}
	plan.dirTotal = len(tasks)
	if plan.dirTotal == 0 {
		plan.dirTotal = len(plan.targets)
	}
	return plan, nil
}

// enumeratePackageDirs walks an install directory and returns scan tasks for each
// directory, along with the total file count (excluding hidden paths).
func enumeratePackageDirs(pkgName, installDir string) ([]appScanTask, int, error) {
	var tasks []appScanTask
	var fileCount int
	var walk func(scanDir string) error
	walk = func(scanDir string) error {
		tasks = append(tasks, appScanTask{
			pkgName:    pkgName,
			installDir: installDir,
			scanDir:    scanDir,
		})
		entries, err := os.ReadDir(scanDir)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			path := filepath.Join(scanDir, entry.Name())
			rel, relErr := filepath.Rel(installDir, path)
			if relErr != nil || IsHiddenInstallPath(rel) {
				continue
			}
			if entry.IsDir() {
				if err := walk(path); err != nil {
					return err
				}
				continue
			}
			fileCount++
		}
		return nil
	}
	if err := walk(installDir); err != nil {
		return nil, 0, err
	}
	return tasks, fileCount, nil
}

// enumerateAppScanTasksParallel enumerates scan tasks for multiple packages in parallel,
// respecting GC worker limits and reporting progress if provided.
func enumerateAppScanTasksParallel(
	targets []struct {
		pkgName    string
		installDir string
	},
	prog *gcProgress,
) ([]appScanTask, int, error) {
	workers := gcWorkers(len(targets))
	if workers < maxGCWorkers {
		workers = maxGCWorkers
	}
	if workers > len(targets) {
		workers = len(targets)
	}

	var all []appScanTask
	var diskFiles atomic.Int64
	var mu sync.Mutex
	var wg sync.WaitGroup
	var firstErr error
	var errOnce sync.Once
	sem := make(chan struct{}, workers)

	for _, target := range targets {
		target := target
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			tasks, files, err := enumeratePackageDirs(target.pkgName, target.installDir)
			if err != nil {
				errOnce.Do(func() { firstErr = err })
				return
			}
			if prog != nil {
				prog.complete(1)
			}
			diskFiles.Add(int64(files))
			mu.Lock()
			all = append(all, tasks...)
			mu.Unlock()
		}()
	}
	wg.Wait()
	return all, int(diskFiles.Load()), firstErr
}

// scanAppInstallDirsParallel scans pre-enumerated install directories using an existing store key index.
func scanAppInstallDirsParallel(
	plan appScanPlan,
	store *store.Store,
	keyIndex map[fileRefKey]string,
	prog *gcProgress,
) (map[string]bool, error) {
	refs := make(map[string]bool)
	if len(plan.targets) == 0 {
		return refs, nil
	}

	pkgDirCounts := make(map[string]int, len(plan.targets))
	pkgDirsDone := make(map[string]*atomic.Int32, len(plan.targets))
	for _, task := range plan.tasks {
		pkgDirCounts[task.pkgName]++
	}
	for _, target := range plan.targets {
		pkgDirsDone[target.pkgName] = &atomic.Int32{}
	}

	workers := gcWorkers(len(plan.tasks))
	if workers < maxGCWorkers {
		workers = maxGCWorkers
	}
	if workers > len(plan.tasks) {
		workers = len(plan.tasks)
	}

	var refsMu sync.Mutex
	var filesDone atomic.Int64
	var dirsDone atomic.Int64
	var taskIdx atomic.Int64
	var wg sync.WaitGroup
	var firstErr error
	var errOnce sync.Once
	lastFileReport := int64(0)

	reportProgress := func(packagesDone int) {
		if prog == nil {
			return
		}
		files := int(filesDone.Load())
		if files != plan.fileTotal && files > 0 && files-int(lastFileReport) < 100 && packagesDone < len(plan.targets) {
			return
		}
		lastFileReport = int64(files)
		prog.reportAppScan(
			files, plan.fileTotal,
			int(dirsDone.Load()), plan.dirTotal,
			packagesDone, len(plan.targets),
		)
	}

	processDir := func(task appScanTask) {
		defer func() {
			dirsDone.Add(1)
			pkgDirsDone[task.pkgName].Add(1)
			reportProgress(countPackagesDone(pkgDirsDone, pkgDirCounts))
		}()

		entries, readErr := os.ReadDir(task.scanDir)
		if readErr != nil {
			errOnce.Do(func() { firstErr = readErr })
			return
		}

		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			path := filepath.Join(task.scanDir, entry.Name())
			relPath, relErr := filepath.Rel(task.installDir, path)
			if relErr != nil || IsHiddenInstallPath(relPath) {
				continue
			}

			hash, hashErr := resolveInstallFileHash(path, store, keyIndex)
			if hashErr != nil {
				continue
			}
			refsMu.Lock()
			refs[hash] = true
			refsMu.Unlock()

			n := filesDone.Add(1)
			if prog != nil {
				prog.complete(1)
				if n == 1 || n%100 == 0 || int(n) == plan.fileTotal {
					reportProgress(countPackagesDone(pkgDirsDone, pkgDirCounts))
				}
			}
		}
	}

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				idx := taskIdx.Add(1) - 1
				if int(idx) >= len(plan.tasks) {
					return
				}
				processDir(plan.tasks[idx])
			}
		}()
	}
	wg.Wait()

	if firstErr != nil {
		return nil, firstErr
	}
	reportProgress(len(plan.targets))
	return refs, nil
}

// countPackagesDone counts how many packages have completed scanning all their directories.
func countPackagesDone(pkgDirsDone map[string]*atomic.Int32, pkgDirCounts map[string]int) int {
	done := 0
	for name, counter := range pkgDirsDone {
		if int(counter.Load()) >= pkgDirCounts[name] {
			done++
		}
	}
	return done
}
