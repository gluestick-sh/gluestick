//! Data-root resolution.
//!
//! The data root is the one directory that holds the data Gluestick owns:
//! `apps/`, `shims/`, `store/`, `buckets/` and `cache/`. Configuration is not
//! part of it: `config.json` lives beside the program that edits it (see
//! [`crate::config::config_dir`]), so the CLI and the desktop app share data
//! while keeping independent settings.
//!
//! Resolution order (first hit wins):
//!   1. an explicit `--root` override
//!   2. the `GLUE_ROOT` environment variable
//!   3. the root this installation was pointed at, `<install>\data-root.txt`
//!      (written by `glue root set`, and by the installers as
//!      `%USERPROFILE%\.glue` so both products share one workspace)
//!   4. the installation directory itself — `glue.exe` beside the
//!      `.glue-install` marker the installer writes; only a fallback for an
//!      installation whose `data-root.txt` is missing
//!   5. the saved root in `HKCU\Software\Gluestick\DataRoot` — only for
//!      binaries that do not belong to an installation
//!   6. `%USERPROFILE%\.glue`
//!
//! Nothing here hardcodes an installation path.
//!
//! Steps 3 and 4 come before the registry on purpose: the registry holds one
//! value for the whole user, so an installation that found it set by *another*
//! installation (an older copy, a second build) would send its cache
//! maintenance and installs into that other tree.

use std::path::{Path, PathBuf};

/// Environment variable overriding the base data root.
///
/// An explicit, per-invocation override (development, tests, sandboxes).
pub const ROOT_ENV: &str = "GLUE_ROOT";

/// Registry key (under `HKEY_CURRENT_USER`) holding the saved data root.
pub const REGISTRY_KEY: &str = r"Software\Gluestick";

/// Registry value name (under [`REGISTRY_KEY`]) holding the data root path.
pub const REGISTRY_VALUE: &str = "DataRoot";

/// Marker file the installer writes into its own directory.
///
/// The installation layout used to be recognised by a `data\` subdirectory
/// beside `glue.exe`. That made the marker and the workspace the same thing,
/// which broke down as soon as the workspace moved up next to the binaries: a
/// `cargo build` leaves `target\debug\glue.exe`, and without a separate marker
/// every development tree would identify itself as an installation. This file
/// is written only by the installer, so nothing else can impersonate one.
pub const INSTALL_MARKER: &str = ".glue-install";

/// File beside `glue.exe` recording the root this installation was pointed at.
///
/// The registry keeps one value per user, so it cannot say which installation a
/// choice belongs to; this file can. The installers write
/// `%USERPROFILE%\.glue` here, and `glue root set` updates every installation
/// so the CLI and the desktop app cannot drift apart.
pub const INSTALL_PREF_FILE: &str = "data-root.txt";

/// The CLI binary an installation root must contain.
const INSTALL_EXE: &str = "glue.exe";

/// How many parent directories are inspected while looking for the layout.
const INSTALL_ROOT_SEARCH_DEPTH: usize = 4;

/// Where the effective data root came from.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum RootSource {
    /// `--root` on the command line.
    Override,
    /// The `GLUE_ROOT` environment variable.
    Env,
    /// `<install>\data-root.txt`: the root chosen for this installation.
    InstallPref,
    /// `HKCU\Software\Gluestick\DataRoot` (`glue root set`).
    Registry,
    /// The installation's own directory (`glue.exe` beside the marker).
    Install,
    /// The `%USERPROFILE%\.glue` fallback.
    Home,
}

impl RootSource {
    /// Short label for `glue root show` and `glue doctor`.
    pub fn label(self) -> &'static str {
        match self {
            RootSource::Override => "--root",
            RootSource::Env => "GLUE_ROOT",
            RootSource::InstallPref => "installation setting (glue root set)",
            RootSource::Registry => "registry (glue root set)",
            RootSource::Install => "installation layout",
            RootSource::Home => "user profile",
        }
    }
}

/// The `GLUE_ROOT` override, if set and non-empty.
pub fn env_root() -> Option<PathBuf> {
    std::env::var_os(ROOT_ENV)
        .filter(|value| !value.is_empty())
        .map(PathBuf::from)
}

/// Default data root: `GLUE_ROOT`, this installation's own choice, the
/// installation directory, the registry, then `%USERPROFILE%\.glue`.
pub fn default_data_root() -> PathBuf {
    default_data_root_with_source().0
}

