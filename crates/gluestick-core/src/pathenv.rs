//! User PATH integration (`HKCU\Environment\Path`).
//!
//! Covers the Windows user-PATH operations the CLI and installer need:
//! `ensure_user_path_front`, `prepend_dir_to_process_path`,
//! `path_dir_precedes`, `disable_windows_python_aliases`, plus entry removal,
//! which the NSIS uninstaller relies on.
//!
//! Why the installer delegates here instead of editing PATH in NSIS: NSIS
//! builds its runtime strings from an exehead capped at 1024 characters, and
//! the cap cannot be raised from a script (`NSIS_MAX_STRLEN` only changes the
//! compile-time symbol — verified with a probe against a 1211-character user
//! PATH). A read-modify-write in NSIS therefore sees an empty value and wipes
//! the whole user PATH. Rust strings have no such limit.

use std::path::{Path, PathBuf};

/// Registry key (under `HKEY_CURRENT_USER`) holding the user environment.
pub const USER_ENV_KEY: &str = "Environment";

/// Registry value holding the user PATH.
pub const PATH_VALUE: &str = "Path";

/// Registry type of the PATH value, preserved across read-modify-write so a
/// `REG_EXPAND_SZ` value keeps its `%VAR%` references unexpanded.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum PathKind {
    /// `REG_SZ`
    Sz,
    /// `REG_EXPAND_SZ`
    ExpandSz,
}

/// The user PATH as stored in the registry.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct UserPath {
    /// Raw value (environment references are *not* expanded).
    pub value: String,
    /// Registry type of the value.
    pub kind: PathKind,
    /// `false` when the value does not exist at all.
    pub exists: bool,
}

impl UserPath {
    /// The value as a path list (empty when the value is missing).
    pub fn entries(&self) -> Vec<String> {
        split_path_list(&self.value)
    }
}

/// Splits a PATH-list string, trimming each entry and dropping empties.
pub fn split_path_list(value: &str) -> Vec<String> {
    value
        .split(';')
        .map(str::trim)
        .filter(|entry| !entry.is_empty())
        .map(str::to_string)
        .collect()
}

/// Normalises an entry for comparison: trimmed, forward slashes converted,
/// trailing separators dropped.
///
/// A drive root keeps its separator (`C:\` and `C:` are different things:
/// `C:` is a drive-relative path, not the drive root).
pub fn normalize_entry(entry: &str) -> String {
    let trimmed = entry.trim();
    let body = trimmed.trim_end_matches(['\\', '/']);
    if body.len() == 2 && body.as_bytes()[1] == b':' && trimmed.ends_with(['\\', '/']) {
        format!("{body}\\")
    } else {
        body.replace('/', "\\")
    }
}

/// Expands `%VAR%` references against the current process environment
/// (unknown variables are left untouched), so a PATH entry written with
/// environment references still matches its literal form when comparing.
fn expand_env_vars(entry: &str) -> String {
    if !entry.contains('%') {
        return entry.to_string();
    }
    let mut out = String::with_capacity(entry.len());
    let mut rest = entry;
    while let Some(start) = rest.find('%') {
        out.push_str(&rest[..start]);
        let after = &rest[start + 1..];
        match after.find('%') {
            Some(end) if end > 0 => {
                let name = &after[..end];
                match std::env::var(name) {
                    Ok(value) => out.push_str(&value),
                    Err(_) => {
                        out.push('%');
                        out.push_str(name);
                        out.push('%');
                    }
                }
                rest = &after[end + 1..];
            }
            _ => {
                out.push('%');
                out.push_str(after);
                rest = "";
            }
        }
    }
    out.push_str(rest);
    out
}

/// Case-insensitive comparison of two PATH entries, ignoring trailing
/// slashes, slash direction and `%VAR%` spelling differences.
pub fn same_entry(a: &str, b: &str) -> bool {
    normalize_entry(&expand_env_vars(a)).eq_ignore_ascii_case(&normalize_entry(&expand_env_vars(b)))
}

/// Is `dir` (case-insensitively) part of the PATH list `value`?
pub fn path_contains(value: &str, dir: &str) -> bool {
    let wanted = normalize_entry(dir);
    !wanted.is_empty()
        && split_path_list(value)
            .iter()
            .any(|e| same_entry(e, &wanted))
}

/// Does `earlier` come before `later` in the PATH list?
pub fn dir_precedes(list: &str, earlier: &str, later: &str) -> bool {
    let earlier = normalize_entry(earlier);
    let later = normalize_entry(later);
    let mut saw_earlier = false;
    for entry in split_path_list(list) {
        if same_entry(&entry, &later) {
            return saw_earlier;
        }
        if same_entry(&entry, &earlier) {
            saw_earlier = true;
        }
    }
    false
}

