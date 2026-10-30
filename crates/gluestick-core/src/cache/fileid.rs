//! Hardlink identity keys.
//!
//! A identifies a file across hardlinks: same volume + file
//! index. On Windows it comes from `GetFileInformationByHandle`
//! (`VolumeSerialNumber:FileIndexHigh:FileIndexLow`); other hosts have none.

use std::path::Path;

#[cfg(windows)]
pub fn file_ref_key(path: &Path) -> Option<String> {
    use std::os::windows::ffi::OsStrExt;
    use windows_sys::Win32::Foundation::{CloseHandle, GENERIC_READ, INVALID_HANDLE_VALUE};
    use windows_sys::Win32::Storage::FileSystem::{
        CreateFileW, GetFileInformationByHandle, BY_HANDLE_FILE_INFORMATION, FILE_ATTRIBUTE_NORMAL,
        FILE_SHARE_DELETE, FILE_SHARE_READ, FILE_SHARE_WRITE, OPEN_EXISTING,
    };

    // `CreateFileW` does not go through `std`'s automatic verbatim conversion,
    // so a deep install path would fail without the extended form.
    let extended = crate::winpath::to_extended(path);
    let wide: Vec<u16> = extended
        .as_os_str()
        .encode_wide()
        .chain(std::iter::once(0))
        .collect();
    // SAFETY: null-terminated UTF-16 path; null security attributes and
    // template handle. Share mode allows concurrent readers/writers/deletes.
    unsafe {
        let handle = CreateFileW(
            wide.as_ptr(),
            GENERIC_READ,
            FILE_SHARE_READ | FILE_SHARE_WRITE | FILE_SHARE_DELETE,
            std::ptr::null(),
            OPEN_EXISTING,
            FILE_ATTRIBUTE_NORMAL,
            std::ptr::null_mut(),
        );
        if handle == INVALID_HANDLE_VALUE {
            return None;
        }
        let mut info: BY_HANDLE_FILE_INFORMATION = std::mem::zeroed();
        let ok = GetFileInformationByHandle(handle, &mut info);
        CloseHandle(handle);
        if ok == 0 {
            return None;
        }
        Some(format!(
            "{}:{}:{}",
            info.dwVolumeSerialNumber, info.nFileIndexHigh, info.nFileIndexLow
        ))
    }
}

/// Unavailable on non-Windows hosts.
#[cfg(not(windows))]
pub fn file_ref_key(_path: &Path) -> Option<String> {
    None
}

#[cfg(all(test, windows))]
mod tests {
    use super::*;
    use crate::store::Store;
    use crate::testutil::TempDir;

    fn temp_root(tag: &str) -> TempDir {
        TempDir::new(&format!("glue-fileid-test-{tag}"))
    }

    #[test]
    fn file_ref_key_hardlink_matches_store() {
        let root = temp_root("hardlink");
        let store = Store::new(root.join("store")).unwrap();
        store.prereqs().unwrap();

        let hash = store.write(&b"linked-content"[..]).unwrap();
        let target = root.join("apps").join("demo").join("1.0.0").join("bin.exe");
        std::fs::create_dir_all(target.parent().unwrap()).unwrap();
        store.link(&hash, &target).unwrap();

        let store_key = file_ref_key(&store.object_path(&hash)).expect("store file ref key");
        let app_key = file_ref_key(&target).expect("app file ref key");
        assert_eq!(store_key, app_key);
    }

    #[test]
    fn file_ref_key_missing_file_is_none() {
        let root = temp_root("missing");
        assert!(file_ref_key(&root.join("nope.bin")).is_none());
    }

    /// `CreateFileW` is a raw call, so it needs the extended form to reach a
    /// path past MAX_PATH (adopt/scan rely on the identity key).
    #[test]
    fn file_ref_key_supports_deep_paths() {
        let root = temp_root("longpath");
        let segment = "e".repeat(110);
        let dir = root.join("apps").join(&segment).join(&segment);
        std::fs::create_dir_all(&dir).unwrap();
        let file = dir.join("x.bin");
        std::fs::write(&file, b"x").unwrap();
        assert!(file.to_string_lossy().len() > 260);
        assert!(
            file_ref_key(&file).is_some(),
            "CreateFileW must receive the extended form"
        );
        std::fs::remove_dir_all(root.join("apps")).unwrap();
    }
}