/// [`default_data_root`] plus where the value came from.
pub fn default_data_root_with_source() -> (PathBuf, RootSource) {
    if let Some(root) = env_root() {
        return (root, RootSource::Env);
    }
    if let Some(root) = install_pref_root() {
        return (root, RootSource::InstallPref);
    }
    if let Some(root) = installed_data_root() {
        return (root, RootSource::Install);
    }
    // Only binaries outside an installation layout read the shared value: for
    // an installation it may have been written by a different one.
    if let Some(root) = registry_root() {
        return (root, RootSource::Registry);
    }
    (home_data_root(), RootSource::Home)
}

/// The user-profile fallback root, `%USERPROFILE%\.glue`, ignoring every
/// override and the installation layout.
///
/// Used by the desktop's first-run picker, which offers this folder as the
/// default when no data root exists yet.
pub fn home_data_root() -> PathBuf {
    std::env::var_os("USERPROFILE")
        .or_else(|| std::env::var_os("HOME"))
        .map(PathBuf::from)
        .unwrap_or_else(|| PathBuf::from("."))
        .join(".glue")
}

/// The directory holding the running executable.
///
/// A portable copy keeps its data root there; the picker offers it next to
/// [`home_data_root`]. Unlike [`installed_data_root`] this does not require the
/// directory to be an installation, since the user may be about to create one.
pub fn program_data_root() -> PathBuf {
    std::env::current_exe()
        .ok()
        .and_then(|exe| exe.parent().map(Path::to_path_buf))
        .unwrap_or_else(|| PathBuf::from("."))
}

/// Expand a leading `~`/`~\`/`~/` and resolve a relative path against the
/// current directory.
///
/// For paths typed into the desktop's folder field, where `~\data` must mean
/// the user profile rather than a literal `~` directory.
pub fn absolutize_user_path(path: &Path) -> PathBuf {
    let text = path.to_string_lossy();
    let home = home_data_root();
    let Some(home_dir) = home.parent() else {
        return path.to_path_buf();
    };
    let expanded = if text == "~" {
        home_dir.to_path_buf()
    } else if let Some(rest) = text.strip_prefix("~\\").or_else(|| text.strip_prefix("~/")) {
        home_dir.join(rest)
    } else {
        path.to_path_buf()
    };
    if expanded.is_absolute() {
        expanded
    } else {
        std::env::current_dir()
            .map(|cwd| cwd.join(&expanded))
            .unwrap_or(expanded)
    }
}

/// The data root for a command, honouring an explicit override.
pub fn resolve_data_root(override_root: Option<&Path>) -> PathBuf {
    resolve_data_root_with_source(override_root).0
}

/// [`resolve_data_root`] plus where the value came from.
pub fn resolve_data_root_with_source(override_root: Option<&Path>) -> (PathBuf, RootSource) {
    match override_root {
        Some(root) => (root.to_path_buf(), RootSource::Override),
        None => default_data_root_with_source(),
    }
}

/// The installation the running executable belongs to, if any.
///
/// This is the directory holding `glue.exe` and the installer's marker. It is
/// *not* the data root any more: both installers point their `data-root.txt` at
/// `%USERPROFILE%\.glue`, and this directory is only the fallback for a pref
/// file that is missing.
pub fn installed_data_root() -> Option<PathBuf> {
    let exe = std::env::current_exe().ok()?;
    install_root_for(&exe)
}

/// The installation root that owns `exe`, if it lives under one.
///
/// An installation root is a directory that contains `glue.exe` and the
/// [`INSTALL_MARKER`] file the installer writes. Walking up also finds it from
/// helper tools kept under `<INSTALL>\bin\`.
///
/// The marker, not a directory the workspace happens to need, is what makes
/// this safe: `target\debug\glue.exe` is a development build, not an
/// installation, and must never be treated as one.
pub fn install_root_for(exe: &Path) -> Option<PathBuf> {
    let mut dir = exe.parent()?;
    for _ in 0..=INSTALL_ROOT_SEARCH_DEPTH {
        if dir.join(INSTALL_EXE).is_file() && dir.join(INSTALL_MARKER).is_file() {
            return Some(dir.to_path_buf());
        }
        dir = dir.parent()?;
    }
    None
}

/// Every installation this user has registered that still holds `glue.exe`
/// and the installer's marker.
///
/// The CLI and the desktop app are installed separately but share one
/// workspace, so a root change has to reach both. Without this the product
/// that was not running would keep the workspace it was installed with and the
/// two would silently fork.
///
/// The running binary's installation is included even when it is not (or not
/// yet) registered, so the install that is currently executing always records
/// its own choice.
pub fn installation_roots() -> Vec<PathBuf> {
    let mut roots = crate::uninstall::registered_installation_dirs();
    if let Some(current) = installed_data_root() {
        if !roots
            .iter()
            .any(|root| crate::uninstall::same_path(root, &current))
        {
            roots.push(current);
        }
    }
    roots
}

