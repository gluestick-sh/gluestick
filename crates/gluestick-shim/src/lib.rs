//! Command shim — the tiny launcher Gluestick installs as `<name>.exe` on
//! PATH. At runtime it reads its JSON config from
//! `<glue-root>/shims-meta/<name>.json` and execs the real target, proxying
//! stdio and propagating the exit code.
//!
//! The data root is resolved in this order: `GLUE_ROOT`, the root this
//! installation was pointed at (`<install>\data-root.txt`), the installation
//! root (a `<ROOT>\glue.exe` beside the installer's `.glue-install` marker),
//! the root saved by `glue root set` (`HKCU\Software\Gluestick\DataRoot`, read
//! through advapi32 so the stub needs no extra dependency) for binaries
//! outside an installation, then `~/.glue`.
//! Without a compiled stub the engine used to fall back to a `.bat` forwarder;
//! `cargo build -p gluestick-shim` produces the binary it expects instead.
//!
//! `Config` is a shared contract with `gluestick_core::shim::Config` — the
//! JSON field names must stay in sync across both. It is duplicated here
//! rather than imported so the stub keeps its own (small) dependency set: it
//! is copied next to every shim glue creates, so its size matters.

use std::path::{Path, PathBuf};
use std::process::Command;
use std::{env, fs, process};

/// The installer's marker file, and the CLI binary that sits beside it, in an
/// installation root. Both are needed: `glue.exe` alone is what a development
/// build looks like.
const INSTALL_MARKER: &str = ".glue-install";
const INSTALL_EXE: &str = "glue.exe";

/// Registry key (under `HKEY_CURRENT_USER`) and value name holding the data
/// root saved by `glue root set`.
#[cfg(windows)]
const REGISTRY_KEY: &str = "Software\\Gluestick";
#[cfg(windows)]
const REGISTRY_VALUE: &str = "DataRoot";

/// On-disk shim configuration for a single executable.
#[derive(serde::Deserialize)]
struct Config {
    /// Display name (unused by the launcher).
    #[allow(dead_code)]
    name: String,
    /// Actual command to run.
    command: String,
    /// Default arguments, prepended to the caller's.
    #[serde(default)]
    args: Vec<String>,
    /// Package env vars applied at launch (override inherited values).
    #[serde(default)]
    env: std::collections::BTreeMap<String, String>,
    /// Path to the executable (kept for contract parity; `command` is used).
    #[allow(dead_code)]
    #[serde(default)]
    path: String,
}

/// Runs the shim for the executable it was copied to and terminates the
/// process with the target's exit code (1 when it could not be launched).
pub fn run_or_exit() -> ! {
    match run() {
        Ok(code) => process::exit(code),
        Err(err) => fatal(&err),
    }
}

/// Launches the target named by this shim's `<name>.json` config, proxying
/// stdio and returning the child's exit code.
pub fn run() -> Result<i32, String> {
    // The shim name is argv[0]'s file stem: launching `<name>.exe` looks up
    // `shims-meta/<name>.json`.
    let shim_name = env::args_os()
        .next()
        .map(PathBuf::from)
        .and_then(|p| p.file_stem().map(|s| s.to_string_lossy().into_owned()))
        .ok_or_else(|| "shim name unavailable from argv[0]".to_string())?;

    let config_path = shims_meta_dir()?.join(format!("{shim_name}.json"));
    let data = fs::read(&config_path).map_err(|err| format!("shim config not found: {err}"))?;
    let cfg: Config =
        serde_json::from_slice(&data).map_err(|err| format!("invalid shim config: {err}"))?;

    // Defaults first, the caller's arguments after.
    let mut args = cfg.args;
    args.extend(
        env::args_os()
            .skip(1)
            .map(|a| a.to_string_lossy().into_owned()),
    );

    // `CreateProcessW` cannot start a program past MAX_PATH, and the OS error
    // ("cannot find the path") hides why. Package executables normally live at
    // `apps/<pkg>/current/…`, well under the limit; this only triggers when the
    // data root itself is extremely deep.
    if target_path_exceeds_max_path(&cfg.command) {
        return Err(format!(
            "target path exceeds MAX_PATH and cannot be started (move the workspace to a shorter path): {}",
            cfg.command
        ));
    }

    let mut cmd = Command::new(&cfg.command);
    cmd.args(&args);
    // `Command::env` overrides inherited entries, so package env wins over the
    // caller's environment.
    for (k, v) in &cfg.env {
        cmd.env(k, v);
    }

    let status = cmd
        .status()
        .map_err(|err| format!("run {}: {err}", cfg.command))?;
    Ok(status.code().unwrap_or(1))
}