/// Rebuilds `current` with `dirs` first, in order, and every occurrence of
/// them removed from the remainder. Returns the new list and whether it
/// differs from `current`.
pub fn ensure_dirs_first(current: &str, dirs: &[PathBuf]) -> (String, bool) {
    let mut wanted: Vec<String> = Vec::new();
    for dir in dirs {
        let entry = normalize_entry(&dir.to_string_lossy());
        if entry.is_empty() || wanted.iter().any(|e| e.eq_ignore_ascii_case(&entry)) {
            continue;
        }
        wanted.push(entry);
    }
    let mut out = wanted.clone();
    for entry in split_path_list(current) {
        // Only Glue's own entries are filtered/moved. Unrelated entries are
        // kept byte-for-byte (not even de-duplicated or renormalised): a PATH
        // edit must not silently rewrite the rest of the user's list.
        if wanted.iter().any(|w| same_entry(w, &entry)) {
            continue;
        }
        out.push(entry);
    }
    let next = out.join(";");
    (next.clone(), next != current)
}

/// Rebuilds `current` without any entry matching `dirs`. Returns the new list
/// and whether it differs.
pub fn remove_dirs(current: &str, dirs: &[PathBuf]) -> (String, bool) {
    let kept: Vec<String> = split_path_list(current)
        .into_iter()
        .filter(|entry| {
            !dirs
                .iter()
                .any(|dir| same_entry(entry, &dir.to_string_lossy()))
        })
        .collect();
    let next = kept.join(";");
    (next.clone(), next != current)
}

/// The per-user Microsoft Store app-alias directory.
pub fn windows_apps_dir() -> Option<PathBuf> {
    std::env::var_os("LOCALAPPDATA")
        .filter(|v| !v.is_empty())
        .map(|v| PathBuf::from(v).join("Microsoft").join("WindowsApps"))
}

/// Removes the Store `python.exe`/`python3.exe`/`pythonw.exe` app-execution
/// aliases (0-byte stubs) so Glue's shims are what `python` resolves to.
pub fn disable_windows_python_aliases() {
    // The file backend means "do not touch this machine" (tests/sandboxes).
    if path_file().is_some() {
        return;
    }
    let Some(apps) = windows_apps_dir() else {
        return;
    };
    for name in ["python.exe", "python3.exe", "pythonw.exe"] {
        let path = apps.join(name);
        let Ok(meta) = std::fs::symlink_metadata(&path) else {
            continue;
        };
        if meta.is_dir() || meta.len() > 1024 {
            continue;
        }
        let _ = std::fs::remove_file(&path);
    }
}

/// Prepends `dirs` to this process's `PATH` so child processes launched from
/// the same command see them.
pub fn prepend_dirs_to_process_path(dirs: &[PathBuf]) {
    let current = std::env::var("PATH").unwrap_or_default();
    let (next, changed) = ensure_dirs_first(&current, dirs);
    if changed {
        std::env::set_var("PATH", next);
    }
}

/// Directory containing the running `glue.exe` (the `bin` directory of an
/// installed tree).
pub fn current_exe_dir() -> Option<PathBuf> {
    std::env::current_exe()
        .ok()
        .and_then(|exe| exe.parent().map(Path::to_path_buf))
}

/// Environment variable redirecting PATH reads/writes to a file.
///
/// Used by integration tests and sandboxes that must not touch the real
/// `HKCU\Environment\Path` (the same idea as `GLUE_WORKSPACES_FILE`). The file
/// holds the raw PATH value and is treated as `REG_SZ`.
pub const PATH_FILE_ENV: &str = "GLUE_PATH_FILE";

/// The file backend, when [`PATH_FILE_ENV`] is set.
fn path_file() -> Option<PathBuf> {
    std::env::var_os(PATH_FILE_ENV)
        .filter(|value| !value.is_empty())
        .map(PathBuf::from)
}

/// Reads the user PATH (file backend when [`PATH_FILE_ENV`] is set).
pub fn user_path() -> Result<UserPath, String> {
    if let Some(file) = path_file() {
        return match std::fs::read_to_string(&file) {
            Ok(value) => Ok(UserPath {
                value,
                kind: PathKind::Sz,
                exists: true,
            }),
            Err(err) if err.kind() == std::io::ErrorKind::NotFound => Ok(UserPath {
                value: String::new(),
                kind: PathKind::Sz,
                exists: false,
            }),
            Err(err) => Err(format!("read {}: {err}", file.display())),
        };
    }
    native_user_path()
}

