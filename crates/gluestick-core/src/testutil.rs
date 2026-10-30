//! Shared test fixtures.
//!
//! One shared scratch-directory implementation for the whole crate, so:
//!
//!   - every test gets a uniquely named directory under the OS temp dir,
//!   - cleanup is best-effort and never fails a test,
//!   - the slow-filesystem caveat below is handled in one place.
//!
//! On virtualized/networked filesystems a recursive delete of a wide tree can
//! take tens of seconds, so tests must never depend on cleanup for
//! correctness — only on the directory being empty at creation time.

#![cfg(test)]

use std::ops::Deref;
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicU64, Ordering};

static COUNTER: AtomicU64 = AtomicU64::new(0);

/// A uniquely named scratch directory that removes itself on drop.
///
/// Named `<prefix>-<pid>-<seq>`. The directory is created eagerly, so a stale
/// directory from a previous run is never observed by the test that owns it.
pub struct TempDir {
    path: PathBuf,
}

impl TempDir {
    /// Create a scratch directory whose name starts with `prefix`.
    pub fn new(prefix: &str) -> TempDir {
        let seq = COUNTER.fetch_add(1, Ordering::Relaxed);
        let path = std::env::temp_dir().join(format!("{prefix}-{}-{seq}", std::process::id()));
        let _ = std::fs::remove_dir_all(&path);
        std::fs::create_dir_all(&path).expect("create temp dir");
        TempDir { path }
    }

    /// The directory path.
    pub fn path(&self) -> &Path {
        &self.path
    }

    /// Join a relative path onto the scratch directory.
    pub fn join(&self, rel: impl AsRef<Path>) -> PathBuf {
        self.path.join(rel)
    }
}

impl Drop for TempDir {
    fn drop(&mut self) {
        // Best effort: a failure here must not mask the test result, and on
        // slow filesystems the delete may be deferred to the OS anyway.
        //
        // `GLUE_TEST_KEEP_TEMP=1` skips the teardown entirely: on this machine
        // (Parallels + AV hooks) deleting a few hundred small files costs more
        // than the test body, and a full run leaves ~200 directories behind.
        // CI keeps the default so runs stay self-cleaning.
        if std::env::var_os("GLUE_TEST_KEEP_TEMP").is_some() {
            return;
        }
        let _ = std::fs::remove_dir_all(&self.path);
    }
}

/// Compress `data` into a raw DEFLATE stream (no zlib/gzip wrapper) for ZIP
/// and tar fixtures.
pub(crate) fn deflate_raw(data: &[u8]) -> Vec<u8> {
    let mut output = vec![0u8; zlib_rs::compress_bound(data.len())];
    let mut encoder = zlib_rs::Deflate::new(6, false, 15);
    let status = encoder
        .compress(data, &mut output, zlib_rs::DeflateFlush::Finish)
        .expect("compress test fixture");
    assert!(
        matches!(status, zlib_rs::Status::StreamEnd),
        "deflate fixture did not finish: {status:?}"
    );
    output.truncate(encoder.total_out() as usize);
    output
}

impl AsRef<Path> for TempDir {
    fn as_ref(&self) -> &Path {
        &self.path
    }
}

/// Deref to the underlying path so a `TempDir` can be passed anywhere a
/// `&Path` is expected (`&td`, `td.join(..)`, `td.display()`, …) without an
/// explicit `.path()` — this keeps call sites that used to hold a `PathBuf`
/// unchanged when migrated to `TempDir`.
impl Deref for TempDir {
    type Target = Path;

    fn deref(&self) -> &Path {
        &self.path
    }
}
