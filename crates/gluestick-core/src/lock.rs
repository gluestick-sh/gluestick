//! Cross-process lock over the data root's mutable state.
//!
//! Mutating commands (`install`/`update`/`uninstall`/`reset`, bucket
//! clone/pull, cache index edits) take this lock before touching `apps/`,
//! `shims/`, `buckets/` or the cache index. Read-only commands never take it,
//! so `list`/`search`/`info`/`doctor` keep running while an install is in
//! progress.
//!
//! The lock is an OS advisory file lock (`LockFileEx` on Windows, `flock` on
//! Unix): the kernel releases it when the owning process exits, so a crash
//! cannot leave a stale lock behind. The file itself is intentionally never
//! deleted — unlinking a lock file while another process waits on it would
//! give the two processes different identities for the same lock.

use std::fs::{File, TryLockError};
use std::path::{Path, PathBuf};
use std::sync::Mutex;
use std::time::{Duration, Instant};

use thiserror::Error;

/// How long a mutating command waits for another glue process before giving
/// up with [`LockError::Busy`].
pub const LOCK_WAIT_TIMEOUT: Duration = Duration::from_secs(60);

/// Longer wait for work that is already under way in the background: re-taking
/// the hold after an install download, or an install queueing behind a
/// maintenance scan. Giving up after a minute turned a slow neighbour into a
/// failed install; a queued task row already tells the user it is waiting.
pub const LOCK_WAIT_BACKGROUND: Duration = Duration::from_secs(5 * 60);

const RETRY_INTERVAL: Duration = Duration::from_millis(150);
const LOCK_FILE: &str = "glue.lock";

/// Errors acquiring the cross-process mutation lock.
#[derive(Debug, Clone, Error)]
pub enum LockError {
    #[error("open lock file {path}: {message}")]
    Open { path: PathBuf, message: String },
    #[error("lock {path}: {message}")]
    Lock { path: PathBuf, message: String },
    #[error(
        "another glue process is modifying {root} (lock file: {path}); waited {}s — retry after it finishes",
        waited.as_secs()
    )]
    Busy {
        root: PathBuf,
        path: PathBuf,
        waited: Duration,
    },
}

/// Exclusive guard held for the duration of one mutating operation.
///
/// The hold can be given up for a long, non-mutating phase and taken again
/// afterwards ([`Self::release_for_fetch`] / [`Self::restore_after_fetch`]), so
/// an install's multi-minute download does not queue unrelated operations
/// (bucket sync, version switch) behind it. While the hold is given up the data
/// root is protected by other means: [`crate::cache::inflight`] marks the
/// running install so the cache GC leaves its freshly ingested blobs alone.
#[derive(Debug)]
pub struct MutationLock {
    root: PathBuf,
    state: Mutex<LockState>,
}

#[derive(Debug)]
struct LockState {
    /// `None` while the hold has been given up for a fetch phase.
    file: Option<File>,
    /// Threads currently inside a fetch phase; the hold comes back when the
    /// last one leaves.
    fetches: usize,
}

/// Open `cache/glue.lock` and take the OS lock on it, waiting `timeout`.
fn lock_file(root: &Path, timeout: Duration) -> Result<File, LockError> {
    let cache_dir = root.join("cache");
    std::fs::create_dir_all(&cache_dir).map_err(|err| LockError::Open {
        path: cache_dir.clone(),
        message: err.to_string(),
    })?;
    let path = cache_dir.join(LOCK_FILE);
    let file = std::fs::OpenOptions::new()
        .create(true)
        .truncate(false)
        .read(true)
        .write(true)
        .open(&path)
        .map_err(|err| LockError::Open {
            path: path.clone(),
            message: err.to_string(),
        })?;

    let started = Instant::now();
    loop {
        match file.try_lock() {
            Ok(()) => return Ok(file),
            Err(TryLockError::WouldBlock) => {
                if started.elapsed() >= timeout {
                    return Err(LockError::Busy {
                        root: root.to_path_buf(),
                        path,
                        waited: started.elapsed(),
                    });
                }
                std::thread::sleep(RETRY_INTERVAL);
            }
            Err(TryLockError::Error(err)) => {
                return Err(LockError::Lock {
                    path,
                    message: err.to_string(),
                })
            }
        }
    }
}

impl MutationLock {
    /// Acquire the lock, waiting up to [`LOCK_WAIT_TIMEOUT`].
    pub fn acquire(root: &Path) -> Result<MutationLock, LockError> {
        Self::acquire_with_timeout(root, LOCK_WAIT_TIMEOUT)
    }

