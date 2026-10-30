//! SQLite index at `<root>/cache/index.db` — the mapping from packages to
//! store hashes and install metadata.
//!
//! Tables:
//!   - `packages` / `package_files`: content-store blob index per package
//!     (kept after uninstall for fast reinstall)
//!   - `installed_packages`: currently installed packages with paths/metadata
//!   - `install_history`: install/uninstall audit rows (cascade on package
//!     delete)
//!   - `activity_log`: global activity feed with no foreign keys
//!   - `catalog_entries` / `catalog_buckets`: cached bucket manifests for the
//!     search index (see [`super::catalog_cache`]); never touched by `cache
//!     clear`/`rebuild`/`gc`
//!
//! On-disk scanning companions live in sibling modules: `rebuild`
//! (Rebuild / SyncInstalledFromPackages / prune), `scan` (store + app
//! scans), `refs` (referenced hashes / orphan GC).

use std::collections::BTreeMap;
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex};

use rusqlite::{params, Connection};
use serde::{Deserialize, Serialize};
use thiserror::Error;

use super::catalog_cache::CatalogCache;

#[derive(Debug, Error)]
pub enum IndexError {
    #[error("failed to create cache dir: {0}")]
    CreateDir(#[source] std::io::Error),
    #[error("failed to open database {path}: {source}")]
    Open {
        path: PathBuf,
        #[source]
        source: rusqlite::Error,
    },
    #[error("failed to initialize schema: {0}")]
    Schema(#[source] rusqlite::Error),
    #[error(transparent)]
    Sql(#[from] rusqlite::Error),
    #[error("no rows updated for {0}")]
    NoRows(String),
    #[error("package {0} is not installed")]
    NotInstalled(String),
    #[error("migrate from JSON: {0}")]
    Migrate(String),
    #[error(transparent)]
    Scan(#[from] crate::cache::scan::ScanError),
    /// The scan resolved no cache-store blobs although installs still hold
    /// files, so committing would silently wipe the whole content cache.
    #[error(
        "rebuild scanned 0 packages but {0} installed package(s) still hold files; refusing to \
         wipe the cache index (if those installs are really gone, delete the cache index file \
         and rebuild again)"
    )]
    RebuildScanEmpty(usize),
}

pub type Result<T> = std::result::Result<T, IndexError>;

/// A single package in the cache index.
#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
pub struct PackageEntry {
    pub version: String,
    /// ISO 8601 (RFC 3339) timestamp.
    pub installed: String,
    /// hash → filename.
    #[serde(default)]
    pub files: BTreeMap<String, String>,
    /// Total size in bytes.
    pub size: i64,
}

/// A package in the `installed_packages` table.
#[derive(Debug, Clone, Default, PartialEq)]
pub struct InstalledPackage {
    pub name: String,
    pub version: String,
    pub install_dir: String,
    /// RFC 3339 timestamp string (empty when unparseable in the source row).
    pub install_time: String,
    pub size: i64,
    pub metadata: serde_json::Map<String, serde_json::Value>,
}

/// One row of install_history / activity_log as a JSON-shaped map (mirrors
/// `map[string]any` records).
pub type LogRecord = serde_json::Map<String, serde_json::Value>;

/// SQLite-backed index. Interior `Mutex`.RWMutex` guard
/// around the single connection (caps the pool at one connection too).
///
/// `Arc<Mutex<Connection>>` rather than a bare `Mutex` so the catalog cache
/// ([`super::catalog_cache::CatalogCache`]) can share the same connection
/// instead of opening a second one — sqlite serialises writers anyway, and a
/// second handle would only add lock contention.
#[derive(Clone)]
pub struct Index {
    pub(crate) db: Arc<Mutex<Connection>>,
    path: PathBuf,
}

/// An open failure carrying the database it happened on.
///
/// The SQLite message alone ("unable to open database file") does not say
/// *which* file, and the desktop can open several data roots in one session.
fn open_error(path: &Path, source: rusqlite::Error) -> IndexError {
    IndexError::Open {
        path: path.to_path_buf(),
        source,
    }
}

impl Index {
    /// Open or create `<root>/cache/index.db`, migrating a legacy
    /// `cache-index.json` when present.
    pub fn new(root_dir: &Path) -> Result<Index> {
        let cache_dir = root_dir.join("cache");
        std::fs::create_dir_all(&cache_dir).map_err(IndexError::CreateDir)?;
        let db_path = cache_dir.join("index.db");

        let mut db = Connection::open(&db_path).map_err(|source| open_error(&db_path, source))?;
        // Connection-scoped pragmas. applies all three through the DSN
        // (`?_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)&...`); these
        // two are per-connection state, so they must be re-applied on every
        // open no matter what the file says.
        db.pragma_update(None, "foreign_keys", "ON")
            .and_then(|_| db.pragma_update(None, "busy_timeout", 30000u32))
            .map_err(|source| open_error(&db_path, source))?;
        // `journal_mode` is the odd one out: it is *persistent*, stored in the
        // database header. Re-asserting it on every open therefore asks sqlite
        // to switch to a mode it is already in — a pragma round trip on every
        // process start (measured ~90 us here) that can additionally block for
        // up to `busy_timeout` behind another glue process holding the write
        // lock, all to change nothing. Read the mode back and only switch when
        // it genuinely differs (a fresh file reports "delete").
        let mode: String = db
            .pragma_query_value(None, "journal_mode", |row| row.get(0))
            .map_err(|source| open_error(&db_path, source))?;
        if !mode.eq_ignore_ascii_case("wal") {
            db.pragma_update(None, "journal_mode", "WAL")
                .map_err(|source| open_error(&db_path, source))?;
        }

        init_schema(&db).map_err(IndexError::Schema)?;

        // Migrate legacy JSON cache index when present.
        let json_path = root_dir.join("cache-index.json");
        if json_path.is_file() {
            match migrate_from_json(&mut db, &json_path) {
                Ok(()) => {
                    let _ = std::fs::rename(&json_path, json_path.with_extension("json.bak"));
                }
                Err(_e) => {
                    // Logs a warning and continues; same here.
                }
            }
        }

        Ok(Index {
            db: Arc::new(Mutex::new(db)),
            path: db_path,
        })
    }

    /// Path of the underlying database file.
    pub fn db_path(&self) -> &Path {
        &self.path
    }

    /// A [`CatalogCache`] sharing this connection (persistent catalog rows).
    pub(crate) fn catalog_cache(&self) -> CatalogCache {
        CatalogCache::new(Arc::clone(&self.db))
    }

    /// Record a package's files in the index (install timestamp = now).
    pub fn add(
        &self,
        pkg_name: &str,
        version: &str,
        files: &BTreeMap<String, String>,
        total_size: i64,
    ) -> Result<()> {
        let installed = rfc3339_now();
        self.add_no_save(pkg_name, version, files, total_size, &installed)
    }

    /// Insert a packages row and replace package_files without touching
    /// installed_packages.
    fn add_no_save(
        &self,
        pkg_name: &str,
        version: &str,
        files: &BTreeMap<String, String>,
        total_size: i64,
        installed: &str,
    ) -> Result<()> {
        let mut db = self.db.lock().unwrap();
        let tx = db.transaction()?;
        tx.execute(
            "INSERT OR REPLACE INTO packages (name, version, installed, size) VALUES (?, ?, ?, ?)",
            params![pkg_name, version, installed, total_size],
        )?;
        tx.execute(
            "DELETE FROM package_files WHERE package_name = ?",
            params![pkg_name],
        )?;
        {
            let mut stmt = tx.prepare(
                "INSERT INTO package_files (package_name, hash, filename) VALUES (?, ?, ?)",
            )?;
            for (hash, filename) in files {
                stmt.execute(params![pkg_name, hash, filename])?;
            }
        }
        tx.commit()?;
        Ok(())
    }

    /// Drop content-cache rows (packages + package_files) and
    /// installed_packages for `pkg_name`. install_history rows cascade;
    /// activity_log rows are kept.
    pub fn remove(&self, pkg_name: &str) -> Result<()> {
        let mut db = self.db.lock().unwrap();
        let tx = db.transaction()?;
        tx.execute("DELETE FROM packages WHERE name = ?", params![pkg_name])?;
        tx.execute(
            "DELETE FROM installed_packages WHERE name = ?",
            params![pkg_name],
        )?;
        tx.commit()?;
        Ok(())
    }

    /// Update the indexed version without changing file entries (reset).
    pub fn set_active_version(&self, pkg_name: &str, version: &str) -> Result<()> {
        self.set_active_version_install_dir(pkg_name, version, "")
    }

    /// Also update installed_packages when switching active version.
    pub fn set_active_version_install_dir(
        &self,
        pkg_name: &str,
        version: &str,
        install_dir: &str,
    ) -> Result<()> {
        let mut db = self.db.lock().unwrap();
        let tx = db.transaction()?;
        let n = tx.execute(
            "UPDATE packages SET version = ? WHERE name = ?",
            params![version, pkg_name],
        )?;
        if n == 0 {
            return Err(IndexError::NoRows(pkg_name.to_string()));
        }
        if !install_dir.is_empty() {
            tx.execute(
                "UPDATE installed_packages SET version = ?, install_dir = ? WHERE name = ?",
                params![version, install_dir, pkg_name],
            )?;
        } else {
            tx.execute(
                "UPDATE installed_packages SET version = ? WHERE name = ?",
                params![version, pkg_name],
            )?;
        }
        tx.commit()?;
        Ok(())
    }

    /// A package's entry, including file mappings.
    pub fn get(&self, pkg_name: &str) -> Option<PackageEntry> {
        let db = self.db.lock().unwrap();
        let mut entry: PackageEntry = db
            .query_row(
                "SELECT version, installed, size FROM packages WHERE name = ?",
                params![pkg_name],
                |row| {
                    Ok(PackageEntry {
                        version: row.get(0)?,
                        installed: row.get(1)?,
                        size: row.get(2)?,
                        files: BTreeMap::new(),
                    })
                },
            )
            .ok()?;
        let mut stmt = db
            .prepare("SELECT hash, filename FROM package_files WHERE package_name = ?")
            .ok()?;
        let rows = stmt
            .query_map(params![pkg_name], |row| {
                Ok((row.get::<_, String>(0)?, row.get::<_, String>(1)?))
            })
            .ok()?;
        for row in rows.flatten() {
            entry.files.insert(row.0, row.1);
        }
        Some(entry)
    }

    /// Package metadata without loading package_files.
    pub fn list_packages(&self) -> BTreeMap<String, PackageEntry> {
        let db = self.db.lock().unwrap();
        list_packages_locked(&db)
    }

    /// All packages including every file mapping (two queries).
    pub fn list(&self) -> BTreeMap<String, PackageEntry> {
        let db = self.db.lock().unwrap();
        let mut result = list_packages_locked(&db);
        if result.is_empty() {
            return result;
        }
        if let Ok(mut stmt) = db.prepare("SELECT package_name, hash, filename FROM package_files") {
            if let Ok(rows) = stmt.query_map([], |row| {
                Ok((
                    row.get::<_, String>(0)?,
                    row.get::<_, String>(1)?,
                    row.get::<_, String>(2)?,
                ))
            }) {
                for row in rows.flatten() {
                    if let Some(entry) = result.get_mut(&row.0) {
                        entry.files.insert(row.1, row.2);
                    }
                }
            }
        }
        result
    }

    /// Number of indexed files for one package (the `glue info` "indexed
    /// files" value). `None` when the database query fails.
    pub fn package_file_count(&self, pkg_name: &str) -> Option<i64> {
        let db = self.db.lock().unwrap();
        db.query_row(
            "SELECT COUNT(*) FROM package_files WHERE package_name = ?",
            params![pkg_name],
            |row| row.get(0),
        )
        .ok()
    }

    /// package_name → file row count from package_files.
    pub fn package_file_counts(&self) -> Result<BTreeMap<String, i64>> {
        let db = self.db.lock().unwrap();
        let mut stmt =
            db.prepare("SELECT package_name, COUNT(*) FROM package_files GROUP BY package_name")?;
        let rows = stmt.query_map([], |row| {
            Ok((row.get::<_, String>(0)?, row.get::<_, i64>(1)?))
        })?;
        let mut counts = BTreeMap::new();
        for row in rows {
            let (name, count) = row?;
            counts.insert(name, count);
        }
        Ok(counts)
    }

    /// Package name → version only (bulk update checks).
    pub fn list_package_versions(&self) -> Result<BTreeMap<String, String>> {
        let db = self.db.lock().unwrap();
        let mut stmt = db.prepare("SELECT name, version FROM packages")?;
        let rows = stmt.query_map([], |row| {
            Ok((row.get::<_, String>(0)?, row.get::<_, String>(1)?))
        })?;
        let mut out = BTreeMap::new();
        for row in rows {
            let (name, version) = row?;
            out.insert(name, version);
        }
        Ok(out)
    }

    /// Number of rows in packages.
    pub fn count_packages(&self) -> Result<i64> {
        let db = self.db.lock().unwrap();
        Ok(db.query_row("SELECT COUNT(*) FROM packages", [], |row| row.get(0))?)
    }

    /// Sum of packages.size across all indexed packages.
    pub fn sum_installed_size(&self) -> Result<i64> {
        let db = self.db.lock().unwrap();
        Ok(
            db.query_row("SELECT COALESCE(SUM(size), 0) FROM packages", [], |row| {
                row.get(0)
            })?,
        )
    }

    /// All hashes for a package.
    pub fn get_files_for_package(&self, pkg_name: &str) -> Vec<String> {
        let db = self.db.lock().unwrap();
        let Ok(mut stmt) = db.prepare("SELECT hash FROM package_files WHERE package_name = ?")
        else {
            return Vec::new();
        };
        let Ok(rows) = stmt.query_map(params![pkg_name], |row| row.get::<_, String>(0)) else {
            return Vec::new();
        };
        rows.flatten().collect()
    }

    /// package_files row counts for the given package names.
    pub fn indexed_file_count_for_packages(&self, pkg_names: &[String]) -> Result<i64> {
        if pkg_names.is_empty() {
            return Ok(0);
        }
        let db = self.db.lock().unwrap();
        let query = format!(
            "SELECT COUNT(*) FROM package_files WHERE package_name IN ({})",
            sql_placeholders(pkg_names.len())
        );
        let mut stmt = db.prepare(&query)?;
        let count = stmt.query_row(rusqlite::params_from_iter(pkg_names.iter()), |row| {
            row.get(0)
        })?;
        Ok(count)
    }

    /// Other indexed packages that reference any of `hashes` (excluding
    /// `exclude`), sorted by name.
    pub fn packages_sharing_hashes(&self, hashes: &[String], exclude: &str) -> Vec<String> {
        let unique: std::collections::BTreeSet<&str> = hashes
            .iter()
            .map(|s| s.as_str())
            .filter(|h| !h.is_empty())
            .collect();
        if unique.is_empty() {
            return Vec::new();
        }

        let all: Vec<&str> = unique.into_iter().collect();
        let mut shared: std::collections::BTreeSet<String> = std::collections::BTreeSet::new();
        let db = self.db.lock().unwrap();
        // Chunked to stay well below SQLite's bound-parameter limit.
        for chunk in all.chunks(900) {
            let placeholders = std::iter::repeat_n("?", chunk.len())
                .collect::<Vec<_>>()
                .join(",");
            let sql = format!(
                "SELECT DISTINCT package_name FROM package_files \
                 WHERE package_name != ? AND hash IN ({placeholders})"
            );
            let Ok(mut stmt) = db.prepare(&sql) else {
                continue;
            };
            let mut values: Vec<&str> = Vec::with_capacity(chunk.len() + 1);
            values.push(exclude);
            values.extend(chunk.iter().copied());
            let mapped = stmt.query_map(rusqlite::params_from_iter(values), |row| {
                row.get::<_, String>(0)
            });
            if let Ok(rows) = mapped {
                for pkg in rows.flatten() {
                    shared.insert(pkg);
                }
            }
        }
        shared.into_iter().collect()
    }

    /// Record an active install and append a success row to install_history.
    pub fn add_installed(
        &self,
        pkg_name: &str,
        version: &str,
        install_dir: &str,
        size: i64,
        metadata: &serde_json::Map<String, serde_json::Value>,
    ) -> Result<()> {
        let install_time = rfc3339_now();
        let metadata_json = serde_json::to_string(metadata).unwrap_or_else(|_| "{}".to_string());
        let mut db = self.db.lock().unwrap();
        let tx = db.transaction()?;
        tx.execute(
            "INSERT OR REPLACE INTO installed_packages (name, version, install_dir, install_time, size, metadata) VALUES (?, ?, ?, ?, ?, ?)",
            params![pkg_name, version, install_dir, install_time, size, metadata_json],
        )?;
        tx.execute(
            "INSERT INTO install_history (operation, package_name, version, status, timestamp, details) VALUES (?, ?, ?, ?, ?, ?)",
            params![
                "install",
                pkg_name,
                version,
                "success",
                install_time,
                format!("{{\"install_dir\":\"{install_dir}\"}}")
            ],
        )?;
        tx.commit()?;
        Ok(())
    }

    /// Store version-lock state in installed_packages.metadata.
    pub fn set_version_locked(&self, pkg_name: &str, locked: bool) -> Result<()> {
        let db = self.db.lock().unwrap();
        let metadata_json: String = db
            .query_row(
                "SELECT metadata FROM installed_packages WHERE name = ?",
                params![pkg_name],
                |row| row.get(0),
            )
            .map_err(|e| match e {
                rusqlite::Error::QueryReturnedNoRows => {
                    IndexError::NotInstalled(pkg_name.to_string())
                }
                other => IndexError::Sql(other),
            })?;
        let mut metadata = parse_installed_metadata_json(&metadata_json);
        if locked {
            metadata.insert("versionLocked".to_string(), serde_json::Value::Bool(true));
        } else {
            metadata.remove("versionLocked");
        }
        let encoded = serde_json::to_string(&metadata).unwrap_or_else(|_| "{}".to_string());
        db.execute(
            "UPDATE installed_packages SET metadata = ? WHERE name = ?",
            params![encoded, pkg_name],
        )?;
        Ok(())
    }

    /// An installed package.
    pub fn get_installed(&self, pkg_name: &str) -> Option<InstalledPackage> {
        let db = self.db.lock().unwrap();
        db.query_row(
            "SELECT version, install_dir, install_time, size, metadata FROM installed_packages WHERE name = ?",
            params![pkg_name],
            |row| {
                let metadata_json: String = row.get(4)?;
                Ok(InstalledPackage {
                    name: pkg_name.to_string(),
                    version: row.get(0)?,
                    install_dir: row.get(1)?,
                    install_time: row.get(2)?,
                    size: row.get(3)?,
                    metadata: parse_installed_metadata_json(&metadata_json),
                })
            },
        )
        .ok()
    }

    /// All installed packages, newest first.
    pub fn list_installed(&self) -> Result<Vec<InstalledPackage>> {
        let db = self.db.lock().unwrap();
        let mut stmt = db.prepare(
            "SELECT name, version, install_dir, install_time, size, metadata FROM installed_packages ORDER BY install_time DESC",
        )?;
        let rows = stmt.query_map([], |row| {
            let metadata_json: String = row.get(5)?;
            Ok(InstalledPackage {
                name: row.get(0)?,
                version: row.get(1)?,
                install_dir: row.get(2)?,
                install_time: row.get(3)?,
                size: row.get(4)?,
                metadata: parse_installed_metadata_json(&metadata_json),
            })
        })?;
        let mut out = Vec::new();
        for row in rows.flatten() {
            out.push(row);
        }
        Ok(out)
    }

    /// Remove installed_packages and log uninstall in install_history (or
    /// activity_log when the content-cache row is already gone). Content
    /// cache (packages / package_files) is kept for fast reinstall.
    pub fn remove_installed(&self, pkg_name: &str) -> Result<()> {
        let mut db = self.db.lock().unwrap();
        let tx = db.transaction()?;
        let version: String = tx
            .query_row(
                "SELECT version FROM installed_packages WHERE name = ?",
                params![pkg_name],
                |row| row.get(0),
            )
            .unwrap_or_default();
        let ts = rfc3339_now();
        let pkg_exists: i64 = tx.query_row(
            "SELECT COUNT(*) FROM packages WHERE name = ?",
            params![pkg_name],
            |row| row.get(0),
        )?;
        if pkg_exists > 0 {
            tx.execute(
                "INSERT INTO install_history (operation, package_name, version, status, timestamp, details) VALUES (?, ?, ?, ?, ?, ?)",
                params!["uninstall", pkg_name, version, "success", ts, "{}"],
            )?;
        } else {
            // installed_packages has no FK to packages; log to activity_log
            // when the content-cache row is gone.
            tx.execute(
                "INSERT INTO activity_log (operation, package_name, version, status, timestamp, details) VALUES (?, ?, ?, ?, ?, ?)",
                params!["uninstall", pkg_name, version, "success", ts, "{}"],
            )?;
        }
        tx.execute(
            "DELETE FROM installed_packages WHERE name = ?",
            params![pkg_name],
        )?;
        tx.commit()?;
        Ok(())
    }

    /// Append one activity log row (install/uninstall/update, ...). Unlike
    /// install_history, activity_log has no FK and is not cascade-deleted.
    pub fn record_activity(
        &self,
        operation: &str,
        pkg_name: &str,
        version: &str,
        status: &str,
        details: Option<&serde_json::Map<String, serde_json::Value>>,
    ) -> Result<()> {
        let details_json = details
            .map(|d| serde_json::to_string(d).unwrap_or_else(|_| "{}".to_string()))
            .unwrap_or_else(|| "{}".to_string());
        let db = self.db.lock().unwrap();
        db.execute(
            "INSERT INTO activity_log (operation, package_name, version, status, timestamp, details) VALUES (?, ?, ?, ?, ?, ?)",
            params![operation, pkg_name, version, status, rfc3339_now(), details_json],
        )?;
        Ok(())
    }

    /// activity_log rows (newest first), optionally filtered by package.
    pub fn get_activity_log(&self, pkg_name: &str, limit: i64) -> Result<Vec<LogRecord>> {
        let db = self.db.lock().unwrap();
        query_named_history_table(&db, "activity_log", pkg_name, limit)
    }

    /// install_history rows (newest first), optionally filtered by package.
    pub fn query_install_history(&self, pkg_name: &str, limit: i64) -> Result<Vec<LogRecord>> {
        let db = self.db.lock().unwrap();
        query_named_history_table(&db, "install_history", pkg_name, limit)
    }

    /// Number of activity log rows, optionally since a timestamp (RFC 3339).
    pub fn count_activity_log(&self, since: &str) -> Result<i64> {
        let db = self.db.lock().unwrap();
        if since.is_empty() {
            Ok(db.query_row("SELECT COUNT(*) FROM activity_log", [], |row| row.get(0))?)
        } else {
            Ok(db.query_row(
                "SELECT COUNT(*) FROM activity_log WHERE timestamp >= ?",
                params![since],
                |row| row.get(0),
            )?)
        }
    }

    /// Activity log rows with optional time filter, limit, and offset
    /// (includes row id).
    pub fn query_activity_log(
        &self,
        since: &str,
        limit: i64,
        offset: i64,
    ) -> Result<Vec<LogRecord>> {
        let db = self.db.lock().unwrap();
        let mut query =
            "SELECT id, operation, package_name, version, status, timestamp, details FROM activity_log"
                .to_string();
        let mut args: Vec<Box<dyn rusqlite::ToSql>> = Vec::new();
        if !since.is_empty() {
            query.push_str(" WHERE timestamp >= ?");
            args.push(Box::new(since.to_string()));
        }
        query.push_str(" ORDER BY timestamp DESC");
        if limit > 0 {
            query.push_str(" LIMIT ?");
            args.push(Box::new(limit));
        }
        if offset > 0 {
            query.push_str(" OFFSET ?");
            args.push(Box::new(offset));
        }
        let mut stmt = db.prepare(&query)?;
        let params: Vec<&dyn rusqlite::ToSql> = args.iter().map(|b| b.as_ref()).collect();
        let rows = stmt.query_map(params.as_slice(), |row| {
            let id: i64 = row.get(0)?;
            let operation: String = row.get(1)?;
            let pkg_name: String = row.get(2)?;
            let version: String = row.get(3)?;
            let status: String = row.get(4)?;
            let timestamp: String = row.get(5)?;
            let details: String = row.get(6)?;
            let mut record = parse_history_log_record(
                &operation, &pkg_name, &version, &status, &timestamp, &details,
            );
            record.insert("id".to_string(), serde_json::Value::from(id));
            Ok(record)
        })?;
        let mut out = Vec::new();
        for row in rows {
            out.push(row?);
        }
        Ok(out)
    }

    /// Remove all activity log rows.
    pub fn clear_activity_log(&self) -> Result<i64> {
        self.clear_activity_log_since("")
    }

    /// Remove activity log rows with timestamp >= `since` (RFC 3339). Empty
    /// `since` deletes all rows. Returns the number of rows removed.
    pub fn clear_activity_log_since(&self, since: &str) -> Result<i64> {
        let db = self.db.lock().unwrap();
        if since.is_empty() {
            Ok(db.execute("DELETE FROM activity_log", [])? as i64)
        } else {
            Ok(db.execute(
                "DELETE FROM activity_log WHERE timestamp >= ?",
                params![since],
            )? as i64)
        }
    }

    /// Remove a single activity log row by id.
    pub fn delete_activity_log_by_id(&self, id: i64) -> Result<()> {
        let db = self.db.lock().unwrap();
        db.execute("DELETE FROM activity_log WHERE id = ?", params![id])?;
        Ok(())
    }
}

fn init_schema(db: &Connection) -> rusqlite::Result<()> {
    db.execute_batch(
        "
        CREATE TABLE IF NOT EXISTS packages (
            name TEXT PRIMARY KEY,
            version TEXT NOT NULL,
            installed TEXT NOT NULL,
            size INTEGER NOT NULL
        );

        CREATE TABLE IF NOT EXISTS package_files (
            package_name TEXT NOT NULL,
            hash TEXT NOT NULL,
            filename TEXT NOT NULL,
            PRIMARY KEY (package_name, hash),
            FOREIGN KEY (package_name) REFERENCES packages(name) ON DELETE CASCADE
        );

        CREATE TABLE IF NOT EXISTS installed_packages (
            name TEXT PRIMARY KEY,
            version TEXT NOT NULL,
            install_dir TEXT NOT NULL,
            install_time TEXT NOT NULL,
            size INTEGER,
            metadata JSON,
            CHECK (name NOT LIKE '%/%')  -- Prevent directory-like names
        );

        CREATE TABLE IF NOT EXISTS install_history (
            id INTEGER PRIMARY KEY AUTOINCREMENT,
            operation TEXT NOT NULL,
            package_name TEXT NOT NULL,
            version TEXT NOT NULL,
            status TEXT NOT NULL,
            timestamp TEXT NOT NULL,
            details JSON,
            FOREIGN KEY (package_name) REFERENCES packages(name) ON DELETE CASCADE
        );

        -- Standalone activity log (no FK; rows survive package uninstall / cascade deletes)
        CREATE TABLE IF NOT EXISTS activity_log (
            id INTEGER PRIMARY KEY AUTOINCREMENT,
            operation TEXT NOT NULL,
            package_name TEXT NOT NULL,
            version TEXT NOT NULL,
            status TEXT NOT NULL,
            timestamp TEXT NOT NULL,
            details TEXT
        );

        -- Parsed bucket manifests for the search index. Purely a cache: losing
        -- these rows only costs a re-parse. No FK to `packages` on purpose, so
        -- `cache clear`/`rebuild`/`gc` cannot invalidate them.
        CREATE TABLE IF NOT EXISTS catalog_entries (
            bucket TEXT NOT NULL,
            name TEXT NOT NULL,
            description TEXT NOT NULL,
            version TEXT NOT NULL,
            homepage TEXT NOT NULL,
            archived INTEGER NOT NULL,
            deprecated INTEGER NOT NULL,
            PRIMARY KEY (bucket, name)
        );

        -- Fingerprint of the manifest tree each bucket's rows were built from.
        CREATE TABLE IF NOT EXISTS catalog_buckets (
            bucket TEXT PRIMARY KEY,
            stamp TEXT NOT NULL,
            updated TEXT NOT NULL
        );

        CREATE INDEX IF NOT EXISTS idx_packages_installed ON packages(installed);
        CREATE INDEX IF NOT EXISTS idx_package_files_hash ON package_files(hash);
        CREATE INDEX IF NOT EXISTS idx_install_history_operation ON install_history(operation);
        CREATE INDEX IF NOT EXISTS idx_install_history_package ON install_history(package_name);
        CREATE INDEX IF NOT EXISTS idx_activity_log_timestamp ON activity_log(timestamp);
        ",
    )
}

/// Legacy `cache-index.json` shape.
#[derive(Deserialize)]
struct JsonIndex {
    #[serde(default)]
    packages: BTreeMap<String, PackageEntry>,
}

fn migrate_from_json(db: &mut Connection, json_path: &Path) -> Result<()> {
    let data = std::fs::read(json_path).map_err(|e| IndexError::Migrate(e.to_string()))?;
    let json_index: JsonIndex =
        serde_json::from_slice(&data).map_err(|e| IndexError::Migrate(e.to_string()))?;
    let tx = db.transaction()?;
    for (name, entry) in &json_index.packages {
        tx.execute(
            "INSERT OR REPLACE INTO packages (name, version, installed, size) VALUES (?, ?, ?, ?)",
            params![name, entry.version, entry.installed, entry.size],
        )?;
        tx.execute(
            "DELETE FROM package_files WHERE package_name = ?",
            params![name],
        )?;
        let mut stmt = tx
            .prepare("INSERT INTO package_files (package_name, hash, filename) VALUES (?, ?, ?)")?;
        for (hash, filename) in &entry.files {
            stmt.execute(params![name, hash, filename])?;
        }
    }
    tx.commit()?;
    Ok(())
}

fn list_packages_locked(db: &Connection) -> BTreeMap<String, PackageEntry> {
    let mut result = BTreeMap::new();
    let Ok(mut stmt) = db.prepare("SELECT name, version, installed, size FROM packages") else {
        return result;
    };
    let Ok(rows) = stmt.query_map([], |row| {
        Ok((
            row.get::<_, String>(0)?,
            PackageEntry {
                version: row.get(1)?,
                installed: row.get(2)?,
                size: row.get(3)?,
                files: BTreeMap::new(),
            },
        ))
    }) else {
        return result;
    };
    for row in rows.flatten() {
        result.insert(row.0, row.1);
    }
    result
}

/// "?,?,?" for SQL IN (...) clauses.
fn sql_placeholders(n: usize) -> String {
    if n == 0 {
        return String::new();
    }
    let mut s = String::from("?");
    for _ in 1..n {
        s.push_str(",?");
    }
    s
}

/// Decode installed_packages.metadata; empty map on error or "null".
fn parse_installed_metadata_json(
    metadata_json: &str,
) -> serde_json::Map<String, serde_json::Value> {
    if metadata_json.is_empty() || metadata_json == "null" {
        return serde_json::Map::new();
    }
    match serde_json::from_str::<serde_json::Map<String, serde_json::Value>>(metadata_json) {
        Ok(m) => m,
        Err(_) => serde_json::Map::new(),
    }
}

/// Parse a history log row into a JSON-shaped record.
fn parse_history_log_record(
    operation: &str,
    pkg_name: &str,
    version: &str,
    status: &str,
    timestamp: &str,
    details: &str,
) -> LogRecord {
    let mut record = serde_json::Map::new();
    record.insert("operation".to_string(), operation.into());
    record.insert("package_name".to_string(), pkg_name.into());
    record.insert("version".to_string(), version.into());
    record.insert("status".to_string(), status.into());
    record.insert("timestamp".to_string(), timestamp.into());
    let details_value = if details.is_empty() {
        serde_json::Value::Object(serde_json::Map::new())
    } else {
        serde_json::from_str(details).unwrap_or(serde_json::Value::Object(serde_json::Map::new()))
    };
    record.insert("details".to_string(), details_value);
    record
}

fn query_named_history_table(
    db: &Connection,
    table: &str,
    pkg_name: &str,
    limit: i64,
) -> Result<Vec<LogRecord>> {
    let from_clause = match table {
        "activity_log" => "activity_log",
        "install_history" => "install_history",
        other => return Err(IndexError::Migrate(format!("unknown log table: {other}"))),
    };
    let mut query = format!(
        "SELECT operation, package_name, version, status, timestamp, details FROM {from_clause}"
    );
    let mut args: Vec<Box<dyn rusqlite::ToSql>> = Vec::new();
    if !pkg_name.is_empty() {
        query.push_str(" WHERE package_name = ?");
        args.push(Box::new(pkg_name.to_string()));
    }
    query.push_str(" ORDER BY timestamp DESC");
    if limit > 0 {
        query.push_str(" LIMIT ?");
        args.push(Box::new(limit));
    }
    let mut stmt = db.prepare(&query)?;
    let params: Vec<&dyn rusqlite::ToSql> = args.iter().map(|b| b.as_ref()).collect();
    let rows = stmt.query_map(params.as_slice(), |row| {
        Ok(parse_history_log_record(
            &row.get::<_, String>(0)?,
            &row.get::<_, String>(1)?,
            &row.get::<_, String>(2)?,
            &row.get::<_, String>(3)?,
            &row.get::<_, String>(4)?,
            &row.get::<_, String>(5)?,
        ))
    })?;
    let mut out = Vec::new();
    for row in rows {
        out.push(row?);
    }
    Ok(out)
}

// ---------------------------------------------------------------------------
// RFC 3339 local-time timestamps
// ---------------------------------------------------------------------------

/// Current local time formatted as RFC 3339 with numeric offset, e.g.
/// `2026-10-15T23:33:13+08:00`. writes local time; mixing UTC `Z` rows
/// with -written local rows would break lexical ORDER BY, so we match.
#[cfg(windows)]
pub fn rfc3339_now() -> String {
    use windows_sys::Win32::Foundation::{FILETIME, SYSTEMTIME};
    use windows_sys::Win32::System::SystemInformation::{GetLocalTime, GetSystemTime};
    use windows_sys::Win32::System::Time::SystemTimeToFileTime;

    unsafe {
        let mut local: SYSTEMTIME = std::mem::zeroed();
        let mut utc: SYSTEMTIME = std::mem::zeroed();
        GetLocalTime(&mut local);
        GetSystemTime(&mut utc);
        let mut local_ft: FILETIME = std::mem::zeroed();
        let mut utc_ft: FILETIME = std::mem::zeroed();
        SystemTimeToFileTime(&local, &mut local_ft);
        SystemTimeToFileTime(&utc, &mut utc_ft);
        let local_100ns = ((local_ft.dwHighDateTime as i64) << 32) | local_ft.dwLowDateTime as i64;
        let utc_100ns = ((utc_ft.dwHighDateTime as i64) << 32) | utc_ft.dwLowDateTime as i64;
        let offset_minutes = (local_100ns - utc_100ns) / (60 * 10_000_000);
        let sign = if offset_minutes < 0 { '-' } else { '+' };
        let abs = offset_minutes.abs();
        format!(
            "{:04}-{:02}-{:02}T{:02}:{:02}:{:02}{}{:02}:{:02}",
            local.wYear,
            local.wMonth,
            local.wDay,
            local.wHour,
            local.wMinute,
            local.wSecond,
            sign,
            abs / 60,
            abs % 60
        )
    }
}

/// Non-Windows fallback: UTC `Z` (the project targets Windows; tests run
/// there). Civil-from-days conversion of the Unix epoch.
#[cfg(not(windows))]
pub fn rfc3339_now() -> String {
    let secs = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap_or_default()
        .as_secs() as i64;
    let days = secs.div_euclid(86_400);
    let tod = secs.rem_euclid(86_400);
    let (h, m, s) = (tod / 3600, (tod % 3600) / 60, tod % 60);
    // Howard Hinnant's civil_from_days.
    let z = days + 719_468;
    let era = z.div_euclid(146_097);
    let doe = z.rem_euclid(146_097);
    let yoe = (doe - doe / 1460 + doe / 36524 - doe / 146_096) / 365;
    let y = yoe + era * 400;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
    let mp = (5 * doy + 2) / 153;
    let d = doy - (153 * mp + 2) / 5 + 1;
    let month = if mp < 10 { mp + 3 } else { mp - 9 };
    let year = if month <= 2 { y + 1 } else { y };
    format!("{year:04}-{month:02}-{d:02}T{h:02}:{m:02}:{s:02}Z")
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::testutil::TempDir;

    fn temp_index(tag: &str) -> (Index, TempDir) {
        let dir = TempDir::new(&format!("glue-cache-test-{tag}"));
        (Index::new(dir.path()).unwrap(), dir)
    }

    fn files(pairs: &[(&str, &str)]) -> BTreeMap<String, String> {
        pairs
            .iter()
            .map(|(h, f)| (h.to_string(), f.to_string()))
            .collect()
    }

    #[test]
    fn rfc3339_format() {
        let ts = rfc3339_now();
        // YYYY-MM-DDTHH:MM:SS±HH:MM (or Z on non-windows)
        assert!(ts.len() >= 25 - 1, "ts = {ts}");
        assert_eq!(&ts[4..5], "-");
        assert_eq!(&ts[10..11], "T");
        assert!(ts.contains('+') || ts.contains('Z') || ts[19..].contains('-'));
    }

    fn journal_mode_of(idx: &Index) -> String {
        idx.db
            .lock()
            .unwrap()
            .pragma_query_value(None, "journal_mode", |row| row.get(0))
            .unwrap()
    }

    /// `journal_mode` is the one *persistent* pragma, so `Index::new` reads it
    /// back instead of writing it every time. Both branches matter: a fresh
    /// file must end up in WAL, and reopening an existing WAL file must keep
    /// the mode and the data without rewriting the header.
    #[test]
    fn journal_mode_is_wal_and_survives_reopen() {
        let dir = TempDir::new("glue-cache-test-wal");
        {
            let idx = Index::new(dir.path()).unwrap();
            assert_eq!(journal_mode_of(&idx).to_lowercase(), "wal");
            idx.add("git", "2.45.0", &files(&[("h1", "bin/git.exe")]), 10)
                .unwrap();
        }
        let idx = Index::new(dir.path()).unwrap();
        assert_eq!(journal_mode_of(&idx).to_lowercase(), "wal");
        assert_eq!(idx.get("git").unwrap().version, "2.45.0");
    }

    /// A database left in a non-WAL mode (older build, `sqlite3` CLI, a
    /// restored file) must be switched back on open.
    #[test]
    fn non_wal_database_is_switched_back_to_wal() {
        let dir = TempDir::new("glue-cache-test-wal-switch");
        {
            let idx = Index::new(dir.path()).unwrap();
            idx.db
                .lock()
                .unwrap()
                .pragma_update(None, "journal_mode", "DELETE")
                .unwrap();
            assert_eq!(journal_mode_of(&idx).to_lowercase(), "delete");
        }
        let idx = Index::new(dir.path()).unwrap();
        assert_eq!(journal_mode_of(&idx).to_lowercase(), "wal");
    }

    #[test]
    fn add_get_remove_package() {
        let (idx, _dir) = temp_index("crud");
        idx.add(
            "git",
            "2.45.0",
            &files(&[("h1", "bin/git.exe"), ("h2", "README.md")]),
            4096,
        )
        .unwrap();

        let entry = idx.get("git").unwrap();
        assert_eq!(entry.version, "2.45.0");
        assert_eq!(entry.size, 4096);
        assert_eq!(entry.files.get("h1").unwrap(), "bin/git.exe");
        assert!(!entry.installed.is_empty());

        // Re-add replaces files.
        idx.add("git", "2.46.0", &files(&[("h3", "bin/git.exe")]), 5000)
            .unwrap();
        let entry = idx.get("git").unwrap();
        assert_eq!(entry.version, "2.46.0");
        assert_eq!(entry.files.len(), 1);
        assert!(entry.files.contains_key("h3"));

        assert!(idx.get("missing").is_none());
        assert_eq!(idx.count_packages().unwrap(), 1);
        assert_eq!(idx.sum_installed_size().unwrap(), 5000);
        assert_eq!(idx.get_files_for_package("git"), vec!["h3"]);

        idx.remove("git").unwrap();
        assert!(idx.get("git").is_none());
    }

    #[test]
    fn list_variants() {
        let (idx, _dir) = temp_index("list");
        idx.add("a", "1.0", &files(&[("ha", "a.exe")]), 10).unwrap();
        idx.add("b", "2.0", &files(&[("hb1", "b1"), ("hb2", "b2")]), 20)
            .unwrap();

        let pkgs = idx.list_packages();
        assert_eq!(pkgs.len(), 2);
        assert!(pkgs["a"].files.is_empty()); // metadata only

        let all = idx.list();
        assert_eq!(all["b"].files.len(), 2);

        let versions = idx.list_package_versions().unwrap();
        assert_eq!(versions["a"], "1.0");

        let counts = idx.package_file_counts().unwrap();
        assert_eq!(counts["b"], 2);

        assert_eq!(
            idx.indexed_file_count_for_packages(&["a".to_string(), "b".to_string()])
                .unwrap(),
            3
        );
        assert_eq!(idx.indexed_file_count_for_packages(&[]).unwrap(), 0);
    }

    #[test]
    fn packages_sharing_hashes_sorted() {
        let (idx, _dir) = temp_index("sharing");
        idx.add("zeta", "1", &files(&[("shared", "z")]), 1).unwrap();
        idx.add("alpha", "1", &files(&[("shared", "a")]), 1)
            .unwrap();
        idx.add("lonely", "1", &files(&[("unique", "l")]), 1)
            .unwrap();

        let sharing = idx.packages_sharing_hashes(&["shared".to_string()], "alpha");
        assert_eq!(sharing, vec!["zeta"]);
        let sharing = idx.packages_sharing_hashes(&["shared".to_string()], "");
        assert_eq!(sharing, vec!["alpha", "zeta"]);
        assert!(idx.packages_sharing_hashes(&[], "").is_empty());
        assert!(idx
            .packages_sharing_hashes(&["nope".to_string()], "")
            .is_empty());
    }

    #[test]
    fn set_active_version_requires_row() {
        let (idx, _dir) = temp_index("active");
        idx.add("git", "1.0", &files(&[]), 0).unwrap();
        idx.set_active_version("git", "1.1").unwrap();
        assert_eq!(idx.get("git").unwrap().version, "1.1");
        assert!(matches!(
            idx.set_active_version("ghost", "9"),
            Err(IndexError::NoRows(_))
        ));
    }

    /// `list_packages` is the metadata-only bulk read behind `glue list`; it
    /// must return exactly what `get` returns minus the file map, so dropping
    /// the `package_files` query can never change a listed package.
    #[test]
    fn list_packages_matches_get_without_files() {
        let (idx, _dir) = temp_index("list-packages-meta");
        idx.add(
            "git",
            "2.45.0",
            &files(&[("h1", "bin/git.exe"), ("h2", "README.md")]),
            4096,
        )
        .unwrap();
        idx.add("empty", "1.0", &files(&[]), 7).unwrap();

        let bulk = idx.list_packages();
        for name in ["git", "empty"] {
            let meta = bulk.get(name).expect("bulk row");
            let full = idx.get(name).expect("full row");
            assert_eq!(meta.version, full.version);
            assert_eq!(meta.installed, full.installed);
            assert_eq!(meta.size, full.size);
            // The bulk read never pays for file detail.
            assert!(meta.files.is_empty());
        }
        assert_eq!(idx.get("git").unwrap().files.len(), 2);
        assert_eq!(idx.get("empty").unwrap().files.len(), 0);
    }

    #[test]
    fn installed_lifecycle_and_history() {
        let (idx, _dir) = temp_index("installed");
        idx.add("git", "2.45.0", &files(&[("h", "git.exe")]), 100)
            .unwrap();
        idx.add_installed(
            "git",
            "2.45.0",
            "C:/apps/git/2.45.0",
            100,
            &serde_json::Map::new(),
        )
        .unwrap();

        let inst = idx.get_installed("git").unwrap();
        assert_eq!(inst.version, "2.45.0");
        assert_eq!(inst.install_dir, "C:/apps/git/2.45.0");
        assert!(!inst.install_time.is_empty());

        assert_eq!(idx.list_installed().unwrap().len(), 1);

        // install_history got a success row (FK requires the packages row).
        let history = idx.query_install_history("git", 10).unwrap();
        assert_eq!(history.len(), 1);
        assert_eq!(history[0]["operation"], "install");
        assert_eq!(history[0]["status"], "success");

        // Version lock roundtrip via metadata.
        idx.set_version_locked("git", true).unwrap();
        assert_eq!(
            idx.get_installed("git")
                .unwrap()
                .metadata
                .get("versionLocked"),
            Some(&serde_json::Value::Bool(true))
        );
        idx.set_version_locked("git", false).unwrap();
        assert!(idx
            .get_installed("git")
            .unwrap()
            .metadata
            .get("versionLocked")
            .is_none());
        assert!(matches!(
            idx.set_version_locked("ghost", true),
            Err(IndexError::NotInstalled(_))
        ));

        // Uninstall: content cache rows stay, installed row goes, history logs.
        idx.remove_installed("git").unwrap();
        assert!(idx.get_installed("git").is_none());
        assert!(idx.get("git").is_some());
        let history = idx.query_install_history("git", 10).unwrap();
        assert_eq!(history.len(), 2);
        // Both rows share the same second; ORDER BY timestamp DESC leaves the
        // tie order undefined (same in), so assert set membership.
        let ops: Vec<&str> = history
            .iter()
            .map(|r| r["operation"].as_str().unwrap())
            .collect();
        assert!(ops.contains(&"install"), "ops = {ops:?}");
        assert!(ops.contains(&"uninstall"), "ops = {ops:?}");
        let uninstall_row = history
            .iter()
            .find(|r| r["operation"] == "uninstall")
            .unwrap();
        assert_eq!(uninstall_row["status"], "success");
    }

    #[test]
    fn remove_installed_without_cache_row_logs_activity() {
        let (idx, _dir) = temp_index("activity-uninstall");
        // installed_packages row whose packages row is already gone (e.g. after
        // a full purge + re-registration). AddInstalled/FK forbids reaching this
        // state through the public API, so insert the row directly — the
        // remove_installed branch under test is the activity_log fallback.
        {
            let db = idx.db.lock().unwrap();
            db.execute(
                "INSERT INTO installed_packages (name, version, install_dir, install_time, size, metadata) VALUES ('ghost', '1.0', 'C:/apps/ghost/1.0', '2020-01-01T00:00:00+08:00', 0, '{}')",
                [],
            )
            .unwrap();
        }
        idx.remove_installed("ghost").unwrap();
        assert!(idx.get_installed("ghost").is_none());
        let log = idx.get_activity_log("ghost", 10).unwrap();
        assert_eq!(log.len(), 1);
        assert_eq!(log[0]["operation"], "uninstall");
        // install_history must NOT get a row for an unknown package (FK).
        assert!(idx.query_install_history("ghost", 10).unwrap().is_empty());
    }

    #[test]
    fn activity_log_crud() {
        let (idx, _dir) = temp_index("activity");
        idx.record_activity("install", "a", "1.0", "success", None)
            .unwrap();
        let mut details = serde_json::Map::new();
        details.insert("reason".to_string(), "test".into());
        idx.record_activity("update", "b", "2.0", "failed", Some(&details))
            .unwrap();

        assert_eq!(idx.count_activity_log("").unwrap(), 2);
        let all = idx.get_activity_log("", 0).unwrap();
        assert_eq!(all.len(), 2);
        assert_eq!(all[0]["operation"], "update"); // newest first
        assert_eq!(all[1]["operation"], "install");
        assert_eq!(all[0]["details"]["reason"], "test");

        let filtered = idx.get_activity_log("a", 10).unwrap();
        assert_eq!(filtered.len(), 1);

        // Query with id + limit/offset.
        let page = idx.query_activity_log("", 1, 0).unwrap();
        assert_eq!(page.len(), 1);
        let id = page[0]["id"].as_i64().unwrap();
        idx.delete_activity_log_by_id(id).unwrap();
        assert_eq!(idx.count_activity_log("").unwrap(), 1);

        let removed = idx.clear_activity_log().unwrap();
        assert_eq!(removed, 1);
        assert_eq!(idx.count_activity_log("").unwrap(), 0);
    }

    #[test]
    fn legacy_json_migration() {
        let dir = TempDir::new("glue-cache-test-migrate");
        std::fs::write(
            dir.join("cache-index.json"),
            r#"{"packages":{"oldpkg":{"version":"0.9","installed":"2020-01-01T00:00:00Z","files":{"hh":"old.exe"},"size":42}}}"#,
        )
        .unwrap();
        let idx = Index::new(&dir).unwrap();
        let entry = idx.get("oldpkg").unwrap();
        assert_eq!(entry.version, "0.9");
        assert_eq!(entry.installed, "2020-01-01T00:00:00Z");
        assert_eq!(entry.files["hh"], "old.exe");
        // JSON renamed away to .bak.
        assert!(!dir.join("cache-index.json").exists());
        assert!(dir.join("cache-index.json.bak").exists());
    }

    #[test]
    fn sql_placeholders_format() {
        assert_eq!(sql_placeholders(0), "");
        assert_eq!(sql_placeholders(1), "?");
        assert_eq!(sql_placeholders(3), "?,?,?");
    }
}
