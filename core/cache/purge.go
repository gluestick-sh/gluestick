package cache

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/gluestick-sh/core/apps"
	"github.com/gluestick-sh/core/humanize"
	"github.com/gluestick-sh/core/store"
	"github.com/gluestick-sh/core/message"
)

// ClearAll removes every package from the index. Cache store objects on disk are not deleted.
func (idx *Index) ClearAll() error {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	tx, err := idx.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM installed_packages`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM packages`); err != nil {
		return err
	}
	return tx.Commit()
}

// PurgePackage removes a package from the index and deletes its cache store objects when no
// other index entry or apps/ hardlink references the same hash.
// appsDir is typically <glue-root>/apps; pass "" to consider the index only.
func PurgePackage(idx *Index, store *store.Store, appsDir, pkgName string) (removedFiles int, freedBytes int64, err error) {
	return PurgePackageWithProgress(idx, store, appsDir, pkgName, nil)
}

// PurgePackageWithProgress is like PurgePackage but reports phased progress.
func PurgePackageWithProgress(idx *Index, store *store.Store, appsDir, pkgName string, report GCProgressReporter) (removedFiles int, freedBytes int64, err error) {
	prog := newGCProgress(report)
	if prog != nil {
		prog.reportInfo(GCPhasePrepare, message.PurgePrepare, map[string]interface{}{"name": pkgName})
	}

	if _, ok := idx.Get(pkgName); !ok {
		return 0, 0, fmt.Errorf("package not in cache index: %s", pkgName)
	}

	hashes := idx.GetFilesForPackage(pkgName)
	relatedPkgs := idx.PackagesSharingHashes(hashes, pkgName)
	scanBudget := planPurgeScanBudget(idx, appsDir, pkgName, relatedPkgs, len(hashes))

	if prog != nil {
		planned := int64(1) + gcFinalizeScanUnit + int64(len(hashes))
		if appsDir != "" {
			planned += 1 + int64(scanBudget.primaryFiles)
			if scanBudget.secondaryWalkBudget > 0 {
				planned += 1 + int64(scanBudget.secondaryWalkBudget)
			}
		}
		if planned < 2 {
			planned = 2
		}
		prog.addPlanned(planned)
		prog.reportInfo("index", message.PurgeRemovingIndex, map[string]interface{}{
			"name":  pkgName,
			"files": len(hashes),
		})
	}

	if err := idx.Remove(pkgName); err != nil {
		return 0, 0, err
	}
	if prog != nil {
		prog.complete(1)
		prog.report(GCPhaseCollect, message.PurgeIndexRemoved, map[string]interface{}{"name": pkgName})
	}

	indexRefs, err := idx.indexReferencedHashes()
	if err != nil {
		return removedFiles, freedBytes, err
	}

	purgedOnDisk := packageHasInstallDir(appsDir, pkgName)
	appScanHashes := purgeAppScanCandidates(hashes, indexRefs)

	var appsRefs map[string]bool
	if appsDir != "" && store != nil && len(appScanHashes) > 0 {
		appsRefs, err = AppsReferenceCandidateHashes(appsDir, store, idx, pkgName, appScanHashes, relatedPkgs, prog, scanBudget, !purgedOnDisk)
		if err != nil {
			return removedFiles, freedBytes, err
		}
	} else {
		appsRefs = make(map[string]bool)
		skipPurgeAppScanProgress(prog, scanBudget)
	}

	if prog != nil {
		prog.complete(gcFinalizeScanUnit)
		prog.reportInfo(GCPhaseCollect, message.PurgeCheckingRefs, nil)
	}

	var toDelete []storeOrphan
	for _, hash := range hashes {
		if indexRefs[hash] || appsRefs[hash] {
			continue
		}
		objPath := store.ObjectPath(hash)
		info, statErr := os.Stat(objPath)
		if statErr != nil {
			if os.IsNotExist(statErr) {
				continue
			}
			return removedFiles, freedBytes, fmt.Errorf("stat cache store object %s: %w", hash[:min(8, len(hash))], statErr)
		}
		toDelete = append(toDelete, storeOrphan{path: objPath, size: info.Size()})
	}

	if prog != nil {
		skipDeletes := len(hashes) - len(toDelete)
		if skipDeletes > 0 {
			prog.complete(int64(skipDeletes))
		}
	}

	if len(toDelete) == 0 {
		if prog != nil {
			prog.reportComplete(GCPhaseComplete, message.PurgeCompleteNothing, map[string]interface{}{
				"name": pkgName,
			})
		}
		return 0, 0, nil
	}

	if prog != nil {
		prog.report(GCPhaseDelete, message.PurgeDeletingFile, map[string]interface{}{
			"current": 0,
			"total":   len(toDelete),
		})
	}
	removedFiles, freedBytes, err = deleteStoreOrphansParallel(toDelete, prog, message.PurgeDeletingFile)
	if prog != nil && err == nil {
		prog.reportComplete(GCPhaseComplete, message.PurgeCompleteFreed, map[string]interface{}{
			"removed": removedFiles,
			"freed":   humanize.FormatBytes(freedBytes),
		})
	}
	return removedFiles, freedBytes, err
}

func pkgNamesSet(names []string) map[string]struct{} {
	set := make(map[string]struct{}, len(names))
	for _, name := range names {
		set[name] = struct{}{}
	}
	return set
}

func packageHasInstallDir(appsDir, pkgName string) bool {
	if appsDir == "" || pkgName == "" {
		return false
	}
	_, _, ok := apps.ActiveInstallDir(filepath.Join(appsDir, pkgName))
	return ok
}

// purgeAppScanCandidates returns hashes not already kept by another package's cache index.
func purgeAppScanCandidates(hashes []string, indexRefs map[string]bool) []string {
	if len(hashes) == 0 {
		return nil
	}
	out := make([]string, 0)
	for _, hash := range hashes {
		if hash == "" || indexRefs[hash] {
			continue
		}
		out = append(out, hash)
	}
	return out
}

func skipPurgeAppScanProgress(prog *gcProgress, budget purgeScanBudget) {
	if prog == nil {
		return
	}
	var units int64 = 1
	if budget.primaryFiles > 0 {
		units += 1 + int64(budget.primaryFiles)
	}
	if budget.secondaryWalkBudget > 0 {
		units += 1 + int64(budget.secondaryWalkBudget)
	}
	prog.complete(units)
}
