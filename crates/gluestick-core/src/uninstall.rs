//! Removing the per-user installation that the installer created.
//!
//! The installer writes two things outside its own directory: the user `PATH`
//! entries (through `glue path setup`) and the per-user uninstall entry in
//! `HKCU`. This module owns the reverse of the entry itself — deleting it,
//! deleting the files, and the self-delete dance — while the CLI owns the PATH
//! half, because `glue path remove` already knows exactly which directories it
//! added.
//!
//! The workspace is *not* part of the installation: it holds installed
//! packages and user data. Everything here keeps it unless [`Plan::purge`] is
//! set, which mirrors `--purge`.
//!
//! Self-delete: a running `glue.exe` cannot delete itself on Windows. The
//! installer therefore copies the executable into a staging directory under
//! `%TEMP%` and lets that copy delete the installation after this process has
//! exited ([`spawn_self_delete`] / [`run_self_delete`]).

use std::path::{Path, PathBuf};

/// The registry key holding every per-user uninstall entry.
const UNINSTALL_PARENT: &str = r"Software\Microsoft\Windows\CurrentVersion\Uninstall";

/// The uninstall entry the installer writes, and Apps & Features reads.
pub const UNINSTALL_KEY: &str =
    r"Software\Microsoft\Windows\CurrentVersion\Uninstall\Gluestick CLI";

/// Entries written by the legacy NSIS installer, one per architecture.
pub const LEGACY_UNINSTALL_KEYS: [&str; 2] = [
    r"Software\Microsoft\Windows\CurrentVersion\Uninstall\Gluestick CLI amd64",
    r"Software\Microsoft\Windows\CurrentVersion\Uninstall\Gluestick CLI arm64",
];

/// The marker file the installer writes; the installation's identity.
const INSTALL_MARKER: &str = ".glue-install";
const INSTALL_EXE: &str = "glue.exe";

/// How long the staging copy waits for the uninstalling process to exit.
///
/// Generous: the process may be waiting on a locked shim or an antivirus scan.
/// If it never exits, nothing is deleted and the entry stays, which is the safe
/// direction — a half-removed installation is worse than an unremoved one.
const WAIT_FOR_EXIT_SECS: u64 = 60;

/// What an uninstall removes.
#[derive(Debug, Clone)]
pub struct Plan {
    /// Directory holding `glue.exe` (the installation).
    pub install_dir: PathBuf,
    /// The workspace; kept unless `purge`.
    pub data_root: PathBuf,
    /// Delete the workspace too.
    pub purge: bool,
}

impl Plan {
    pub fn new(
        install_dir: impl Into<PathBuf>,
        data_root: impl Into<PathBuf>,
        purge: bool,
    ) -> Plan {
        Plan {
            install_dir: install_dir.into(),
            data_root: data_root.into(),
            purge,
        }
    }

    /// Whether the workspace is the installation directory itself.
    ///
    /// It normally is not: both installers point `data-root.txt` at the shared
    /// `%USERPROFILE%\.glue`. The two can still match when that file is missing
    /// (the workspace then falls back to the installation directory) or when an
    /// older installation still keeps its data beside the binaries.
    pub fn data_is_install_dir(&self) -> bool {
        same_path(&self.install_dir, &self.data_root)
    }
}

/// What [`remove_install_files`] actually did, for the summary line.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct Removed {
    /// Top-level entries deleted from the installation directory.
    pub entries: usize,
    /// The workspace was left in place.
    pub data_kept: bool,
    /// The workspace was deleted (`--purge`).
    pub data_removed: bool,
    /// Workspace files that could not be deleted because a running program
    /// holds them. Non-empty means `--purge` only partly succeeded.
    pub locked: Vec<PathBuf>,
}

/// Whether `dir` is an installation: `glue.exe` beside the installer's marker.
///
/// Deliberately the same test [`crate::paths::install_root_for`] uses, so the
/// uninstaller can only ever remove a tree the rest of the CLI recognises as an
/// installation. On its own this is **not** enough to justify deleting the
/// directory: a `cargo run` in a checkout produces `target\debug\glue.exe`,
/// which satisfies the first half. See [`is_managed_installation`] for the check
/// that gates removal.
pub fn is_installation(dir: &Path) -> bool {
    dir.join(INSTALL_EXE).is_file() && dir.join(INSTALL_MARKER).is_file()
}

