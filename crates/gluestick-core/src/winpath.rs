//! Windows long-path helpers.
//!
//! Rust's `std::fs` converts paths that exceed MAX_PATH (260) to the verbatim
//! form (`\\?\`) on its own, so the engine's file I/O works on deep package
//! trees without any per-call work. Two things still need explicit help:
//!
//! - **Raw Win32 calls** (`CreateFileW`, `SHGetFileInfoW`, …) bypass that
//!   conversion and fail past MAX_PATH.
//! - **External tools** (PowerShell hooks, `msiexec`, `cmd`-builtins) depend on
//!   the machine-wide `LongPathsEnabled` policy, which this module can report.
//!
//! Process creation is the one place long paths are *not* supported at all:
//! `CreateProcessW` does not launch a program whose path exceeds MAX_PATH, so
//! executables must stay in a short directory (package executables live at
//! `apps/<pkg>/current/…`, which is normally well under the limit).

use std::path::Path;

/// `path` in the verbatim form raw Win32 calls require.
///
/// Verbatim paths disable `.`/`..` normalisation and drive-relative
/// resolution, so only absolute, already-normalised paths are converted;
/// relative or empty paths are returned unchanged. Paths that are already
/// verbatim (or a device namespace) are returned as-is.
pub fn to_extended(path: &Path) -> std::path::PathBuf {
    #[cfg(not(windows))]
    {
        return path.to_path_buf();
    }

    #[cfg(windows)]
    {
        use std::ffi::OsString;
        use std::path::{Component, PathBuf, Prefix};

        if path.as_os_str().is_empty() || !path.is_absolute() {
            return path.to_path_buf();
        }
        let mut components = path.components();
        let Some(Component::Prefix(prefix)) = components.next() else {
            return path.to_path_buf();
        };
        match prefix.kind() {
            // Already extended (or a device namespace such as \\.\COM1).
            Prefix::VerbatimDisk(_)
            | Prefix::Verbatim(_)
            | Prefix::VerbatimUNC(_, _)
            | Prefix::DeviceNS(_) => path.to_path_buf(),
            Prefix::UNC(server, share) => {
                // \\server\share\x → \\?\UNC\server\share\x
                let mut out = OsString::from(r"\\?\UNC\");
                out.push(server);
                out.push(r"\");
                out.push(share);
                for component in components {
                    if let Component::Normal(name) = component {
                        out.push(r"\");
                        out.push(name);
                    }
                }
                PathBuf::from(out)
            }
            _ => {
                // Drive-absolute (C:\x) or rooted (\) paths.
                let mut out = OsString::from(r"\\?\");
                out.push(path.as_os_str());
                PathBuf::from(out)
            }
        }
    }
}

/// Whether Windows long-path support is enabled system-wide (`LongPathsEnabled`
/// under `HKLM\SYSTEM\CurrentControlSet\Control\FileSystem`).
///
/// `None` off Windows, or when the value cannot be read. This is what external
/// tools (PowerShell hooks, `msiexec`, `cmd`) rely on; the Rust side does not
/// need it because `std::fs` uses verbatim paths.
pub fn long_paths_enabled() -> Option<bool> {
    #[cfg(not(windows))]
    {
        None
    }

    #[cfg(windows)]
    {
        use windows_sys::Win32::System::Registry::{
            RegGetValueW, HKEY_LOCAL_MACHINE, RRF_RT_REG_DWORD,
        };

        fn wide(value: &str) -> Vec<u16> {
            value.encode_utf16().chain(std::iter::once(0)).collect()
        }

        let key = wide(r"SYSTEM\CurrentControlSet\Control\FileSystem");
        let name = wide("LongPathsEnabled");
        let mut data: u32 = 0;
        let mut size = std::mem::size_of::<u32>() as u32;
        // SAFETY: both strings are NUL-terminated; `data`/`size` describe a
        // valid 4-byte buffer for a REG_DWORD read.
        let status = unsafe {
            RegGetValueW(
                HKEY_LOCAL_MACHINE,
                key.as_ptr(),
                name.as_ptr(),
                RRF_RT_REG_DWORD,
                std::ptr::null_mut(),
                (&mut data as *mut u32).cast(),
                &mut size,
            )
        };
        (status == 0).then_some(data != 0).or_else(|| {
            // Value absent (`ERROR_FILE_NOT_FOUND`): the policy default is off.
            (status == 2).then_some(false)
        })
    }
}

/// Whether `path` is longer than the legacy MAX_PATH limit.
///
/// Used to explain a failure up front instead of surfacing the opaque
/// `os error 3` that `CreateProcessW` returns for such a program path.
pub fn exceeds_max_path(path: &Path) -> bool {
    #[cfg(windows)]
    {
        use std::os::windows::ffi::OsStrExt;
        path.as_os_str().encode_wide().count() >= 260
    }
    #[cfg(not(windows))]
    {
        let _ = path;
        false
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[cfg(windows)]
    #[test]
    fn drive_paths_are_prefixed() {
        assert_eq!(
            to_extended(Path::new(r"C:\Users\me\.glue\apps\pkg\1.0")),
            std::path::PathBuf::from(r"\\?\C:\Users\me\.glue\apps\pkg\1.0")
        );
    }

    #[cfg(windows)]
    #[test]
    fn unc_paths_use_the_unc_form() {
        assert_eq!(
            to_extended(Path::new(r"\\server\share\dir\file")),
            std::path::PathBuf::from(r"\\?\UNC\server\share\dir\file")
        );
    }

    #[cfg(windows)]
    #[test]
    fn already_extended_paths_are_unchanged() {
        for path in [r"\\?\C:\dir\file", r"\\?\UNC\server\share\file"] {
            assert_eq!(to_extended(Path::new(path)), std::path::PathBuf::from(path));
        }
    }

    #[cfg(windows)]
    #[test]
    fn relative_and_empty_paths_are_unchanged() {
        assert_eq!(
            to_extended(Path::new(r"apps\pkg\1.0")),
            std::path::PathBuf::from(r"apps\pkg\1.0")
        );
        assert_eq!(to_extended(Path::new("")), std::path::PathBuf::new());
    }

    #[cfg(windows)]
    #[test]
    fn max_path_boundary() {
        let short = std::path::PathBuf::from(r"C:\x");
        assert!(!exceeds_max_path(&short));
        let long = format!(r"C:\{}", "a".repeat(300));
        assert!(exceeds_max_path(Path::new(&long)));
    }

    #[cfg(windows)]
    #[test]
    fn long_paths_policy_is_readable() {
        // Either state is fine; the call must simply succeed on Windows.
        assert!(long_paths_enabled().is_some());
    }
}
