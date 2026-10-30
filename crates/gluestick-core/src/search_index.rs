//! In-memory catalog / search index over installed buckets.
//!
//! The expensive manifest scan is deferred until the first query that actually
//! needs it, so commands that never search or list the catalog (install,
//! uninstall, info, doctor, …) pay no parsing cost. Mutation takes `&mut
//! self`, queries take `&self`; the build is guarded by an interior `Mutex` so
//! it can be triggered from `&self` on first access, behind a readiness flag.

use std::collections::{BTreeMap, BTreeSet};
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};

use crate::bucket::Bucket;
use crate::manifest;

/// One package row in the catalog/search index.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct Entry {
    pub name: String,
    pub bucket: String,
    pub description: String,
    pub version: String,
    pub homepage: String,
    /// Manifest under `deprecated/` (Scoop archive).
    pub archived: bool,
    /// Manifest JSON `deprecated` field set.
    pub deprecated: bool,
}

impl Entry {
    /// Whether the entry is treated as deprecated for browse/search purposes
    /// (archived manifest or deprecated field set).
    pub fn browse_deprecated(&self) -> bool {
        self.archived || self.deprecated
    }
}

/// Persistent backing for bucket scans.
///
/// Implemented by the SQLite cache index ([`crate::cache::catalog_cache`]) so a
/// warm `search` does not have to re-parse every manifest in every bucket. The
/// trait lives here, rather than the implementation, to keep this module free
/// of SQLite dependencies (tests use an in-memory fake).
///
/// Stamps are computed by the caller (see [`bucket_fingerprint`]) so one
/// directory walk serves both the cache lookup and the scan.
pub trait EntryCache: Send + Sync {
    /// Cached entries for `bucket`, or `None` when `stamp` differs from the
    /// stamp they were stored under.
    fn load(&self, bucket: &str, stamp: &str) -> Option<Vec<Entry>>;

    /// Replace the cached entries for `bucket`, recording `stamp`.
    fn store(&self, bucket: &str, stamp: &str, entries: &[Entry]);

    /// Drop cached entries for a bucket that is no longer registered.
    fn forget(&self, bucket: &str);
}

/// A flat, sorted list of all packages across loaded buckets.
///
/// Rebuilt entirely (`rebuild`) or patched incrementally (`load_bucket` /
/// `remove_bucket`) as buckets are added, removed or updated.
///
/// The manifest scan is **lazy**: `set_roots` records the bucket roots but the
/// index is only built on the first query that needs it (`ensure_built`). This
/// keeps the type free of a background thread while still avoiding the cost for
/// commands that never touch the catalog.
///
/// An optional [`EntryCache`] makes that first build cheap across processes:
/// parsing 4,928 manifests costs ~3 s here, while re-validating them by
/// fingerprint and reading the rows back costs ~0.3 s.
#[derive(Default)]
pub struct Index {
    /// (root, name) pairs to build from, set by `set_roots` / `rebuild`.
    roots: Vec<(PathBuf, String)>,
    /// Built index data, guarded so the first query can populate it from `&self`.
    data: Mutex<IndexData>,
    /// Whether `data` reflects `roots` yet (double-checked lazy-build flag).
    built: AtomicBool,
    /// Optional persistent backing for per-bucket scans.
    cache: Option<Arc<dyn EntryCache>>,
}

impl std::fmt::Debug for Index {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Index")
            .field("roots", &self.roots)
            .field("built", &self.built.load(Ordering::Relaxed))
            .field("persistent", &self.cache.is_some())
            .finish()
    }
}

/// Lock-guarded payload of [`Index`].
#[derive(Debug, Default)]
struct IndexData {
    entries: Vec<Entry>,
    /// bucket name -> root path, for buckets currently indexed.
    loaded_buckets: BTreeMap<String, PathBuf>,
}

/// Catalog key for hidden-package lookups: lowercased `bucket/name`.
pub fn catalog_package_key(bucket: &str, name: &str) -> String {
    format!("{}/{name}", bucket.to_lowercase()).to_lowercase()
}

/// Whether a package is hidden by user/catalog policy.
pub fn is_hidden_catalog_entry(hidden: &BTreeSet<String>, bucket: &str, name: &str) -> bool {
    if hidden.is_empty() {
        return false;
    }
    hidden.contains(&catalog_package_key(bucket, name))
}

impl Index {
    /// An empty search index (roots supplied later via `set_roots`).
    pub fn new() -> Index {
        Index {
            roots: Vec::new(),
            data: Mutex::new(IndexData::default()),
            built: AtomicBool::new(false),
            cache: None,
        }
    }

    /// Attach a persistent backing store so bucket scans survive process exit.
    pub fn set_entry_cache(&mut self, cache: Arc<dyn EntryCache>) {
        self.cache = Some(cache);
    }

    /// Record the bucket roots to build from. The (potentially expensive)
    /// manifest scan is deferred until the first query, so commands that never
    /// search or list the catalog pay nothing. Call [`Index::rebuild`] for an
    /// immediate synchronous build (e.g. after a bucket change).
    pub fn set_roots(&mut self, roots: Vec<(PathBuf, String)>) {
        self.roots = roots;
    }

