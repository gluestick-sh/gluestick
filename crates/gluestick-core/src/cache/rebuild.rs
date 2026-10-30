//! Cache index rebuild and installed-sync
//!
//! - `rebuild`: scan `apps/` on disk (parallel, no DB lock), then rewrite
//!   `packages`/`package_files` in one transaction, then prune stale
//!   installed rows.
//! - `sync_installed_from_packages`: register `installed_packages` only when
//!   the version dir still exists on disk (preserving metadata like version
//!   locks); content cache rows are kept for fast reinstall.
//! - `prune_uninstalled_packages`: drop installed rows with no install on
//!   disk, keeping content cache rows.

use std::path::Path;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::Mutex;

use crate::apps;
use crate::cache::index::{Index, IndexError, Result};
use crate::cache::scan;
use crate::cache::workers::{gc_workers, WORKER_STACK_SIZE};
use crate::store::Store;

/// Progress callback for rebuild: package name, version, file count
/// (`RebuildPackageFunc`; nil disables callbacks).
pub type RebuildPackageFunc<'a> = &'a (dyn Fn(&str, &str, usize) + Send + Sync);

/// One package's on-disk file map ready for packages/package_files insert
/// `files` maps cache store hash → install-relative path.
#[derive(Debug, Clone)]
pub struct RebuildPackage {
    pub name: String,
    pub version: String,
    pub files: std::collections::BTreeMap<String, String>,
    pub total_size: i64,
}

/// Read-only snapshot of one `installed_packages` row, used to decide whether
/// the on-disk state already matches the index (and skip the write).
struct InstalledSnapshot {
    version: String,
    install_dir: String,
    install_time: String,
    /// Nullable in the schema (external writers may leave it empty).
    size: Option<i64>,
    metadata: Option<String>,
}

/// An `installed_packages` row that differs from the snapshot.
struct InstalledUpsert {
    name: String,
    version: String,
    install_dir: String,
    install_time: String,
    size: i64,
    metadata: String,
}

/// `apps/<pkg>/<version>` exists as a non-empty directory.
///
/// The single implementation of this check, shared with
/// `Engine::list_installed`.
pub(crate) fn package_version_installed(root_dir: &Path, pkg_name: &str, version: &str) -> bool {
    if version.is_empty() {
        return false;
    }
    let dir = apps::pkg_root(root_dir, pkg_name).join(version);
    match std::fs::read_dir(&dir) {
        Ok(mut entries) => entries.next().is_some(),
        Err(_) => false,
    }
}

/// scanPackagesForRebuild discovers active installs under appsDir and maps
/// each hardlinked file to its cache store hash via the store index. Work
/// runs in parallel (gcWorkers) so Rebuild does not hold idx.mu while walking
/// large install trees.
pub fn scan_packages_for_rebuild(
    apps_dir: &Path,
    store: &Store,
    on_package: Option<RebuildPackageFunc<'_>>,
) -> scan::Result<Vec<RebuildPackage>> {
    // In-memory (device, inode) → hash map; built once and shared across workers.
    let key_index = scan::build_store_file_ref_index(store)?;
    let targets = scan::list_app_scan_targets(apps_dir)?;
    if targets.is_empty() {
        return Ok(Vec::new());
    }

    let workers = gc_workers(targets.len());
    let out: Mutex<Vec<RebuildPackage>> = Mutex::new(Vec::new());
    let next = AtomicUsize::new(0);

    std::thread::scope(|scope| {
        for _ in 0..workers {
            let (out, next) = (&out, &next);
            let (apps_dir, key_index, store, targets) = (apps_dir, &key_index, store, &targets);
            std::thread::Builder::new()
                .stack_size(WORKER_STACK_SIZE)
                .spawn_scoped(scope, move || loop {
                    let i = next.fetch_add(1, Ordering::Relaxed);
                    if i >= targets.len() {
                        break;
                    }
                    let target = &targets[i];
                    // Re-resolve the active version
                    // skip packages with no current/ version on disk.
                    let Some((_, version)) =
                        apps::active_install_dir(&apps_dir.join(&target.pkg_name))
                    else {
                        continue;
                    };
                    let (files, total_size) =
                        scan::scan_install_dir(&target.install_dir, store, Some(key_index));
                    if files.is_empty() {
                        continue; // empty or non-cache store install dir
                    }
                    let pkg = RebuildPackage {
                        name: target.pkg_name.clone(),
                        version: version.clone(),
                        files,
                        total_size,
                    };
                    let file_count = pkg.files.len();
                    out.lock().unwrap().push(pkg);
                    if let Some(cb) = on_package {
                        cb(&target.pkg_name, &version, file_count);
                    }
                })
                .expect("spawn cache worker");
        }
    });

    Ok(out.into_inner().unwrap())
}