/// Whether a target path is longer than Windows can pass to `CreateProcessW`.
fn target_path_exceeds_max_path(path: &str) -> bool {
    #[cfg(windows)]
    {
        path.encode_utf16().count() >= 260
    }
    #[cfg(not(windows))]
    {
        let _ = path;
        false
    }
}

/// `<glue-root>/shims-meta` — the engine writes one JSON per shim here.
///
/// `GLUE_ROOT` wins, then this installation's own choice (`data-root.txt`) or
/// its directory, then the saved registry root for binaries outside an
/// installation, then `~/.glue`. The registry holds one value for the whole
/// user, so an installation must not inherit a root another installation (the
/// desktop app, say) saved.
fn shims_meta_dir() -> Result<PathBuf, String> {
    let env_root = env::var_os("GLUE_ROOT")
        .filter(|v| !v.is_empty())
        .map(PathBuf::from);
    let install_root =
        install_data_root().map(|install| install_pref_root(&install).unwrap_or(install));
    let root = resolve_root(env_root, install_root, saved_root(), default_root())
        .ok_or("home directory unavailable ($HOME is not defined)")?;
    Ok(root.join("shims-meta"))
}

/// The root this installation was pointed at (`<install>\data-root.txt`).
///
/// Written by `glue root set` when the CLI lives in an installation. An empty
/// or missing file means "use the installation directory".
fn install_pref_root(install: &Path) -> Option<PathBuf> {
    let text = fs::read_to_string(install.join("data-root.txt")).ok()?;
    let trimmed = text.trim();
    if trimmed.is_empty() {
        None
    } else {
        Some(PathBuf::from(trimmed))
    }
}

/// The data root saved by `glue root set` (`HKCU\Software\Gluestick\DataRoot`).
///
/// Read with a raw `RegGetValueW` binding: adding `windows-sys` to the stub
/// would cost more binary size than the ~30 lines below. A saved root is
/// trusted only while it still exists, so deleting the directory (or running
/// the uninstaller) falls back to the installation layout instead of leaving
/// every shim pointing at a dead path.
#[cfg(windows)]
fn saved_root() -> Option<PathBuf> {
    use std::ptr::null_mut;

    #[link(name = "advapi32")]
    unsafe extern "system" {
        fn RegGetValueW(
            hkey: isize,
            sub_key: *const u16,
            value_name: *const u16,
            flags: u32,
            value_type: *mut u32,
            data: *mut u8,
            data_size: *mut u32,
        ) -> i32;
    }

    /// `HKEY_CURRENT_USER`: `(LONG)0x80000001` sign-extended.
    const HKEY_CURRENT_USER: isize = -2147483647;
    /// Restrict the read to `REG_SZ`.
    const RRF_RT_REG_SZ: u32 = 0x0000_0002;

    let sub_key = wide(REGISTRY_KEY);
    let value = wide(REGISTRY_VALUE);
    let mut size: u32 = 0;
    // SAFETY: a NULL data buffer asks only for the required size; both names
    // are NUL-terminated and outlive the call.
    let rc = unsafe {
        RegGetValueW(
            HKEY_CURRENT_USER,
            sub_key.as_ptr(),
            value.as_ptr(),
            RRF_RT_REG_SZ,
            null_mut(),
            null_mut(),
            &mut size,
        )
    };
    if rc != 0 || size < 2 {
        return None;
    }
    let mut buffer = vec![0u16; size as usize / 2 + 1];
    let mut capacity = (buffer.len() * 2) as u32;
    // SAFETY: `buffer` is a writable UTF-16 buffer of `capacity` bytes.
    let rc = unsafe {
        RegGetValueW(
            HKEY_CURRENT_USER,
            sub_key.as_ptr(),
            value.as_ptr(),
            RRF_RT_REG_SZ,
            null_mut(),
            buffer.as_mut_ptr() as *mut u8,
            &mut capacity,
        )
    };
    if rc != 0 {
        return None;
    }
    let len = buffer.iter().position(|c| *c == 0).unwrap_or(buffer.len());
    let text = String::from_utf16_lossy(&buffer[..len]);
    let saved = PathBuf::from(text.trim());
    saved.is_dir().then_some(saved)
}