/// Writes the user PATH (file backend when [`PATH_FILE_ENV`] is set).
pub fn set_user_path(value: &str, kind: PathKind) -> Result<(), String> {
    if let Some(file) = path_file() {
        if let Some(dir) = file.parent() {
            std::fs::create_dir_all(dir)
                .map_err(|err| format!("create {}: {err}", dir.display()))?;
        }
        return std::fs::write(&file, value)
            .map_err(|err| format!("write {}: {err}", file.display()));
    }
    native_set_user_path(value, kind)
}

/// Deletes the user PATH value (file backend when [`PATH_FILE_ENV`] is set).
pub fn delete_user_path() -> Result<(), String> {
    if let Some(file) = path_file() {
        return match std::fs::remove_file(&file) {
            Ok(()) => Ok(()),
            Err(err) if err.kind() == std::io::ErrorKind::NotFound => Ok(()),
            Err(err) => Err(format!("delete {}: {err}", file.display())),
        };
    }
    native_delete_user_path()
}

/// Tells Explorer and friends that the environment changed, so new processes
/// pick up the new PATH without a logoff. A no-op for the file backend.
pub fn broadcast_environment_change() {
    if path_file().is_some() {
        return;
    }
    native_broadcast_environment_change();
}

/// Reads the user PATH from the registry.
#[cfg(windows)]
fn native_user_path() -> Result<UserPath, String> {
    win::read_user_path()
}

/// Reads the user PATH from the registry (non-Windows stub: empty).
#[cfg(not(windows))]
fn native_user_path() -> Result<UserPath, String> {
    Ok(UserPath {
        value: String::new(),
        kind: PathKind::Sz,
        exists: false,
    })
}

/// Writes the user PATH, preserving the registry type.
#[cfg(windows)]
fn native_set_user_path(value: &str, kind: PathKind) -> Result<(), String> {
    win::write_user_path(value, kind)
}

/// Writes the user PATH (non-Windows stub: no-op).
#[cfg(not(windows))]
fn native_set_user_path(_value: &str, _kind: PathKind) -> Result<(), String> {
    Ok(())
}

/// Deletes the user PATH value entirely.
#[cfg(windows)]
fn native_delete_user_path() -> Result<(), String> {
    win::delete_user_path()
}

/// Deletes the user PATH value entirely (non-Windows stub: no-op).
#[cfg(not(windows))]
fn native_delete_user_path() -> Result<(), String> {
    Ok(())
}

/// Broadcasts `WM_SETTINGCHANGE` (Windows only).
#[cfg(windows)]
fn native_broadcast_environment_change() {
    win::broadcast_environment_change();
}

/// Broadcasts `WM_SETTINGCHANGE` (non-Windows: no-op).
#[cfg(not(windows))]
fn native_broadcast_environment_change() {}

#[cfg(windows)]
mod win {
    use std::ptr;

    use windows_sys::Win32::Foundation::{
        ERROR_FILE_NOT_FOUND, ERROR_MORE_DATA, ERROR_SUCCESS, HWND, LPARAM, WPARAM,
    };
    use windows_sys::Win32::System::Registry::{
        RegCloseKey, RegDeleteValueW, RegOpenKeyExW, RegQueryValueExW, RegSetValueExW, HKEY,
        HKEY_CURRENT_USER, KEY_QUERY_VALUE, KEY_SET_VALUE, REG_EXPAND_SZ, REG_SZ,
    };
    use windows_sys::Win32::UI::WindowsAndMessaging::{
        SendMessageTimeoutW, HWND_BROADCAST, SMTO_ABORTIFHUNG, WM_SETTINGCHANGE,
    };

    use super::{PathKind, UserPath};

    /// An open registry key that closes itself on drop.
    struct RegKey(HKEY);

    impl Drop for RegKey {
        fn drop(&mut self) {
            // SAFETY: the handle came from a successful `RegOpenKeyExW`.
            unsafe { RegCloseKey(self.0) };
        }
    }

    fn open(access: u32) -> Result<RegKey, String> {
        let subkey = to_wide(super::USER_ENV_KEY);
        let mut handle: HKEY = ptr::null_mut();
        // SAFETY: `handle` is a valid out-pointer and `subkey` is
        // NUL-terminated and outlives the call.
        let rc =
            unsafe { RegOpenKeyExW(HKEY_CURRENT_USER, subkey.as_ptr(), 0, access, &mut handle) };
        if rc != ERROR_SUCCESS {
            return Err(format!("open user Environment key: registry error {rc}"));
        }
        Ok(RegKey(handle))
    }