/// Refuse to commit an empty index while installs still hold files.
///
/// Note how rare this trigger is: `resolve_install_file_hash` falls back to
/// hashing the file when the store index has no entry for it, so even an
/// install that was never hardlinked into the CAS produces rows. Reaching
/// `scanned == 0` therefore means the scan itself failed wholesale
/// (unreadable tree, everything hidden) — which is exactly when committing
/// would destroy the index for nothing.
fn guard_empty_scan(apps_dir: &Path, scanned: usize) -> Result<()> {
    if scanned > 0 {
        return Ok(());
    }
    let populated = populated_install_count(apps_dir);
    if populated > 0 {
        return Err(IndexError::RebuildScanEmpty(populated));
    }
    Ok(())
}

/// Number of packages whose active install dir holds at least one regular
/// file besides `install.json`.
///
/// Only used by the guard above, so it short-circuits at the first payload
/// file of each package: no hashing, no full tree walk.
fn populated_install_count(apps_dir: &Path) -> usize {
    let Ok(entries) = std::fs::read_dir(apps_dir) else {
        return 0;
    };
    let mut count = 0;
    for entry in entries.flatten() {
        let Ok(file_type) = entry.file_type() else {
            continue;
        };
        if !file_type.is_dir() {
            continue;
        }
        let pkg_name = entry.file_name().to_string_lossy().into_owned();
        if pkg_name.starts_with('.') {
            continue;
        }
        let Some((install_dir, _)) = apps::active_install_dir(&apps_dir.join(&pkg_name)) else {
            continue;
        };
        if install_dir_has_payload(&install_dir) {
            count += 1;
        }
    }
    count
}

/// `install_dir` contains a regular file other than the install record.
fn install_dir_has_payload(install_dir: &Path) -> bool {
    let mut stack = vec![install_dir.to_path_buf()];
    while let Some(dir) = stack.pop() {
        let Ok(entries) = std::fs::read_dir(&dir) else {
            continue;
        };
        for entry in entries.flatten() {
            let Ok(file_type) = entry.file_type() else {
                continue;
            };
            let name = entry.file_name();
            let name = name.to_string_lossy();
            if name.starts_with('.') {
                continue; // hidden install path
            }
            if file_type.is_dir() {
                stack.push(entry.path());
                continue;
            }
            if dir == install_dir && name == apps::INSTALL_RECORD_FILE {
                continue;
            }
            return true;
        }
    }
    false
}

