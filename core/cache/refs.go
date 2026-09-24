package cache

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/gluestick-sh/core/humanize"
	"github.com/gluestick-sh/core/message"
	"github.com/gluestick-sh/core/store"
)

// AppReferencedHashes returns cache store content hashes still linked under apps/ (latest version per package).
func AppReferencedHashes(appsDir string, store *store.Store) (map[string]bool, error) {
	return appReferencedHashes(appsDir, store, nil, nil)
}

// AppReferencedHashesWithProgress is like AppReferencedHashes but reports scan progress.
func AppReferencedHashesWithProgress(appsDir string, store *store.Store, report GCProgressReporter) (map[string]bool, error) {
	return appReferencedHashes(appsDir, store, nil, newGCProgress(report))
}

func appReferencedHashes(appsDir string, store *store.Store, idx *Index, prog *gcProgress) (map[string]bool, error) {
	if appsDir == "" || store == nil {
		return map[string]bool{}, nil
	}
	plan, err := buildAppScanPlanWithProgress(appsDir, idx, store, prog, 0)
	if err != nil {
		return nil, err
	}
	if len(plan.targets) == 0 {
		if prog != nil {
			prog.reportInfo(GCPhaseScan, message.GCScanAppsEmpty, map[string]interface{}{
				"path": FriendlyDisplayPath(appsDir),
			})
		}
		return map[string]bool{}, nil
	}
	if prog != nil {
		prog.report(GCPhaseScan, message.GCAppsScanPlan, map[string]interface{}{
			"fileTotal": plan.fileTotal,
			"dirTotal":  plan.dirTotal,
			"packages":  len(plan.targets),
		})
	}
	keyIndex, _, err := scanStoreParallel(store, prog)
	if err != nil {
		return nil, err
	}
	refs, err := scanAppInstallDirsParallel(plan, store, keyIndex, prog)
	if err != nil {
		return nil, err
	}
	if prog != nil {
		prog.report(GCPhaseScan, message.GCAppsScanComplete, map[string]interface{}{
			"count": len(refs),
		})
	}
	return refs, nil
}

// indexReferencedHashes returns every hash listed in package_files.
func (idx *Index) indexReferencedHashes() (map[string]bool, error) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	refs := make(map[string]bool)
	rows, err := idx.db.Query(`SELECT DISTINCT hash FROM package_files`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var hash string
		if err := rows.Scan(&hash); err != nil {
			continue
		}
		refs[hash] = true
	}
	return refs, nil
}

// CollectReferencedHashes unions index entries and on-disk apps hardlinks.
func CollectReferencedHashes(idx *Index, appsDir string, store *store.Store) (map[string]bool, error) {
	indexRefs, err := idx.indexReferencedHashes()
	if err != nil {
		return nil, err
	}
	appsRefs, err := appReferencedHashes(appsDir, store, idx, nil)
	if err != nil {
		return nil, err
	}
	for hash := range appsRefs {
		indexRefs[hash] = true
	}
	return indexRefs, nil
}

// PurgeOrphanBlobs deletes store objects not referenced by the index or apps hardlinks.
func PurgeOrphanBlobs(idx *Index, store *store.Store, appsDir string) (removed int, freedBytes int64, err error) {
	return PurgeOrphanBlobsWithProgress(idx, store, appsDir, nil)
}

// PurgeOrphanBlobsWithProgress is like PurgeOrphanBlobs but reports phased progress.
func PurgeOrphanBlobsWithProgress(idx *Index, store *store.Store, appsDir string, report GCProgressReporter) (removed int, freedBytes int64, err error) {
	if store == nil {
		return 0, 0, fmt.Errorf("cas store is required")
	}
	prog := newGCProgress(report)
	if prog != nil {
		prog.reportInfo(GCPhasePrepare, message.GCPrepareStore, map[string]interface{}{
			"path": FriendlyDisplayPath(store.Path()),
		})
		prog.reportInfo(GCPhaseCollect, message.GCReadingIndexRefs, nil)
	}
	indexRefs, err := idx.indexReferencedHashes()
	if err != nil {
		return 0, 0, err
	}
	if prog != nil {
		prog.reportInfo(GCPhaseCollect, message.GCIndexRefCount, map[string]interface{}{
			"count": len(indexRefs),
		})
	}

	var storeObjects []storeObjectEntry
	if appsDir != "" {
		if _, statErr := os.Stat(appsDir); statErr == nil {
			if prog != nil {
				prog.reportInfo(GCPhaseScan, message.GCScanAppsStart, map[string]interface{}{
					"path": FriendlyDisplayPath(appsDir),
				})
			}
			plan, planErr := buildAppScanPlanWithProgress(appsDir, idx, store, prog, gcFinalizeScanUnit)
			if planErr != nil {
				return 0, 0, planErr
			}
			if prog != nil && len(plan.targets) > 0 {
				prog.report(GCPhaseScan, message.GCAppsScanPlan, map[string]interface{}{
					"fileTotal": plan.fileTotal,
					"dirTotal":  plan.dirTotal,
					"packages":  len(plan.targets),
				})
			}
			var keyIndex map[fileRefKey]string
			keyIndex, storeObjects, err = scanStoreParallel(store, prog)
			if err != nil {
				return 0, 0, err
			}
			if len(plan.targets) > 0 {
				appsRefs, scanErr := scanAppInstallDirsParallel(plan, store, keyIndex, prog)
				if scanErr != nil {
					return 0, 0, scanErr
				}
				for hash := range appsRefs {
					indexRefs[hash] = true
				}
				if prog != nil {
					prog.report(GCPhaseScan, message.GCAppsScanComplete, map[string]interface{}{
						"count": len(appsRefs),
					})
				}
			}
		} else {
			if prog != nil {
				prog.report(GCPhaseScan, message.GCScanAppsSkipped, nil)
			}
			shards := countStoreShards(store)
			if prog != nil {
				prog.addPlanned(int64(shards) + gcFinalizeScanUnit)
			}
			_, storeObjects, err = scanStoreParallel(store, prog)
		}
	} else {
		if prog != nil {
			prog.report(GCPhaseScan, message.GCScanAppsSkipped, nil)
		}
		shards := countStoreShards(store)
		if prog != nil {
			prog.addPlanned(int64(shards) + gcFinalizeScanUnit)
		}
		_, storeObjects, err = scanStoreParallel(store, prog)
	}
	if err != nil {
		return 0, 0, err
	}

	return deleteUnreferencedStoreObjects(store, indexRefs, storeObjects, prog)
}