#[cfg(not(windows))]
fn saved_root() -> Option<PathBuf> {
    None
}

#[cfg(windows)]
fn wide(text: &str) -> Vec<u16> {
    text.encode_utf16().chain(std::iter::once(0)).collect()
}

/// The data directory of the installation owning this executable, if any.
///
/// The shim copies live under `<ROOT>\shims`; walking up, the first ancestor
/// that contains `glue.exe` beside the installer's marker file is the
/// installation root, which is also its data root. `glue.exe` on its own is not
/// enough — a development tree has one too, and resolving to it would make every
/// shim point at `target\debug`.
fn install_data_root() -> Option<PathBuf> {
    let exe = env::current_exe().ok()?;
    let mut dir = exe.parent()?;
    for _ in 0..=4 {
        if dir.join(INSTALL_EXE).is_file() && dir.join(INSTALL_MARKER).is_file() {
            return Some(dir.to_path_buf());
        }
        dir = dir.parent()?;
    }
    None
}

/// Picks the data root the shim should use.
///
/// An explicit `GLUE_ROOT` always wins. An installation's own root beats the
/// per-user saved root, which the registry shares across installations; the
/// saved root is only a fallback for binaries outside an installation. The
/// default root is the last resort.
fn resolve_root(
    glue_root: Option<PathBuf>,
    install_root: Option<PathBuf>,
    saved_root: Option<PathBuf>,
    default_root: Option<PathBuf>,
) -> Option<PathBuf> {
    glue_root.or(install_root).or(saved_root).or(default_root)
}

/// The default data root (`~/.glue`), the last resort.
fn default_root() -> Option<PathBuf> {
    let home = env::var_os("USERPROFILE")
        .or_else(|| env::var_os("HOME"))
        .filter(|v| !v.is_empty())?;
    Some(PathBuf::from(home).join(".glue"))
}

fn fatal(message: &str) -> ! {
    eprintln!("glue shim error: {message}");
    process::exit(1);
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn glue_root_wins_over_every_saved_root() {
        let root = resolve_root(
            Some(PathBuf::from("/explicit")),
            Some(PathBuf::from("/install")),
            Some(PathBuf::from("/saved")),
            Some(PathBuf::from("/default")),
        );
        assert_eq!(root, Some(PathBuf::from("/explicit")));
    }

    /// The registry holds one value for the user; an installation must not
    /// borrow a root another installation saved there.
    #[test]
    fn the_installation_wins_over_the_saved_root() {
        let root = resolve_root(
            None,
            Some(PathBuf::from("/install")),
            Some(PathBuf::from("/saved")),
            Some(PathBuf::from("/default")),
        );
        assert_eq!(root, Some(PathBuf::from("/install")));
    }

    #[test]
    fn the_saved_root_wins_over_the_default_root_outside_an_installation() {
        let root = resolve_root(
            None,
            None,
            Some(PathBuf::from("/saved")),
            Some(PathBuf::from("/default")),
        );
        assert_eq!(root, Some(PathBuf::from("/saved")));
    }

    #[test]
    fn the_default_root_is_the_last_resort() {
        let root = resolve_root(None, None, None, Some(PathBuf::from("/default")));
        assert_eq!(root, Some(PathBuf::from("/default")));
        assert_eq!(resolve_root(None, None, None, None), None);
    }

    #[test]
    fn the_installation_pref_file_is_read() {
        let dir = std::env::temp_dir().join(format!("glue-shim-pref-{}", std::process::id()));
        let _ = fs::remove_dir_all(&dir);
        fs::create_dir_all(&dir).unwrap();

        assert_eq!(install_pref_root(&dir), None);

        fs::write(dir.join("data-root.txt"), "  /custom/root \r\n").unwrap();
        assert_eq!(install_pref_root(&dir), Some(PathBuf::from("/custom/root")));

        let _ = fs::remove_dir_all(&dir);
    }

    #[test]
    fn an_over_long_target_is_refused() {
        // 260 UTF-16 units is exactly the limit CreateProcessW cannot pass.
        let long = "x".repeat(260);
        assert_eq!(target_path_exceeds_max_path(&long), cfg!(windows));
        assert!(!target_path_exceeds_max_path(
            "C:\\glue\\apps\\ripgrep\\current\\rg.exe"
        ));
    }
}