    pub(super) fn read_user_path() -> Result<UserPath, String> {
        let key = open(KEY_QUERY_VALUE)?;
        let name = to_wide(super::PATH_VALUE);
        let mut kind = 0u32;
        let mut size: u32 = 0;
        // SAFETY: a NULL data pointer asks for the required byte size only.
        let rc = unsafe {
            RegQueryValueExW(
                key.0,
                name.as_ptr(),
                ptr::null_mut(),
                &mut kind,
                ptr::null_mut(),
                &mut size,
            )
        };
        if rc == ERROR_FILE_NOT_FOUND {
            return Ok(UserPath {
                value: String::new(),
                kind: PathKind::Sz,
                exists: false,
            });
        }
        if rc != ERROR_SUCCESS {
            return Err(format!("read user PATH size: registry error {rc}"));
        }

        // PATH can legitimately exceed 32 KiB; grow if another writer raced us.
        for _ in 0..4 {
            let mut buffer = vec![0u16; size as usize / 2 + 1];
            let mut capacity = (buffer.len() * 2) as u32;
            let mut actual_kind = kind;
            // SAFETY: `buffer` is a writable UTF-16 buffer of `capacity`
            // bytes, as the API expects for a REG_SZ/REG_EXPAND_SZ value.
            let rc = unsafe {
                RegQueryValueExW(
                    key.0,
                    name.as_ptr(),
                    ptr::null_mut(),
                    &mut actual_kind,
                    buffer.as_mut_ptr() as *mut u8,
                    &mut capacity,
                )
            };
            if rc == ERROR_MORE_DATA {
                size = capacity;
                continue;
            }
            if rc != ERROR_SUCCESS {
                return Err(format!("read user PATH: registry error {rc}"));
            }
            kind = actual_kind;
            buffer.truncate(capacity as usize / 2);
            while buffer.last() == Some(&0) {
                buffer.pop();
            }
            return Ok(UserPath {
                value: String::from_utf16_lossy(&buffer),
                kind: kind_from_registry(kind),
                exists: true,
            });
        }
        Err("read user PATH: value kept growing".to_string())
    }

    pub(super) fn write_user_path(value: &str, kind: PathKind) -> Result<(), String> {
        let key = open(KEY_SET_VALUE)?;
        let name = to_wide(super::PATH_VALUE);
        let data = to_wide(value);
        // SAFETY: `data` is a NUL-terminated UTF-16 buffer that outlives the
        // call; `key` is a valid open handle.
        let rc = unsafe {
            RegSetValueExW(
                key.0,
                name.as_ptr(),
                0,
                registry_from_kind(kind),
                data.as_ptr() as *const u8,
                (data.len() * 2) as u32,
            )
        };
        if rc != ERROR_SUCCESS {
            return Err(format!("write user PATH: registry error {rc}"));
        }
        Ok(())
    }

    pub(super) fn delete_user_path() -> Result<(), String> {
        let key = open(KEY_SET_VALUE)?;
        let name = to_wide(super::PATH_VALUE);
        // SAFETY: `key` is a valid open handle and `name` is NUL-terminated.
        let rc = unsafe { RegDeleteValueW(key.0, name.as_ptr()) };
        if rc != ERROR_SUCCESS && rc != ERROR_FILE_NOT_FOUND {
            return Err(format!("delete user PATH: registry error {rc}"));
        }
        Ok(())
    }

    pub(super) fn broadcast_environment_change() {
        let target = to_wide("Environment");
        let mut result: usize = 0;
        // SAFETY: HWND_BROADCAST needs no window handle, the string outlives
        // the call, and `result` is a valid out-pointer. Failures (no
        // Explorer running, timeout) are non-fatal.
        unsafe {
            SendMessageTimeoutW(
                HWND_BROADCAST as HWND,
                WM_SETTINGCHANGE,
                0 as WPARAM,
                target.as_ptr() as LPARAM,
                SMTO_ABORTIFHUNG,
                5000,
                &mut result,
            );
        }
    }

    fn kind_from_registry(kind: u32) -> PathKind {
        if kind == REG_EXPAND_SZ {
            PathKind::ExpandSz
        } else {
            PathKind::Sz
        }
    }

    fn registry_from_kind(kind: PathKind) -> u32 {
        match kind {
            PathKind::Sz => REG_SZ,
            PathKind::ExpandSz => REG_EXPAND_SZ,
        }
    }