impl Index {
    /// Rebuild scans installed apps and rebuilds the cache index from on-disk
    /// hardlinks. Returns the number of packages scanned.
    ///
    /// **Intentional divergence from.** `Rebuild` deletes every
    /// `package_files`/`packages` row and then re-inserts what the scan found,
    /// in one transaction. If the scan resolves nothing it commits an empty
    /// table and the entire content cache is gone — does this silently. We
    /// refuse when installs still hold files: a rebuild that found nothing to
    /// insert cannot be recovering anything, so the only effect of committing
    /// is destruction.
    pub fn rebuild(
        &self,
        apps_dir: &Path,
        store: &Store,
        on_package: Option<RebuildPackageFunc<'_>>,
    ) -> Result<usize> {
        // Rejects a nil store ("cas store is required"); in Rust the
        // reference is non-null by construction.
        let scanned = scan_packages_for_rebuild(apps_dir, store, on_package)?;
        guard_empty_scan(apps_dir, scanned.len())?;

        let installed = crate::cache::index::rfc3339_now();
        let mut db = self.db.lock().unwrap();
        let tx = db.transaction()?;
        tx.execute("DELETE FROM package_files", [])?;
        tx.execute("DELETE FROM packages", [])?;
        {
            let mut file_stmt = tx.prepare(
                "INSERT INTO package_files (package_name, hash, filename) VALUES (?, ?, ?)",
            )?;
            for pkg in &scanned {
                tx.execute(
                    "INSERT OR REPLACE INTO packages (name, version, installed, size) VALUES (?, ?, ?, ?)",
                    rusqlite::params![pkg.name, pkg.version, installed, pkg.total_size],
                )?;
                for (hash, filename) in &pkg.files {
                    file_stmt.execute(rusqlite::params![pkg.name, hash, filename])?;
                }
            }
        }
        tx.commit()?;
        drop(db);

        // Prune with filepath.Dir(appsDir) — the glue root above apps/.
        let root_dir = apps_dir.parent().unwrap_or(apps_dir);
        self.prune_uninstalled_packages(root_dir)?;
        Ok(scanned.len())
    }

    /// SyncInstalledFromPackages registers installed_packages only when the
    /// version dir still exists on disk. Content cache rows in packages are
    /// kept after uninstall for fast reinstall.
    ///
    /// The on-disk state is checked *before* any write: every command opens
    /// the engine, and the old unconditional `INSERT OR REPLACE` per package
    /// dirtied the WAL (and took the write lock) on read-only invocations
    /// too. A sync with no stale rows and no changed rows issues no
    /// transaction while keeping the heal-on-open behaviour.
    pub fn sync_installed_from_packages(&self, root_dir: &Path) -> Result<()> {
        let mut db = self.db.lock().unwrap();

        let mut current: std::collections::BTreeMap<String, InstalledSnapshot> = {
            let mut stmt = db.prepare(
                "SELECT name, version, install_dir, install_time, size, metadata \
                 FROM installed_packages",
            )?;
            let rows = stmt.query_map([], |row| {
                Ok((
                    row.get::<_, String>(0)?,
                    InstalledSnapshot {
                        version: row.get(1)?,
                        install_dir: row.get(2)?,
                        install_time: row.get(3)?,
                        size: row.get(4)?,
                        metadata: row.get(5)?,
                    },
                ))
            })?;
            rows.flatten().collect()
        };

        // Pass 1: drop installed rows whose version dir vanished.
        let stale: Vec<String> = current
            .iter()
            .filter(|(name, row)| !package_version_installed(root_dir, name, &row.version))
            .map(|(name, _)| name.clone())
            .collect();
        // Pass 2 sees the post-pass-1 state (runs them sequentially), so a
        // vanished row must not lend its metadata to a newly registered one.
        for name in &stale {
            current.remove(name);
        }

        // Pass 2: register packages whose version dir exists on disk.
        let package_rows: Vec<(String, String, String, i64)> = {
            let mut stmt = db.prepare("SELECT name, version, installed, size FROM packages")?;
            let rows = stmt.query_map([], |row| {
                Ok((
                    row.get::<_, String>(0)?,
                    row.get::<_, String>(1)?,
                    row.get::<_, String>(2)?,
                    row.get::<_, i64>(3)?,
                ))
            })?;
            rows.flatten().collect()
        };
        let mut upserts: Vec<InstalledUpsert> = Vec::new();
        for (name, version, installed, size) in package_rows {
            if !package_version_installed(root_dir, &name, &version) {
                continue;
            }
            let install_dir = root_dir
                .join("apps")
                .join(&name)
                .join(&version)
                .to_string_lossy()
                .into_owned();
            let install_time = if installed.is_empty() {
                crate::cache::index::rfc3339_now()
            } else {
                installed
            };
            let metadata_json = current
                .get(&name)
                .and_then(|row| row.metadata.as_deref())
                .filter(|m| !m.is_empty() && *m != "null")
                .unwrap_or("{}")
                .to_string();
            let unchanged = current.get(&name).is_some_and(|row| {
                row.version == version
                    && row.install_dir == install_dir
                    && row.install_time == install_time
                    && row.size == Some(size)
                    && row.metadata.as_deref() == Some(metadata_json.as_str())
            });
            if unchanged {
                continue;
            }
            upserts.push(InstalledUpsert {
                name,
                version,
                install_dir,
                install_time,
                size,
                metadata: metadata_json,
            });
        }

        if stale.is_empty() && upserts.is_empty() {
            return Ok(());
        }

        let tx = db.transaction()?;
        for name in &stale {
            tx.execute(
                "DELETE FROM installed_packages WHERE name = ?",
                rusqlite::params![name],
            )?;
        }
        for row in &upserts {
            tx.execute(
                "INSERT OR REPLACE INTO installed_packages \
                 (name, version, install_dir, install_time, size, metadata) \
                 VALUES (?, ?, ?, ?, ?, ?)",
                rusqlite::params![
                    row.name,
                    row.version,
                    row.install_dir,
                    row.install_time,
                    row.size,
                    row.metadata
                ],
            )?;
        }
        tx.commit()?;
        Ok(())
    }