    /// Entries for one bucket: from the persistent cache when its fingerprint
    /// still matches, otherwise from a fresh manifest scan (which also
    /// refreshes the cache). Exactly one directory walk either way.
    ///
    /// `rebuild`/`load_bucket` go through here too, so a "forced" rescan of an
    /// unchanged bucket is free — the fingerprint *is* the freshness check.
    fn scan_bucket(&self, root: &Path, name: &str) -> Vec<Entry> {
        let files = bucket_manifest_files(root, name);
        let stamp = fingerprint_of(root, name, &files);
        if let Some(cache) = &self.cache {
            if let Some(entries) = cache.load(name, &stamp) {
                return entries;
            }
        }
        let entries = scan_entries_from_files(root, &files, name);
        if let Some(cache) = &self.cache {
            cache.store(name, &stamp, &entries);
        }
        entries
    }

    /// Build the index from `roots` on first access. Cheap and idempotent once
    /// `built` is set; uses a double-checked flag so the lock is only taken the
    /// first time.
    fn ensure_built(&self) {
        if self.built.load(Ordering::Acquire) {
            return;
        }
        let mut guard = self.data.lock().unwrap();
        if self.built.load(Ordering::Acquire) {
            return;
        }
        let mut entries = Vec::new();
        for (root, name) in &self.roots {
            entries.extend(self.scan_bucket(root, name));
        }
        entries.sort_by(|a, b| a.bucket.cmp(&b.bucket).then_with(|| a.name.cmp(&b.name)));
        let loaded_buckets = self
            .roots
            .iter()
            .map(|(root, name)| (name.clone(), root.clone()))
            .collect();
        *guard = IndexData {
            entries,
            loaded_buckets,
        };
        self.built.store(true, Ordering::Release);
    }

    /// All entries, in index order (bucket asc, then name asc).
    ///
    /// Clones because the data lives behind a lock (see [`Index`]).
    pub fn entries(&self) -> Vec<Entry> {
        self.ensure_built();
        self.data.lock().unwrap().entries.clone()
    }

    /// Number of indexed entries.
    pub fn len(&self) -> usize {
        self.ensure_built();
        self.data.lock().unwrap().entries.len()
    }

    /// Whether the index holds no entries.
    pub fn is_empty(&self) -> bool {
        self.ensure_built();
        self.data.lock().unwrap().entries.is_empty()
    }

    /// Rescan every bucket and replace the full index (immediate, synchronous).
    ///
    /// Buckets whose fingerprint is unchanged are served from the persistent
    /// cache, so this is cheap when nothing changed and exact when something
    /// did.
    pub fn rebuild(&mut self, buckets: &[Bucket]) {
        self.roots = buckets
            .iter()
            .map(|b| (b.root.clone(), b.name.clone()))
            .collect();
        let mut entries = Vec::new();
        for b in buckets {
            entries.extend(self.scan_bucket(&b.root, &b.name));
        }
        entries.sort_by(|a, b| a.bucket.cmp(&b.bucket).then_with(|| a.name.cmp(&b.name)));
        let loaded_buckets = buckets
            .iter()
            .map(|b| (b.name.clone(), b.root.clone()))
            .collect();
        let mut guard = self.data.lock().unwrap();
        guard.entries = entries;
        guard.loaded_buckets = loaded_buckets;
        drop(guard);
        self.built.store(true, Ordering::Release);
    }

    /// Drop all entries and tracking state for one bucket.
    pub fn remove_bucket(&mut self, bucket_name: &str) {
        self.ensure_built();
        let mut guard = self.data.lock().unwrap();
        guard.entries.retain(|e| e.bucket != bucket_name);
        guard.loaded_buckets.remove(bucket_name);
        drop(guard);
        if let Some(cache) = &self.cache {
            cache.forget(bucket_name);
        }
    }

    /// Roots of currently indexed buckets.
    pub fn loaded_bucket_roots(&self) -> BTreeMap<String, PathBuf> {
        self.ensure_built();
        self.data.lock().unwrap().loaded_buckets.clone()
    }

    /// Rescan one bucket and replace its entries in the index.
    pub fn load_bucket(&mut self, bucket_root: &Path, bucket_name: &str) {
        self.ensure_built();
        let entries = self.scan_bucket(bucket_root, bucket_name);
        let mut guard = self.data.lock().unwrap();
        guard.entries.retain(|e| e.bucket != bucket_name);
        guard.entries.extend(entries);
        guard
            .loaded_buckets
            .insert(bucket_name.to_string(), bucket_root.to_path_buf());
    }

    /// Whether the named bucket is currently indexed.
    pub fn has_loaded_bucket(&self, bucket_name: &str) -> bool {
        self.ensure_built();
        self.data
            .lock()
            .unwrap()
            .loaded_buckets
            .contains_key(bucket_name)
    }

    /// Packages whose name or description contains `query` (case-insensitive).
    ///
    /// `buckets` limits results to those bucket names; hidden entries are
    /// excluded. An empty query yields no matches.
    pub fn search(&self, query: &str, buckets: &[String], hidden: &BTreeSet<String>) -> Vec<Entry> {
        self.ensure_built();
        let lower_query = query.trim().to_lowercase();
        if lower_query.is_empty() {
            return Vec::new();
        }
        let bucket_filter: BTreeSet<&str> = buckets.iter().map(String::as_str).collect();
        let guard = self.data.lock().unwrap();
        guard
            .entries
            .iter()
            .filter(|entry| !is_hidden_catalog_entry(hidden, &entry.bucket, &entry.name))
            .filter(|entry| {
                bucket_filter.is_empty() || bucket_filter.contains(entry.bucket.as_str())
            })
            .filter(|entry| {
                entry.name.to_lowercase().contains(&lower_query)
                    || (!entry.description.is_empty()
                        && entry.description.to_lowercase().contains(&lower_query))
            })
            .cloned()
            .collect()
    }

