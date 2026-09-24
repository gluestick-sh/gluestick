package cache

import (
	"os"
	"sync"
	"sync/atomic"

	"github.com/gluestick-sh/core/store"
	"github.com/gluestick-sh/core/message"
)

// purgeScanBudget is the app-scan work reserved up front so progress never hits 100% before a
// possible secondary install scan (and skip) completes.
type purgeScanBudget struct {
	primaryFiles        int
	secondaryWalkBudget int
}

func planPurgeScanBudget(idx *Index, appsDir, pkgName string, relatedPkgs []string, candidateCount int) purgeScanBudget {
	if appsDir == "" {
		return purgeScanBudget{}
	}
	primarySet := pkgNamesSet(append([]string{pkgName}, relatedPkgs...))
	primaryFiles := estimateAppScanFiles(idx, appsDir, primarySet, nil)
	if primaryFiles <= 0 {
		primaryFiles = candidateCount
	}
	secondaryWalkBudget := estimateAppScanFiles(idx, appsDir, nil, primarySet)
	return purgeScanBudget{
		primaryFiles:        primaryFiles,
		secondaryWalkBudget: secondaryWalkBudget,
	}
}

// AppsReferenceCandidateHashes finds which candidate cache store hashes are still hardlinked under apps/.
// It scans the target package and packages that share cache files first; only if some files may
// still be deleted does it scan remaining installed packages (safety for cross-package hardlinks).
func AppsReferenceCandidateHashes(
	appsDir string,
	store *store.Store,
	idx *Index,
	priorityPkg string,
	candidates []string,
	relatedPkgNames []string,
	prog *gcProgress,
	budget purgeScanBudget,
	purgedUninstalled bool,
) (map[string]bool, error) {
	refs := make(map[string]bool)
	remaining := make(map[string]struct{}, len(candidates))
	for _, hash := range candidates {
		if hash != "" {
			remaining[hash] = struct{}{}
		}
	}
	if appsDir == "" || store == nil || len(remaining) == 0 {
		return refs, nil
	}

	if purgedUninstalled {
		exclude := map[string]struct{}{priorityPkg: {}}
		refs, _, err := scanAppsForCandidates(appsDir, store, priorityPkg, remaining, nil, exclude, prog)
		if err != nil {
			return nil, err
		}
		if prog != nil && budget.secondaryWalkBudget > 0 {
			// scan budget was planned for primary+secondary; uninstalled purge uses one pass.
			skipSecondaryPurgeScan(prog, budget.secondaryWalkBudget)
		}
		return refs, nil
	}

	primaryAllowed := make(map[string]struct{}, 1+len(relatedPkgNames))
	primaryAllowed[priorityPkg] = struct{}{}
	for _, name := range relatedPkgNames {
		primaryAllowed[name] = struct{}{}
	}

	primaryRefs, _, err := scanAppsForCandidates(appsDir, store, priorityPkg, remaining, primaryAllowed, nil, prog)
	if err != nil {
		return nil, err
	}
	for hash := range primaryRefs {
		refs[hash] = true
		delete(remaining, hash)
	}
	if len(remaining) == 0 {
		skipSecondaryPurgeScan(prog, budget.secondaryWalkBudget)
		return refs, nil
	}

	secondaryRefs, scanned, err := scanAppsForCandidates(appsDir, store, priorityPkg, remaining, nil, primaryAllowed, prog)
	if err != nil {
		return refs, err
	}
	for hash := range secondaryRefs {
		refs[hash] = true
	}
	if prog != nil && budget.secondaryWalkBudget > scanned {
		prog.complete(int64(budget.secondaryWalkBudget - scanned))
	}
	return refs, nil
}

func skipSecondaryPurgeScan(prog *gcProgress, secondaryWalkBudget int) {
	if prog == nil || secondaryWalkBudget <= 0 {
		return
	}
	prog.complete(1 + int64(secondaryWalkBudget))
}

