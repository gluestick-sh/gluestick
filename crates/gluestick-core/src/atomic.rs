//! Atomic replacement for small metadata files.
//!
//! `config.json`, `install.json` and shim metadata are read by other commands
//! (and by the desktop shell) while a mutation is in progress. Writing them
//! in place can expose a truncated file after a crash; staging the bytes in a
//! sibling temp file and renaming it into place makes the replacement atomic
//! for readers — they see either the old file or the new one, never a torn
//! write.

use std::io::{self, Write};
use std::path::Path;
use std::sync::atomic::{AtomicU64, Ordering};

static TEMP_SEQ: AtomicU64 = AtomicU64::new(0);

/// Write `contents` to `path` via a sibling temp file + rename.
///
/// The temp file lives in the same directory so the rename stays on one
/// volume (and is therefore atomic). No `sync_all`: a power loss may lose the
/// newest write, but it cannot leave a partially written destination file.
pub(crate) fn write_atomic(path: &Path, contents: &[u8]) -> io::Result<()> {
    let dir = path.parent().unwrap_or_else(|| Path::new("."));
    let file_name = path
        .file_name()
        .map(|name| name.to_string_lossy().into_owned())
        .unwrap_or_else(|| "file".to_string());
    let seq = TEMP_SEQ.fetch_add(1, Ordering::Relaxed);
    let tmp = dir.join(format!(".{file_name}.tmp-{}-{seq}", std::process::id()));

    let staged = (|| -> io::Result<()> {
        let mut file = std::fs::OpenOptions::new()
            .write(true)
            .create(true)
            .truncate(true)
            .open(&tmp)?;
        file.write_all(contents)?;
        file.flush()
    })();
    if let Err(err) = staged {
        let _ = std::fs::remove_file(&tmp);
        return Err(err);
    }
    if let Err(err) = std::fs::rename(&tmp, path) {
        let _ = std::fs::remove_file(&tmp);
        return Err(err);
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::testutil::TempDir;

    #[test]
    fn writes_and_replaces_in_place() {
        let dir = TempDir::new("glue-atomic");
        let path = dir.join("meta.json");

        write_atomic(&path, b"one").unwrap();
        assert_eq!(std::fs::read(&path).unwrap(), b"one");

        write_atomic(&path, b"two").unwrap();
        assert_eq!(std::fs::read(&path).unwrap(), b"two");

        // No staging files are left behind.
        let leftovers: Vec<String> = std::fs::read_dir(dir.path())
            .unwrap()
            .flatten()
            .map(|entry| entry.file_name().to_string_lossy().into_owned())
            .filter(|name| name.contains(".tmp-"))
            .collect();
        assert!(leftovers.is_empty(), "{leftovers:?}");
    }

    #[test]
    fn missing_parent_directory_is_an_error() {
        let dir = TempDir::new("glue-atomic-missing");
        let path = dir.join("missing").join("meta.json");
        assert!(write_atomic(&path, b"x").is_err());
    }
}
