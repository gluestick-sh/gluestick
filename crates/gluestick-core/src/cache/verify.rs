//! Store integrity verification (`glue cache verify`).
//!
//! Store objects are trusted by name everywhere else: an install that hits the
//! cache links the blob without re-hashing it. A blob corrupted after ingest —
//! by an in-place edit through one of its hardlinks, by antivirus, or by a
//! power loss between the rename and the data flush — therefore stays invisible
//! until someone re-hashes it. This module does exactly that: every object's
//! content is re-hashed and compared with the name it is stored under.

use std::path::PathBuf;
use std::sync::atomic::{AtomicI64, AtomicUsize, Ordering};
use std::sync::Mutex;

use crate::store::Store;

use super::refs::{CachePhase, GcPhaseFn, RefsError};
use super::scan;
use super::workers::{gc_workers, WORKER_STACK_SIZE};

/// A store object whose content does not hash to its name, or that cannot be
/// read at all.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct CorruptBlob {
    pub hash: String,
    pub path: PathBuf,
    /// Read error for an unreadable object; empty for a hash mismatch.
    pub error: String,
}

/// Outcome of [`verify_store`].
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct StoreVerifyReport {
    /// Objects whose content matched their hash.
    pub checked: usize,
    /// Bytes covered by those objects.
    pub bytes_checked: i64,
    /// Objects that failed verification, in scan order.
    pub corrupt: Vec<CorruptBlob>,
    /// Installed packages whose indexed files reference the corrupt blobs.
    /// A blob and its installed files are hardlinks to one inode, so when the
    /// corruption came from an in-place edit through an install, the
    /// installed copy is corrupt too — deleting the blob does not fix it;
    /// these packages need a reinstall. Populated by `Engine::verify_cache`.
    pub affected_packages: Vec<String>,
}

/// Re-hash every store object and report the ones that do not match.
///
/// Read-only: nothing is deleted here. Callers decide what to do with the
/// result (`glue cache verify --repair` deletes the corrupt blobs so the next
/// install re-downloads them).
pub fn verify_store(
    store: &Store,
    on_phase: Option<GcPhaseFn<'_>>,
) -> Result<StoreVerifyReport, RefsError> {
    let phase = |msg: CachePhase| {
        if let Some(report) = on_phase {
            report(&msg);
        }
    };

    phase(CachePhase::ScanStore);
    let scan_cb = |done: usize| phase(CachePhase::ScanStoreProgress { done });
    // Verification only needs the objects: skipping the hardlink index avoids
    // opening every blob for its file id first.
    let objects = scan::scan_store_objects_with_progress(store, Some(&scan_cb))?;
    // Real object names only (`hh/<62 hex>`); anything else under the store is
    // not a blob and must not be reported as corruption.
    let objects: Vec<_> = objects
        .into_iter()
        .filter(|object| is_store_hash(&object.hash))
        .collect();
    phase(CachePhase::VerifyObjects {
        objects: objects.len(),
    });

    let checked = AtomicUsize::new(0);
    let bytes = AtomicI64::new(0);
    let corrupt: Mutex<Vec<CorruptBlob>> = Mutex::new(Vec::new());
    let next = AtomicUsize::new(0);
    let verified = AtomicUsize::new(0);
    let total = objects.len();
    let step = (total / 100).max(1);
    let workers = gc_workers(objects.len());

    std::thread::scope(|scope| {
        for _ in 0..workers {
            let (checked, bytes, corrupt, next, objects, verified) =
                (&checked, &bytes, &corrupt, &next, &objects, &verified);
            // A spawn failure must surface, not abort the process (release
            // builds run with panic=abort).
            std::thread::Builder::new()
                .stack_size(WORKER_STACK_SIZE)
                .spawn_scoped(scope, move || loop {
                    let i = next.fetch_add(1, Ordering::Relaxed);
                    if i >= objects.len() {
                        break;
                    }
                    let object = &objects[i];
                    match crate::hash::hash_file(&object.path) {
                        Ok(hash) if hash == object.hash => {
                            checked.fetch_add(1, Ordering::Relaxed);
                            bytes.fetch_add(object.size, Ordering::Relaxed);
                        }
                        Ok(_) => corrupt.lock().unwrap().push(CorruptBlob {
                            hash: object.hash.clone(),
                            path: object.path.clone(),
                            error: String::new(),
                        }),
                        Err(e) => corrupt.lock().unwrap().push(CorruptBlob {
                            hash: object.hash.clone(),
                            path: object.path.clone(),
                            error: e.to_string(),
                        }),
                    }
                    let done = verified.fetch_add(1, Ordering::Relaxed) + 1;
                    if let Some(report) = on_phase {
                        if done % step == 0 || done == total {
                            report(&CachePhase::VerifyProgress { done, total });
                        }
                    }
                })
                .map_err(RefsError::Spawn)?;
        }
        Ok::<(), RefsError>(())
    })?;

    Ok(StoreVerifyReport {
        checked: checked.load(Ordering::Relaxed),
        bytes_checked: bytes.load(Ordering::Relaxed),
        corrupt: corrupt
            .into_inner()
            .unwrap_or_else(|poisoned| poisoned.into_inner()),
        // Filled in by `Engine::verify_cache`, which owns the cache index.
        affected_packages: Vec::new(),
    })
}

/// Whether `hash` is a full SHA-256 hex digest (the only names that are blobs).
fn is_store_hash(hash: &str) -> bool {
    hash.len() == 64 && hash.bytes().all(|b| b.is_ascii_hexdigit())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::testutil::TempDir;

    #[test]
    fn verify_flags_a_corrupt_blob() {
        let root = TempDir::new("glue-cache-verify");
        let store = Store::new(root.path().join("store")).unwrap();
        let good = store.write(&b"good"[..]).unwrap();
        let bad = store.write(&b"tampered"[..]).unwrap();
        // Rewriting the object in place is what an in-place edit through a
        // hardlinked install file does to the blob.
        std::fs::write(store.object_path(&bad), b"changed!").unwrap();

        let report = verify_store(&store, None).unwrap();
        assert_eq!(report.checked, 1);
        assert_eq!(report.corrupt.len(), 1);
        assert_eq!(report.corrupt[0].hash, bad);
        assert!(report.corrupt[0].error.is_empty());
        assert!(store.has(&good));
    }
}