/// The saved data root (`HKCU\Software\Gluestick\DataRoot`), if set.
///
/// Only binaries that do not live in an installation read this: for one that
/// does, [`install_pref_root`] or the installation directory decides. `glue root
/// set` creates the directory, and `glue root show` reports a missing one.
pub fn registry_root() -> Option<PathBuf> {
    read_registry_root()
}

/// The root this installation was pointed at (`<install>\data-root.txt`).
///
/// Used as-is, like [`registry_root`], but scoped to one installation: the
/// registry holds a single value per user, which a second installation would
/// otherwise inherit.
pub fn install_pref_root() -> Option<PathBuf> {
    let text = std::fs::read_to_string(install_pref_file()?).ok()?;
    let trimmed = text.trim();
    if trimmed.is_empty() {
        return None;
    }
    Some(PathBuf::from(trimmed))
}

/// Record `path` as this installation's data root.
///
/// Returns an error outside an installation layout: a standalone binary has no
/// directory of its own, and the registry is the fallback there.
pub fn set_install_pref_root(path: &Path) -> Result<(), String> {
    let install = installed_data_root()
        .ok_or_else(|| "not running from an installation directory".to_string())?;
    set_install_pref_root_at(&install, path)
}

/// Record `path` as the data root of the installation rooted at `install`.
pub fn set_install_pref_root_at(install: &Path, path: &Path) -> Result<(), String> {
    let file = install.join(INSTALL_PREF_FILE);
    std::fs::write(&file, path.display().to_string())
        .map_err(|e| format!("write {}: {e}", file.display()))
}

/// Forget this installation's own choice, falling back to the installation
/// directory.
pub fn clear_install_pref_root() -> Result<(), String> {
    let Some(file) = install_pref_file() else {
        return Ok(());
    };
    match std::fs::remove_file(file) {
        Ok(()) => Ok(()),
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(()),
        Err(e) => Err(e.to_string()),
    }
}

/// `<install>\data-root.txt` for the running executable, when it lives in an
/// installation.
fn install_pref_file() -> Option<PathBuf> {
    let exe = std::env::current_exe().ok()?;
    install_root_for(&exe).map(|root| root.join(INSTALL_PREF_FILE))
}

/// Saves `path` as the user's data root.
pub fn set_registry_root(path: &Path) -> Result<(), String> {
    write_registry_root(path)
}

/// Removes the saved data root, falling back to the layout/home resolution.
pub fn clear_registry_root() -> Result<(), String> {
    delete_registry_root()
}

/// Save `path` as the data root of every installation, plus the per-user value
/// standalone binaries and shims read.
///
/// Succeeds when at least one destination could be written, so a read-only
/// installation directory does not make the choice unusable.
pub fn save_data_root(path: &Path) -> Result<(), String> {
    let mut wrote_pref = false;
    let mut first_error: Option<String> = None;
    for install in installation_roots() {
        match set_install_pref_root_at(&install, path) {
            Ok(()) => wrote_pref = true,
            Err(error) => {
                if first_error.is_none() {
                    first_error = Some(error);
                }
            }
        }
    }
    let registry = set_registry_root(path);
    if wrote_pref || registry.is_ok() {
        return Ok(());
    }
    let pref_error = first_error.unwrap_or_else(|| "no installation directory".to_string());
    match registry {
        Err(registry_error) => Err(format!("{pref_error}; {registry_error}")),
        Ok(()) => Ok(()),
    }
}

/// Forget every saved data root.
///
/// An installation cannot simply drop its `data-root.txt`: the fallback would
/// be the installation directory itself, and the workspace would reappear under
/// `Programs` instead of `%USERPROFILE%\.glue`. Reset therefore points every
/// installation back at the shared home default. A standalone binary just
/// deletes the registry value.
pub fn clear_saved_root() -> Result<(), String> {
    if !installation_roots().is_empty() {
        return save_data_root(&home_data_root());
    }
    let pref = clear_install_pref_root();
    let registry = clear_registry_root();
    match (pref, registry) {
        (Ok(()), _) | (Err(_), Ok(())) => Ok(()),
        (Err(pref_error), Err(registry_error)) => Err(format!("{pref_error}; {registry_error}")),
    }
}