    /// Catalog rows with optional bucket filter, text query and deprecated filtering.
    pub fn list_entries(
        &self,
        bucket_name: &str,
        query: &str,
        hide_deprecated: bool,
        hidden: &BTreeSet<String>,
    ) -> Vec<Entry> {
        self.ensure_built();
        let lower_query = query.trim().to_lowercase();
        let guard = self.data.lock().unwrap();
        guard
            .entries
            .iter()
            .filter(|entry| !is_hidden_catalog_entry(hidden, &entry.bucket, &entry.name))
            .filter(|entry| !(hide_deprecated && entry.browse_deprecated()))
            .filter(|entry| bucket_name.is_empty() || entry.bucket == bucket_name)
            .filter(|entry| {
                if lower_query.is_empty() {
                    return true;
                }
                entry.name.to_lowercase().contains(&lower_query)
                    || (!entry.description.is_empty()
                        && entry.description.to_lowercase().contains(&lower_query))
            })
            .cloned()
            .collect()
    }

    /// Number of visible indexed packages per bucket.
    pub fn count_by_bucket(
        &self,
        hide_deprecated: bool,
        hidden: &BTreeSet<String>,
    ) -> BTreeMap<String, usize> {
        self.ensure_built();
        let guard = self.data.lock().unwrap();
        let mut counts: BTreeMap<String, usize> = BTreeMap::new();
        for entry in &guard.entries {
            if is_hidden_catalog_entry(hidden, &entry.bucket, &entry.name) {
                continue;
            }
            if hide_deprecated && entry.browse_deprecated() {
                continue;
            }
            *counts.entry(entry.bucket.clone()).or_insert(0) += 1;
        }
        counts
    }

    /// Total number of visible indexed packages across buckets.
    pub fn count_all(&self, hide_deprecated: bool, hidden: &BTreeSet<String>) -> usize {
        self.ensure_built();
        let guard = self.data.lock().unwrap();
        guard
            .entries
            .iter()
            .filter(|entry| !is_hidden_catalog_entry(hidden, &entry.bucket, &entry.name))
            .filter(|entry| !(hide_deprecated && entry.browse_deprecated()))
            .count()
    }

    /// All index entries whose name matches `pkg_name` (case-insensitive).
    pub fn find_exact_name(&self, pkg_name: &str) -> Vec<Entry> {
        self.ensure_built();
        let guard = self.data.lock().unwrap();
        guard
            .entries
            .iter()
            .filter(|entry| entry.name.eq_ignore_ascii_case(pkg_name))
            .cloned()
            .collect()
    }

    /// Pick one index entry for a bare package name.
    ///
    /// Preference order: preferred bucket (non-deprecated)
    /// → preferred bucket (any) → main (non-deprecated) → any non-deprecated →
    /// first match.
    pub fn resolve_package(&self, name: &str, preferred_bucket: &str) -> Option<Entry> {
        let matches = self.find_exact_name(name);
        if matches.is_empty() {
            return None;
        }
        let pick = |pred: &dyn Fn(&Entry) -> bool| matches.iter().find(|e| pred(e)).cloned();
        if !preferred_bucket.is_empty() {
            if let Some(e) =
                pick(&|e: &Entry| e.bucket == preferred_bucket && !e.browse_deprecated())
            {
                return Some(e);
            }
            if let Some(e) = pick(&|e: &Entry| e.bucket == preferred_bucket) {
                return Some(e);
            }
        }
        if let Some(e) = pick(&|e: &Entry| e.bucket == "main" && !e.browse_deprecated()) {
            return Some(e);
        }
        if let Some(e) = pick(&|e: &Entry| !e.browse_deprecated()) {
            return Some(e);
        }
        Some(matches[0].clone())
    }

    /// Align the index with `buckets` without a full rebuild.
    ///
    /// Drops buckets removed locally, indexes newly added ones, and — when
    /// `refresh_existing` is set — rescans buckets that were already indexed
    /// (e.g. after a bucket update).
    pub fn sync(&mut self, buckets: &[Bucket], refresh_existing: bool) {
        self.ensure_built();
        let loaded = self.data.lock().unwrap().loaded_buckets.clone();
        let current: BTreeMap<String, PathBuf> = buckets
            .iter()
            .map(|b| (b.name.clone(), b.root.clone()))
            .collect();

        // Drop buckets that were removed locally.
        for name in loaded.keys() {
            if !current.contains_key(name) {
                self.remove_bucket(name);
            }
        }
        // Index newly added buckets right away.
        for (name, root) in &current {
            if !self.has_loaded_bucket(name) {
                self.load_bucket(root, name);
            }
        }
        // Rescan buckets that already exist (e.g. after bucket update).
        if refresh_existing {
            for (name, root) in &current {
                if self.has_loaded_bucket(name) {
                    self.load_bucket(root, name);
                }
            }
        }
    }
}

/// One manifest discovered under a bucket root, with the metadata that feeds
/// the catalog-cache fingerprint.
#[derive(Debug, Clone)]
pub struct ManifestFile {
    pub path: PathBuf,
    /// Modification time in nanoseconds since the Unix epoch (0 if unknown).
    pub mtime_nanos: u64,
    pub len: u64,
}

/// Lists package manifest JSON paths under a Scoop bucket repo.
///
/// Walks the standard Scoop layout dirs and returns one path per package name;
/// later dirs do not override an already-seen package (first wins). `.git` trees
/// are skipped.
pub fn bucket_manifest_paths(bucket_root: &Path, bucket_name: &str) -> Vec<PathBuf> {
    bucket_manifest_files(bucket_root, bucket_name)
        .into_iter()
        .map(|f| f.path)
        .collect()
}

