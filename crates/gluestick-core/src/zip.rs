//! ZIP member index persistence.
//!
//! Maps zip-archive relative member paths to CAS store hashes so reinstalls
//! can hard-link members instead of re-extracting. The downloader itself only
//! stores the zip as one whole blob; the engine populates the index after
//! extracting and the downloader reads it back here to attach a link-ready
//! member map to download results.

use std::collections::BTreeMap;
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicU64, Ordering};

use serde::{Deserialize, Serialize};

use crate::store::Store;

const ZIP_MEMBER_INDEX_FORMAT_ADOPT: u32 = 3;

static TEMP_COUNTER: AtomicU64 = AtomicU64::new(0);

/// A loaded member index: relPath → CAS hash, plus the total install bytes.
pub struct ZipMemberIndex {
    pub format: u32,
    pub files: BTreeMap<String, String>,
    pub total_bytes: u64,
}

/// On-disk shape, including the deprecated `sizes` field kept for indexes
/// built before `total_bytes` existed.
#[derive(Serialize, Deserialize)]
struct ZipMemberIndexFile {
    #[serde(default, skip_serializing_if = "is_zero_u32")]
    format: u32,
    files: BTreeMap<String, String>,
    #[serde(default, skip_serializing_if = "is_zero_u64")]
    total_bytes: u64,
    #[serde(default, skip_serializing_if = "BTreeMap::is_empty")]
    sizes: BTreeMap<String, u64>,
}

fn is_zero_u32(v: &u32) -> bool {
    *v == 0
}

fn is_zero_u64(v: &u64) -> bool {
    *v == 0
}

/// `<storeRoot>/.zip-index/<zipHash>.json`
pub fn index_path(store_root: &Path, zip_hash: &str) -> PathBuf {
    store_root
        .join(".zip-index")
        .join(format!("{zip_hash}.json"))
}

/// Persist the member map atomically (temp file + rename). No-op for an empty
/// hash or an empty member set.
pub fn save(
    store_root: &Path,
    zip_hash: &str,
    files: &BTreeMap<String, String>,
    total_bytes: u64,
) -> std::io::Result<()> {
    if zip_hash.is_empty() || files.is_empty() {
        return Ok(());
    }
    let dir = store_root.join(".zip-index");
    std::fs::create_dir_all(&dir)
        .map_err(|e| std::io::Error::new(e.kind(), format!("create zip index dir: {e}")))?;
    let payload = serde_json::to_vec(&ZipMemberIndexFile {
        format: ZIP_MEMBER_INDEX_FORMAT_ADOPT,
        files: files.clone(),
        total_bytes,
        sizes: BTreeMap::new(),
    })
    .map_err(|e| std::io::Error::new(std::io::ErrorKind::InvalidData, e))?;

    let seq = TEMP_COUNTER.fetch_add(1, Ordering::Relaxed);
    let tmp_path = dir.join(format!("zip-index-{}-{}.tmp", std::process::id(), seq));
    std::fs::write(&tmp_path, &payload)?;
    let path = index_path(store_root, zip_hash);
    if let Err(e) = std::fs::rename(&tmp_path, &path) {
        let _ = std::fs::remove_file(&tmp_path);
        return Err(e);
    }
    Ok(())
}

/// Load a member index; `None` when absent, unreadable, malformed, or empty.
pub fn load(store_root: &Path, zip_hash: &str) -> Option<ZipMemberIndex> {
    if zip_hash.is_empty() {
        return None;
    }
    let data = std::fs::read(index_path(store_root, zip_hash)).ok()?;
    let idx: ZipMemberIndexFile = serde_json::from_slice(&data).ok()?;
    if idx.files.is_empty() {
        return None;
    }
    // Legacy indexes carry per-member `sizes` instead of `total_bytes`.
    let mut total = idx.total_bytes;
    if total == 0 && !idx.sizes.is_empty() {
        for size in idx.sizes.values() {
            total += size;
        }
    }
    Some(ZipMemberIndex {
        format: idx.format,
        files: idx.files,
        total_bytes: total,
    })
}

/// Delete a (stale) member index; missing files are ignored.
pub fn remove(store_root: &Path, zip_hash: &str) {
    let _ = std::fs::remove_file(index_path(store_root, zip_hash));
}

