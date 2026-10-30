//! On-disk scan helpers shared by cache Rebuild, installed-sync and GC
//!
//! Two scans matter:
//!   - **store scan**: walk `<store>` once, building the hardlink-identity
//!     index (`fileRefKey → hash`) and listing every object (shard-parallel).
//!   - **app scan**: enumerate `apps/<pkg>/<version>` directories and map
//!     each hardlinked file back to its store hash (dir-task-parallel).
//!
//! Progress reporting is CLI-side and arrives with the
//! engine command layer; the scan cores are progress-free here.

use std::collections::{BTreeMap, BTreeSet};
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::Mutex;

use crate::apps;
use crate::cache::fileid::file_ref_key;
use crate::cache::paths::is_hidden_install_path;
use crate::cache::workers::{gc_workers, WORKER_STACK_SIZE};
use crate::cache::Index;
use crate::store::Store;

/// Hardlink identity key → cache store hash.
pub type FileRefIndex = BTreeMap<String, String>;

#[derive(Debug, thiserror::Error)]
pub enum ScanError {
    #[error(transparent)]
    Io(#[from] std::io::Error),
    #[error("hash file: {0}")]
    HashFile(#[source] std::io::Error),
    /// Boxed to break the IndexError ↔ ScanError conversion cycle.
    #[error(transparent)]
    Index(Box<crate::cache::IndexError>),
}

impl From<crate::cache::IndexError> for ScanError {
    fn from(e: crate::cache::IndexError) -> Self {
        ScanError::Index(Box::new(e))
    }
}

pub type Result<T> = std::result::Result<T, ScanError>;

/// One cache store object: path, content hash and size.
pub struct StoreObjectEntry {
    pub path: PathBuf,
    pub hash: String,
    pub size: i64,
    /// When the object appeared on disk (creation time, falling back to
    /// modification). The GC uses it to leave alone blobs an install that is
    /// still running has just ingested.
    pub created: Option<std::time::SystemTime>,
}

/// A package with an active install directory (appScanPlan.targets entry).
#[derive(Debug, Clone)]
pub struct AppScanTarget {
    pub pkg_name: String,
    pub install_dir: PathBuf,
}

/// One directory to scan inside a package install.
#[derive(Debug, Clone)]
pub struct AppScanTask {
    pub pkg_name: String,
    pub install_dir: PathBuf,
    pub scan_dir: PathBuf,
}

/// Pre-enumerated scan plan, without progress fields.
#[derive(Debug, Default)]
pub struct AppScanPlan {
    pub targets: Vec<AppScanTarget>,
    pub tasks: Vec<AppScanTask>,
    pub file_total: i64,
    pub dir_total: i64,
}

/// + `ToSlash` for install-relative paths.
fn to_slash(rel: &Path) -> String {
    rel.to_string_lossy().replace('\\', "/")
}

// ---------------------------------------------------------------------------
// Store scan
// ---------------------------------------------------------------------------

/// hashFromStorePath extracts the content hash from a store object path:
/// either `<root>/hh/rest…` (two-char shard) or a flat 64-hex file name.
/// Anything else (shard dirs, temp files) is not an object.
pub fn hash_from_store_path(store_root: &Path, path: &Path) -> Option<String> {
    let rel = path.strip_prefix(store_root).ok()?;
    let rel = to_slash(rel);
    if rel.contains('/') {
        let parts: Vec<&str> = rel.split('/').collect();
        if parts.len() == 2 && parts[0].len() == 2 {
            return Some(format!("{}{}", parts[0], parts[1]));
        }
        return None;
    }
    if rel.len() == 64 {
        return Some(rel);
    }
    None
}

/// Walk `dir` and hand every regular file to `on_file`
fn walk_files<F>(dir: &Path, on_file: &mut F)
where
    F: FnMut(&Path, &std::fs::Metadata),
{
    let Ok(entries) = std::fs::read_dir(dir) else {
        return;
    };
    for entry in entries.flatten() {
        let path = entry.path();
        let Ok(file_type) = entry.file_type() else {
            continue;
        };
        if file_type.is_dir() {
            walk_files(&path, on_file);
            continue;
        }
        if let Ok(meta) = entry.metadata() {
            on_file(&path, &meta);
        }
    }
}

/// scanStoreParallel: walk the cache store once, building the hardlink key
/// index and listing every object. Shards (`hh/` dirs and flat files) are
/// processed by a worker pool).
pub fn scan_store(store: &Store) -> Result<(FileRefIndex, Vec<StoreObjectEntry>)> {
    scan_store_with_progress(store, None)
}

/// [`scan_store`] with a callback reporting how many objects have been seen
/// so far (throttled to keep a cold scan's progress cheap).
pub fn scan_store_with_progress(
    store: &Store,
    on_progress: Option<&(dyn Fn(usize) + Sync)>,
) -> Result<(FileRefIndex, Vec<StoreObjectEntry>)> {
    let index: Mutex<FileRefIndex> = Mutex::new(FileRefIndex::new());
    let objects = scan_store_objects_impl(store, Some(&index), on_progress)?;
    Ok((index.into_inner().unwrap(), objects))
}

/// Store objects only, without the hardlink identity index.
///
/// Building that index opens every blob for its file id, which verification
/// has no use for (`cache verify` re-reads the contents anyway).
pub fn scan_store_objects_with_progress(
    store: &Store,
    on_progress: Option<&(dyn Fn(usize) + Sync)>,
) -> Result<Vec<StoreObjectEntry>> {
    scan_store_objects_impl(store, None, on_progress)
}

fn scan_store_objects_impl(
    store: &Store,
    index: Option<&Mutex<FileRefIndex>>,
    on_progress: Option<&(dyn Fn(usize) + Sync)>,
) -> Result<Vec<StoreObjectEntry>> {
    let root = store.path().to_path_buf();
    let mut shards: Vec<String> = Vec::new();
    for entry in std::fs::read_dir(&root)? {
        let Ok(entry) = entry else { continue };
        let name = entry.file_name().to_string_lossy().into_owned();
        if name.starts_with('.') {
            continue;
        }
        shards.push(name);
    }

    let objects: Mutex<Vec<StoreObjectEntry>> = Mutex::new(Vec::new());
    let next = AtomicUsize::new(0);
    let seen = AtomicUsize::new(0);
    let workers = gc_workers(shards.len());

    std::thread::scope(|scope| {
        for _ in 0..workers {
            let (index, objects, next, root, shards, seen) =
                (&index, &objects, &next, &root, &shards, &seen);
            std::thread::Builder::new()
                .stack_size(WORKER_STACK_SIZE)
                .spawn_scoped(scope, move || loop {
                    let i = next.fetch_add(1, Ordering::Relaxed);
                    if i >= shards.len() {
                        break;
                    }
                    let name = &shards[i];
                    let entry_path = root.join(name);
                    // Os.Stat follows symlinks.
                    let Ok(info) = std::fs::metadata(&entry_path) else {
                        continue;
                    };
                    if info.is_dir() && name.len() == 2 {
                        walk_files(&entry_path, &mut |path, meta| {
                            record_store_object(
                                path,
                                meta,
                                root,
                                *index,
                                objects,
                                seen,
                                on_progress,
                            );
                        });
                    } else if !info.is_dir() {
                        record_store_object(
                            &entry_path,
                            &info,
                            root,
                            *index,
                            objects,
                            seen,
                            on_progress,
                        );
                    }
                })
                .expect("spawn cache worker");
        }
    });

    Ok(objects.into_inner().unwrap())
}

#[allow(clippy::too_many_arguments)]
fn record_store_object(
    path: &Path,
    info: &std::fs::Metadata,
    root: &Path,
    index: Option<&Mutex<FileRefIndex>>,
    objects: &Mutex<Vec<StoreObjectEntry>>,
    seen: &AtomicUsize,
    on_progress: Option<&(dyn Fn(usize) + Sync)>,
) {
    let Some(hash) = hash_from_store_path(root, path) else {
        return;
    };
    if let Some(index) = index {
        if let Some(key) = file_ref_key(path) {
            index.lock().unwrap().insert(key, hash.clone());
        }
    }
    objects.lock().unwrap().push(StoreObjectEntry {
        path: path.to_path_buf(),
        hash,
        size: info.len() as i64,
        created: info.created().or_else(|_| info.modified()).ok(),
    });
    if let Some(report) = on_progress {
        let n = seen.fetch_add(1, Ordering::Relaxed) + 1;
        if n.is_multiple_of(512) {
            report(n);
        }
    }
}

/// buildStoreFileRefIndex maps hardlink identity keys to cache store hashes
/// for every store object.
pub fn build_store_file_ref_index(store: &Store) -> Result<FileRefIndex> {
    let (index, _objects) = scan_store(store)?;
    Ok(index)
}

/// resolveInstallFileHash resolves an installed file's cache store hash via
/// the hardlink key index, falling back to re-hashing the file
pub fn resolve_install_file_hash(
    path: &Path,
    store: &Store,
    key_index: Option<&FileRefIndex>,
) -> Result<String> {
    if let Some(index) = key_index {
        if let Some(key) = file_ref_key(path) {
            if let Some(hash) = index.get(&key) {
                return Ok(hash.clone());
            }
        }
    }
    store.hash_for_path(path).map_err(ScanError::HashFile)
}

/// scanInstallDir maps cache store hash → install-relative path for one
/// install directory, plus the total payload bytes (store object size when
/// the blob exists, file size otherwise — archive-only extracts).
/// Missing directories and unreadable files are skipped
pub fn scan_install_dir(
    install_dir: &Path,
    store: &Store,
    key_index: Option<&FileRefIndex>,
) -> (BTreeMap<String, String>, i64) {
    let mut files: BTreeMap<String, String> = BTreeMap::new();
    let mut total_size: i64 = 0;
    walk_files(install_dir, &mut |path, meta| {
        let Ok(rel) = path.strip_prefix(install_dir) else {
            return;
        };
        let rel_path = to_slash(rel);
        if is_hidden_install_path(&rel_path) {
            return;
        }
        let Ok(hash) = resolve_install_file_hash(path, store, key_index) else {
            return;
        };
        if !files.contains_key(&hash) {
            total_size += store.payload_size(&hash, meta.len() as i64);
        }
        files.insert(hash, rel_path);
    });
    (files, total_size)
}

// ---------------------------------------------------------------------------
// App scan
// ---------------------------------------------------------------------------

/// listAppScanTargets scans the apps directory and returns packages with
/// their active install directories (missing apps dir → empty list).
pub fn list_app_scan_targets(apps_dir: &Path) -> Result<Vec<AppScanTarget>> {
    if apps_dir.as_os_str().is_empty() {
        return Ok(Vec::new());
    }
    let entries = match std::fs::read_dir(apps_dir) {
        Ok(entries) => entries,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(Vec::new()),
        Err(e) => return Err(ScanError::Io(e)),
    };
    let mut targets = Vec::new();
    for entry in entries.flatten() {
        let Ok(file_type) = entry.file_type() else {
            continue;
        };
        if !file_type.is_dir() {
            continue;
        }
        let pkg_name = entry.file_name().to_string_lossy().into_owned();
        let pkg_dir = apps_dir.join(&pkg_name);
        if let Some((install_dir, _version)) = apps::active_install_dir(&pkg_dir) {
            targets.push(AppScanTarget {
                pkg_name,
                install_dir,
            });
        }
    }
    // Os.ReadDir returns entries sorted by filename.
    targets.sort_by(|a, b| a.pkg_name.cmp(&b.pkg_name));
    Ok(targets)
}

/// enumeratePackageDirs walks an install directory and returns one scan task
/// per directory, plus the total file count (excluding hidden paths).
/// Read errors propagate.
pub fn enumerate_package_dirs(
    pkg_name: &str,
    install_dir: &Path,
) -> std::io::Result<(Vec<AppScanTask>, usize)> {
    fn walk(
        pkg_name: &str,
        install_dir: &Path,
        scan_dir: &Path,
        tasks: &mut Vec<AppScanTask>,
        file_count: &mut usize,
    ) -> std::io::Result<()> {
        tasks.push(AppScanTask {
            pkg_name: pkg_name.to_string(),
            install_dir: install_dir.to_path_buf(),
            scan_dir: scan_dir.to_path_buf(),
        });
        for entry in std::fs::read_dir(scan_dir)? {
            let entry = entry?;
            let path = entry.path();
            let Ok(rel) = path.strip_prefix(install_dir) else {
                continue;
            };
            if is_hidden_install_path(&to_slash(rel)) {
                continue;
            }
            if entry.file_type()?.is_dir() {
                walk(pkg_name, install_dir, &path, tasks, file_count)?;
                continue;
            }
            *file_count += 1;
        }
        Ok(())
    }
    let mut tasks = Vec::new();
    let mut file_count = 0usize;
    walk(
        pkg_name,
        install_dir,
        install_dir,
        &mut tasks,
        &mut file_count,
    )?;
    Ok((tasks, file_count))
}

/// enumerateAppScanTasksParallel enumerates scan tasks for multiple packages
/// in parallel (≥ maxGCWorkers, capped by target count).
pub fn enumerate_app_scan_tasks_parallel(
    targets: &[AppScanTarget],
) -> Result<(Vec<AppScanTask>, usize)> {
    if targets.is_empty() {
        return Ok((Vec::new(), 0));
    }
    let workers = gc_workers(targets.len()).min(targets.len());
    let all: Mutex<Vec<AppScanTask>> = Mutex::new(Vec::new());
    let disk_files = AtomicUsize::new(0);
    let first_err: Mutex<Option<std::io::Error>> = Mutex::new(None);
    let next = AtomicUsize::new(0);

    std::thread::scope(|scope| {
        for _ in 0..workers {
            let (all, disk_files, first_err, next, targets) =
                (&all, &disk_files, &first_err, &next, targets);
            std::thread::Builder::new()
                .stack_size(WORKER_STACK_SIZE)
                .spawn_scoped(scope, move || loop {
                    let i = next.fetch_add(1, Ordering::Relaxed);
                    if i >= targets.len() {
                        break;
                    }
                    let target = &targets[i];
                    match enumerate_package_dirs(&target.pkg_name, &target.install_dir) {
                        Ok((tasks, files)) => {
                            disk_files.fetch_add(files, Ordering::Relaxed);
                            all.lock().unwrap().extend(tasks);
                        }
                        Err(e) => {
                            let mut slot = first_err.lock().unwrap();
                            if slot.is_none() {
                                *slot = Some(e);
                            }
                        }
                    }
                })
                .expect("spawn cache worker");
        }
    });

    if let Some(e) = first_err.into_inner().unwrap() {
        return Err(ScanError::Io(e));
    }
    Ok((
        all.into_inner().unwrap(),
        disk_files.load(Ordering::Relaxed),
    ))
}

/// buildAppScanPlan creates a scan plan: targets first (with their indexed
/// file counts), then per-directory tasks. `idx` is optional — passes nil
/// from `AppReferencedHashes`, where the index count is unknown.
///
/// `plan` is built field-by-field (not via struct-update syntax) because later
/// fields are only filled once `targets` is known to be non-empty.
#[allow(clippy::field_reassign_with_default)]
pub fn build_app_scan_plan(apps_dir: &Path, idx: Option<&Index>) -> Result<AppScanPlan> {
    let mut plan = AppScanPlan::default();
    plan.targets = list_app_scan_targets(apps_dir)?;
    if plan.targets.is_empty() {
        return Ok(plan);
    }
    let pkg_names: Vec<String> = plan.targets.iter().map(|t| t.pkg_name.clone()).collect();
    // Nil Index → 0 (nil receiver check in IndexedFileCountForPackages).
    let sqlite_files = match idx {
        Some(idx) => idx.indexed_file_count_for_packages(&pkg_names)?,
        None => 0,
    };
    let (tasks, disk_files) = enumerate_app_scan_tasks_parallel(&plan.targets)?;
    plan.tasks = tasks;
    plan.file_total = sqlite_files.max(disk_files as i64);
    if plan.file_total <= 0 {
        plan.file_total = plan.targets.len() as i64;
    }
    plan.dir_total = plan.tasks.len() as i64;
    if plan.dir_total == 0 {
        plan.dir_total = plan.targets.len() as i64;
    }
    Ok(plan)
}

/// scanAppInstallDirsParallel scans pre-enumerated install directories using
/// an existing store key index and returns every referenced content hash.
pub fn scan_app_install_dirs_parallel(
    plan: &AppScanPlan,
    store: &Store,
    key_index: &FileRefIndex,
) -> Result<BTreeSet<String>> {
    scan_app_install_dirs_parallel_with_progress(plan, store, key_index, None)
}

/// [`scan_app_install_dirs_parallel`] reporting `(done, total)` scan
/// directories, so a slow multi-package scan can show progress.
pub fn scan_app_install_dirs_parallel_with_progress(
    plan: &AppScanPlan,
    store: &Store,
    key_index: &FileRefIndex,
    on_progress: Option<&(dyn Fn(usize, usize) + Sync)>,
) -> Result<BTreeSet<String>> {
    let refs: BTreeSet<String> = BTreeSet::new();
    if plan.targets.is_empty() || plan.tasks.is_empty() {
        return Ok(refs);
    }
    let total = plan.tasks.len();
    let workers = gc_workers(total).min(total);
    let refs_mutex: Mutex<BTreeSet<String>> = Mutex::new(refs);
    let first_err: Mutex<Option<std::io::Error>> = Mutex::new(None);
    let next = AtomicUsize::new(0);
    let done = AtomicUsize::new(0);
    let report_done = |done: &AtomicUsize| {
        if let Some(report) = on_progress {
            report(done.fetch_add(1, Ordering::Relaxed) + 1, total);
        } else {
            done.fetch_add(1, Ordering::Relaxed);
        }
    };

    std::thread::scope(|scope| {
        for _ in 0..workers {
            let (refs_mutex, first_err, next, done, report_done) =
                (&refs_mutex, &first_err, &next, &done, &report_done);
            std::thread::Builder::new()
                .stack_size(WORKER_STACK_SIZE)
                .spawn_scoped(scope, move || loop {
                    let i = next.fetch_add(1, Ordering::Relaxed);
                    if i >= plan.tasks.len() {
                        break;
                    }
                    let task = &plan.tasks[i];
                    let entries = match std::fs::read_dir(&task.scan_dir) {
                        Ok(entries) => entries,
                        Err(e) => {
                            let mut slot = first_err.lock().unwrap();
                            if slot.is_none() {
                                *slot = Some(e);
                            }
                            drop(slot);
                            report_done(done);
                            continue;
                        }
                    };
                    for entry in entries.flatten() {
                        let Ok(file_type) = entry.file_type() else {
                            continue;
                        };
                        if file_type.is_dir() {
                            continue;
                        }
                        let path = entry.path();
                        let Ok(rel) = path.strip_prefix(&task.install_dir) else {
                            continue;
                        };
                        if is_hidden_install_path(&to_slash(rel)) {
                            continue;
                        }
                        let Ok(hash) = resolve_install_file_hash(&path, store, Some(key_index))
                        else {
                            continue;
                        };
                        refs_mutex.lock().unwrap().insert(hash);
                    }
                    report_done(done);
                })
                .expect("spawn cache worker");
        }
    });

    if let Some(e) = first_err.into_inner().unwrap() {
        return Err(ScanError::Io(e));
    }
    Ok(refs_mutex.into_inner().unwrap())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::store::Store;
    use crate::testutil::TempDir;

    fn test_root(tag: &str) -> TempDir {
        TempDir::new(&format!("glue-cache-scan-{tag}"))
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
    fn hash_from_store_path_cases() {
        let store_root = PathBuf::from("/store");
        let hash64 = "a".repeat(64);

        let (got, ok) = (
            hash_from_store_path(
                &store_root,
                &store_root.join(&hash64[..2]).join(&hash64[2..]),
            ),
            true,
        );
        assert!(ok);
        assert_eq!(got.as_deref(), Some(hash64.as_str()));

        let flat = hash_from_store_path(&store_root, &store_root.join(&hash64));
        assert_eq!(flat.as_deref(), Some(hash64.as_str()));

        // Prefix directory itself.
        assert!(hash_from_store_path(&store_root, &store_root.join("ab")).is_none());
        // Temp file.
        assert!(hash_from_store_path(&store_root, &store_root.join(".tmp-foo")).is_none());
        // Outside the store root.
        assert!(hash_from_store_path(&store_root, Path::new("/elsewhere/ab/cdef")).is_none());
        // Two-char flat name is not a hash.
        assert!(hash_from_store_path(&store_root, &store_root.join("ab")).is_none());
    }

    #[test]
    fn scan_store_lists_objects_and_hardlink_keys() {
        let root = test_root("scan-store");
        let store = test_store(&root);
        let h1 = store.write(&b"one"[..]).unwrap();
        let h2 = store.write(&b"two"[..]).unwrap();
        // A flat 64-hex file is also an object (legacy layout).
        let flat = "b".repeat(64);
        write_cache_store_object(&store, &flat, b"flat");
        // Non-object noise.
        std::fs::write(store.path().join(".tmp-123"), b"junk").unwrap();

        let (key_index, objects) = scan_store(&store).unwrap();
        let hashes: BTreeSet<&str> = objects.iter().map(|o| o.hash.as_str()).collect();
        assert!(hashes.contains(h1.as_str()), "hashes = {hashes:?}");
        assert!(hashes.contains(h2.as_str()));
        assert!(hashes.contains(flat.as_str()));
        assert_eq!(
            objects.len(),
            3,
            "objects = {:?}",
            objects.iter().map(|o| &o.hash).collect::<Vec<_>>()
        );

        // Hardlink identity: the store object resolves through the key index.
        let target = root.join("apps").join("demo").join("1.0.0").join("app.exe");
        std::fs::create_dir_all(target.parent().unwrap()).unwrap();
        store.link(&h1, &target).unwrap();
        let key = file_ref_key(&target).unwrap();
        assert_eq!(key_index.get(&key).map(String::as_str), Some(h1.as_str()));

        // Re-scan picks up nothing new but stays correct.
        let (key_index2, objects2) = scan_store(&store).unwrap();
        assert_eq!(objects2.len(), 3);
        assert_eq!(key_index2.get(&key).map(String::as_str), Some(h1.as_str()));
    }

    #[test]
    fn scan_install_dir_skips_hidden_files() {
        let root = test_root("scan-hidden");
        let store = test_store(&root);
        let install_dir = root.join("install");
        std::fs::create_dir_all(&install_dir).unwrap();

        let visible_hash = store.write(&b"visible"[..]).unwrap();
        store
            .link(&visible_hash, &install_dir.join("tool.exe"))
            .unwrap();
        std::fs::write(install_dir.join(".intermediate.tar"), b"hidden").unwrap();

        let (files, _total) = scan_install_dir(&install_dir, &store, None);
        assert_eq!(files.len(), 1, "files = {files:?}");
        assert_eq!(
            files.get(&visible_hash).map(String::as_str),
            Some("tool.exe")
        );
    }

    #[test]
    fn scan_install_dir_archive_only_extract_counts_file_size() {
        let root = test_root("scan-archive");
        let store = test_store(&root);
        let install_dir = root.join("install");
        std::fs::create_dir_all(&install_dir).unwrap();
        let payload = b"extracted-app";
        std::fs::write(install_dir.join("app.exe"), payload).unwrap();

        let (files, total) = scan_install_dir(&install_dir, &store, None);
        assert_eq!(files.len(), 1);
        assert_eq!(total, payload.len() as i64);
    }

    #[test]
    fn scan_install_dir_prefers_key_index_over_rehash() {
        let root = test_root("scan-keyindex");
        let store = test_store(&root);
        let install_dir = root.join("install");
        std::fs::create_dir_all(&install_dir).unwrap();
        let hash = store.write(&b"payload-bytes"[..]).unwrap();
        store.link(&hash, &install_dir.join("bin/app.exe")).unwrap();

        let (key_index, _objects) = scan_store(&store).unwrap();
        let (files, total) = scan_install_dir(&install_dir, &store, Some(&key_index));
        assert_eq!(files.len(), 1);
        assert_eq!(files.get(&hash).map(String::as_str), Some("bin/app.exe"));
        assert_eq!(total, "payload-bytes".len() as i64);

        // Without the index the file is re-hashed to the same value.
        let (files2, total2) = scan_install_dir(&install_dir, &store, None);
        assert_eq!(files2, files);
        assert_eq!(total2, total);
    }

    #[test]
    fn list_app_scan_targets_active_and_missing() {
        let root = test_root("list-targets");
        let store = test_store(&root);
        let apps_dir = root.join("apps");

        // Missing apps dir → empty.
        assert!(list_app_scan_targets(&apps_dir).unwrap().is_empty());

        // Installed package with a file under its active version dir.
        let install_dir = apps_dir.join("tool").join("1.0.0");
        std::fs::create_dir_all(&install_dir).unwrap();
        let hash = store.write(&b"bin"[..]).unwrap();
        store.link(&hash, &install_dir.join("tool.exe")).unwrap();
        // Package with only an empty version dir is not a target.
        std::fs::create_dir_all(apps_dir.join("empty").join("9.9.9")).unwrap();

        let targets = list_app_scan_targets(&apps_dir).unwrap();
        assert_eq!(targets.len(), 1);
        assert_eq!(targets[0].pkg_name, "tool");
        assert_eq!(targets[0].install_dir, install_dir);
    }

    #[test]
    fn scan_app_install_dirs_parallel_large_package_uses_subdirs() {
        let root = test_root("scan-subdirs");
        let store = test_store(&root);
        let idx = test_index(&root);
        let apps_dir = root.join("apps");
        let large_install = apps_dir.join("freecad").join("1.0.0");
        let small_install = apps_dir.join("tool").join("1.0.0");
        std::fs::create_dir_all(&large_install).unwrap();
        std::fs::create_dir_all(&small_install).unwrap();

        let mut hashes = Vec::new();
        let mut index_files = BTreeMap::new();
        for i in 0..10u8 {
            let letter = (b'a' + i) as char;
            let sub = large_install.join("lib").join(format!("mod{letter}"));
            std::fs::create_dir_all(&sub).unwrap();
            let hash = store.write(format!("large-{letter}").as_bytes()).unwrap();
            hashes.push(hash.clone());
            index_files.insert(hash.clone(), format!("lib/mod{letter}/part.bin"));
            store.link(&hash, &sub.join("part.bin")).unwrap();
        }
        let small_hash = store.write(&b"small"[..]).unwrap();
        hashes.push(small_hash.clone());
        index_files.insert(small_hash, "tool.exe".to_string());
        store
            .link(hashes.last().unwrap(), &small_install.join("tool.exe"))
            .unwrap();
        idx.add("freecad", "1.0.0", &index_files, 11).unwrap();

        let plan = build_app_scan_plan(&apps_dir, Some(&idx)).unwrap();
        assert_eq!(plan.targets.len(), 2);
        // 1 root + 1 lib + 10 mod dirs for freecad + 1 root for tool.
        assert_eq!(plan.tasks.len(), 13, "tasks = {}", plan.tasks.len());
        let (key_index, objects) = scan_store(&store).unwrap();
        assert_eq!(objects.len(), 11);
        let refs = scan_app_install_dirs_parallel(&plan, &store, &key_index).unwrap();
        for hash in &hashes {
            assert!(refs.contains(hash), "missing hash {} in refs", &hash[..8]);
        }
    }

    #[test]
    fn scan_app_install_dirs_parallel_many_subdirs_no_deadlock() {
        let root = test_root("scan-wide");
        let store = test_store(&root);
        let idx = test_index(&root);
        let apps_dir = root.join("apps");
        let install_dir = apps_dir.join("wide").join("1.0.0");
        std::fs::create_dir_all(&install_dir).unwrap();

        const SUBDIRS: usize = 300;
        for i in 0..SUBDIRS {
            let sub = install_dir.join("branch").join(format!("dir{i:03}"));
            std::fs::create_dir_all(&sub).unwrap();
            let hash = store.write(format!("blob-{i}").as_bytes()).unwrap();
            store.link(&hash, &sub.join("file.bin")).unwrap();
        }

        let plan = build_app_scan_plan(&apps_dir, Some(&idx)).unwrap();
        let (key_index, objects) = scan_store(&store).unwrap();
        assert_eq!(objects.len(), SUBDIRS);
        let refs = scan_app_install_dirs_parallel(&plan, &store, &key_index).unwrap();
        assert_eq!(refs.len(), SUBDIRS);
    }

    #[test]
    fn scan_app_install_dirs_parallel_read_error_propagates() {
        // A task whose directory disappears after planning must surface the
        // read error (firstErr from os.ReadDir inside processDir).
        let root = test_root("scan-readerr");
        let store = test_store(&root);
        let idx = test_index(&root);
        let apps_dir = root.join("apps");
        let install_dir = apps_dir.join("ghost").join("1.0.0");
        std::fs::create_dir_all(install_dir.join("sub")).unwrap();
        std::fs::write(install_dir.join("sub/f.bin"), b"x").unwrap();

        let mut plan = build_app_scan_plan(&apps_dir, Some(&idx)).unwrap();
        assert_eq!(plan.tasks.len(), 2);
        std::fs::remove_dir_all(&install_dir).unwrap();
        // Re-point the plan at the now-missing directory (simulates a race).
        for task in &mut plan.tasks {
            task.scan_dir = install_dir.join("sub");
            task.install_dir = install_dir.join("sub");
        }
        let (key_index, _objects) = scan_store(&store).unwrap();
        assert!(scan_app_install_dirs_parallel(&plan, &store, &key_index).is_err());
    }
}