/// Every manifest of a bucket with its stat data, sorted by path.
///
/// This is the single directory walk: [`bucket_manifest_paths`] and
/// [`bucket_fingerprint`] both derive from it, so the catalog cache adds no
/// extra filesystem traffic.
pub fn bucket_manifest_files(bucket_root: &Path, bucket_name: &str) -> Vec<ManifestFile> {
    // Scoop lookup priority: bucket/ > root > <bucket_name>/ > deprecated/.
    // The previous implementation walked `bucket/` explicitly and then walked
    // `root` recursively, re-enumerating every file under `bucket/` (~2× the
    // entries and stats). Prune only `bucket/` from the root pass: it is
    // inserted first either way, and leaving the other dirs in the recursive
    // walk preserves the original traversal order used for same-name
    // collisions between `<bucket_name>/` and `deprecated/`.
    let bucket_dir = bucket_root.join("bucket");
    let dirs = [
        bucket_dir.clone(),
        bucket_root.to_path_buf(),
        bucket_root.join(bucket_name),
        bucket_root.join("deprecated"),
    ];

    let mut seen: BTreeMap<String, ManifestFile> = BTreeMap::new();
    collect_manifest_paths(&dirs[0], &mut seen);
    walk_json_files(&dirs[1], &mut seen, &[&bucket_dir]);
    collect_manifest_paths(&dirs[2], &mut seen);
    collect_manifest_paths(&dirs[3], &mut seen);
    let mut files: Vec<ManifestFile> = seen.into_values().collect();
    files.sort_by(|a, b| a.path.cmp(&b.path));
    files
}

/// Stable fingerprint of a bucket's manifests.
///
/// Covers the root path plus, for every manifest, its package name, mtime and
/// size — so editing a file, adding or removing one all invalidate the cache,
/// while an untouched bucket keeps its rows. Deliberately *not* a content hash:
/// reading 4,928 files is exactly the cost being avoided.
pub fn bucket_fingerprint(bucket_root: &Path, bucket_name: &str) -> String {
    let files = bucket_manifest_files(bucket_root, bucket_name);
    fingerprint_of(bucket_root, bucket_name, &files)
}

/// Bump whenever entry extraction changes (fields read, files skipped, …).
///
/// The fingerprint only covers the *manifest tree*, so without this a parser
/// fix would leave every previously cached bucket stale-but-valid and the new
/// behaviour would never reach existing users.
const CATALOG_CACHE_VERSION: &str = "4";

fn fingerprint_of(bucket_root: &Path, bucket_name: &str, files: &[ManifestFile]) -> String {
    let mut h: u64 = 0xcbf2_9ce4_8422_2325; // FNV-1a 64 offset basis
    fnv1a(&mut h, CATALOG_CACHE_VERSION.as_bytes());
    let root = bucket_root.to_string_lossy();
    fnv1a(&mut h, root.as_bytes());
    fnv1a(&mut h, bucket_name.as_bytes());
    for file in files {
        // The name is part of the path, so hashing the path covers it.
        fnv1a(&mut h, file.path.to_string_lossy().as_bytes());
        fnv1a(&mut h, &file.mtime_nanos.to_le_bytes());
        fnv1a(&mut h, &file.len.to_le_bytes());
    }
    format!("{h:016x}")
}

fn fnv1a(h: &mut u64, bytes: &[u8]) {
    for b in bytes {
        *h ^= u64::from(*b);
        *h = h.wrapping_mul(0x0000_0100_0000_01b3);
    }
}

fn collect_manifest_paths(dir: &Path, seen: &mut BTreeMap<String, ManifestFile>) {
    let is_dir = std::fs::metadata(dir).map(|m| m.is_dir()).unwrap_or(false);
    if !is_dir {
        return;
    }
    walk_json_files(dir, seen, &[]);
}

/// Recursive walk that skips `.git` (.WalkDir` + `SkipDir`)
/// and does not descend into `skip_dirs` (dirs the caller walks itself).
///
/// `DirEntry::file_type` and `DirEntry::metadata` are served from the Windows
/// directory-enumeration buffer, whereas `fs::metadata` opens every entry —
/// measured 5,222 entries in ~9 ms vs ~102 ms for the old walk. Only reparse
/// points (junctions/symlinks) need a follow-up stat, which mirrors the old
/// follow-everything behaviour for the entries where the target type matters.
fn walk_json_files(dir: &Path, seen: &mut BTreeMap<String, ManifestFile>, skip_dirs: &[&Path]) {
    let entries = match std::fs::read_dir(dir) {
        Ok(e) => e,
        Err(_) => return,
    };
    for entry in entries.flatten() {
        let path = entry.path();
        let file_name = entry.file_name().to_string_lossy().into_owned();
        // Checked before the type branches so a junction named e.g. `bucket`
        // is pruned too: `file_type()` reports it as a link, not a dir.
        if skip_dirs.iter().any(|skip| path == *skip) {
            continue;
        }
        let Ok(file_type) = entry.file_type() else {
            continue;
        };
        if file_type.is_dir() {
            if file_name == ".git" {
                continue;
            }
            walk_json_files(&path, seen, skip_dirs);
            continue;
        }
        // A reparse point may resolve to a directory (junction); follow it
        // exactly as the previous unconditional `fs::metadata` did.
        let followed = if file_type.is_file() {
            None
        } else {
            match std::fs::metadata(&path) {
                Ok(meta) if meta.is_dir() => {
                    if file_name != ".git" {
                        walk_json_files(&path, seen, skip_dirs);
                    }
                    continue;
                }
                Ok(meta) => Some(meta),
                Err(_) => continue,
            }
        };
        let ext = path
            .extension()
            .map(|e| e.to_string_lossy().to_lowercase())
            .unwrap_or_default();
        if ext != "json" {
            continue;
        }
        let pkg_name = file_name
            .strip_suffix(&format!(".{ext}"))
            .unwrap_or("")
            .to_string();
        if pkg_name.is_empty() {
            continue;
        }
        let meta = match followed {
            Some(meta) => meta,
            None => match entry.metadata() {
                Ok(meta) => meta,
                Err(_) => continue,
            },
        };
        let mtime_nanos = meta
            .modified()
            .ok()
            .and_then(|t| t.duration_since(std::time::UNIX_EPOCH).ok())
            .map(|d| d.as_nanos() as u64)
            .unwrap_or(0);
        seen.entry(pkg_name).or_insert(ManifestFile {
            path,
            mtime_nanos,
            len: meta.len(),
        });
    }
}