/// Whether `dir` is a *managed* installation, i.e. one this machine's uninstall
/// entry points at.
///
/// This is the gate that keeps an uninstall from removing a source checkout
/// without an explicit path. A development build under `target\<profile>\`
/// looks exactly like an installation to [`is_installation`], so the layout
/// check alone would let an uninstall wipe a source checkout. Requiring the
/// registry entry means only a directory the installer deliberately registered
/// can be removed by default; anything else needs an explicit install
/// directory.
pub fn is_managed_installation(dir: &Path) -> bool {
    is_installation(dir)
        && registered_install_dir()
            .as_deref()
            .is_some_and(|registered| same_path(registered, dir))
}

/// Case-insensitive path comparison with trailing separators and `/` normalized.
pub fn same_path(a: &Path, b: &Path) -> bool {
    fn key(path: &Path) -> String {
        let text = path.to_string_lossy().replace('/', "\\");
        let text = text
            .trim_end_matches('\\')
            .trim_end_matches("/.")
            .to_string();
        text.to_lowercase()
    }
    key(a) == key(b)
}

/// Whether `path` is `dir` or lives inside it.
///
/// Used to warn before an uninstall: Windows cannot remove a directory that a
/// running process uses as its current directory, so the shell's own folder
/// would survive the removal.
pub fn is_inside(path: &Path, dir: &Path) -> bool {
    let key = |path: &Path| {
        path.to_string_lossy()
            .replace('/', "\\")
            .trim_end_matches('\\')
            .to_lowercase()
    };
    let path = key(path);
    let dir = key(dir);
    !dir.is_empty() && (path == dir || path.starts_with(&format!("{dir}\\")))
}

/// The version recorded in the current uninstall entry.
pub fn registered_version() -> Option<String> {
    read_string(UNINSTALL_KEY, "DisplayVersion")
}

/// The installation the uninstall entry points at, if it has one.
pub fn registered_install_dir() -> Option<PathBuf> {
    let location = read_string(UNINSTALL_KEY, "InstallLocation")?;
    let trimmed = location.trim();
    if trimmed.is_empty() {
        None
    } else {
        Some(PathBuf::from(trimmed))
    }
}

/// Every Gluestick installation registered in this user's uninstall entries.
///
/// Both installers (CLI and desktop, plus their architecture-suffixed legacy
/// keys) are found by name prefix. A directory counts only while it still holds
/// `glue.exe` and the installer's marker, so an entry left behind by a manual
/// delete does not point at something that is no longer there.
pub fn registered_installation_dirs() -> Vec<PathBuf> {
    let mut dirs: Vec<PathBuf> = Vec::new();
    for name in win::subkeys(UNINSTALL_PARENT) {
        if !name.to_ascii_lowercase().starts_with("gluestick") {
            continue;
        }
        let key = format!(r"{UNINSTALL_PARENT}\{name}");
        let Some(location) = win::read_string(&key, "InstallLocation") else {
            continue;
        };
        let dir = PathBuf::from(location.trim());
        if !is_installation(&dir) || dirs.iter().any(|known| same_path(known, &dir)) {
            continue;
        }
        dirs.push(dir);
    }
    dirs
}

/// Deletes the uninstall entry. `Ok(false)` means there was nothing to delete.
pub fn remove_uninstall_entry() -> Result<bool, String> {
    delete_key(UNINSTALL_KEY)
}

/// Deletes legacy NSIS entries whose `InstallLocation` is this installation.
///
/// A legacy entry that points somewhere else is left alone: that installation
/// still exists and its own uninstaller is the one that should run.
pub fn prune_legacy_entries(install: &Path) -> Result<Vec<String>, String> {
    let mut removed = Vec::new();
    for key in LEGACY_UNINSTALL_KEYS {
        let Some(location) = read_string(key, "InstallLocation") else {
            continue;
        };
        if !same_path(Path::new(location.trim()), install) {
            continue;
        }
        if delete_key(key)? {
            removed.push(key.to_string());
        }
    }
    Ok(removed)
}