    /// PruneUninstalledPackages removes stale installed_packages rows for
    /// packages with no install on disk. Content cache entries in
    /// packages/package_files are kept for fast reinstall
    pub fn prune_uninstalled_packages(&self, root_dir: &Path) -> Result<usize> {
        let names: Vec<String> = {
            let db = self.db.lock().unwrap();
            let mut stmt = db.prepare("SELECT name FROM installed_packages")?;
            let rows = stmt.query_map([], |row| row.get::<_, String>(0))?;
            rows.flatten().collect()
        };
        let stale: Vec<String> = names
            .into_iter()
            .filter(|name| apps::active_install_dir(&apps::pkg_root(root_dir, name)).is_none())
            .collect();
        if stale.is_empty() {
            return Ok(0);
        }
        let mut db = self.db.lock().unwrap();
        let tx = db.transaction()?;
        for name in &stale {
            tx.execute(
                "DELETE FROM installed_packages WHERE name = ?",
                rusqlite::params![name],
            )?;
        }
        tx.commit()?;
        Ok(stale.len())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::store::Store;
    use crate::testutil::TempDir;
    use std::collections::BTreeMap;

    fn test_root(tag: &str) -> TempDir {
        TempDir::new(&format!("glue-cache-rebuild-{tag}"))
    }

    fn test_store(root: &Path) -> Store {
        let store = Store::new(root.join("store")).unwrap();
        store.prereqs().unwrap();
        store
    }

    fn test_index(root: &Path) -> Index {
        Index::new(root).unwrap()
    }

    fn install_app(
        root: &Path,
        pkg: &str,
        version: &str,
        files: &[(&str, &[u8])],
    ) -> BTreeMap<String, String> {
        let install_dir = root.join("apps").join(pkg).join(version);
        std::fs::create_dir_all(&install_dir).unwrap();
        let mut map = BTreeMap::new();
        for (rel, content) in files {
            let target = install_dir.join(rel);
            std::fs::create_dir_all(target.parent().unwrap()).unwrap();
            std::fs::write(&target, content).unwrap();
            map.insert(crate::hash::hash_bytes(content), rel.to_string());
        }
        map
    }

    #[test]
    fn rebuild_from_apps() {
        let root = test_root("rebuild");
        let store = test_store(&root);
        let apps_dir = root.join("apps");
        let install_dir = apps_dir.join("make").join("4.4.1");
        std::fs::create_dir_all(&install_dir).unwrap();

        let hash = store.write(&b"tool-binary"[..]).unwrap();
        store
            .link(&hash, &install_dir.join("bin").join("make.exe"))
            .unwrap();

        let idx = test_index(&root);
        let count = idx.rebuild(&apps_dir, &store, None).unwrap();
        assert_eq!(count, 1);

        let entry = idx.get("make").unwrap();
        assert_eq!(entry.version, "4.4.1");
        assert_eq!(entry.files.len(), 1);
        assert_eq!(
            entry.files.get(&hash).map(String::as_str),
            Some("bin/make.exe")
        );
        assert_eq!(entry.size, "tool-binary".len() as i64);
    }

    #[test]
    fn rebuild_on_package_callback_fires() {
        let root = test_root("rebuild-cb");
        let store = test_store(&root);
        let apps_dir = root.join("apps");
        install_app(&root, "alpha", "1.0.0", &[("a.exe", b"aaa")]);
        install_app(&root, "beta", "2.0.0", &[("b.exe", b"bbb")]);

        let idx = test_index(&root);
        let seen: Mutex<Vec<(String, String, usize)>> = Mutex::new(Vec::new());
        let count = idx
            .rebuild(
                &apps_dir,
                &store,
                Some(&|name, version, files| {
                    seen.lock()
                        .unwrap()
                        .push((name.to_string(), version.to_string(), files));
                }),
            )
            .unwrap();
        assert_eq!(count, 2);
        let mut seen = seen.into_inner().unwrap();
        seen.sort();
        assert_eq!(
            seen,
            vec![
                ("alpha".to_string(), "1.0.0".to_string(), 1),
                ("beta".to_string(), "2.0.0".to_string(), 1),
            ]
        );
    }

    #[test]
    fn rebuild_replaces_previous_rows() {
        let root = test_root("rebuild-replace");
        let store = test_store(&root);
        let apps_dir = root.join("apps");
        let idx = test_index(&root);

        // Pre-existing stale rows are wiped by the rebuild transaction.
        let mut stale = BTreeMap::new();
        stale.insert("deadbeef".to_string(), "stale.exe".to_string());
        idx.add("ghost", "0.1", &stale, 1).unwrap();

        install_app(&root, "real", "1.0.0", &[("r.exe", b"real")]);
        let count = idx.rebuild(&apps_dir, &store, None).unwrap();
        assert_eq!(count, 1);
        assert!(idx.get("ghost").is_none());
        assert!(idx.get("real").is_some());
    }

    #[test]
    fn rebuild_empty_apps_dir_is_noop() {
        let root = test_root("rebuild-empty");
        let store = test_store(&root);
        let idx = test_index(&root);
        let count = idx.rebuild(&root.join("apps"), &store, None).unwrap();
        assert_eq!(count, 0);
    }

    #[test]
    fn sync_installed_from_packages_preserves_version_lock() {
        let root = test_root("sync-lock");
        let idx = test_index(&root);

        let mut files = BTreeMap::new();
        files.insert("abc".to_string(), "vim.exe".to_string());
        idx.add("vim", "9.1.0000", &files, 1024).unwrap();

        let install_dir = root.join("apps").join("vim").join("9.1.0000");
        std::fs::create_dir_all(&install_dir).unwrap();
        std::fs::write(install_dir.join("vim.exe"), b"vim").unwrap();
        idx.add_installed(
            "vim",
            "9.1.0000",
            install_dir.to_string_lossy().as_ref(),
            1024,
            &serde_json::Map::new(),
        )
        .unwrap();
        idx.set_version_locked("vim", true).unwrap();

        idx.sync_installed_from_packages(&root).unwrap();

        let inst = idx.get_installed("vim").unwrap();
        assert_eq!(
            inst.metadata.get("versionLocked"),
            Some(&serde_json::Value::Bool(true))
        );
        // install_dir rebuilt from the on-disk layout.
        assert_eq!(inst.install_dir, install_dir.to_string_lossy());
        assert_eq!(inst.version, "9.1.0000");
    }

    /// SQLite's per-connection change counter: any INSERT/UPDATE/DELETE
    /// bumps it, so an unchanged sync must leave it untouched.
    fn total_changes(idx: &Index) -> i64 {
        idx.db
            .lock()
            .unwrap()
            .query_row("SELECT total_changes()", [], |row| row.get(0))
            .unwrap()
    }

    /// Read-only commands open the engine every invocation; a sync that finds
    /// the index already matching disk must not issue any write (the old code
    /// rewrote every installed row, growing the WAL each time).
    #[test]
    fn sync_installed_is_a_noop_when_state_matches() {
        let root = test_root("sync-noop");
        let idx = test_index(&root);
        install_app(&root, "pkg", "1.0.0", &[("pkg.exe", b"p")]);
        let mut files = BTreeMap::new();
        files.insert("h".to_string(), "pkg.exe".to_string());
        idx.add("pkg", "1.0.0", &files, 1).unwrap();

        // First sync registers the row (one write).
        idx.sync_installed_from_packages(&root).unwrap();
        assert_eq!(idx.list_installed().unwrap().len(), 1);

        let before = total_changes(&idx);
        idx.sync_installed_from_packages(&root).unwrap();
        assert_eq!(total_changes(&idx), before);
        assert_eq!(idx.list_installed().unwrap().len(), 1);

        // A real disk change is still detected and written.
        std::fs::remove_dir_all(root.join("apps").join("pkg")).unwrap();
        idx.sync_installed_from_packages(&root).unwrap();
        assert!(idx.list_installed().unwrap().is_empty());
        assert!(total_changes(&idx) > before);
    }

    #[test]
    fn sync_installed_registers_and_drops() {
        let root = test_root("sync-registers");
        let idx = test_index(&root);

        // On disk one; the other package has no install dir.
        install_app(&root, "present", "1.0.0", &[("p.exe", b"p")]);
        let mut files = BTreeMap::new();
        files.insert("h".to_string(), "x.exe".to_string());
        idx.add("present", "1.0.0", &files, 1).unwrap();
        idx.add("absent", "2.0.0", &files, 1).unwrap();

        idx.sync_installed_from_packages(&root).unwrap();

        let installed = idx.list_installed().unwrap();
        assert_eq!(installed.len(), 1);
        assert_eq!(installed[0].name, "present");
        assert_eq!(installed[0].version, "1.0.0");
        assert_eq!(installed[0].size, 1);
        // install_time falls back to the packages row's `installed` column.
        assert!(!installed[0].install_time.is_empty());
        // Content cache rows stay for both.
        assert!(idx.get("present").is_some());
        assert!(idx.get("absent").is_some());
    }

    #[test]
    fn sync_installed_drops_row_with_missing_version() {
        let root = test_root("sync-drop");
        let idx = test_index(&root);
        let mut files = BTreeMap::new();
        files.insert("h".to_string(), "x.exe".to_string());
        idx.add("gone", "1.0.0", &files, 1).unwrap();
        idx.add_installed(
            "gone",
            "1.0.0",
            &root.join("apps/gone/1.0.0").to_string_lossy(),
            1,
            &serde_json::Map::new(),
        )
        .unwrap();

        idx.sync_installed_from_packages(&root).unwrap();
        assert!(idx.get_installed("gone").is_none());
        assert!(idx.get("gone").is_some()); // content cache kept
    }

    #[test]
    fn prune_uninstalled_packages_removes_stale_index() {
        // installed_packages rows are pruned; content cache rows stay.
        let root = test_root("prune");
        let idx = test_index(&root);

        let mut vim_files = BTreeMap::new();
        vim_files.insert("abc".to_string(), "vim.exe".to_string());
        idx.add("vim", "9.1.0000", &vim_files, 1024).unwrap();
        let mut git_files = BTreeMap::new();
        git_files.insert("def".to_string(), "git.exe".to_string());
        idx.add("git", "2.0.0", &git_files, 2048).unwrap();

        // git has an install on disk, vim does not; neither has an
        // installed_packages row, so nothing is stale.
        let install_dir = root.join("apps").join("git").join("2.0.0");
        std::fs::create_dir_all(&install_dir).unwrap();
        std::fs::write(install_dir.join("git.exe"), b"git").unwrap();

        let removed = idx.prune_uninstalled_packages(&root).unwrap();
        assert_eq!(removed, 0);
        assert!(idx.get("vim").is_some());
        assert!(idx.get("git").is_some());

        // Now with an installed row for a package whose install vanished.
        idx.add_installed(
            "vim",
            "9.1.0000",
            &root.join("apps/vim/9.1.0000").to_string_lossy(),
            1024,
            &serde_json::Map::new(),
        )
        .unwrap();
        idx.add_installed(
            "git",
            "2.0.0",
            &install_dir.to_string_lossy(),
            2048,
            &serde_json::Map::new(),
        )
        .unwrap();
        let removed = idx.prune_uninstalled_packages(&root).unwrap();
        assert_eq!(removed, 1);
        assert!(idx.get_installed("vim").is_none());
        assert!(idx.get_installed("git").is_some());
        assert!(idx.get("vim").is_some()); // content cache kept
    }

    #[test]
    fn scan_packages_for_rebuild_skips_missing_active_dir() {
        let root = test_root("scan-packages");
        let store = test_store(&root);
        // Package dir exists but only an empty version dir → no target.
        std::fs::create_dir_all(root.join("apps/empty/9.9.9")).unwrap();
        let pkgs = scan_packages_for_rebuild(&root.join("apps"), &store, None).unwrap();
        assert!(pkgs.is_empty());
    }

    #[test]
    fn package_version_installed_rules() {
        let root = test_root("pvi");
        std::fs::create_dir_all(root.join("apps/pkg/1.0.0")).unwrap();
        assert!(!package_version_installed(&root, "pkg", "1.0.0")); // empty dir
        std::fs::write(root.join("apps/pkg/1.0.0/f"), b"x").unwrap();
        assert!(package_version_installed(&root, "pkg", "1.0.0"));
        assert!(!package_version_installed(&root, "pkg", "")); // empty version
        assert!(!package_version_installed(&root, "pkg", "2.0.0")); // missing dir
    }

    /// The empty-scan guard: committing an empty index while installs still
    /// hold files destroys the content cache for nothing (does it).
    #[test]
    fn guard_empty_scan_rejects_populated_installs() {
        let root = test_root("guard-populated");
        let apps_dir = root.join("apps");
        std::fs::create_dir_all(apps_dir.join("git/2.0.0")).unwrap();
        std::fs::write(apps_dir.join("git/2.0.0/git.exe"), b"git").unwrap();
        std::fs::create_dir_all(apps_dir.join("vim/9.1")).unwrap();
        std::fs::write(apps_dir.join("vim/9.1/vim.exe"), b"vim").unwrap();

        let err = guard_empty_scan(&apps_dir, 0).expect_err("must refuse");
        match err {
            IndexError::RebuildScanEmpty(n) => assert_eq!(n, 2),
            other => panic!("unexpected error: {other}"),
        }
        // The message tells the user how to force the wipe if it is intended.
        assert!(err.to_string().contains("delete the cache index file"));
    }

    #[test]
    fn guard_empty_scan_allows_empty_root() {
        let root = test_root("guard-empty");
        assert!(guard_empty_scan(&root.join("apps"), 0).is_ok());
    }

    /// Only `install.json` is not payload: the scan indexes that file too, so
    /// a metadata-only dir never reaches the guard anyway.
    #[test]
    fn guard_empty_scan_ignores_metadata_only_installs() {
        let root = test_root("guard-meta");
        let apps_dir = root.join("apps");
        let dir = apps_dir.join("hollow/1.0.0");
        std::fs::create_dir_all(&dir).unwrap();
        std::fs::write(dir.join(apps::INSTALL_RECORD_FILE), b"{}").unwrap();
        assert!(guard_empty_scan(&apps_dir, 0).is_ok());
    }

    /// Hidden paths are not payload.
    #[test]
    fn guard_empty_scan_ignores_hidden_files() {
        let root = test_root("guard-hidden");
        let apps_dir = root.join("apps");
        let dir = apps_dir.join("hidden/1.0.0");
        std::fs::create_dir_all(dir.join(".git")).unwrap();
        std::fs::write(dir.join(".git/config"), b"x").unwrap();
        assert!(guard_empty_scan(&apps_dir, 0).is_ok());
    }

    /// A non-empty scan is never blocked, however the tree looks.
    #[test]
    fn guard_empty_scan_allows_non_empty_scan() {
        let root = test_root("guard-scan");
        let apps_dir = root.join("apps");
        std::fs::create_dir_all(apps_dir.join("git/2.0.0")).unwrap();
        std::fs::write(apps_dir.join("git/2.0.0/git.exe"), b"git").unwrap();
        assert!(guard_empty_scan(&apps_dir, 1).is_ok());
    }
}