/// Whether every member blob exists in the store (link path is safe).
/// Duplicate hashes are only checked once.
pub fn ready(store: &Store, idx: &ZipMemberIndex) -> bool {
    if idx.files.is_empty() {
        return false;
    }
    let mut seen: std::collections::BTreeSet<&str> = std::collections::BTreeSet::new();
    for hash in idx.files.values() {
        if hash.is_empty() {
            return false;
        }
        if !seen.insert(hash.as_str()) {
            continue;
        }
        if !store.has(hash) {
            return false;
        }
    }
    true
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::testutil::TempDir;

    fn temp_store(tag: &str) -> (Store, TempDir) {
        let dir = TempDir::new(&format!("glue-zipidx-{tag}"));
        let store = Store::new(dir.path()).unwrap();
        (store, dir)
    }

    #[test]
    fn save_and_load_roundtrip() {
        let (store, _dir) = temp_store("save");
        let mut files = BTreeMap::new();
        files.insert("bin/tool.exe".to_string(), "a".repeat(64));
        files.insert("LICENSE".to_string(), "b".repeat(64));
        save(store.path(), "zip1", &files, 42).unwrap();

        let idx = load(store.path(), "zip1").expect("index loads");
        assert_eq!(idx.format, ZIP_MEMBER_INDEX_FORMAT_ADOPT);
        assert_eq!(idx.files, files);
        assert_eq!(idx.total_bytes, 42);
        // Format 3 is always serialized (never omitted on disk).
        let raw = std::fs::read_to_string(index_path(store.path(), "zip1")).unwrap();
        assert!(raw.contains("\"format\":3"), "raw = {raw}");
    }

    #[test]
    fn empty_hash_or_files_is_noop() {
        let (store, _dir) = temp_store("empty");
        let mut files = BTreeMap::new();
        assert!(save(store.path(), "", &files, 1).is_ok());
        files.insert("a".to_string(), "b".to_string());
        assert!(save(store.path(), "zip1", &files, 1).is_ok());
        assert!(!index_path(store.path(), "").exists());
        assert!(load(store.path(), "").is_none());
        assert!(load(store.path(), "missing").is_none());
    }

    #[test]
    fn legacy_sizes_fallback_sums_total() {
        let (store, _dir) = temp_store("legacy");
        std::fs::create_dir_all(store.path().join(".zip-index")).unwrap();
        let payload = r#"{"files":{"a":"h1","b":"h2"},"sizes":{"a":3,"b":4}}"#;
        std::fs::write(index_path(store.path(), "legacy1"), payload).unwrap();
        let idx = load(store.path(), "legacy1").expect("legacy index loads");
        assert_eq!(idx.format, 0);
        assert_eq!(idx.total_bytes, 7);
    }

    #[test]
    fn ready_tracks_member_blobs() {
        // Ready only when the adopt format loads and every blob is present.
        let (store, _dir) = temp_store("ready");
        let hash = store.write(&b"tool"[..]).unwrap();
        let mut files = BTreeMap::new();
        files.insert("bin/tool.exe".to_string(), "missinghash".to_string());
        let idx = ZipMemberIndex {
            format: ZIP_MEMBER_INDEX_FORMAT_ADOPT,
            files,
            total_bytes: 4,
        };
        assert!(!ready(&store, &idx), "missing blobs are not ready");

        let mut files = BTreeMap::new();
        files.insert("bin/tool.exe".to_string(), hash.clone());
        // Duplicate hash counts once but must exist.
        files.insert("bin/tool (copy).exe".to_string(), hash);
        save(store.path(), "zip1", &files, 4).unwrap();
        let idx = load(store.path(), "zip1").unwrap();
        assert!(ready(&store, &idx), "existing member blobs are ready");
    }

    #[test]
    fn corrupt_index_loads_as_none() {
        let (store, _dir) = temp_store("corrupt");
        std::fs::create_dir_all(store.path().join(".zip-index")).unwrap();
        std::fs::write(index_path(store.path(), "bad"), b"{not json").unwrap();
        assert!(load(store.path(), "bad").is_none());
        // Empty files map also fails.
        std::fs::write(index_path(store.path(), "empty"), br#"{"files":{}}"#).unwrap();
        assert!(load(store.path(), "empty").is_none());
    }

    #[test]
    fn remove_ignores_missing() {
        let (store, _dir) = temp_store("remove");
        remove(store.path(), "nope");
        let mut files = BTreeMap::new();
        files.insert("a".to_string(), "h".to_string());
        save(store.path(), "zip1", &files, 1).unwrap();
        assert!(index_path(store.path(), "zip1").exists());
        remove(store.path(), "zip1");
        assert!(!index_path(store.path(), "zip1").exists());
    }
}