/// Deletes the installation's files.
///
/// The caller must have exited the running `glue.exe` first — that is what
/// [`run_self_delete`] waits for. The workspace is kept unless [`Plan::purge`],
/// and a workspace living outside the installation directory is only ever
/// touched by `--purge`.
///
/// The installation and the workspace are removed in two separate passes, and
/// only the first is fatal. A workspace holds installed applications, so a
/// program running out of it — including the very tool the user is talking to —
/// locks files that `--purge` cannot remove. Treating that as a failure of the
/// whole uninstall would leave `glue.exe` in place, which is the opposite of
/// what the user asked for. Instead the locked files are reported and the
/// installation still goes.
pub fn remove_install_files(plan: &Plan) -> Result<Removed, String> {
    if !plan.install_dir.is_dir() {
        return Err(format!("{} is not a directory", plan.install_dir.display()));
    }

    let mut removed = Removed::default();

    if plan.data_is_install_dir() {
        // The usual case since the workspace moved up beside the binaries: the
        // installation directory *is* the workspace, so a blanket delete would
        // take the packages with it. Delete the known installation files by
        // name, then hand the rest to the workspace pass.
        remove_known_install_files(&plan.install_dir, &mut removed)?;
        if plan.purge {
            remove_workspace(&plan.install_dir, &mut removed);
            removed.data_removed = removed.locked.is_empty();
        } else {
            removed.data_kept = true;
        }
        return Ok(removed);
    }

    // The workspace points elsewhere, so everything in the installation
    // directory belongs to the installation.
    let entries = std::fs::read_dir(&plan.install_dir)
        .map_err(|e| format!("read {}: {e}", plan.install_dir.display()))?;
    for entry in entries {
        let entry = entry.map_err(|e| format!("read {}: {e}", plan.install_dir.display()))?;
        remove_tree(&entry.path())?;
        removed.entries += 1;
    }

    if plan.purge && plan.data_root.is_dir() {
        remove_workspace(&plan.data_root, &mut removed);
        removed.data_removed = removed.locked.is_empty();
    } else {
        removed.data_kept = plan.data_root.is_dir();
    }

    // An emptied installation directory should not outlive its contents.
    let _ = std::fs::remove_dir(&plan.install_dir);

    Ok(removed)
}

/// The files the installer puts in an installation directory.
///
/// Kept as an allowlist rather than "everything except the workspace": with the
/// workspace in the same directory there is no way to tell a package store from
/// an installation file by position, and deleting a name nobody listed is how an
/// uninstall would eat user data. New installation files must be added here.
const INSTALL_FILES: [&str; 4] = [INSTALL_EXE, "shim.exe", "Uninstall.exe", INSTALL_MARKER];

/// Files that belong to the program without being binaries: the installer's
/// bookkeeping and the program's own configuration.
///
/// `config.json` is here because configuration is program-scoped: it lives
/// beside `glue.exe`, not in the shared workspace, so an uninstall removes it
/// with the program it describes.
const INSTALL_AUX_FILES: [&str; 4] = [
    crate::config::CONFIG_FILE,
    "data-root.txt",
    "install.log",
    "uninstall-error.log",
];

/// Deletes the installation's own files, leaving the workspace in place.
fn remove_known_install_files(dir: &Path, removed: &mut Removed) -> Result<(), String> {
    for name in INSTALL_FILES {
        let path = dir.join(name);
        if path.exists() {
            match remove_tree(&path) {
                Ok(()) => removed.entries += 1,
                Err(error) => {
                    // `glue.exe` is the installation; anything else (the shim,
                    // marker or Uninstall.exe) may be held by a running process
                    // without preventing the rest of the removal.
                    if name == INSTALL_EXE {
                        return Err(error);
                    }
                }
            }
        }
    }
    // The binaries are the installation: if `glue.exe` is still there afterwards
    // the removal failed, and saying so beats reporting a half-install.
    if dir.join(INSTALL_EXE).exists() {
        return Err(format!(
            "could not delete {}",
            dir.join(INSTALL_EXE).display()
        ));
    }
    for name in INSTALL_AUX_FILES {
        let path = dir.join(name);
        if path.is_file() {
            let _ = std::fs::remove_file(path);
        }
    }
    Ok(())
}

/// Deletes a workspace, recording whatever a running program refused to release.
fn remove_workspace(workspace: &Path, removed: &mut Removed) {
    let mut locked = Vec::new();
    remove_collecting(workspace, &mut locked);
    if !locked.is_empty() {
        removed.data_removed = false;
        removed.locked.extend(locked);
    }
}

/// Removes `path`, gathering the individual files that could not be deleted.
///
/// `remove_dir_all` reports only the directory it gave up on, which tells a user
/// nothing when the culprit is a binary three levels down. When the bulk delete
/// fails this retries entry by entry, so the report can name the locked file.
fn remove_collecting(path: &Path, locked: &mut Vec<PathBuf>) {
    if try_remove(path).is_ok() {
        return;
    }
    if !path.is_dir() {
        locked.push(path.to_path_buf());
        return;
    }

    let before = locked.len();
    let Ok(entries) = std::fs::read_dir(path) else {
        locked.push(path.to_path_buf());
        return;
    };
    let children: Vec<PathBuf> = entries.flatten().map(|entry| entry.path()).collect();
    for child in &children {
        remove_collecting(child, locked);
    }
    // Everything inside went, so the directory itself should go too. Failing
    // now means something holds the directory handle itself; only report that
    // when no child already explained the failure.
    if try_remove(path).is_err() && locked.len() == before {
        locked.push(path.to_path_buf());
    }
}