/// Parse manifest JSON files into index rows; unreadable files are skipped.
pub fn scan_bucket_manifest_entries(bucket_root: &Path, bucket_name: &str) -> Vec<Entry> {
    let files = bucket_manifest_files(bucket_root, bucket_name);
    scan_entries_from_files(bucket_root, &files, bucket_name)
}

/// Below this many manifests a serial parse wins: thread setup and per-file
/// IO dominate, and the bucket is small enough that the wall clock is noise.
const MIN_PARALLEL_PARSE_ITEMS: usize = 128;

/// Parsing is dominated by per-file open/read latency (small manifests, with
/// Defender in the path), so two workers per core hide that latency better
/// than one; the cap keeps file-handle and stack use bounded.
const MAX_PARSE_WORKERS: usize = 8;

fn parse_workers(item_count: usize) -> usize {
    if item_count < MIN_PARALLEL_PARSE_ITEMS {
        return 1;
    }
    let cpus = std::thread::available_parallelism()
        .map(|p| p.get())
        .unwrap_or(1);
    cpus.saturating_mul(2)
        .min(MAX_PARSE_WORKERS)
        .min(item_count)
        .max(1)
}

/// Parse an already-enumerated manifest list into index rows.
///
/// Split from the walk so the catalog cache can reuse one enumeration for both
/// the fingerprint and the parse. Large buckets are parsed in parallel: a cold
/// scan of ~4,900 manifests is the 3 s first-search-after-`bucket update`
/// cost. Chunk output is concatenated then sorted by name, so the result is
/// identical to a serial scan regardless of worker count.
pub fn scan_entries_from_files(
    bucket_root: &Path,
    files: &[ManifestFile],
    bucket_name: &str,
) -> Vec<Entry> {
    let workers = parse_workers(files.len());
    let mut entries = if workers <= 1 {
        parse_entry_chunk(bucket_root, files, bucket_name)
    } else {
        let chunk_size = files.len().div_ceil(workers);
        let chunks: Vec<Vec<Entry>> = std::thread::scope(|scope| {
            let handles: Vec<_> = files
                .chunks(chunk_size)
                .map(|chunk| {
                    std::thread::Builder::new()
                        .stack_size(crate::cache::workers::WORKER_STACK_SIZE)
                        .spawn_scoped(scope, move || {
                            parse_entry_chunk(bucket_root, chunk, bucket_name)
                        })
                        .expect("spawn manifest parse worker")
                })
                .collect();
            handles
                .into_iter()
                .map(|h| h.join().expect("manifest parse worker panicked"))
                .collect()
        });
        chunks.into_iter().flatten().collect()
    };
    entries.sort_by(|a, b| a.name.cmp(&b.name));
    entries
}

