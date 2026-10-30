//! Free-space queries for install preflight checks.
//!
//! A full volume otherwise surfaces as a write error halfway through
//! extraction: the partial install is cleaned up, but the user has already paid
//! the download and the wait. Callers ask this module before starting work and
//! fail with an actionable message instead.

use std::path::Path;

/// Bytes available to the caller on the volume holding `path`.
///
/// The nearest existing ancestor is used, so a directory that has not been
/// created yet still resolves to its volume. `None` when the host cannot
/// answer (non-Windows, or no resolvable ancestor).
pub fn available_space(path: &Path) -> Option<u64> {
    imp::available_space(path)
}

#[cfg(windows)]
mod imp {
    use std::os::windows::ffi::OsStrExt;
    use std::path::{Path, PathBuf};

    use windows_sys::Win32::Storage::FileSystem::GetDiskFreeSpaceExW;

    pub fn available_space(path: &Path) -> Option<u64> {
        let dir = existing_ancestor(path)?;
        let wide: Vec<u16> = dir
            .as_os_str()
            .encode_wide()
            .chain(std::iter::once(0))
            .collect();
        let mut available: u64 = 0;
        let mut total: u64 = 0;
        let mut free_total: u64 = 0;
        // SAFETY: `wide` is NUL-terminated and the three out-pointers are valid
        // for writes for the duration of the call.
        let ok = unsafe {
            GetDiskFreeSpaceExW(wide.as_ptr(), &mut available, &mut total, &mut free_total)
        };
        (ok != 0).then_some(available)
    }

    /// First ancestor of `path` that exists as a directory.
    fn existing_ancestor(path: &Path) -> Option<PathBuf> {
        let mut current = Some(path);
        while let Some(candidate) = current {
            if candidate.as_os_str().is_empty() {
                break;
            }
            if candidate.is_dir() {
                return Some(candidate.to_path_buf());
            }
            current = candidate.parent();
        }
        std::env::current_dir().ok()
    }
}

#[cfg(not(windows))]
mod imp {
    use std::path::Path;

    pub fn available_space(_path: &Path) -> Option<u64> {
        None
    }
}

#[cfg(all(test, windows))]
mod tests {
    use super::*;

    #[test]
    fn available_space_reports_the_temp_volume() {
        let dir = std::env::temp_dir();
        let free = available_space(&dir).expect("free space is queryable on Windows");
        assert!(free > 0, "free space should be positive, got {free}");
    }

    #[test]
    fn available_space_walks_up_to_an_existing_ancestor() {
        let dir = std::env::temp_dir()
            .join("glue-disk-space-missing")
            .join("deep");
        assert!(!dir.exists());
        assert!(available_space(&dir).is_some());
    }
}