fn remove_tree(path: &Path) -> Result<(), String> {
    try_remove(path).map_err(|e| format!("remove {}: {e}", path.display()))
}

/// Deletes a file or a directory tree, treating "already gone" as success.
fn try_remove(path: &Path) -> std::io::Result<()> {
    let result = if path.is_dir() {
        std::fs::remove_dir_all(path)
    } else {
        std::fs::remove_file(path)
    };
    match result {
        Ok(()) => Ok(()),
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(()),
        Err(e) => Err(e),
    }
}

/// Copies this executable into `%TEMP%` and starts the cleanup it will run once
/// this process exits.
///
/// Returns the staging directory, which the copy deletes when it is done.
///
/// A helper that exits immediately — a wrong subcommand name, a broken copy —
/// would otherwise fail silently, after the caller has already told the user
/// the files are being deleted. The helper's output therefore goes to a log
/// beside it, and a short probe reports an early exit as an error.
pub fn spawn_self_delete(plan: &Plan) -> Result<PathBuf, String> {
    let exe = std::env::current_exe().map_err(|e| format!("locate the running glue.exe: {e}"))?;
    let pid = std::process::id();

    let stage = std::env::temp_dir().join(format!("glue-uninstall-{pid}-{}", nonce()));
    std::fs::create_dir_all(&stage).map_err(|e| format!("create {}: {e}", stage.display()))?;
    let copy = stage.join(INSTALL_EXE);
    std::fs::copy(&exe, &copy)
        .map_err(|e| format!("copy {} to {}: {e}", exe.display(), copy.display()))?;

    let log_path = stage.join("finalize.log");
    let log = std::fs::File::create(&log_path)
        .map_err(|e| format!("create {}: {e}", log_path.display()))?;
    let log_err = log
        .try_clone()
        .map_err(|e| format!("open {}: {e}", log_path.display()))?;

    let mut command = std::process::Command::new(&copy);
    command
        .arg("self")
        .arg("finalize")
        .arg("--wait-pid")
        .arg(pid.to_string())
        .arg("--install-dir")
        .arg(&plan.install_dir)
        .arg("--data-root")
        .arg(&plan.data_root);
    if plan.purge {
        command.arg("--purge");
    }
    command
        .stdin(std::process::Stdio::null())
        .stdout(std::process::Stdio::from(log))
        .stderr(std::process::Stdio::from(log_err));
    // Detach: the copy must outlive this process and must not share (or wait
    // on) the console the user is looking at.
    detach(&mut command);

    let mut child = command.spawn().map_err(|e| {
        let _ = std::fs::remove_dir_all(&stage);
        format!("start {}: {e}", copy.display())
    })?;

    // A helper that is still running after this window is healthy: it is
    // waiting for this process to exit. One that has already exited failed
    // before it could do anything.
    if let Some(status) = wait_briefly(&mut child, std::time::Duration::from_millis(750)) {
        let output = std::fs::read_to_string(&log_path).unwrap_or_default();
        let detail = output.trim();
        let _ = std::fs::remove_dir_all(&stage);
        return Err(if detail.is_empty() {
            format!("the file-removal helper exited immediately ({status})")
        } else {
            format!("the file-removal helper failed ({status}): {detail}")
        });
    }

    Ok(stage)
}

/// Waits up to `limit` for `child` to exit, returning its status if it did.
fn wait_briefly(
    child: &mut std::process::Child,
    limit: std::time::Duration,
) -> Option<std::process::ExitStatus> {
    let deadline = std::time::Instant::now() + limit;
    loop {
        match child.try_wait() {
            Ok(Some(status)) => return Some(status),
            Ok(None) => {
                if std::time::Instant::now() >= deadline {
                    return None;
                }
                std::thread::sleep(std::time::Duration::from_millis(25));
            }
            // The status is unavailable (not "still running"): treat it as a
            // failure so the caller does not claim a removal that never began.
            Err(_) => return None,
        }
    }
}

