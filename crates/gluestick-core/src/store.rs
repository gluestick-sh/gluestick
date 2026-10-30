//! Content-addressable storage (CAS): files are keyed by their SHA-256 hash so
//! identical content is stored once and installs can hardlink blobs instead of
//! copying them.

use std::io::Read;
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicU64, AtomicU8, Ordering};
use std::sync::Arc;

use crate::hash::{hex_encode, Sha256};
use thiserror::Error;

use crate::hash;

#[derive(Debug, Error)]
pub enum StoreError {
    #[error("failed to create cache store directory: {0}")]
    CreateDir(#[source] std::io::Error),
    #[error("failed to create temp file: {0}")]
    CreateTemp(#[source] std::io::Error),
    #[error("failed to write data: {0}")]
    WriteData(#[source] std::io::Error),
    #[error("failed to move object to {0}: {1}")]
    MoveObject(String, #[source] std::io::Error),
    #[error("source object not found: {0}: {1}")]
    SourceNotFound(String, #[source] std::io::Error),
    #[error("failed to create target directory: {0}")]
    TargetDir(#[source] std::io::Error),
    #[error("failed to create hardlink: {0} (copy fallback: {1})")]
    LinkFailed(std::io::Error, std::io::Error),
    #[error("hash stream: {0}")]
    HashStream(#[source] std::io::Error),
    #[error("read member: {0}")]
    ReadMember(#[source] std::io::Error),
    /// The content hash is known but the object is not in the store; the
    /// caller must supply a fresh reader for `write`.
    #[error("store: reader retry required")]
    NeedReaderRetry,
    #[error(transparent)]
    Io(#[from] std::io::Error),
}

pub type Result<T> = std::result::Result<T, StoreError>;

/// Temp-file uniqueness across concurrent writers within this process
/// (uses os.CreateTemp with a random pattern).
static TEMP_COUNTER: AtomicU64 = AtomicU64::new(0);

/// Blobs at least this large are flushed to disk before the rename.
///
/// A power loss can otherwise persist the rename while the data is still in
/// the write cache, leaving a truncated object under a valid hash name — worse
/// than a leftover `.tmp-` file, which `cache gc` now sweeps. Small members
/// are excluded: a 2 000-file package would pay 2 000 flushes, and their bytes
/// can always be re-extracted from the still-present archive.
const SYNC_MIN_BYTES: u64 = 4 * 1024 * 1024;

/// Content-addressable storage root. Objects live at `<root>/hh/rest...` where
/// `hh` is the first two hex chars of the SHA-256 digest.
///
/// Cloning shares the same on-disk root (the handle is just a path), which is
/// how the downloader and the engine each hold a store handle. Clones also
/// share the shard-directory memo below, so a clone never re-creates a shard
/// its sibling already made.
#[derive(Clone)]
pub struct Store {
    root_dir: PathBuf,
    /// Which of the 256 shard dirs (`00`..`ff`) this root already has,
    /// memoized as one atomic byte each.
    ///
    /// Creates all 256 in `Prereqs()` once at startup and `Write`/`Adopt`
    /// then never call MkdirAll again. This implementation dropped that eager 256-syscall
    /// startup cost (see `Services::new`), so the lazy create has to be
    /// memoized here instead — otherwise every ingested file pays a
    /// `create_dir_all`. Measured in-process on this host: 800 such calls cost
    /// 351 ms (a `CreateDirectoryW` that fails with `ERROR_ALREADY_EXISTS`,
    /// then a stat), against 0.8 ms once memoized — ~440x, and a 22k-file
    /// install does it 22k times.
    ///
    /// Safe to memoize because nothing in this crate ever removes a shard dir:
    /// store GC deletes object *files*, and install/uninstall only touches
    /// `apps/` (has the same exposure via its one-shot `Prereqs`).
    shard_dirs: Arc<[AtomicU8; 256]>,
}

impl Store {
    /// Create a new store instance, ensuring the root directory exists.
    pub fn new(root_dir: impl Into<PathBuf>) -> Result<Store> {
        let root_dir = root_dir.into();
        std::fs::create_dir_all(&root_dir).map_err(StoreError::CreateDir)?;
        Ok(Store {
            root_dir,
            shard_dirs: Arc::new(std::array::from_fn(|_| AtomicU8::new(0))),
        })
    }

    /// The storage root directory.
    pub fn path(&self) -> &Path {
        &self.root_dir
    }

    /// Stream data into the store, returning its SHA-256 hash.
    pub fn write(&self, mut r: impl Read) -> Result<String> {
        // Unique temp file in the store root to avoid concurrent-write conflicts.
        let seq = TEMP_COUNTER.fetch_add(1, Ordering::Relaxed);
        let tmp_path = self
            .root_dir
            .join(format!(".tmp-{}-{}", std::process::id(), seq));

        let mut tmp = match std::fs::File::create(&tmp_path) {
            Ok(f) => f,
            Err(e) => return Err(StoreError::CreateTemp(e)),
        };
        let mut hasher = Sha256::new();
        let mut buf = [0u8; 128 * 1024];
        let mut written: u64 = 0;
        loop {
            match r.read(&mut buf) {
                Ok(0) => break,
                Ok(n) => {
                    hasher.update(&buf[..n]);
                    written += n as u64;
                    if let Err(e) = std::io::Write::write_all(&mut tmp, &buf[..n]) {
                        drop(tmp);
                        let _ = std::fs::remove_file(&tmp_path);
                        return Err(StoreError::WriteData(e));
                    }
                }
                Err(e) => {
                    drop(tmp);
                    let _ = std::fs::remove_file(&tmp_path);
                    return Err(StoreError::WriteData(e));
                }
            }
        }
        // Flush large blobs before the rename (see SYNC_MIN_BYTES).
        if written >= SYNC_MIN_BYTES {
            if let Err(e) = tmp.sync_all() {
                drop(tmp);
                let _ = std::fs::remove_file(&tmp_path);
                return Err(StoreError::WriteData(e));
            }
        }
        // Close BEFORE rename on Windows (small blobs skip the flush above; a
        // crash then leaves a `.tmp-` orphan, which `cache gc` sweeps).
        drop(tmp);

        let sum = hex_encode(&hasher.finish());
        let final_path = self.object_path(&sum);
        // Ensure the two-char prefix dir exists
        // Memoized per
        // shard — see `Store::shard_dirs`.
        self.ensure_shard_dir(&sum, &final_path)
            .map_err(StoreError::CreateDir)?;
        if let Err(e) = std::fs::rename(&tmp_path, &final_path) {
            // May already exist (identical content stored concurrently).
            if final_path.exists() {
                let _ = std::fs::remove_file(&tmp_path);
                return Ok(sum);
            }
            let _ = std::fs::remove_file(&tmp_path);
            return Err(StoreError::MoveObject(final_path.display().to_string(), e));
        }
        Ok(sum)
    }

    /// Move an already-complete file `src` into the CAS keyed by `hash`,
    /// bypassing the read-and-rewrite of [`Store::write`].
    ///
    /// Used by the resumable-partial download path: the bytes have already been
    /// written to disk once, so re-copying them into a temp file would be
    /// wasted IO. `src` lives under the store root (the `.partial` dir), so the
    /// rename is a same-volume O(1) operation; a copy fallback covers the rare
    /// cross-volume case. When the content is already stored (e.g. a concurrent
    /// download of the same blob), the duplicate `src` is simply discarded.
    pub fn ingest_move(&self, src: &Path, hash: &str) -> Result<()> {
        let final_path = self.object_path(hash);
        if final_path.exists() {
            let _ = std::fs::remove_file(src);
            return Ok(());
        }
        if let Some(dir) = final_path.parent() {
            std::fs::create_dir_all(dir).map_err(StoreError::CreateDir)?;
        }
        // Best-effort flush: on a sudden power loss the rename could otherwise
        // persist ahead of the bytes and leave a truncated object under a valid
        // hash name. Read-only handles cannot flush on Windows, hence `write`.
        if let Ok(file) = std::fs::OpenOptions::new().write(true).open(src) {
            let _ = file.sync_all();
        }
        match std::fs::rename(src, &final_path) {
            Ok(()) => Ok(()),
            Err(_) => {
                // Cross-volume (or other) rename failure: copy then delete the
                // source — a last-resort fallback, not the common path.
                std::fs::copy(src, &final_path).map_err(StoreError::WriteData)?;
                let _ = std::fs::remove_file(src);
                Ok(())
            }
        }
    }

    /// HashReader returns the SHA-256 hex digest of `r` without storing it
    ///
    /// Deliberately a 128 KiB read loop instead of `io::copy`: std's
    /// `io::copy` requests only `DEFAULT_BUF_SIZE` (8 KiB — measured on this
    /// toolchain: 2561 reads for a 20 MiB file) per `read`, and this is the
    /// hashing pass of every `.part` → CAS ingest (i.e. every file over
    /// `DIRECT_STREAM_MAX_BYTES` and every parallel-range download). Measured
    /// sequential read throughput at 8 KiB vs 128 KiB on this machine:
    /// ~447 MB/s vs ~1.7 GB/s — for a 150 MiB artifact that is ~335 ms of
    /// pure syscall waste versus ~90 ms. `hash_file` and `write` already use
    /// the same 128 KiB loop; this keeps the three hash paths consistent.
    pub fn hash_reader(&self, mut r: impl Read) -> Result<String> {
        let mut hasher = Sha256::new();
        let mut buf = [0u8; 128 * 1024];
        loop {
            match r.read(&mut buf) {
                Ok(0) => break,
                Ok(n) => hasher.update(&buf[..n]),
                Err(e) => return Err(StoreError::HashStream(e)),
            }
        }
        Ok(hex_encode(&hasher.finish()))
    }

    /// WriteMember stores `r` in the cache store. When `uncompressed_size` is
    /// known and small (≤ 2 MiB), a single read hashes and stores (or skips
    /// when `dedup_likely` and the blob already exists). For large members
    /// with `dedup_likely`, hashes first and returns `NeedReaderRetry` when
    /// storage is needed.
    pub fn write_member(
        &self,
        mut r: impl Read,
        uncompressed_size: i64,
        dedup_likely: bool,
    ) -> Result<String> {
        /// Largest zip member buffered in memory for single-pass ingest
        const MAX_SMALL_MEMBER_BYTES: i64 = 2 << 20;
        if (0..=MAX_SMALL_MEMBER_BYTES).contains(&uncompressed_size) {
            let mut buf = vec![0u8; uncompressed_size as usize];
            r.read_exact(&mut buf).map_err(StoreError::ReadMember)?;
            let hash = crate::hash::hash_bytes(&buf);
            if dedup_likely && self.has(&hash) {
                return Ok(hash);
            }
            self.put_prehashed(&hash, &buf)?;
            return Ok(hash);
        }
        if dedup_likely {
            let hash = self.hash_reader(&mut r)?;
            if self.has(&hash) {
                return Ok(hash);
            }
            return Err(StoreError::NeedReaderRetry);
        }
        self.write(r)
    }

    /// Store already-hashed bytes: skip when the object exists, otherwise
    /// write to a `.tmp-` file and rename into place.
    fn put_prehashed(&self, hash: &str, data: &[u8]) -> Result<()> {
        let final_path = self.object_path(hash);
        if final_path.exists() {
            return Ok(());
        }
        let seq = TEMP_COUNTER.fetch_add(1, Ordering::Relaxed);
        let tmp_path = self
            .root_dir
            .join(format!(".tmp-{}-{}", std::process::id(), seq));
        {
            let mut tmp = std::fs::File::create(&tmp_path).map_err(StoreError::CreateTemp)?;
            if let Err(e) = std::io::Write::write_all(&mut tmp, data) {
                drop(tmp);
                let _ = std::fs::remove_file(&tmp_path);
                return Err(StoreError::WriteData(e));
            }
            // Close BEFORE rename on Windows.
            drop(tmp);
        }
        // Ensure the two-char prefix dir exists (mirrors `write`).
        self.ensure_shard_dir(hash, &final_path)
            .map_err(StoreError::CreateDir)?;
        if let Err(e) = std::fs::rename(&tmp_path, &final_path) {
            // Identical content may have been stored concurrently.
            if final_path.exists() {
                let _ = std::fs::remove_file(&tmp_path);
                return Ok(());
            }
            let _ = std::fs::remove_file(&tmp_path);
            return Err(StoreError::MoveObject(final_path.display().to_string(), e));
        }
        Ok(())
    }

    /// Full path for a given hash: two-level directory layout
    /// (`store/ab/cdef...`) to keep directory entry counts low.
    pub fn object_path(&self, hash: &str) -> PathBuf {
        if hash.len() < 4 {
            return self.root_dir.join(hash);
        }
        let (prefix, rest) = hash.split_at(2);
        self.root_dir.join(prefix).join(rest)
    }

    /// Whether an object exists in the store.
    pub fn has(&self, hash: &str) -> bool {
        self.object_path(hash).is_file()
    }

    /// Hardlink the stored object to `target_path`, falling back to a copy
    /// when hardlinks are unavailable (cross-volume, exFAT, etc.).
    pub fn link(&self, hash: &str, target_path: &Path) -> Result<()> {
        let src_path = self.object_path(hash);
        if let Err(e) = std::fs::metadata(&src_path) {
            return Err(StoreError::SourceNotFound(hash.to_string(), e));
        }
        if let Some(dir) = target_path.parent() {
            ensure_dir_exists(dir).map_err(StoreError::TargetDir)?;
        }
        let _ = std::fs::remove_file(target_path);
        if let Err(link_err) = std::fs::hard_link(&src_path, target_path) {
            if let Err(copy_err) = materialize_copy(&src_path, target_path) {
                return Err(StoreError::LinkFailed(link_err, copy_err));
            }
        }
        Ok(())
    }

    /// Remove an object from the store. Missing objects are ignored.
    pub fn delete(&self, hash: &str) -> Result<()> {
        match std::fs::remove_file(self.object_path(hash)) {
            Ok(()) => Ok(()),
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(()),
            Err(e) => Err(StoreError::Io(e)),
        }
    }

    /// Take an existing on-disk file into the store without re-reading its
    /// bytes: hash it, then hardlink (or copy) it into place.
    ///
    /// Used when an install directory has already been materialized on disk
    /// (extracted by 7z, say) and its files need to become CAS blobs so future
    /// installs can hardlink instead of re-extracting —
    /// `Store.Adopt`.
    pub fn adopt(&self, path: &Path) -> Result<String> {
        let hash = self
            .hash_for_path(path)
            .map_err(|e| StoreError::SourceNotFound(path.display().to_string(), e))?;
        self.adopt_with_hash(path, &hash)?;
        Ok(hash)
    }

    /// Take an existing file into the store under a caller-computed hash.
    ///
    /// Same insert logic as [`Store::adopt`] without the hashing pass: used
    /// when the extractor already computed the digest while writing the bytes.
    pub fn adopt_with_hash(&self, path: &Path, hash: &str) -> Result<()> {
        let dest = self.object_path(hash);
        if dest.exists() {
            return Ok(());
        }
        self.ensure_shard_dir(hash, &dest)
            .map_err(StoreError::TargetDir)?;
        if let Err(link_err) = std::fs::hard_link(path, &dest) {
            // A concurrent adopt may have won the race in the meantime.
            if dest.exists() {
                return Ok(());
            }
            materialize_copy(path, &dest)
                .map_err(|copy_err| StoreError::LinkFailed(link_err, copy_err))?;
        }
        Ok(())
    }

    /// SHA-256 hash of a file on disk.
    pub fn hash_for_path(&self, path: &Path) -> std::io::Result<String> {
        hash::hash_file(path)
    }

    /// Indexed payload bytes for a file hash: the store object size when the
    /// blob exists, otherwise `file_size` (archive-only installs).
    pub fn payload_size(&self, hash: &str, file_size: i64) -> i64 {
        match std::fs::metadata(self.object_path(hash)) {
            Ok(m) => m.len() as i64,
            Err(_) => file_size,
        }
    }

    /// Create the 256 prefix directories (00-ff) for fast object storage.
    pub fn prereqs(&self) -> Result<()> {
        for i in 0..256u32 {
            let dir = self.root_dir.join(format!("{i:02x}"));
            std::fs::create_dir_all(dir).map_err(StoreError::CreateDir)?;
        }
        // All 256 now exist: record it so later writes skip the check.
        for slot in self.shard_dirs.iter() {
            slot.store(1, Ordering::Release);
        }
        Ok(())
    }

    /// Create the two-char shard directory holding `object`, skipping the
    /// syscall when this handle already created it.
    ///
    /// Returns the raw IO error so each caller keeps its own
    /// `StoreError` variant (wraps these differently per entry point).
    fn ensure_shard_dir(&self, hash: &str, object: &Path) -> std::io::Result<()> {
        let idx = shard_index(hash);
        if let Some(i) = idx {
            // Acquire pairs with the Release store below: the flag is only
            // published once `create_dir_all` has returned, so a reader that
            // observes it may skip a directory that really exists.
            if self.shard_dirs[i].load(Ordering::Acquire) != 0 {
                return Ok(());
            }
        }
        let Some(dir) = object.parent() else {
            return Ok(());
        };
        std::fs::create_dir_all(dir)?;
        if let Some(i) = idx {
            self.shard_dirs[i].store(1, Ordering::Release);
        }
        Ok(())
    }
}

/// Shard index (0..=255) for a hash, i.e. its first two hex digits; `None`
/// when the hash is too short or not hex (those paths sit directly under the
/// root, mirroring `object_path`).
fn shard_index(hash: &str) -> Option<usize> {
    let bytes = hash.as_bytes();
    if bytes.len() < 4 {
        return None;
    }
    let hi = (bytes[0] as char).to_digit(16)?;
    let lo = (bytes[1] as char).to_digit(16)?;
    Some((hi * 16 + lo) as usize)
}

/// `create_dir_all`, but stat first.
///
/// `std::fs::create_dir_all` starts with a `CreateDirectoryW` that fails with
/// `ERROR_ALREADY_EXISTS` and only then stats the path, so on the common
/// "directory is already there" path it costs two syscalls where one would do.
/// stats first, so this also restores parity on the one
/// path where it matters: `Store::Link` calls `MkdirAll` unconditionally, once
/// per installed file. Measured in-process on this host: 800 links cost 435 ms
/// with a per-file `create_dir_all` versus 194 ms through `Store::link`.
fn ensure_dir_exists(dir: &Path) -> std::io::Result<()> {
    if dir.is_dir() {
        return Ok(());
    }
    std::fs::create_dir_all(dir)
}

/// Copy-then-rename fallback for filesystems without hardlink support.
fn materialize_copy(src: &Path, dst: &Path) -> std::io::Result<()> {
    let seq = TEMP_COUNTER.fetch_add(1, Ordering::Relaxed);
    let tmp_path = dst.with_extension(format!("tmp-{:08x}", (seq as u32) ^ 0x5a5a_0000));
    let copy_result = (|| {
        let mut input = std::fs::File::open(src)?;
        let mut out = std::fs::File::create(&tmp_path)?;
        std::io::copy(&mut input, &mut out)?;
        // Same power-loss window as `Store::write`: flush before the rename.
        out.sync_all()?;
        Ok(())
    })();
    if copy_result.is_err() {
        let _ = std::fs::remove_file(&tmp_path);
        return copy_result;
    }
    if let Err(e) = std::fs::rename(&tmp_path, dst) {
        let _ = std::fs::remove_file(&tmp_path);
        return Err(e);
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::testutil::TempDir;

    fn temp_store(tag: &str) -> (Store, TempDir) {
        let dir = TempDir::new(&format!("glue-store-test-{tag}"));
        let store = Store::new(dir.path()).unwrap();
        (store, dir)
    }

    #[test]
    fn write_and_read_back_by_hash() {
        let (store, dir) = temp_store("write");
        let h = store.write(&b"hello glue"[..]).unwrap();
        // SHA-256("hello glue")
        assert_eq!(h, hash::hash_bytes(b"hello glue"));
        assert!(store.has(&h));
        let p = store.object_path(&h);
        assert!(p.starts_with(&dir));
        assert_eq!(std::fs::read(&p).unwrap(), b"hello glue");
        // Two-level layout: <root>/hh/rest
        let parent = p
            .parent()
            .unwrap()
            .file_name()
            .unwrap()
            .to_string_lossy()
            .into_owned();
        assert_eq!(parent, &h[..2]);
    }

    #[test]
    fn write_is_idempotent_for_same_content() {
        let (store, _dir) = temp_store("idem");
        let h1 = store.write(&b"same"[..]).unwrap();
        let h2 = store.write(&b"same"[..]).unwrap();
        assert_eq!(h1, h2);
    }

    #[test]
    fn object_path_short_hash_no_prefix() {
        let (store, dir) = temp_store("short");
        assert_eq!(store.object_path("abc"), dir.join("abc"));
    }

    #[test]
    fn link_creates_hardlink_or_copy() {
        let (store, dir) = temp_store("link");
        let h = store.write(&b"linkme"[..]).unwrap();
        let target = dir.join("out").join("deep").join("file.bin");
        store.link(&h, &target).unwrap();
        assert_eq!(std::fs::read(&target).unwrap(), b"linkme");
        // Re-link over an existing target.
        store.link(&h, &target).unwrap();
        assert_eq!(std::fs::read(&target).unwrap(), b"linkme");
        // Missing source errors.
        let missing = "00".repeat(32);
        assert!(matches!(
            store.link(&missing, &dir.join("x")),
            Err(StoreError::SourceNotFound(_, _))
        ));
    }

    /// `link` stats the target dir before creating it (cheaper than
    /// `create_dir_all`, which starts with a doomed `CreateDirectoryW`). The
    /// stat must never make us skip a directory that is genuinely missing —
    /// e.g. install → uninstall → reinstall inside one process, where the
    /// install tree was deleted between the two links.
    #[test]
    fn link_recreates_target_dir_removed_between_calls() {
        let (store, dir) = temp_store("link-recreate");
        let h = store.write(&b"relink"[..]).unwrap();
        let out = dir.join("apps").join("pkg").join("1.0.0");
        let target = out.join("bin").join("tool.exe");

        store.link(&h, &target).unwrap();
        assert_eq!(std::fs::read(&target).unwrap(), b"relink");

        std::fs::remove_dir_all(&out).unwrap();
        assert!(!out.exists());

        store.link(&h, &target).unwrap();
        assert_eq!(std::fs::read(&target).unwrap(), b"relink");
    }

    #[test]
    fn delete_ignores_missing() {
        let (store, _dir) = temp_store("del");
        let h = store.write(&b"gone"[..]).unwrap();
        store.delete(&h).unwrap();
        assert!(!store.has(&h));
        store.delete(&h).unwrap(); // no error on second delete
    }

    #[test]
    fn payload_size_prefers_store_object() {
        let (store, _dir) = temp_store("payload");
        let h = store.write(&b"12345"[..]).unwrap();
        assert_eq!(store.payload_size(&h, 999), 5);
        let missing = "ff".repeat(32);
        assert_eq!(store.payload_size(&missing, 999), 999);
    }

    #[test]
    fn prereqs_creates_all_prefix_dirs() {
        let (store, dir) = temp_store("prereqs");
        store.prereqs().unwrap();
        for i in [0x00u32, 0x0f, 0xa5, 0xff] {
            assert!(dir.join(format!("{i:02x}")).is_dir());
        }
    }

    #[test]
    fn shard_index_maps_hex_prefix() {
        assert_eq!(shard_index("00aa"), Some(0x00));
        assert_eq!(shard_index("7fab"), Some(0x7f));
        assert_eq!(shard_index("ff00"), Some(0xff));
        // Too short / not hex: `object_path` keeps those at the root, so
        // there is no shard to memoize.
        assert_eq!(shard_index("abc"), None);
        assert_eq!(shard_index("zzzz"), None);
        assert_eq!(shard_index(""), None);
    }

    /// The shard memo is only sound if it never claims a directory that is
    /// not there: a false positive would make `write` skip the `create_dir_all`
    /// and then fail the rename. 300 blobs cover every shard more than once,
    /// so the memoized set must equal the on-disk set exactly.
    #[test]
    fn shard_memo_matches_disk() {
        let (store, dir) = temp_store("shard-memo");
        for i in 0..300 {
            let h = store.write(format!("blob-{i}").as_bytes()).unwrap();
            assert!(store.object_path(&h).is_file(), "blob {i} missing");
        }
        for i in 0..256usize {
            let on_disk = dir.join(format!("{i:02x}")).is_dir();
            let memoized = store.shard_dirs[i].load(Ordering::Acquire) != 0;
            assert_eq!(memoized, on_disk, "shard {i:02x}");
        }
    }

    /// Clones share the memo, so a handle created from another one must not
    /// re-create shards (that is the whole point of holding it in an `Arc`).
    #[test]
    fn shard_memo_is_shared_across_clones() {
        let (store, _dir) = temp_store("shard-clone");
        let h = store.write(&b"shared"[..]).unwrap();
        let idx = shard_index(&h).unwrap();
        assert_ne!(store.shard_dirs[idx].load(Ordering::Acquire), 0);
        let clone = store.clone();
        assert_ne!(clone.shard_dirs[idx].load(Ordering::Acquire), 0);
        // Both handles still resolve the same blob.
        assert!(clone.has(&h));
    }

    /// Concurrent writers race on the same shard bits; every object must
    /// still land (a lost `create_dir_all` would fail the rename).
    #[test]
    fn concurrent_writes_populate_shards_without_loss() {
        let (store, _dir) = temp_store("shard-concurrent");
        std::thread::scope(|scope| {
            for t in 0..8u32 {
                let store = store.clone();
                scope.spawn(move || {
                    for i in 0..40u32 {
                        let payload = format!("thread-{t}-blob-{i}");
                        let h = store.write(payload.as_bytes()).unwrap();
                        assert!(store.object_path(&h).is_file());
                    }
                });
            }
        });
        for t in 0..8u32 {
            for i in 0..40u32 {
                let payload = format!("thread-{t}-blob-{i}");
                let h = hash::hash_bytes(payload.as_bytes());
                assert!(store.has(&h), "lost blob thread-{t}-blob-{i}");
            }
        }
    }

    #[test]
    fn hash_for_path_matches_hash_bytes() {
        let (store, dir) = temp_store("hashpath");
        let f = dir.join("data.bin");
        std::fs::write(&f, b"abc").unwrap();
        assert_eq!(store.hash_for_path(&f).unwrap(), hash::hash_bytes(b"abc"));
    }

    #[test]
    fn write_member_small_dedup() {
        let (store, _dir) = temp_store("member-small");
        store.prereqs().unwrap();
        let data = b"small member payload";
        let size = data.len() as i64;

        let hash1 = store.write_member(&data[..], size, false).unwrap();
        assert!(store.has(&hash1));

        // Second write with dedup skips the blob write and matches.
        let hash2 = store.write_member(&data[..], size, true).unwrap();
        assert_eq!(hash1, hash2);
        assert_eq!(hash1, hash::hash_bytes(data));
    }

    #[test]
    fn write_member_large_dedup() {
        let (store, _dir) = temp_store("member-large");
        store.prereqs().unwrap();
        const MAX_SMALL_MEMBER_BYTES: usize = 2 << 20;
        let data = vec![b'x'; MAX_SMALL_MEMBER_BYTES + 1];
        let size = data.len() as i64;

        let hash1 = store.write(&data[..]).unwrap();
        // dedupLikely + already stored → hash returned, no rewrite.
        let hash2 = store.write_member(&data[..], size, true).unwrap();
        assert_eq!(hash1, hash2);
    }

    #[test]
    fn write_member_large_needs_retry() {
        let (store, _dir) = temp_store("member-retry");
        store.prereqs().unwrap();
        const MAX_SMALL_MEMBER_BYTES: usize = 2 << 20;
        let data = vec![b'y'; MAX_SMALL_MEMBER_BYTES + 1];
        let size = data.len() as i64;

        let err = store.write_member(&data[..], size, true).unwrap_err();
        assert!(matches!(err, StoreError::NeedReaderRetry), "err = {err:?}");
        // Caller retries with a fresh reader.
        let hash = store.write(&data[..]).unwrap();
        assert!(store.has(&hash));
    }

    #[test]
    fn hash_reader_matches_store_write() {
        let (store, _dir) = temp_store("hashreader");
        assert_eq!(
            store.hash_reader(&b"stream me"[..]).unwrap(),
            hash::hash_bytes(b"stream me")
        );
    }

    /// Regression guard for the CAS-ingest read amplification: `io::copy`
    /// reads in 8 KiB slices (measured on this toolchain), which made the
    /// `.part` → CAS hashing pass ~3.8x slower than a 128 KiB loop. Assert
    /// the reader is never asked for a small buffer again.
    #[test]
    fn hash_reader_reads_in_large_chunks() {
        use std::cell::RefCell;
        use std::rc::Rc;
        struct SmallRequestGuard {
            inner: &'static [u8],
            off: usize,
            sizes: Rc<RefCell<Vec<usize>>>,
            returned: Rc<RefCell<usize>>,
        }
        impl std::io::Read for SmallRequestGuard {
            fn read(&mut self, buf: &mut [u8]) -> std::io::Result<usize> {
                self.sizes.borrow_mut().push(buf.len());
                let take = (self.inner.len() - self.off).min(buf.len());
                buf[..take].copy_from_slice(&self.inner[self.off..self.off + take]);
                self.off += take;
                *self.returned.borrow_mut() += take;
                Ok(take)
            }
        }
        // 3 x 128 KiB + a 1000-byte tail (content irrelevant).
        let data: &'static [u8] = Box::leak(vec![0u8; 3 * 128 * 1024 + 1000].into_boxed_slice());
        let (store, _dir) = temp_store("hashreader-buf");
        let sizes = Rc::new(RefCell::new(Vec::new()));
        let returned = Rc::new(RefCell::new(0usize));
        let guard = SmallRequestGuard {
            inner: data,
            off: 0,
            sizes: Rc::clone(&sizes),
            returned: Rc::clone(&returned),
        };
        store.hash_reader(guard).unwrap();
        // The regression guard: EVERY read request must ask for the full
        // 128 KiB buffer. `io::copy` (the old impl) asks for 8 KiB.
        let sizes = sizes.borrow();
        assert!(
            sizes.iter().all(|&s| s == 128 * 1024),
            "small read requests: {sizes:?}"
        );
        assert_eq!(*returned.borrow(), data.len());
        // 3 full + 1 tail + 1 EOF read.
        assert_eq!(sizes.len(), 5, "unexpected read count: {sizes:?}");
    }

    #[test]
    fn write_member_short_reader_errors() {
        // Io.ReadFull semantics: fewer bytes than declared → error.
        let (store, _dir) = temp_store("member-short");
        let err = store.write_member(&b"tiny"[..], 64, false).unwrap_err();
        assert!(matches!(err, StoreError::ReadMember(_)), "err = {err:?}");
    }

    #[test]
    fn write_member_unknown_size_no_dedup_streams() {
        // Negative size (unknown) + dedupLikely=false → plain streaming write
        // (WriteMember tail branch). Note parity: unknown size WITH
        // dedupLikely still goes through the hash-first branch and returns
        // ErrNeedReaderRetry (see write_member_unknown_size_dedup_needs_retry).
        let (store, _dir) = temp_store("member-unknown");
        let hash = store.write_member(&b"streamed"[..], -1, false).unwrap();
        assert_eq!(hash, hash::hash_bytes(b"streamed"));
        assert!(store.has(&hash));
    }

    #[test]
    fn write_member_unknown_size_dedup_needs_retry() {
        // WriteMember: the dedup branch does not check for a known size,
        // so unknown size + dedupLikely still returns ErrNeedReaderRetry.
        let (store, _dir) = temp_store("member-unknown-dedup");
        let err = store.write_member(&b"streamed"[..], -1, true).unwrap_err();
        assert!(matches!(err, StoreError::NeedReaderRetry), "err = {err:?}");
        // Caller retries with a fresh reader.
        let hash = store.write(&b"streamed"[..]).unwrap();
        assert!(store.has(&hash));
    }

    /// `std::fs` converts paths past MAX_PATH to the extended form on its own,
    /// so a deep install tree must hash, hardlink and read back unchanged.
    #[cfg(windows)]
    #[test]
    fn deep_paths_round_trip_through_the_store() {
        let dir = TempDir::new("glue-store-longpath");
        let store = Store::new(dir.path().join("store")).unwrap();
        store.prereqs().unwrap();

        let segment = "d".repeat(110);
        let install_dir = dir.path().join("apps").join(&segment).join(&segment);
        std::fs::create_dir_all(&install_dir).unwrap();
        let source = install_dir.join("payload.bin");
        std::fs::write(&source, b"deep content").unwrap();
        assert!(
            source.to_string_lossy().len() > 260,
            "test path must exceed MAX_PATH: {}",
            source.display()
        );

        let hash = store.adopt(&source).unwrap();
        assert!(store.has(&hash));
        // The raw Win32 identity lookup (no std conversion) must work too.
        assert!(crate::cache::fileid::file_ref_key(&source).is_some());

        let linked = install_dir.join("linked.bin");
        store.link(&hash, &linked).unwrap();
        assert_eq!(std::fs::read(&linked).unwrap(), b"deep content");

        std::fs::remove_dir_all(dir.path().join("apps")).unwrap();
    }
}