    fn to_wide(s: &str) -> Vec<u16> {
        s.encode_utf16().chain(std::iter::once(0)).collect()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn p(s: &str) -> PathBuf {
        PathBuf::from(s)
    }

    #[test]
    fn split_keeps_entries_and_drops_empties() {
        assert_eq!(split_path_list(r"C:\A;; C:\B ;"), vec![r"C:\A", r"C:\B"]);
        assert!(split_path_list("").is_empty());
    }

    #[test]
    fn normalize_keeps_drive_roots_but_drops_other_trailing_slashes() {
        assert_eq!(normalize_entry(r" C:\Tools\ "), r"C:\Tools");
        assert_eq!(normalize_entry(r"C:\"), r"C:\");
        assert_eq!(normalize_entry(r"\\server\share\"), r"\\server\share");
        // `C:` is a drive-relative path, NOT the drive root — it must not be
        // "normalised" into `C:\`.
        assert_eq!(normalize_entry("C:"), "C:");
        // Forward slashes are unified so both spellings compare equal.
        assert_eq!(normalize_entry("C:/Tools/"), r"C:\Tools");
    }

    #[test]
    fn same_entry_matches_across_slashes_and_env_vars() {
        assert!(same_entry(r"C:\Glue\data\shims", "C:/Glue/data/shims/"));
        let home = std::env::var("USERPROFILE").unwrap();
        assert!(same_entry(
            r"%USERPROFILE%\.glue\shims",
            &format!(r"{home}\.glue\shims")
        ));
        // Unknown variables stay literal and simply do not match.
        assert!(!same_entry(r"%GLUE_NO_SUCH_VAR%\shims", r"C:\x\shims"));
    }

    #[test]
    fn path_contains_is_case_insensitive() {
        assert!(path_contains(r"C:\Windows;c:\tools\", r"C:\Tools"));
        assert!(!path_contains(r"C:\Windows", r"C:\Tools"));
        assert!(!path_contains("", ""));
    }

    #[test]
    fn ensure_dirs_first_moves_them_and_keeps_unrelated_entries() {
        let dirs = vec![p(r"C:\Glue\data\shims"), p(r"C:\Glue\bin")];
        let (next, changed) = ensure_dirs_first(
            r"C:\A;c:\glue\bin\;C:\B;C:\Glue\data\shims;C:\A;C:\B\",
            &dirs,
        );
        // Glue's entries are moved to the front and de-duplicated; everything
        // else survives untouched, including a repeated entry and a trailing
        // backslash (a PATH edit must not spring-clean the user's list).
        assert_eq!(next, r"C:\Glue\data\shims;C:\Glue\bin;C:\A;C:\B;C:\A;C:\B\");
        assert!(changed);
    }

    #[test]
    fn ensure_dirs_first_is_idempotent() {
        let dirs = vec![p(r"C:\Glue\data\shims"), p(r"C:\Glue\bin")];
        let current = r"C:\Glue\data\shims;C:\Glue\bin;C:\A";
        let (next, changed) = ensure_dirs_first(current, &dirs);
        assert_eq!(next, current);
        assert!(!changed);
    }

    #[test]
    fn ensure_dirs_first_creates_a_path_from_nothing() {
        let (next, changed) = ensure_dirs_first("", &[p(r"C:\Glue\bin")]);
        assert_eq!(next, r"C:\Glue\bin");
        assert!(changed);
    }

    #[test]
    fn remove_dirs_is_idempotent_and_preserves_order() {
        let dirs = vec![p(r"C:\Glue\data\shims"), p(r"C:\Glue\bin")];
        let (next, changed) = remove_dirs(r"C:\Glue\data\shims;C:\A;c:\GLUE\BIN\;C:\B", &dirs);
        assert_eq!(next, r"C:\A;C:\B");
        assert!(changed);

        let (again, changed_again) = remove_dirs(&next, &dirs);
        assert_eq!(again, next);
        assert!(!changed_again);
    }

    #[test]
    fn remove_dirs_from_a_single_entry_yields_empty() {
        let (next, changed) = remove_dirs(r"C:\Glue\bin", &[p(r"C:\Glue\bin")]);
        assert_eq!(next, "");
        assert!(changed);
    }

    #[test]
    fn dir_precedes_matches_go_semantics() {
        let list = r"C:\WindowsApps;C:\Glue\data\shims;C:\A";
        assert!(dir_precedes(list, r"C:\WindowsApps", r"C:\Glue\data\shims"));
        assert!(!dir_precedes(
            list,
            r"C:\Glue\data\shims",
            r"C:\WindowsApps"
        ));
        assert!(!dir_precedes(list, r"C:\Missing", r"C:\A"));
        // `later` absent: returns false.
        assert!(!dir_precedes(list, r"C:\WindowsApps", r"C:\Nope"));
    }
}
