//! Referenced-hash collection and orphan-blob GC
//!
//! GC unions the DB index refs (`package_files`) with on-disk refs under
//! `apps/` (hardlinks resolved through the store key index) and deletes every
//! store object that neither references. Progress reporting
//! is CLI-side and arrives with the engine command layer.

use std::collections::BTreeSet;
use std::path::Path;
use std::sync::atomic::{AtomicI64, AtomicUsize, Ordering};
use std::time::Duration;

use crate::cache::index::Index;
use crate::cache::scan::{self, ScanError, StoreObjectEntry};
use crate::cache::workers::{gc_workers, WORKER_STACK_SIZE};
use crate::store::Store;

#[derive(Debug, thiserror::Error)]
pub enum RefsError {
    #[error(transparent)]
    Scan(#[from] ScanError),
    #[error(transparent)]
    Index(#[from] crate::cache::IndexError),
    #[error("spawn verify worker: {0}")]
    Spawn(#[source] std::io::Error),
}

pub type Result<T> = std::result::Result<T, RefsError>;

/// A store object not referenced by anything, pending deletion.
pub struct StoreOrphan {
    pub path: std::path::PathBuf,
    pub size: i64,
    pub hash: String,
}

/// AppReferencedHashes returns cache store content hashes still linked under
/// `apps/` (latest version per package). Without an index the file counts in
/// the scan plan are disk-only (passes a nil Index).
pub fn app_referenced_hashes(
    apps_dir: &Path,
    store: &Store,
    idx: Option<&Index>,
) -> Result<BTreeSet<String>> {
    if apps_dir.as_os_str().is_empty() {
        return Ok(BTreeSet::new());
    }
    let plan = scan::build_app_scan_plan(apps_dir, idx)?;
    if plan.targets.is_empty() {
        return Ok(BTreeSet::new());
    }
    let (key_index, _objects) = scan::scan_store(store)?;
    Ok(scan::scan_app_install_dirs_parallel(
        &plan, store, &key_index,
    )?)
}
impl Index {
    /// indexReferencedHashes returns every hash listed in package_files.
    pub fn index_referenced_hashes(
        &self,
    ) -> std::result::Result<BTreeSet<String>, crate::cache::IndexError> {
        let db = self.db.lock().unwrap();
        let mut stmt = db.prepare("SELECT DISTINCT hash FROM package_files")?;
        let rows = stmt.query_map([], |row| row.get::<_, String>(0))?;
        let mut refs = BTreeSet::new();
        for hash in rows.flatten() {
            // Scan errors skip the row.
            refs.insert(hash);
        }
        Ok(refs)
    }
}

/// CollectReferencedHashes unions index entries and on-disk apps hardlinks.
pub fn collect_referenced_hashes(
    idx: &Index,
    apps_dir: &Path,
    store: &Store,
) -> Result<BTreeSet<String>> {
    let mut refs = idx.index_referenced_hashes()?;
    let apps_refs = app_referenced_hashes(apps_dir, store, Some(idx))?;
    refs.extend(apps_refs);
    Ok(refs)
}

/// One phase transition of a cache GC / verify run.
///
/// A verbose progress ledger would add work units, percentages and per-shard /
/// per-package lines; this is the reduced form: structured events the
/// frontends render in their own language (the CLI prints English lines, the
/// desktop translates them — a bare `&str` would hardcode English for both).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum CachePhase {
    /// Stale `.tmp-*` files were swept: (removed, freed bytes).
    RemovedStaleTemps { removed: usize, freed: i64 },
    /// The cache index scan is starting.
    ScanCacheIndex,
    /// Index scan done; the cache store scan is starting.
    IndexRefs { referenced: usize },
    /// Scanning installed packages for hardlinked blobs.
    ScanAppPackages { packages: usize },
    /// Store scan progress: `done` objects walked so far (total unknown until
    /// the walk finishes).
    ScanStoreProgress { done: usize },
    /// Installed-package scan progress.
    ScanAppProgress { done: usize, total: usize },
    /// Store scan done: total objects vs. referenced blobs.
    Scanned { objects: usize, referenced: usize },
    /// Blobs kept because a running install created them recently.
    KeepInflight { blobs: usize },
    /// About to delete the orphan blobs.
    DeleteOrphans { orphans: usize },
    /// Orphan deletion progress.
    DeleteProgress { done: usize, total: usize },
    /// The cache store scan is starting (verify).
    ScanStore,
    /// About to re-hash N store objects (verify).
    VerifyObjects { objects: usize },
    /// Verification progress: objects re-hashed so far.
    VerifyProgress { done: usize, total: usize },
}

/// Progress callback for [`purge_orphan_blobs`] / `verify_store`: one event
/// per phase transition, which is what makes a multi-minute scan look alive.
pub type GcPhaseFn<'a> = &'a (dyn Fn(&CachePhase) + Sync);

/// Temp files older than this are crashed leftovers from `Store::write` /
/// `put_prehashed`.
///
/// Bootstrap downloads write through the store **without** the mutation lock,
/// so an in-flight download must never be swept; an hour is far beyond any
/// single artifact transfer.
const STALE_TEMP_MAX_AGE: Duration = Duration::from_secs(60 * 60);

/// Delete `.tmp-*` files at the store root that are older than
/// [`STALE_TEMP_MAX_AGE`]. Returns `(removed, freed bytes)`.
///
/// The object scan skips dot entries, so these never show up as orphans and
/// would otherwise accumulate forever after a crash mid-write.
fn sweep_stale_temp_files(store_root: &Path) -> (usize, i64) {
    let Ok(entries) = std::fs::read_dir(store_root) else {
        return (0, 0);
    };
    let mut removed = 0;
    let mut freed = 0i64;
    for entry in entries.flatten() {
        let is_temp = entry
            .file_name()
            .to_str()
            .is_some_and(|name| name.starts_with(".tmp-"));
        if !is_temp {
            continue;
        }
        let Ok(meta) = entry.metadata() else {
            continue;
        };
        if !meta.is_file() {
            continue;
        }
        let stale = meta
            .modified()
            .ok()
            .and_then(|mtime| mtime.elapsed().ok())
            .is_some_and(|age| age >= STALE_TEMP_MAX_AGE);
        if stale && std::fs::remove_file(entry.path()).is_ok() {
            removed += 1;
            freed += meta.len() as i64;
        }
    }
    (removed, freed)
}

/// PurgeOrphanBlobs deletes store objects not referenced by the index or
/// apps hardlinks. Returns (removed count, freed bytes).
///
/// The scan runs before anything is deleted, so aborting during it leaves the
/// store untouched.
pub fn purge_orphan_blobs(
    idx: &Index,
    store: &Store,
    apps_dir: &Path,
    keep_since: Option<std::time::SystemTime>,
    on_phase: Option<GcPhaseFn<'_>>,
) -> Result<(usize, i64)> {
    let orphans = plan_orphan_blobs(idx, store, apps_dir, keep_since, on_phase)?;
    delete_orphans(&orphans, on_phase)
}

/// Scan phase of GC: collect every blob that no index row or app hardlink
/// references. Nothing is deleted here, so callers may run it **without** the
/// data-root mutation lock — a cold scan takes minutes, and holding the lock
/// for it made every other maintenance op wait and eventually time out.
pub fn plan_orphan_blobs(
    idx: &Index,
    store: &Store,
    apps_dir: &Path,
    keep_since: Option<std::time::SystemTime>,
    on_phase: Option<GcPhaseFn<'_>>,
) -> Result<Vec<StoreOrphan>> {
    let phase = |msg: CachePhase| {
        if let Some(report) = on_phase {
            report(&msg);
        }
    };
    // Quiet no-op when there is nothing to sweep, so the common `cache gc`
    // output is unchanged.
    let (temps_removed, temps_freed) = sweep_stale_temp_files(store.path());
    if temps_removed > 0 {
        phase(CachePhase::RemovedStaleTemps {
            removed: temps_removed,
            freed: temps_freed,
        });
    }
    phase(CachePhase::ScanCacheIndex);
    let mut referenced = idx.index_referenced_hashes()?;
    phase(CachePhase::IndexRefs {
        referenced: referenced.len(),
    });
    // A cold store scan dominates the wall clock on a large cache; report how
    // many objects it has walked so the UI does not look hung.
    let scan_cb = |done: usize| phase(CachePhase::ScanStoreProgress { done });
    let store_objects: Vec<StoreObjectEntry>;
    if !apps_dir.as_os_str().is_empty() && std::fs::metadata(apps_dir).is_ok() {
        // apps dir present: union index refs with on-disk hardlink refs.
        let plan = scan::build_app_scan_plan(apps_dir, Some(idx))?;
        let (key_index, objects) = scan::scan_store_with_progress(store, Some(&scan_cb))?;
        store_objects = objects;
        if !plan.targets.is_empty() {
            phase(CachePhase::ScanAppPackages {
                packages: plan.targets.len(),
            });
            let app_cb =
                |done: usize, total: usize| phase(CachePhase::ScanAppProgress { done, total });
            let apps_refs = scan::scan_app_install_dirs_parallel_with_progress(
                &plan,
                store,
                &key_index,
                Some(&app_cb),
            )?;
            referenced.extend(apps_refs);
        }
    } else {
        // Apps dir missing or empty: index refs only (os.Stat branch).
        let (_key_index, objects) = scan::scan_store_with_progress(store, Some(&scan_cb))?;
        store_objects = objects;
    }
    phase(CachePhase::Scanned {
        objects: store_objects.len(),
        referenced: referenced.len(),
    });
    Ok(collect_orphans(
        &referenced,
        &store_objects,
        keep_since,
        on_phase,
    ))
}

/// Delete phase of GC: remove orphan blobs collected by
/// [`plan_orphan_blobs`]. Emits the `DeleteOrphans` phase and progress.
pub fn delete_orphans(
    orphans: &[StoreOrphan],
    on_phase: Option<GcPhaseFn<'_>>,
) -> Result<(usize, i64)> {
    if orphans.is_empty() {
        return Ok((0, 0));
    }
    if let Some(report) = on_phase {
        report(&CachePhase::DeleteOrphans {
            orphans: orphans.len(),
        });
    }
    delete_store_orphans_parallel(orphans, on_phase)
}

/// Filter store objects down to the unreferenced ones.
fn collect_orphans(
    referenced: &BTreeSet<String>,
    store_objects: &[StoreObjectEntry],
    keep_since: Option<std::time::SystemTime>,
    on_phase: Option<GcPhaseFn<'_>>,
) -> Vec<StoreOrphan> {
    let mut in_flight = 0usize;
    let orphans: Vec<StoreOrphan> = store_objects
        .iter()
        .filter(|obj| !referenced.contains(&obj.hash))
        .filter(|obj| {
            // An install that is still running holds no data-root lock while it
            // downloads, and its blobs are not referenced yet: anything created
            // since it started must survive this sweep.
            let protected = match (keep_since, obj.created) {
                (Some(floor), Some(created)) => created >= floor,
                _ => false,
            };
            if protected {
                in_flight += 1;
            }
            !protected
        })
        .map(|obj| StoreOrphan {
            path: obj.path.clone(),
            size: obj.size,
            hash: obj.hash.clone(),
        })
        .collect();
    if in_flight > 0 {
        if let Some(report) = on_phase {
            report(&CachePhase::KeepInflight { blobs: in_flight });
        }
    }
    orphans
}

/// RemoveUnreferencedBlobs deletes store blobs whose hash is not in
/// `referenced`.
pub fn remove_unreferenced_blobs(
    store: &Store,
    referenced: &BTreeSet<String>,
) -> Result<(usize, i64)> {
    let (_key_index, store_objects) = scan::scan_store(store)?;
    let orphans = collect_orphans(referenced, &store_objects, None, None);
    delete_orphans(&orphans, None)
}

/// deleteStoreOrphansParallel removes orphans with a worker pool. Remove
/// errors are ignored
fn delete_store_orphans_parallel(
    orphans: &[StoreOrphan],
    on_phase: Option<GcPhaseFn<'_>>,
) -> Result<(usize, i64)> {
    let total = orphans.len();
    let step = (total / 100).max(1);
    let workers = gc_workers(total);
    let removed = AtomicUsize::new(0);
    let freed = AtomicI64::new(0);
    let next = AtomicUsize::new(0);
    let processed = AtomicUsize::new(0);

    std::thread::scope(|scope| {
        for _ in 0..workers {
            let (removed, freed, next, processed) = (&removed, &freed, &next, &processed);
            std::thread::Builder::new()
                .stack_size(WORKER_STACK_SIZE)
                .spawn_scoped(scope, move || loop {
                    let i = next.fetch_add(1, Ordering::Relaxed);
                    if i >= total {
                        break;
                    }
                    let orphan = &orphans[i];
                    if std::fs::remove_file(&orphan.path).is_ok() {
                        removed.fetch_add(1, Ordering::Relaxed);
                        freed.fetch_add(orphan.size, Ordering::Relaxed);
                    }
                    let done = processed.fetch_add(1, Ordering::Relaxed) + 1;
                    if let Some(report) = on_phase {
                        if done % step == 0 || done == total {
                            report(&CachePhase::DeleteProgress { done, total });
                        }
                    }
                })
                .expect("spawn cache worker");
        }
    });

    Ok((
        removed.load(Ordering::Relaxed),
        freed.load(Ordering::Relaxed),
    ))
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::store::Store;
    use crate::testutil::TempDir;

    fn test_root(tag: &str) -> TempDir {
        TempDir::new(&format!("glue-cache-refs-{tag}"))
    }

    fn test_store(root: &Path) -> Store {
        let store = Store::new(root.join("store")).unwrap();
        store.prereqs().unwrap();
        store
    }

    fn test_index(root: &Path) -> Index {
        Index::new(root).unwrap()
    }

    /// WriteCacheStoreObject: place raw bytes at a chosen hash path.
    fn write_cache_store_object(store: &Store, hash: &str, content: &[u8]) {
        let obj_path = store.object_path(hash);
        std::fs::create_dir_all(obj_path.parent().unwrap()).unwrap();
        std::fs::write(obj_path, content).unwrap();
    }

    #[test]
    fn app_referenced_hashes_empty_inputs() {
        let root = test_root("empty-inputs");
        let store = test_store(&root);

        // Empty apps dir path.
        assert!(app_referenced_hashes(Path::new(""), &store, None)
            .unwrap()
            .is_empty());
        // Missing apps dir.
        let missing = root.join("no-such-apps");
        assert!(app_referenced_hashes(&missing, &store, None)
            .unwrap()
            .is_empty());
    }

    #[test]
    fn app_referenced_hashes_latest_version_only() {
        let root = test_root("latest-only");
        let store = test_store(&root);

        let current_hash = store.write(&b"current-version"[..]).unwrap();

        let apps_dir = root.join("apps");
        let old_dir = apps_dir.join("tool").join("1.0.0");
        let new_dir = apps_dir.join("tool").join("2.0.0");
        std::fs::create_dir_all(&old_dir).unwrap();
        std::fs::create_dir_all(&new_dir).unwrap();
        // Plain (non-store) file in the OLD version — never scanned.
        std::fs::write(old_dir.join("old.exe"), b"obsolete").unwrap();
        store
            .link(&current_hash, &new_dir.join("tool.exe"))
            .unwrap();

        let refs = app_referenced_hashes(&apps_dir, &store, None).unwrap();
        assert!(refs.contains(&current_hash), "refs = {refs:?}");
        assert_eq!(refs.len(), 1, "refs = {refs:?}");
    }

    #[test]
    fn collect_referenced_hashes_union_index_and_apps() {
        let root = test_root("union");
        let store = test_store(&root);
        let idx = test_index(&root);

        let index_only =
            "aaaabbbbccccddddeeeeffff00001111222233334444555566667777888899990000".to_string();
        let apps_only_hash = store.write(&b"apps-only"[..]).unwrap();
        write_cache_store_object(&store, &index_only, b"index-only");

        let mut files = std::collections::BTreeMap::new();
        files.insert(index_only.clone(), "a.exe".to_string());
        idx.add("pkg-a", "1.0", &files, 10).unwrap();

        let apps_dir = root.join("apps");
        let install_dir = apps_dir.join("pkg-b").join("1.0");
        std::fs::create_dir_all(&install_dir).unwrap();
        store
            .link(&apps_only_hash, &install_dir.join("b.exe"))
            .unwrap();

        let refs = collect_referenced_hashes(&idx, &apps_dir, &store).unwrap();
        assert!(refs.contains(&index_only), "refs = {refs:?}");
        assert!(refs.contains(&apps_only_hash), "refs = {refs:?}");
    }

    #[test]
    fn purge_orphan_blobs_removes_orphan() {
        let root = test_root("purge-orphan");
        let store = test_store(&root);
        let idx = test_index(&root);

        let kept =
            "aaaabbbbccccddddeeeeffff00001111222233334444555566667777888899990000".to_string();
        let orphan =
            "1111222233334444555566667777888899990000aaaabbbbccccddddeeeeffff1111".to_string();
        write_cache_store_object(&store, &kept, b"keep");
        write_cache_store_object(&store, &orphan, b"orphan");

        let mut files = std::collections::BTreeMap::new();
        files.insert(kept.clone(), "upm.exe".to_string());
        idx.add("upm", "1.0.0", &files, 4).unwrap();

        let (removed, freed) =
            purge_orphan_blobs(&idx, &store, &root.join("apps"), None, None).unwrap();
        assert_eq!(removed, 1);
        assert_eq!(freed, "orphan".len() as i64);
        assert!(store.has(&kept));
        assert!(!store.has(&orphan));
    }

    #[test]
    fn purge_orphan_blobs_keeps_blobs_from_a_running_install() {
        let root = test_root("purge-inflight");
        let store = test_store(&root);
        let idx = test_index(&root);

        // An install that started before the blob was ingested.
        let floor = std::time::SystemTime::now();
        std::thread::sleep(std::time::Duration::from_millis(20));
        let orphan =
            "33334444555566667777888899990000aaaabbbbccccddddeeeeffff0000111122".to_string();
        write_cache_store_object(&store, &orphan, b"downloading");

        let (removed, _freed) =
            purge_orphan_blobs(&idx, &store, &root.join("apps"), Some(floor), None).unwrap();
        assert_eq!(removed, 0, "an in-flight install's blob must survive");
        assert!(store.has(&orphan));

        // With nothing in flight it is collected as usual.
        let (removed, _freed) =
            purge_orphan_blobs(&idx, &store, &root.join("apps"), None, None).unwrap();
        assert_eq!(removed, 1);
        assert!(!store.has(&orphan));
    }

    #[test]
    fn purge_orphan_blobs_keeps_apps_only_reference() {
        let root = test_root("purge-apps-ref");
        let store = test_store(&root);
        let idx = test_index(&root);

        let orphan =
            "222233334444555566667777888899990000aaaabbbbccccddddeeeeffff00001111".to_string();
        let apps_hash = store.write(&b"installed"[..]).unwrap();
        write_cache_store_object(&store, &orphan, b"orphan");

        let apps_dir = root.join("apps");
        let install_dir = apps_dir.join("node").join("20.0.0");
        std::fs::create_dir_all(&install_dir).unwrap();
        store
            .link(&apps_hash, &install_dir.join("node.exe"))
            .unwrap();

        let (removed, _freed) = purge_orphan_blobs(&idx, &store, &apps_dir, None, None).unwrap();
        assert_eq!(removed, 1);
        assert!(store.has(&apps_hash), "apps-referenced blob should remain");
        assert!(!store.has(&orphan), "orphan should be deleted");
    }

    #[test]
    fn sweep_removes_only_stale_temp_files() {
        let root = test_root("sweep-temp");
        let store = test_store(&root);
        let old = store.path().join(".tmp-1-0");
        let fresh = store.path().join(".tmp-1-1");
        std::fs::write(&old, b"old").unwrap();
        std::fs::write(&fresh, b"fresh").unwrap();

        let handle = std::fs::File::options().write(true).open(&old).unwrap();
        handle
            .set_modified(std::time::SystemTime::now() - std::time::Duration::from_secs(2 * 3600))
            .unwrap();
        drop(handle);

        let (removed, freed) = sweep_stale_temp_files(store.path());
        assert_eq!(removed, 1);
        assert_eq!(freed, 3);
        assert!(!old.exists(), "stale temp file removed");
        assert!(fresh.exists(), "an in-flight temp file is never swept");
    }

    #[test]
    fn remove_unreferenced_blobs_counts() {
        let root = test_root("remove-unref");
        let store = test_store(&root);
        let keep = store.write(&b"keep"[..]).unwrap();
        let drop = store.write(&b"drop"[..]).unwrap();

        let mut referenced = BTreeSet::new();
        referenced.insert(keep.clone());
        let (removed, freed) = remove_unreferenced_blobs(&store, &referenced).unwrap();
        assert_eq!(removed, 1);
        assert_eq!(freed, "drop".len() as i64);
        assert!(store.has(&keep));
        assert!(!store.has(&drop));
    }

    #[test]
    fn index_referenced_hashes_distinct() {
        let root = test_root("index-refs");
        let store = test_store(&root);
        let idx = test_index(&root);
        let mut files = std::collections::BTreeMap::new();
        files.insert("h1".to_string(), "a.exe".to_string());
        files.insert("h2".to_string(), "b.exe".to_string());
        idx.add("pkg", "1.0", &files, 2).unwrap();

        let refs = idx.index_referenced_hashes().unwrap();
        assert_eq!(refs, BTreeSet::from(["h1".to_string(), "h2".to_string()]));
        let _ = store;
    }
}