fn parse_entry_chunk(bucket_root: &Path, files: &[ManifestFile], bucket_name: &str) -> Vec<Entry> {
    let mut entries = Vec::with_capacity(files.len());
    for file in files {
        let manifest_path = &file.path;
        let pkg_name = manifest_path
            .file_stem()
            .map(|s| s.to_string_lossy().into_owned())
            .unwrap_or_default();
        let mut entry = Entry {
            name: pkg_name,
            bucket: bucket_name.to_string(),
            ..Default::default()
        };

        let m = match manifest::parse_file(manifest_path) {
            Ok(m) => m,
            Err(_) => continue,
        };
        entry.description = m.description.clone();
        entry.version = m.version.clone();
        entry.homepage = m.homepage.clone();
        entry.archived = manifest::is_deprecated_manifest_path(bucket_root, manifest_path);
        entry.deprecated = m.is_deprecated_marked();
        entries.push(entry);
    }
    entries
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::bucket::Bucket;
    use crate::testutil::TempDir;

    fn write_manifest(dir: &Path, name: &str) {
        std::fs::create_dir_all(dir).unwrap();
        let body = r#"{"version":"1.0.0","description":"test","url":"https://example.com/x.zip","hash":"abc"}"#;
        std::fs::write(dir.join(format!("{name}.json")), body).unwrap();
    }

    fn bucket(name: &str, root: PathBuf) -> Bucket {
        Bucket {
            name: name.to_string(),
            repo_url: String::new(),
            root,
            updated: false,
        }
    }

    #[test]
    fn manifest_paths_skip_git_and_keep_first_seen() {
        let root = TempDir::new("glue-sidx-paths");
        let bucket_dir = root.join("main");
        write_manifest(&bucket_dir.join("bucket"), "git");
        write_manifest(&bucket_dir, "git"); // flat duplicate: must not override
        write_manifest(&bucket_dir, "flatonly");
        // A .git tree with json files must be ignored.
        write_manifest(&bucket_dir.join(".git").join("objects"), "junk");

        let paths = bucket_manifest_paths(&bucket_dir, "main");
        let names: Vec<String> = paths
            .iter()
            .map(|p| p.file_stem().unwrap().to_string_lossy().into_owned())
            .collect();
        // Sorted by full path: `<root>/main/bucket/git.json` precedes the
        // flat `<root>/main/flatonly.json` (`b` < `f`).
        assert_eq!(names, vec!["git".to_string(), "flatonly".to_string()]);
        // First-wins: the bucket/ copy is the recorded one (checked before the
        // flat duplicate in the directory walk order).
        let git_path = paths.iter().find(|p| p.ends_with("git.json")).unwrap();
        assert!(git_path.starts_with(bucket_dir.join("bucket")));
        // The .git tree is never walked.
        assert!(!paths.iter().any(|p| p.to_string_lossy().contains(".git")));
    }

    /// Every Scoop layout dir is enumerated (bucket/ is pruned from the
    /// recursive root pass) and first-wins priority is unchanged.
    #[test]
    fn manifest_files_cover_all_layout_dirs_with_bucket_priority() {
        let root = TempDir::new("glue-sidx-dirs");
        let bucket_dir = root.join("main");
        // Same name in every priority dir: the bucket/ copy must win.
        for dir in [
            bucket_dir.join("bucket"),
            bucket_dir.clone(),
            bucket_dir.join("main"),
            bucket_dir.join("deprecated"),
        ] {
            write_manifest(&dir, "prio");
        }
        write_manifest(&bucket_dir.join("bucket"), "only-bucket");
        write_manifest(&bucket_dir, "only-root");
        write_manifest(&bucket_dir.join("main"), "only-name");
        write_manifest(&bucket_dir.join("deprecated"), "only-deprecated");
        write_manifest(&bucket_dir.join("scripts"), "nested");
        std::fs::write(bucket_dir.join("bucket").join("notes.txt"), "x").unwrap();

        let files = bucket_manifest_files(&bucket_dir, "main");
        let by_name = |name: &str| -> PathBuf {
            files
                .iter()
                .find(|f| f.path.file_stem().unwrap().to_string_lossy() == name)
                .unwrap_or_else(|| panic!("{name} missing"))
                .path
                .clone()
        };
        assert_eq!(by_name("prio"), bucket_dir.join("bucket").join("prio.json"));
        assert_eq!(
            by_name("only-bucket"),
            bucket_dir.join("bucket").join("only-bucket.json")
        );
        assert_eq!(by_name("only-root"), bucket_dir.join("only-root.json"));
        assert_eq!(
            by_name("only-name"),
            bucket_dir.join("main").join("only-name.json")
        );
        assert_eq!(
            by_name("only-deprecated"),
            bucket_dir.join("deprecated").join("only-deprecated.json")
        );
        assert_eq!(
            by_name("nested"),
            bucket_dir.join("scripts").join("nested.json")
        );
        assert_eq!(files.len(), 6);
        assert!(files.iter().all(|f| f.path.extension().unwrap() == "json"));
    }

    #[test]
    fn scan_entries_reads_metadata_and_deprecation() {
        let root = TempDir::new("glue-sidx-scan");
        let bucket_dir = root.join("main");
        write_manifest(&bucket_dir.join("bucket"), "git");
        write_manifest(&bucket_dir.join("deprecated"), "old");
        // A malformed manifest is skipped, not fatal.
        std::fs::write(bucket_dir.join("bucket").join("bad.json"), "{not json").unwrap();

        let entries = scan_bucket_manifest_entries(&bucket_dir, "main");
        let names: Vec<&str> = entries.iter().map(|e| e.name.as_str()).collect();
        assert_eq!(names, vec!["git", "old"]);
        let git = entries.iter().find(|e| e.name == "git").unwrap();
        assert_eq!(git.version, "1.0.0");
        assert_eq!(git.description, "test");
        assert!(!git.browse_deprecated());
        let old = entries.iter().find(|e| e.name == "old").unwrap();
        assert!(old.archived);
        assert!(old.browse_deprecated());
    }

    /// A scan above the parallel threshold must match the serial result: every
    /// manifest exactly once, ordered by name, malformed files skipped without
    /// panicking a worker.
    #[test]
    fn scan_entries_parallel_matches_serial_order() {
        let root = TempDir::new("glue-sidx-parallel");
        let bucket_dir = root.join("main").join("bucket");
        // > MIN_PARALLEL_PARSE_ITEMS (128), so the parallel branch runs.
        for i in 0..130 {
            write_manifest(&bucket_dir, &format!("pkg{i:03}"));
        }
        std::fs::write(bucket_dir.join("broken.json"), "{not json").unwrap();

        let entries = scan_bucket_manifest_entries(&root.join("main"), "main");
        let names: Vec<&str> = entries.iter().map(|e| e.name.as_str()).collect();
        let expected: Vec<String> = (0..130).map(|i| format!("pkg{i:03}")).collect();
        let expected_refs: Vec<&str> = expected.iter().map(String::as_str).collect();
        assert_eq!(names, expected_refs);
    }

    #[test]
    fn rebuild_sorts_by_bucket_then_name() {
        let root = TempDir::new("glue-sidx-rebuild");
        write_manifest(&root.join("main").join("bucket"), "zed");
        write_manifest(&root.join("main").join("bucket"), "alpha");
        write_manifest(&root.join("extras").join("bucket"), "mid");

        let mut idx = Index::new();
        idx.rebuild(&[
            bucket("main", root.join("main")),
            bucket("extras", root.join("extras")),
        ]);
        let entries = idx.entries();
        let keyed: Vec<(&str, &str)> = entries
            .iter()
            .map(|e| (e.bucket.as_str(), e.name.as_str()))
            .collect();
        assert_eq!(
            keyed,
            vec![("extras", "mid"), ("main", "alpha"), ("main", "zed")]
        );
        assert!(idx.has_loaded_bucket("main"));
        assert_eq!(idx.count_all(false, &BTreeSet::new()), 3);
    }

    #[test]
    fn search_matches_name_and_description_and_filters_buckets() {
        let root = TempDir::new("glue-sidx-search");
        write_manifest(&root.join("main").join("bucket"), "git");
        std::fs::create_dir_all(root.join("extras").join("bucket")).unwrap();
        std::fs::write(
            root.join("extras").join("bucket").join("zotero.json"),
            r#"{"version":"6","description":"Reference manager build","url":"https://e/z.zip","hash":"h"}"#,
        )
        .unwrap();

        let mut idx = Index::new();
        idx.rebuild(&[
            bucket("main", root.join("main")),
            bucket("extras", root.join("extras")),
        ]);

        assert!(idx.search("", &[], &BTreeSet::new()).is_empty());
        assert_eq!(idx.search("git", &[], &BTreeSet::new()).len(), 1);
        // Description hit, case-insensitive.
        assert_eq!(idx.search("REFERENCE", &[], &BTreeSet::new()).len(), 1);
        // Bucket filter.
        assert_eq!(
            idx.search("e", &["extras".to_string()], &BTreeSet::new())
                .len(),
            1
        );
        assert_eq!(
            idx.search("git", &["extras".to_string()], &BTreeSet::new())
                .len(),
            0
        );
    }

    #[test]
    fn hidden_entries_are_excluded_everywhere() {
        let root = TempDir::new("glue-sidx-hidden");
        write_manifest(&root.join("main").join("deprecated"), "acorn");
        write_manifest(&root.join("main").join("bucket"), "git");

        let mut idx = Index::new();
        idx.rebuild(&[bucket("main", root.join("main"))]);

        let mut hidden = BTreeSet::new();
        hidden.insert(catalog_package_key("main", "acorn"));

        assert_eq!(idx.list_entries("main", "acorn", false, &hidden).len(), 0);
        assert_eq!(idx.count_all(false, &hidden), 1);
        assert_eq!(idx.count_by_bucket(false, &hidden).get("main"), Some(&1));
        assert_eq!(idx.search("acorn", &[], &hidden).len(), 0);
        // Without the hidden set the archived acorn shows up.
        assert_eq!(
            idx.list_entries("main", "acorn", false, &BTreeSet::new())
                .len(),
            1
        );
        // hide_deprecated drops it too.
        assert_eq!(
            idx.list_entries("main", "acorn", true, &BTreeSet::new())
                .len(),
            0
        );
    }

    #[test]
    fn resolve_package_preference_order() {
        let root = TempDir::new("glue-sidx-resolve");
        // Same name in three buckets; extras copy is deprecated (archived).
        write_manifest(&root.join("main").join("bucket"), "foo");
        write_manifest(&root.join("versions").join("bucket"), "foo");
        write_manifest(&root.join("extras").join("deprecated"), "foo");
        write_manifest(&root.join("other").join("bucket"), "foo");

        let mut idx = Index::new();
        idx.rebuild(&[
            bucket("main", root.join("main")),
            bucket("versions", root.join("versions")),
            bucket("extras", root.join("extras")),
            bucket("other", root.join("other")),
        ]);

        // Preferred bucket wins even over main.
        assert_eq!(
            idx.resolve_package("foo", "versions").unwrap().bucket,
            "versions"
        );
        // Preferred bucket that only has a deprecated copy still wins over main? No:
        // non-deprecated preferred first, then deprecated preferred, so extras wins.
        assert_eq!(
            idx.resolve_package("foo", "extras").unwrap().bucket,
            "extras"
        );
        // No preference -> main (non-deprecated).
        assert_eq!(idx.resolve_package("foo", "").unwrap().bucket, "main");
        // Unknown name -> None.
        assert!(idx.resolve_package("nope", "").is_none());
        assert_eq!(idx.find_exact_name("FOO").len(), 4);
    }

    /// In-memory stand-in for the SQLite catalog cache.
    #[derive(Default)]
    struct FakeCache {
        stored: Mutex<BTreeMap<String, (String, Vec<Entry>)>>,
        loads: Mutex<usize>,
        stores: Mutex<usize>,
        forgotten: Mutex<Vec<String>>,
    }

    impl FakeCache {
        fn put(&self, bucket: &str, stamp: &str, entries: Vec<Entry>) {
            self.stored
                .lock()
                .unwrap()
                .insert(bucket.to_string(), (stamp.to_string(), entries));
        }
    }

    impl EntryCache for FakeCache {
        fn load(&self, bucket: &str, stamp: &str) -> Option<Vec<Entry>> {
            *self.loads.lock().unwrap() += 1;
            match self.stored.lock().unwrap().get(bucket) {
                Some((s, entries)) if s == stamp => Some(entries.clone()),
                _ => None,
            }
        }

        fn store(&self, bucket: &str, stamp: &str, entries: &[Entry]) {
            *self.stores.lock().unwrap() += 1;
            self.put(bucket, stamp, entries.to_vec());
        }

        fn forget(&self, bucket: &str) {
            self.stored.lock().unwrap().remove(bucket);
            self.forgotten.lock().unwrap().push(bucket.to_string());
        }
    }

    /// A cache hit must be used *instead of* parsing: the fake returns a
    /// fabricated entry that no manifest on disk could produce, so seeing it
    /// (and not `git`) proves the scan was skipped.
    #[test]
    fn entry_cache_hit_skips_the_manifest_scan() {
        let root = TempDir::new("glue-sidx-hit");
        write_manifest(&root.join("main").join("bucket"), "git");
        let fake = Arc::new(FakeCache::default());

        let mut first = Index::new();
        first.set_entry_cache(fake.clone());
        first.rebuild(&[bucket("main", root.join("main"))]);
        assert_eq!(first.find_exact_name("git").len(), 1);
        assert_eq!(*fake.stores.lock().unwrap(), 1);

        // Same tree, fabricated rows under the same stamp.
        let stamp = bucket_fingerprint(&root.join("main"), "main");
        fake.put(
            "main",
            &stamp,
            vec![Entry {
                name: "from-cache".to_string(),
                bucket: "main".to_string(),
                ..Default::default()
            }],
        );

        let mut second = Index::new();
        second.set_entry_cache(fake.clone());
        second.rebuild(&[bucket("main", root.join("main"))]);
        assert_eq!(second.find_exact_name("from-cache").len(), 1);
        assert_eq!(second.find_exact_name("git").len(), 0);
        // The hit did not trigger another store.
        assert_eq!(*fake.stores.lock().unwrap(), 1);
    }

    /// A changed tree must miss, re-scan, and refresh the stored rows.
    #[test]
    fn entry_cache_miss_rescans_and_refreshes() {
        let root = TempDir::new("glue-sidx-miss");
        let bucket_dir = root.join("main");
        write_manifest(&bucket_dir.join("bucket"), "git");
        let fake = Arc::new(FakeCache::default());

        let mut idx = Index::new();
        idx.set_entry_cache(fake.clone());
        idx.rebuild(&[bucket("main", bucket_dir.clone())]);
        assert_eq!(*fake.stores.lock().unwrap(), 1);

        write_manifest(&bucket_dir.join("bucket"), "vim");
        let mut idx = Index::new();
        idx.set_entry_cache(fake.clone());
        idx.rebuild(&[bucket("main", bucket_dir.clone())]);
        assert_eq!(idx.find_exact_name("vim").len(), 1);
        assert_eq!(*fake.stores.lock().unwrap(), 2);
        // The stored rows now match the new tree.
        let stamp = bucket_fingerprint(&bucket_dir, "main");
        assert_eq!(fake.load("main", &stamp).unwrap().len(), 2);
    }

    #[test]
    fn remove_bucket_forgets_the_cache() {
        let root = TempDir::new("glue-sidx-forget");
        write_manifest(&root.join("main").join("bucket"), "git");
        let fake = Arc::new(FakeCache::default());

        let mut idx = Index::new();
        idx.set_entry_cache(fake.clone());
        idx.rebuild(&[bucket("main", root.join("main"))]);
        idx.remove_bucket("main");
        assert_eq!(*fake.forgotten.lock().unwrap(), vec!["main".to_string()]);
        assert_eq!(idx.find_exact_name("git").len(), 0);
    }

    /// Without a cache the behaviour is unchanged (tests above cover it, but
    /// pin the no-cache path explicitly).
    #[test]
    fn no_cache_still_scans() {
        let root = TempDir::new("glue-sidx-nocache");
        write_manifest(&root.join("main").join("bucket"), "git");
        let mut idx = Index::new();
        idx.rebuild(&[bucket("main", root.join("main"))]);
        assert_eq!(idx.find_exact_name("git").len(), 1);
    }

    #[test]
    fn sync_adds_removes_and_refreshes() {
        let root = TempDir::new("glue-sidx-sync");
        write_manifest(&root.join("main").join("bucket"), "git");

        let mut idx = Index::new();
        idx.sync(&[bucket("main", root.join("main"))], false);
        assert!(idx.has_loaded_bucket("main"));
        assert_eq!(idx.find_exact_name("git").len(), 1);

        // A new bucket appears on disk -> sync indexes it.
        write_manifest(&root.join("extras").join("bucket"), "zotero");
        idx.sync(
            &[
                bucket("main", root.join("main")),
                bucket("extras", root.join("extras")),
            ],
            false,
        );
        assert!(idx.has_loaded_bucket("extras"));
        assert_eq!(idx.find_exact_name("zotero").len(), 1);

        // main disappears -> sync drops it.
        idx.sync(&[bucket("extras", root.join("extras"))], false);
        assert!(!idx.has_loaded_bucket("main"));
        assert_eq!(idx.find_exact_name("git").len(), 0);

        // refresh_existing rescans an already-indexed bucket after an edit.
        write_manifest(&root.join("extras").join("bucket"), "newpkg");
        idx.sync(&[bucket("extras", root.join("extras"))], true);
        assert_eq!(idx.find_exact_name("newpkg").len(), 1);
    }
}
