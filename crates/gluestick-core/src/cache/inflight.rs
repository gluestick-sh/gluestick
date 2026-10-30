//! In-flight install markers.
//!
//! An install gives up the data-root mutation lock while it downloads (see
//! [`crate::lock::MutationLock::release_for_fetch`]), so a cache GC can run at
//! the same time. GC deletes store blobs that nothing references, and a blob a
//! running install has just ingested is exactly that: it is linked into the
//! package and written to the index only at the end.
//!
//! The marker closes that window. Every install drops a timestamped file here
//! before it starts fetching, and GC leaves alone any object created after the
//! oldest live marker — those belong to an install that has not finished yet.

use std::path::{Path, PathBuf};
use std::time::{Duration, SystemTime};

/// Marker directory under the data root's `cache/`.
const DIR: &str = "inflight";

/// A marker this old is a leftover from a crashed install, not a live one.
const STALE_AFTER: Duration = Duration::from_secs(24 * 60 * 60);

/// Removes its marker file when the install ends (any exit path).
pub struct InFlightGuard {
    path: PathBuf,
}

impl InFlightGuard {
    /// Mark `root` as having an install in flight.
    ///
    /// Returns `None` when the marker cannot be written: installs then keep
    /// working, they are just not protected from a concurrent GC (the caller
    /// still holds the mutation lock for most of the run).
    pub fn begin(root: &Path) -> Option<InFlightGuard> {
        let dir = root.join("cache").join(DIR);
        std::fs::create_dir_all(&dir).ok()?;
        let pid = std::process::id();
        let nanos = SystemTime::now()
            .duration_since(SystemTime::UNIX_EPOCH)
            .map(|d| d.as_nanos())
            .unwrap_or(0);
        let path = dir.join(format!("{pid}-{nanos}.marker"));
        std::fs::write(&path, nanos.to_string()).ok()?;
        Some(InFlightGuard { path })
    }
}

impl Drop for InFlightGuard {
    fn drop(&mut self) {
        let _ = std::fs::remove_file(&self.path);
    }
}

/// Creation-time floor for a GC sweep: store objects created at or after this
/// instant may belong to a running install. `None` when nothing is in flight.
///
/// Stale markers (a crashed install) are removed and ignored.
pub fn keep_since(root: &Path) -> Option<SystemTime> {
    let dir = root.join("cache").join(DIR);
    let entries = std::fs::read_dir(&dir).ok()?;
    let now = SystemTime::now();
    let mut oldest: Option<SystemTime> = None;
    for entry in entries.flatten() {
        let Ok(meta) = entry.metadata() else {
            continue;
        };
        if !meta.is_file() {
            continue;
        }
        let Some(started) = meta.created().or_else(|_| meta.modified()).ok() else {
            continue;
        };
        // A marker older than the crash threshold is not a live install.
        if now
            .duration_since(started)
            .map(|age| age >= STALE_AFTER)
            .unwrap_or(false)
        {
            let _ = std::fs::remove_file(entry.path());
            continue;
        }
        oldest = Some(match oldest {
            Some(current) => current.min(started),
            None => started,
        });
    }
    oldest
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::testutil::TempDir;

    #[test]
    fn guard_marks_and_clears_the_root() {
        let root = TempDir::new("glue-inflight-basic");
        assert!(keep_since(root.path()).is_none());
        {
            let _guard = InFlightGuard::begin(root.path()).expect("marker");
            assert!(keep_since(root.path()).is_some());
        }
        assert!(keep_since(root.path()).is_none(), "marker must be removed");
    }

    #[test]
    fn keep_since_is_the_oldest_live_marker() {
        let root = TempDir::new("glue-inflight-oldest");
        let first = InFlightGuard::begin(root.path()).expect("marker");
        std::thread::sleep(Duration::from_millis(20));
        let second = InFlightGuard::begin(root.path()).expect("marker");
        let floor = keep_since(root.path()).expect("floor");
        // Dropping the older marker keeps the newer one as the floor.
        drop(first);
        let newer = keep_since(root.path()).expect("floor");
        assert!(newer >= floor);
        drop(second);
        assert!(keep_since(root.path()).is_none());
    }
}