func estimateAppScanFiles(idx *Index, appsDir string, limitPkgNames, excludePkgNames map[string]struct{}) int {
	if appsDir == "" {
		return 0
	}
	pkgEntries, err := os.ReadDir(appsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		return 0
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
	if len(pkgNames) == 0 {
		return 0
	}
	if idx != nil {
		if count, err := idx.IndexedFileCountForPackages(pkgNames); err == nil && count > 0 {
			return count
		}
	}
	return len(pkgNames)
}

func scanAppsForCandidates(
	appsDir string,
	store *store.Store,
	priorityPkg string,
	remaining map[string]struct{},
	limitPkgNames map[string]struct{},
	excludePkgNames map[string]struct{},
	prog *gcProgress,
) (map[string]bool, int, error) {
	refs := make(map[string]bool)
	if len(remaining) == 0 {
		return refs, 0, nil
	}

	pkgNames, err := listPurgeScanPackages(appsDir, priorityPkg, limitPkgNames, excludePkgNames)
	if err != nil {
		return nil, 0, err
	}

	if prog != nil {
		if len(pkgNames) == 0 {
			if limitPkgNames != nil {
				prog.reportInfo(GCPhaseScan, message.PurgeNoInstallScan, map[string]interface{}{
					"name": priorityPkg,
				})
			}
		} else if limitPkgNames != nil {
			if len(pkgNames) == 1 {
				prog.reportInfo(GCPhaseScan, message.PurgeScanningInstall, map[string]interface{}{
					"name": pkgNames[0],
				})
			} else {
				prog.reportInfo(GCPhaseScan, message.PurgeScanningInstalls, map[string]interface{}{
					"name":  priorityPkg,
					"count": len(pkgNames),
				})
			}
		} else {
			prog.reportInfo(GCPhaseScan, message.PurgeScanningOtherInstalls, map[string]interface{}{
				"count": len(pkgNames),
			})
		}
	}

	keyToHashes := buildStoreKeyMapParallel(store, remaining, prog)
	if prog != nil {
		prog.complete(1)
	}

	tasks, err := enumeratePurgeScanTasksParallel(pkgNames, appsDir, prog)
	if err != nil {
		return nil, 0, err
	}

	filesScanned, err := scanAppsForCandidatesParallel(tasks, keyToHashes, remaining, refs, prog)
	if err != nil {
		return nil, filesScanned, err
	}

	if prog != nil && len(pkgNames) > 0 && limitPkgNames != nil {
		prog.report(GCPhaseScan, message.GCAppsScanComplete, map[string]any{
			"count": len(refs),
		})
	}
	return refs, filesScanned, nil
}

func buildStoreKeyMapParallel(store *store.Store, remaining map[string]struct{}, prog *gcProgress) map[fileRefKey][]string {
	keyToHashes := make(map[fileRefKey][]string)
	if len(remaining) == 0 {
		return keyToHashes
	}

	hashes := make([]string, 0, len(remaining))
	for hash := range remaining {
		hashes = append(hashes, hash)
	}

	workers := gcWorkers(len(hashes))
	if workers <= 1 {
		for i, hash := range hashes {
			if key, ok := fileRefKeyForPath(store.ObjectPath(hash)); ok {
				keyToHashes[key] = append(keyToHashes[key], hash)
			}
			if prog != nil {
				if i == 0 || (i+1)%500 == 0 || i+1 == len(hashes) {
					prog.complete(1)
					prog.reportPurgeFiles("", i+1)
				}
			}
		}
		return keyToHashes
	}

	type pair struct {
		key  fileRefKey
		hash string
	}
	results := make([]pair, 0, len(hashes))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, workers)
	var built atomic.Int64

	for _, hash := range hashes {
		// hash := hash
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			if key, ok := fileRefKeyForPath(store.ObjectPath(hash)); ok {
				mu.Lock()
				results = append(results, pair{key: key, hash: hash})
				mu.Unlock()
			}
			if prog != nil {
				prog.complete(1)
				if n := built.Add(1); n == 1 || n%500 == 0 || int(n) == len(hashes) {
					prog.reportPurgeFiles("", int(n))
				}
			}
		}()
	}
	wg.Wait()

	for _, p := range results {
		keyToHashes[p.key] = append(keyToHashes[p.key], p.hash)
	}
	return keyToHashes
}