/// The staging side of [`spawn_self_delete`]: wait for the uninstalling process
/// to exit, then delete the installation and the staging directory.
pub fn run_self_delete(wait_pid: u32, plan: &Plan, stage: &Path) -> Result<Removed, String> {
    if !wait_for_exit(wait_pid, std::time::Duration::from_secs(WAIT_FOR_EXIT_SECS)) {
        return Err(format!(
            "process {wait_pid} is still running after {WAIT_FOR_EXIT_SECS}s; nothing was deleted"
        ));
    }
    let removed = remove_install_files(plan);
    // Always remove the staging copy, even when the removal above failed: it
    // holds a full glue.exe and nothing uses it after this process exits.
    schedule_staging_cleanup(stage);
    removed
}

/// A value that differs between two runs inside the same process id.
fn nonce() -> u128 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_millis())
        .unwrap_or(0)
}

#[cfg(windows)]
fn detach(command: &mut std::process::Command) {
    use std::os::windows::process::CommandExt;
    /// The copy must not be killed when the parent's console closes, and must
    /// not inherit it — it prints nothing and finishes in under a second.
    const DETACHED_PROCESS: u32 = 0x0000_0008;
    command.creation_flags(DETACHED_PROCESS);
}

#[cfg(not(windows))]
fn detach(_command: &mut std::process::Command) {}

#[cfg(windows)]
fn wait_for_exit(pid: u32, timeout: std::time::Duration) -> bool {
    use windows_sys::Win32::Foundation::CloseHandle;
    use windows_sys::Win32::Storage::FileSystem::SYNCHRONIZE;
    use windows_sys::Win32::System::Threading::{OpenProcess, WaitForSingleObject};

    const WAIT_OBJECT_0: u32 = 0;

    // SAFETY: `OpenProcess` returns a handle owned by this call, closed below.
    // `SYNCHRONIZE` is the least access right that allows waiting. A null
    // handle means the process is already gone, which is the state we want.
    unsafe {
        let handle = OpenProcess(SYNCHRONIZE, 0, pid);
        if handle.is_null() {
            return true;
        }
        let rc = WaitForSingleObject(handle, timeout.as_millis() as u32);
        CloseHandle(handle);
        rc == WAIT_OBJECT_0
    }
}

#[cfg(not(windows))]
fn wait_for_exit(_pid: u32, _timeout: std::time::Duration) -> bool {
    true
}

/// Deletes the staging directory after this process has exited.
///
/// The staging copy cannot delete itself, so it hands the directory to a
/// hidden `cmd` that waits a couple of seconds and then removes it. `ping` is
/// used as the delay, not `timeout`: `timeout` needs a console and fails in a
/// detached process, and it is a `cmd` built-in that reads stdin, so in a
/// windowless child it can hang instead of returning.
///
/// Best effort: a leftover under `%TEMP%` is harmless and the OS cleans it up
/// eventually.
#[cfg(windows)]
fn schedule_staging_cleanup(stage: &Path) {
    use std::os::windows::process::CommandExt;
    /// A hidden console is required for `cmd` to run at all; `DETACHED_PROCESS`
    /// deliberately provides none and breaks the delay. `CREATE_BREAKAWAY_FROM_JOB`
    /// is deliberately *not* used: it fails outright when the caller is in a job
    /// that forbids breakaway, which would silently skip the cleanup.
    const CREATE_NO_WINDOW: u32 = 0x0800_0000;

    let quoted = crate::procutil::cmd_path_arg(stage);
    let mut command = std::process::Command::new("cmd");
    // `raw_arg` bypasses Rust's `\"` escaping, which `cmd.exe` does not
    // understand: the command line handed to it must already be in cmd's own
    // form, with the whole command quoted and the path double-quoted.
    command.raw_arg("/c");
    command.raw_arg(format!(
        "\"ping -n 3 127.0.0.1 >nul & rmdir /s /q \"\"{quoted}\"\"\""
    ));
    command
        .stdin(std::process::Stdio::null())
        .stdout(std::process::Stdio::null())
        .stderr(std::process::Stdio::null())
        .creation_flags(CREATE_NO_WINDOW);
    let _ = command.spawn();
}

#[cfg(not(windows))]
fn schedule_staging_cleanup(stage: &Path) {
    let _ = std::fs::remove_dir_all(stage);
}

#[cfg(windows)]
mod win {
    use std::ptr;

    use windows_sys::Win32::Foundation::{
        ERROR_FILE_NOT_FOUND, ERROR_MORE_DATA, ERROR_PATH_NOT_FOUND, ERROR_SUCCESS,
    };
    use windows_sys::Win32::System::Registry::{
        RegCloseKey, RegDeleteKeyW, RegEnumKeyExW, RegOpenKeyExW, RegQueryValueExW, HKEY,
        HKEY_CURRENT_USER, KEY_READ, REG_EXPAND_SZ, REG_SZ,
    };