#[cfg(windows)]
fn read_registry_root() -> Option<PathBuf> {
    use windows_sys::Win32::System::Registry::{RegGetValueW, HKEY_CURRENT_USER, RRF_RT_REG_SZ};

    let subkey = wide(REGISTRY_KEY);
    let value = wide(REGISTRY_VALUE);
    let mut size: u32 = 0;
    // SAFETY: a NULL buffer asks only for the required size.
    let rc = unsafe {
        RegGetValueW(
            HKEY_CURRENT_USER,
            subkey.as_ptr(),
            value.as_ptr(),
            RRF_RT_REG_SZ,
            std::ptr::null_mut(),
            std::ptr::null_mut(),
            &mut size,
        )
    };
    if rc != 0 || size < 2 {
        return None;
    }
    let mut buffer = vec![0u16; size as usize / 2 + 1];
    let mut capacity: u32 = (buffer.len() * 2) as u32;
    // SAFETY: `buffer` is a writable UTF-16 buffer of `capacity` bytes.
    let rc = unsafe {
        RegGetValueW(
            HKEY_CURRENT_USER,
            subkey.as_ptr(),
            value.as_ptr(),
            RRF_RT_REG_SZ,
            std::ptr::null_mut(),
            buffer.as_mut_ptr() as *mut std::ffi::c_void,
            &mut capacity,
        )
    };
    if rc != 0 {
        return None;
    }
    let len = buffer.iter().position(|c| *c == 0).unwrap_or(buffer.len());
    let text = String::from_utf16_lossy(&buffer[..len]);
    let trimmed = text.trim();
    if trimmed.is_empty() {
        None
    } else {
        Some(PathBuf::from(trimmed))
    }
}

/// Raw `advapi32` bindings for the saved-root value.
///
/// The `windows-sys` wrapper for `RegSetKeyValueW` fails with
/// `ERROR_ACCESS_DENIED` on a key this process can write through the same
/// export called directly (the read-side wrapper works fine), so the two value
/// helpers are bound by hand.
#[cfg(windows)]
mod regval {
    use std::ffi::c_void;

    #[link(name = "advapi32")]
    unsafe extern "system" {
        pub fn RegSetKeyValueW(
            hkey: isize,
            lpsubkey: *const u16,
            lpvaluename: *const u16,
            dwtype: u32,
            lpdata: *const c_void,
            cbdata: u32,
        ) -> i32;
        pub fn RegDeleteKeyValueW(
            hkey: isize,
            lpsubkey: *const u16,
            lpvaluename: *const u16,
        ) -> i32;
    }

    /// `HKEY_CURRENT_USER`: `(LONG)0x80000001`, sign-extended.
    pub const HKEY_CURRENT_USER: isize = -2147483647;
    /// `REG_SZ`.
    pub const SZ: u32 = 1;
}

#[cfg(windows)]
fn write_registry_root(path: &Path) -> Result<(), String> {
    let subkey = wide(REGISTRY_KEY);
    let value = wide(REGISTRY_VALUE);
    let text: Vec<u16> = path
        .to_string_lossy()
        .encode_utf16()
        .chain(std::iter::once(0))
        .collect();
    // SAFETY: the subkey and value names are NUL-terminated, `text` is a
    // NUL-terminated UTF-16 buffer whose byte length is passed, and all three
    // outlive the call. `RegSetKeyValueW` creates the key when missing.
    let rc = unsafe {
        regval::RegSetKeyValueW(
            regval::HKEY_CURRENT_USER,
            subkey.as_ptr(),
            value.as_ptr(),
            regval::SZ,
            text.as_ptr() as *const std::ffi::c_void,
            (text.len() * 2) as u32,
        )
    };
    if rc != 0 {
        // 5 is ERROR_ACCESS_DENIED: the process may not write the user hive
        // (restricted sandboxes do this). Say what still works instead of
        // leaving the user with a bare registry code.
        let hint = if rc == 5 {
            " (this process may not write the user registry; use `--root` or GLUE_ROOT, or run from a normal terminal)"
        } else {
            ""
        };
        return Err(format!(
            "save {REGISTRY_KEY}\\{REGISTRY_VALUE}: registry error {rc}{hint}"
        ));
    }
    Ok(())
}

#[cfg(windows)]
fn delete_registry_root() -> Result<(), String> {
    let subkey = wide(REGISTRY_KEY);
    let name = wide(REGISTRY_VALUE);
    // SAFETY: both names are NUL-terminated and outlive the call.
    let rc = unsafe {
        regval::RegDeleteKeyValueW(regval::HKEY_CURRENT_USER, subkey.as_ptr(), name.as_ptr())
    };
    // ERROR_FILE_NOT_FOUND (2) means nothing was saved; a missing key is fine.
    if rc != 0 && rc != 2 {
        return Err(format!(
            "remove {REGISTRY_KEY}\\{REGISTRY_VALUE}: registry error {rc}"
        ));
    }
    Ok(())
}