    /// Acquire with an explicit wait (tests use short timeouts).
    pub fn acquire_with_timeout(root: &Path, timeout: Duration) -> Result<MutationLock, LockError> {
        let file = lock_file(root, timeout)?;
        Ok(MutationLock {
            root: root.to_path_buf(),
            state: Mutex::new(LockState {
                file: Some(file),
                fetches: 0,
            }),
        })
    }

    /// Give up the hold for a long phase that does not mutate the data root.
    ///
    /// Nesting is counted, so several concurrent installs sharing one guard
    /// keep the hold given up until the last of them is done fetching.
    pub fn release_for_fetch(&self) {
        let mut state = self.state.lock().unwrap_or_else(|e| e.into_inner());
        state.fetches += 1;
        if state.fetches == 1 {
            // Dropping the handle releases the OS lock.
            state.file = None;
        }
    }

    /// Take the hold again once the phase is over.
    pub fn restore_after_fetch(&self) -> Result<(), LockError> {
        let last = {
            let mut state = self.state.lock().unwrap_or_else(|e| e.into_inner());
            if state.fetches == 0 {
                return Ok(());
            }
            state.fetches -= 1;
            state.fetches == 0 && state.file.is_none()
        };
        if !last {
            return Ok(());
        }
        // Acquired outside the state lock: waiting for another process here
        // must not block other threads' release/restore calls.
        let file = lock_file(&self.root, LOCK_WAIT_BACKGROUND)?;
        let mut state = self.state.lock().unwrap_or_else(|e| e.into_inner());
        // A thread may have started fetching again while we waited; the hold
        // then stays given up and that thread's own restore takes it back.
        if state.fetches == 0 && state.file.is_none() {
            state.file = Some(file);
        }
        Ok(())
    }
}

impl Drop for MutationLock {
    fn drop(&mut self) {
        let mut state = self.state.lock().unwrap_or_else(|e| e.into_inner());
        if let Some(file) = state.file.take() {
            let _ = file.unlock();
        }
        state.fetches = 0;
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::testutil::TempDir;

    #[test]
    fn lock_is_exclusive_and_released_on_drop() {
        let root = TempDir::new("glue-lock-exclusive");
        let first =
            MutationLock::acquire_with_timeout(root.path(), Duration::from_millis(10)).unwrap();
        let err = MutationLock::acquire_with_timeout(root.path(), Duration::from_millis(20))
            .expect_err("a second handle must not acquire the lock");
        assert!(matches!(err, LockError::Busy { .. }), "{err:?}");

        drop(first);
        MutationLock::acquire_with_timeout(root.path(), Duration::from_millis(10))
            .expect("lock is released when the guard drops");
    }

    #[test]
    fn release_for_fetch_gives_the_hold_up_and_takes_it_back() {
        let root = TempDir::new("glue-lock-fetch-window");
        let guard =
            MutationLock::acquire_with_timeout(root.path(), Duration::from_millis(10)).unwrap();

        guard.release_for_fetch();
        // Given up: another holder can run (that is the point of the window).
        let other = MutationLock::acquire_with_timeout(root.path(), Duration::from_millis(500))
            .expect("the hold is given up during a fetch");
        drop(other);

        guard.restore_after_fetch().unwrap();
        let err = MutationLock::acquire_with_timeout(root.path(), Duration::from_millis(20))
            .expect_err("the hold must be taken back after the fetch");
        assert!(matches!(err, LockError::Busy { .. }), "{err:?}");
    }

    #[test]
    fn nested_fetches_keep_the_hold_given_up_until_the_last_one_ends() {
        let root = TempDir::new("glue-lock-fetch-nested");
        let guard =
            MutationLock::acquire_with_timeout(root.path(), Duration::from_millis(10)).unwrap();

        guard.release_for_fetch();
        guard.release_for_fetch();
        guard.restore_after_fetch().unwrap();
        // One fetch is still outstanding, so the hold stays given up.
        let other = MutationLock::acquire_with_timeout(root.path(), Duration::from_millis(500))
            .expect("still given up while a fetch is outstanding");
        drop(other);

        guard.restore_after_fetch().unwrap();
        let err = MutationLock::acquire_with_timeout(root.path(), Duration::from_millis(20))
            .expect_err("the hold must be back once every fetch ended");
        assert!(matches!(err, LockError::Busy { .. }), "{err:?}");
    }

    #[test]
    fn lock_file_is_created_and_kept() {
        let root = TempDir::new("glue-lock-file");
        let path = root.path().join("cache").join(LOCK_FILE);
        {
            let _lock = MutationLock::acquire(root.path()).unwrap();
            assert!(path.is_file());
        }
        {
            let _lock = MutationLock::acquire(root.path()).unwrap();
        }
        // Deliberately retained: deleting it would split the lock identity
        // between a waiting process and a new owner.
        assert!(path.is_file());
    }
}