    /// An open registry key that closes itself on drop.
    struct RegKey(HKEY);

    impl Drop for RegKey {
        fn drop(&mut self) {
            // SAFETY: the handle came from a successful `RegOpenKeyExW`.
            unsafe { RegCloseKey(self.0) };
        }
    }

    /// Names of the subkeys directly under `parent`.
    pub(super) fn subkeys(parent: &str) -> Vec<String> {
        let parent_wide = super::wide(parent);
        let mut handle: HKEY = ptr::null_mut();
        // SAFETY: `handle` is a valid out-pointer and `parent_wide` is
        // NUL-terminated and outlives the call.
        let rc = unsafe {
            RegOpenKeyExW(
                HKEY_CURRENT_USER,
                parent_wide.as_ptr(),
                0,
                KEY_READ,
                &mut handle,
            )
        };
        if rc != ERROR_SUCCESS {
            return Vec::new();
        }
        let key = RegKey(handle);

        let mut names = Vec::new();
        let mut index = 0u32;
        loop {
            // Registry key names are limited to 255 characters.
            let mut name = [0u16; 256];
            let mut len = name.len() as u32;
            // SAFETY: `name` is writable for `len` UTF-16 units, and every
            // optional out-parameter is NULL.
            let rc = unsafe {
                RegEnumKeyExW(
                    key.0,
                    index,
                    name.as_mut_ptr(),
                    &mut len,
                    ptr::null_mut(),
                    ptr::null_mut(),
                    ptr::null_mut(),
                    ptr::null_mut(),
                )
            };
            // ERROR_NO_MORE_ITEMS (259) ends the enumeration.
            if rc != ERROR_SUCCESS {
                break;
            }
            names.push(String::from_utf16_lossy(&name[..len as usize]));
            index += 1;
        }
        names
    }

    pub(super) fn read_string(subkey: &str, name: &str) -> Option<String> {
        let subkey_wide = super::wide(subkey);
        let mut handle: HKEY = ptr::null_mut();
        // SAFETY: `handle` is a valid out-pointer and `subkey_wide` is
        // NUL-terminated and outlives the call.
        let rc = unsafe {
            RegOpenKeyExW(
                HKEY_CURRENT_USER,
                subkey_wide.as_ptr(),
                0,
                KEY_READ,
                &mut handle,
            )
        };
        if rc != ERROR_SUCCESS {
            return None;
        }
        let key = RegKey(handle);

        let name_wide = super::wide(name);
        let mut kind = 0u32;
        let mut size: u32 = 0;
        // SAFETY: a NULL data pointer asks for the required byte size only.
        let rc = unsafe {
            RegQueryValueExW(
                key.0,
                name_wide.as_ptr(),
                ptr::null(),
                &mut kind,
                ptr::null_mut(),
                &mut size,
            )
        };
        if rc != ERROR_SUCCESS || size == 0 || size > 64 * 1024 {
            return None;
        }
        if kind != REG_SZ && kind != REG_EXPAND_SZ {
            return None;
        }

        let mut data = vec![0u8; size as usize];
        // SAFETY: `data` is writable for `size` bytes, which the call reported.
        let rc = unsafe {
            RegQueryValueExW(
                key.0,
                name_wide.as_ptr(),
                ptr::null(),
                &mut kind,
                data.as_mut_ptr(),
                &mut size,
            )
        };
        // ERROR_MORE_DATA means the value grew between the two calls; treating
        // it as "no value" is better than reporting a truncated string.
        if rc != ERROR_SUCCESS || size as usize > data.len() {
            return None;
        }

        let mut units: Vec<u16> = data[..size as usize]
            .chunks(2)
            .filter_map(|pair| <[u8; 2]>::try_from(pair).ok())
            .map(u16::from_le_bytes)
            .collect();
        while units.last() == Some(&0) {
            units.pop();
        }
        String::from_utf16(&units).ok()
    }

    pub(super) fn delete_key(subkey: &str) -> Result<bool, String> {
        let subkey_wide = super::wide(subkey);
        // SAFETY: `subkey_wide` is NUL-terminated and outlives the call.
        let rc = unsafe { RegDeleteKeyW(HKEY_CURRENT_USER, subkey_wide.as_ptr()) };
        match rc as u32 {
            ERROR_SUCCESS => Ok(true),
            // Nothing written by this version: the caller reports that.
            ERROR_FILE_NOT_FOUND | ERROR_PATH_NOT_FOUND => Ok(false),
            other => Err(format!("delete {subkey}: registry error {other}")),
        }
    }