#[cfg(not(windows))]
fn read_registry_root() -> Option<PathBuf> {
    None
}

#[cfg(not(windows))]
fn write_registry_root(_path: &Path) -> Result<(), String> {
    Err("saving a workspace in the registry is Windows-only".to_string())
}

#[cfg(not(windows))]
fn delete_registry_root() -> Result<(), String> {
    Ok(())
}

#[cfg(windows)]
fn wide(text: &str) -> Vec<u16> {
    text.encode_utf16().chain(std::iter::once(0)).collect()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn default_data_root_is_under_home() {
        let home = std::env::var_os("USERPROFILE")
            .or_else(|| std::env::var_os("HOME"))
            .map(PathBuf::from)
            .unwrap_or_else(|| PathBuf::from("."));
        // The env override, a saved root and an installed layout all
        // legitimately move the root; only assert the shape when none applies.
        if std::env::var_os(ROOT_ENV).is_none()
            && registry_root().is_none()
            && installed_data_root().is_none()
        {
            assert_eq!(default_data_root(), home.join(".glue"));
        }
    }

    #[test]
    fn install_root_is_recognised_by_its_marker() {
        let dir = std::env::temp_dir().join("glue-install-root-test");
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(dir.join("shims")).expect("create dir");
        std::fs::write(dir.join(INSTALL_EXE), b"MZ").expect("write glue.exe");

        // glue.exe alone is not enough: that is exactly what a development tree
        // looks like, and treating it as an installation is how an uninstall
        // could wipe a checkout.
        assert_eq!(
            install_root_for(&dir.join(INSTALL_EXE)),
            None,
            "without the marker this is a build output, not an installation"
        );

        std::fs::write(dir.join(INSTALL_MARKER), b"").expect("write marker");
        // The CLI itself sits at the root, which is also the data root.
        assert_eq!(install_root_for(&dir.join(INSTALL_EXE)), Some(dir.clone()));
        // Shim copies under <ROOT>\shims resolve back to the same root.
        let shim = dir.join("shims").join("yazi.exe");
        assert_eq!(install_root_for(&shim), Some(dir.clone()));

        // A marker without glue.exe is not an installation either.
        let outside = std::env::temp_dir().join("glue-install-root-outside");
        let _ = std::fs::remove_dir_all(&outside);
        std::fs::create_dir_all(&outside).expect("create dir");
        std::fs::write(outside.join(INSTALL_MARKER), b"").expect("write marker");
        std::fs::write(outside.join("other.exe"), b"MZ").expect("write other.exe");
        assert_eq!(
            install_root_for(&outside.join("other.exe")),
            None,
            "a marker alone does not make an installation"
        );
        let _ = std::fs::remove_dir_all(&outside);
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn the_installation_directories_binaries_define_its_root() {
        let dir = std::env::temp_dir().join("glue-install-root-data-test");
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).expect("create dir");
        std::fs::write(dir.join(INSTALL_EXE), b"MZ").expect("write glue.exe");
        std::fs::write(dir.join(INSTALL_MARKER), b"").expect("write marker");

        let root = install_root_for(&dir.join(INSTALL_EXE)).expect("recognised");
        assert_eq!(
            root, dir,
            "the workspace lives beside the binaries, not in a data\\ subdirectory"
        );
        assert!(
            !dir.join("data").exists(),
            "no data subdirectory is created or required"
        );
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn explicit_override_wins_and_is_labelled() {
        let (root, source) = resolve_data_root_with_source(Some(Path::new(r"C:\data\glue-alt")));
        assert_eq!(root, PathBuf::from(r"C:\data\glue-alt"));
        assert_eq!(source, RootSource::Override);
        assert_eq!(source.label(), "--root");
    }

    /// The installers point every installation at one workspace by writing this
    /// file. It must stay plain UTF-8: the CLI and the shim read it with
    /// `read_to_string`, and an NSIS `FileWrite` would emit UTF-16LE.
    #[test]
    fn the_installation_pref_is_written_as_utf8() {
        let dir = std::env::temp_dir().join("glue-install-pref-test");
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).expect("create dir");

        let target = Path::new(r"C:\Users\tester\.glue");
        set_install_pref_root_at(&dir, target).expect("write pref");
        let text = std::fs::read_to_string(dir.join(INSTALL_PREF_FILE)).expect("utf-8 readable");
        assert_eq!(text, r"C:\Users\tester\.glue");

        let _ = std::fs::remove_dir_all(&dir);
    }
}