// RemoveUnreferencedBlobs deletes store blobs whose hash is not in referenced.
func RemoveUnreferencedBlobs(store *store.Store, referenced map[string]bool) (removed int, freedBytes int64, err error) {
	_, storeObjects, err := scanStoreParallel(store, nil)
	if err != nil {
		return 0, 0, err
	}
	return deleteUnreferencedStoreObjects(store, referenced, storeObjects, nil)
}

type storeOrphan struct {
	path string
	size int64
}

func deleteUnreferencedStoreObjects(
	store *store.Store,
	referenced map[string]bool,
	storeObjects []storeObjectEntry,
	prog *gcProgress,
) (removed int, freedBytes int64, err error) {
	displayRoot := FriendlyDisplayPath(store.Path())
	orphans := filterUnreferencedStoreObjects(storeObjects, referenced)

	if prog != nil {
		if len(orphans) > 0 {
			prog.addPlanned(int64(len(orphans)))
		}
		prog.complete(gcFinalizeScanUnit)
		prog.report(GCPhaseCollect, message.GCRefsCollected, map[string]interface{}{
			"count": len(referenced),
		})
		prog.report(GCPhaseScan, message.GCScanStoreStart, map[string]interface{}{
			"path": displayRoot,
		})
		prog.report(GCPhaseScan, message.GCScanStoreComplete, map[string]interface{}{
			"scanned": len(storeObjects),
			"orphans": len(orphans),
		})
	}

	if len(orphans) == 0 {
		if prog != nil {
			prog.reportComplete(GCPhaseComplete, message.GCNoOrphans, map[string]interface{}{
				"path": displayRoot,
			})
		}
		return 0, 0, nil
	}

	if prog != nil {
		prog.report(GCPhaseDelete, message.GCOrphansFound, map[string]interface{}{
			"count": len(orphans),
		})
	}
	removed, freedBytes, err = deleteStoreOrphansParallel(orphans, prog, message.GCDeletingOrphansBatch)
	if prog != nil && err == nil {
		prog.reportComplete(GCPhaseComplete, message.GCCompleteFreed, map[string]interface{}{
			"removed": removed,
			"freed":   humanize.FormatBytes(freedBytes),
		})
	}
	return removed, freedBytes, err
}

func deleteStoreOrphansParallel(
	orphans []storeOrphan,
	prog *gcProgress,
	deleteMessageKey string,
) (removed int, freedBytes int64, err error) {
	total := len(orphans)
	if total == 0 {
		return 0, 0, nil
	}

	workers := gcWorkers(total)
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	var removedAtomic atomic.Int64
	var freedAtomic atomic.Int64
	var doneAtomic atomic.Int64
	var lastReport int

	for _, orphan := range orphans {
		orphan := orphan
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			if rmErr := os.Remove(orphan.path); rmErr == nil {
				removedAtomic.Add(1)
				freedAtomic.Add(orphan.size)
			}
			if prog != nil {
				prog.complete(1)
				done := int(doneAtomic.Add(1))
				if done == 1 || done-lastReport >= 50 || done == total {
					lastReport = done
					prog.reportDelete(done, total, deleteMessageKey)
				}
			}
		}()
	}
	wg.Wait()
	return int(removedAtomic.Load()), freedAtomic.Load(), nil
}

func hashFromStorePath(storeRoot, path string) (string, bool) {
	rel, err := filepath.Rel(storeRoot, path)
	if err != nil {
		return "", false
	}
	rel = filepath.ToSlash(rel)
	if strings.Contains(rel, "/") {
		parts := strings.Split(rel, "/")
		if len(parts) == 2 && len(parts[0]) == 2 {
			return parts[0] + parts[1], true
		}
		return "", false
	}
	if len(rel) == 64 {
		return rel, true
	}
	return "", false
}

// ScanInstallDir maps cache store hash -> relative path for one install directory.
func ScanInstallDir(installDir string, store *store.Store) (map[string]string, int64, error) {
	return scanInstallDir(installDir, store, nil)
}

// scanInstallDir maps cache store hash -> relative path for one install directory.
func scanInstallDir(installDir string, store *store.Store, keyIndex map[fileRefKey]string) (map[string]string, int64, error) {
	files := make(map[string]string)
	var totalSize int64
	walkErr := filepath.Walk(installDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		relPath, err := filepath.Rel(installDir, path)
		if err != nil || IsHiddenInstallPath(relPath) {
			return nil
		}
		hash, err := resolveInstallFileHash(path, store, keyIndex)
		if err != nil {
			return nil
		}
		if _, exists := files[hash]; !exists {
			totalSize += store.PayloadSize(hash, info.Size())
		}
		files[hash] = filepath.ToSlash(relPath)
		return nil
	})
	return files, totalSize, walkErr
}