    // ERROR_MORE_DATA is referenced in the size-query explanation above; keep
    // the import meaningful for readers of the constant.
    const _: u32 = ERROR_MORE_DATA;
}

#[cfg(not(windows))]
mod win {
    pub(super) fn subkeys(_parent: &str) -> Vec<String> {
        Vec::new()
    }

    pub(super) fn read_string(_subkey: &str, _name: &str) -> Option<String> {
        None
    }

    pub(super) fn delete_key(_subkey: &str) -> Result<bool, String> {
        Err("removing a per-user uninstall entry is Windows-only".to_string())
    }
}

use win::{delete_key, read_string};

#[cfg(windows)]
fn wide(text: &str) -> Vec<u16> {
    text.encode_utf16().chain(std::iter::once(0)).collect()
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::testutil::TempDir;

    /// An installation under a scratch directory, in the current layout: the
    /// binaries and the workspace share one directory, told apart by name.
    fn installation(tag: &str) -> TempDir {
        let dir = TempDir::new(tag);
        std::fs::write(dir.join("glue.exe"), b"MZ").unwrap();
        std::fs::write(dir.join(INSTALL_MARKER), b"").unwrap();
        std::fs::create_dir_all(dir.join("shims")).unwrap();
        std::fs::create_dir_all(dir.join("apps")).unwrap();
        dir
    }

    #[test]
    fn is_installation_needs_both_glue_exe_and_the_marker() {
        let dir = installation("glue-uninstall-layout");
        assert!(is_installation(dir.path()));

        // A development tree has glue.exe but no marker: that is the whole
        // point of the marker.
        let bare = TempDir::new("glue-uninstall-bare");
        std::fs::write(bare.join("glue.exe"), b"MZ").unwrap();
        assert!(!is_installation(bare.path()), "no install marker");

        let empty = TempDir::new("glue-uninstall-empty");
        assert!(!is_installation(empty.path()));
    }

    #[test]
    fn removing_keeps_the_workspace_in_the_same_directory() {
        let dir = installation("glue-uninstall-keep");
        std::fs::write(dir.join("shim.exe"), b"MZ").unwrap();
        std::fs::write(dir.join("config.json"), b"{}").unwrap();
        let plan = Plan::new(dir.path(), dir.path(), false);

        let removed = remove_install_files(&plan).expect("remove");
        // glue.exe, shim.exe and the marker go; the workspace stays.
        assert_eq!(removed.entries, 3, "two binaries plus the marker");
        assert!(removed.data_kept);
        assert!(!removed.data_removed);
        assert!(!dir.join("glue.exe").exists(), "glue.exe must be gone");
        assert!(!dir.join("shim.exe").exists(), "shim.exe must be gone");
        assert!(
            !dir.join(INSTALL_MARKER).exists(),
            "the marker must go, or the directory still looks installed"
        );
        assert!(dir.join("shims").is_dir(), "the workspace must survive");
        assert!(dir.join("apps").is_dir(), "installed packages must survive");
        assert!(
            !dir.join("config.json").exists(),
            "configuration belongs to the program and goes with it"
        );
    }

    #[test]
    fn purging_removes_the_installation_and_the_workspace() {
        let dir = installation("glue-uninstall-purge");
        let plan = Plan::new(dir.path(), dir.path(), true);

        let removed = remove_install_files(&plan).expect("remove");
        assert!(removed.data_removed);
        assert!(removed.locked.is_empty());
        assert!(!dir.path().exists(), "the whole installation must be gone");
    }

    /// A workspace file that cannot be deleted must not stop the installation
    /// from being removed, and must be reported by name.
    #[test]
    fn a_locked_workspace_file_does_not_block_the_uninstall() {
        let dir = installation("glue-uninstall-locked");
        // Something under the workspace that we hold open, the way a running
        // application holds its own binary. `File::open` alone would not do:
        // Rust's default share mode includes FILE_SHARE_DELETE, so the delete
        // would succeed and the test would prove nothing.
        let held_dir = dir.join("apps").join("running");
        std::fs::create_dir_all(&held_dir).unwrap();
        let held = held_dir.join("app.exe");
        std::fs::write(&held, b"MZ").unwrap();
        let _handle = exclusive_hold(&held);

        let plan = Plan::new(dir.path(), dir.path(), true);
        let removed = remove_install_files(&plan).expect("the installation must be removable");

        assert!(!dir.join("glue.exe").exists(), "glue.exe must be gone");
        assert!(
            removed.locked.iter().any(|p| p.ends_with("app.exe")),
            "the locked file must be named: {:?}",
            removed.locked
        );
        assert!(
            !removed.data_removed,
            "a purge that left files behind must not report success"
        );
        assert!(held.exists(), "the held file must still be there");
    }

    /// Opens `path` with no sharing at all, so Windows refuses to delete it.
    #[cfg(windows)]
    fn exclusive_hold(path: &Path) -> std::fs::File {
        use std::os::windows::fs::OpenOptionsExt;
        std::fs::OpenOptions::new()
            .read(true)
            .share_mode(0)
            .open(path)
            .expect("hold the file exclusively")
    }

    #[cfg(not(windows))]
    fn exclusive_hold(path: &Path) -> std::fs::File {
        std::fs::File::open(path).expect("open the file")
    }

    #[test]
    fn the_installation_is_removed_even_when_the_workspace_survives() {
        let dir = installation("glue-uninstall-partial");
        std::fs::write(dir.join("shim.exe"), b"MZ").unwrap();
        let plan = Plan::new(dir.path(), dir.path(), false);

        let removed = remove_install_files(&plan).expect("remove");
        assert_eq!(removed.entries, 3, "two binaries plus the marker");
        assert!(removed.data_kept);
        assert!(!dir.join("glue.exe").exists());
        assert!(!dir.join("shim.exe").exists());
        assert!(dir.join("shims").is_dir(), "the workspace stays");
    }

    #[test]
    fn a_workspace_outside_the_installation_is_only_removed_by_purge() {
        let dir = installation("glue-uninstall-external");
        let workspace = TempDir::new("glue-uninstall-external-data");
        std::fs::write(workspace.join("marker"), b"x").unwrap();

        // The workspace points elsewhere, so everything in the installation
        // directory belongs to the installation and goes, while the external
        // workspace is untouched.
        let plan = Plan::new(dir.path(), workspace.path(), false);
        let removed = remove_install_files(&plan).expect("remove");
        assert!(removed.data_kept);
        assert!(!removed.data_removed);
        assert!(workspace.join("marker").is_file(), "workspace kept");
        assert!(!dir.join("glue.exe").exists(), "the installation is gone");
        assert!(!dir.path().exists(), "and its emptied directory with it");

        // With --purge the external workspace goes too.
        let dir = installation("glue-uninstall-external-2");
        let plan = Plan::new(dir.path(), workspace.path(), true);
        let removed = remove_install_files(&plan).expect("purge");
        assert!(removed.data_removed);
        assert!(!workspace.path().exists(), "workspace purged");
        assert!(!dir.path().exists(), "installation purged too");
    }

    #[test]
    fn a_development_tree_is_not_an_installation() {
        // The shape `cargo run` leaves behind: glue.exe and nothing else. The
        // marker is what keeps a checkout from being treated as installed.
        let dir = TempDir::new("glue-uninstall-dev-tree");
        std::fs::write(dir.join("glue.exe"), b"MZ").unwrap();
        std::fs::write(dir.join("shim.exe"), b"MZ").unwrap();
        assert!(
            !is_installation(dir.path()),
            "a build output is not an installation"
        );
        assert!(
            !is_managed_installation(dir.path()),
            "a checkout must not be removable without --install-dir"
        );
    }

    #[test]
    fn same_path_ignores_case_separators_and_trailing_dots() {
        assert!(same_path(Path::new(r"C:\A\b"), Path::new(r"c:\a\B")));
        assert!(same_path(Path::new(r"C:\A\b\"), Path::new(r"C:\A\b")));
        assert!(same_path(Path::new("C:/A/b"), Path::new(r"C:\A\b")));
        assert!(!same_path(Path::new(r"C:\A\b"), Path::new(r"C:\A\c")));
    }

    #[test]
    fn is_inside_matches_the_folder_and_its_children_only() {
        let dir = Path::new(r"C:\Apps\Glue");
        assert!(is_inside(dir, dir));
        assert!(is_inside(Path::new(r"C:\Apps\Glue\apps\git"), dir));
        assert!(is_inside(Path::new(r"c:/apps/glue/x"), dir));
        assert!(!is_inside(Path::new(r"C:\Apps\Glue-other"), dir));
        assert!(!is_inside(Path::new(r"C:\Apps"), dir));
    }

    #[test]
    fn a_missing_installation_is_reported_instead_of_silently_succeeding() {
        let plan = Plan::new(
            PathBuf::from(r"C:\definitely\not\here"),
            PathBuf::from(r"C:\definitely\not\here\data"),
            false,
        );
        let err = remove_install_files(&plan).unwrap_err();
        assert!(err.contains("not a directory"), "{err}");
    }
}
