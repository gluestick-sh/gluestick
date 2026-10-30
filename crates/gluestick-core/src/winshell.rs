//! Windows side effects of installing a package: `env_set` registry writes and
//! Start-menu `.lnk` shortcuts.
//!
//! Windows-only; on other platforms the entry points are no-ops so the
//! pipeline stays cross-compilable.

use std::collections::BTreeMap;
use std::path::Path;

use crate::manifest::Manifest;
use crate::procutil;

/// Substitutes Scoop `$dir` in an env value.
pub fn expand_manifest_env_value(value: &str, install_dir: &str) -> String {
    value.replace("$dir", install_dir)
}

/// Expanded env vars to apply when launching this package's shims.
pub fn shim_env_for_package(
    m: &Manifest,
    install_dir: &str,
    install_arch: &str,
) -> BTreeMap<String, String> {
    m.env_vars_for_install(install_arch)
        .into_iter()
        .map(|(k, v)| (k, expand_manifest_env_value(&v, install_dir)))
        .collect()
}

/// Writes the manifest's `env_set` to the *user* environment.
///
/// opens `HKCU\Environment` with `SET_VALUE|QUERY_VALUE`
/// and calls `SetStringValue` per entry (an existing value is overwritten).
pub fn apply_package_env_set(
    m: &Manifest,
    install_dir: &str,
    install_arch: &str,
) -> Result<(), String> {
    let raw = m.env_set_for_install(install_arch);
    if raw.is_empty() {
        return Ok(());
    }
    let expanded: BTreeMap<String, String> = raw
        .into_iter()
        .map(|(k, v)| (k, expand_manifest_env_value(&v, install_dir)))
        .collect();
    apply_user_env(&expanded)
}

/// Removes the named values from the user environment.
pub fn remove_user_env(names: &[String]) -> Result<(), String> {
    #[cfg(windows)]
    {
        if names.is_empty() {
            return Ok(());
        }
        let key = open_user_env_key()?;
        for name in names {
            let name = name.trim();
            if name.is_empty() {
                continue;
            }
            // SAFETY: `key` is an open `HKEY`; the name is a NUL-terminated
            // UTF-16 string that outlives the call.
            let name_wide = to_wide(name);
            let rc = unsafe { RegDeleteValueW(key.0, name_wide.as_ptr()) };
            // ERROR_FILE_NOT_FOUND (2) — deleting a missing value is fine.
            if rc != 0 && rc != 2 {
                return Err(format!("remove user env {name}: registry error {rc}"));
            }
        }
        Ok(())
    }
    #[cfg(not(windows))]
    {
        let _ = names;
        Ok(())
    }
}

#[cfg(windows)]
fn apply_user_env(vars: &BTreeMap<String, String>) -> Result<(), String> {
    if vars.is_empty() {
        return Ok(());
    }
    let key = open_user_env_key()?;
    for (name, value) in vars {
        let name = name.trim();
        if name.is_empty() {
            continue;
        }
        let name_wide = to_wide(name);
        let value_wide = to_wide(value);
        // SAFETY: both strings are NUL-terminated UTF-16 buffers that outlive
        // the call; `key` is a valid open `HKEY`.
        let rc = unsafe {
            RegSetValueExW(
                key.0,
                name_wide.as_ptr(),
                0,
                REG_SZ,
                value_wide.as_ptr() as *const u8,
                (value_wide.len() * 2) as u32,
            )
        };
        if rc != 0 {
            return Err(format!("set user env {name}: registry error {rc}"));
        }
    }
    Ok(())
}

#[cfg(not(windows))]
fn apply_user_env(vars: &BTreeMap<String, String>) -> Result<(), String> {
    // Unix has no user-environment registry; stub is a no-op too.
    let _ = vars;
    Ok(())
}

#[cfg(windows)]
mod win {
    pub use windows_sys::Win32::System::Registry::{
        RegCloseKey, RegDeleteValueW, RegOpenKeyExW, RegSetValueExW, HKEY, HKEY_CURRENT_USER,
        KEY_QUERY_VALUE, KEY_SET_VALUE, REG_SZ,
    };
}
#[cfg(windows)]
use win::*;

/// An open `HKEY` that closes itself on drop.
#[cfg(windows)]
struct RegKey(win::HKEY);

#[cfg(windows)]
impl Drop for RegKey {
    fn drop(&mut self) {
        // SAFETY: `self.0` came from a successful `RegOpenKeyExW`.
        unsafe {
            RegCloseKey(self.0);
        }
    }
}

#[cfg(windows)]
fn open_user_env_key() -> Result<RegKey, String> {
    let subkey = to_wide("Environment");
    let mut handle: HKEY = std::ptr::null_mut();
    // SAFETY: `handle` is a valid out-pointer; `subkey` is NUL-terminated and
    // outlives the call.
    let rc = unsafe {
        RegOpenKeyExW(
            HKEY_CURRENT_USER,
            subkey.as_ptr(),
            0,
            KEY_SET_VALUE | KEY_QUERY_VALUE,
            &mut handle,
        )
    };
    if rc != 0 {
        return Err(format!("open user Environment key: registry error {rc}"));
    }
    Ok(RegKey(handle))
}

/// NUL-terminated UTF-16 for the `W` registry APIs.
#[cfg(windows)]
fn to_wide(s: &str) -> Vec<u16> {
    s.encode_utf16().chain(std::iter::once(0)).collect()
}

/// Creates the Start-menu shortcuts declared by the manifest.
///
/// resolves the per-user Start-menu
/// "Programs/Glue Apps" folder via PowerShell, skips entries whose target is
/// missing, and materializes each `.lnk` through `WScript.Shell`.
pub fn create_package_shortcuts(
    install_dir: &Path,
    m: &Manifest,
    install_arch: &str,
) -> Result<(), String> {
    let entries = m.shortcut_entries_for_install(install_arch);
    if entries.is_empty() {
        return Ok(());
    }
    let folder = shortcuts_folder(false)?;
    for sc in &entries {
        let target = install_dir.join(sc.target.replace('/', "\\"));
        if !target.exists() {
            continue;
        }
        create_start_menu_shortcut(&folder, install_dir, &target, &sc.label, &sc.args, &sc.icon)?;
    }
    Ok(())
}

/// Deletes the Start-menu shortcuts declared by the manifest.
pub fn remove_package_shortcuts(m: &Manifest, install_arch: &str) -> Result<(), String> {
    let entries = m.shortcut_entries_for_install(install_arch);
    if entries.is_empty() {
        return Ok(());
    }
    let folder = shortcuts_folder(false)?;
    for sc in &entries {
        if sc.label.is_empty() {
            continue;
        }
        let lnk = shortcut_link_path(&folder, &sc.label);
        if let Err(err) = std::fs::remove_file(&lnk) {
            if err.kind() != std::io::ErrorKind::NotFound {
                return Err(format!("remove shortcut {}: {err}", sc.label));
            }
        }
    }
    Ok(())
}

/// The per-user (or global) "Glue Apps" Start-menu folder.
pub fn shortcuts_folder(global: bool) -> Result<std::path::PathBuf, String> {
    let start_menu = if global {
        "CommonStartMenu"
    } else {
        "StartMenu"
    };
    let script = format!("[Environment]::GetFolderPath('{start_menu}')");
    let output = run_powershell(&script)?;
    let base = decode_powershell_output(&output);
    if base.is_empty() {
        return Err("resolve start menu folder: empty start menu folder".to_string());
    }
    Ok(std::path::Path::new(&base)
        .join("Programs")
        .join("Glue Apps"))
}

/// The `.lnk` path for a shortcut label, nested under the folder when the
/// label contains a directory component.
fn shortcut_link_path(folder: &Path, label: &str) -> std::path::PathBuf {
    folder.join(format!("{}.lnk", label.replace('/', "\\")))
}

/// Writes one `.lnk` via a generated PowerShell `WScript.Shell` script.
fn create_start_menu_shortcut(
    folder: &Path,
    install_dir: &Path,
    target: &Path,
    label: &str,
    args: &str,
    icon: &str,
) -> Result<(), String> {
    if label.is_empty() {
        return Ok(());
    }
    let label_native = label.replace('/', "\\");
    let sub_dir = Path::new(&label_native)
        .parent()
        .map(|p| p.to_path_buf())
        .unwrap_or_default();
    let link_dir = if sub_dir.as_os_str().is_empty() {
        folder.to_path_buf()
    } else {
        folder.join(&sub_dir)
    };
    std::fs::create_dir_all(&link_dir).map_err(|err| format!("create shortcut dir: {err}"))?;
    let base_name = Path::new(&label_native)
        .file_name()
        .map(|n| n.to_string_lossy().into_owned())
        .unwrap_or_else(|| label_native.clone());
    let lnk_path = link_dir.join(format!("{base_name}.lnk"));

    let install_dir_str = install_dir.to_string_lossy();
    let icon_path = if icon.is_empty() {
        String::new()
    } else {
        install_dir
            .join(icon.replace('/', "\\"))
            .to_string_lossy()
            .into_owned()
    };
    let expanded_args = args
        .replace("$dir", &install_dir_str)
        .replace("$original_dir", &install_dir_str);

    let script = format!(
        "$ErrorActionPreference = 'Stop'\n\
         $ws = New-Object -ComObject WScript.Shell\n\
         $path = {path}\n\
         if (Test-Path -LiteralPath $path) {{ Remove-Item -LiteralPath $path -Force }}\n\
         $link = $ws.CreateShortcut($path)\n\
         $link.TargetPath = {target}\n\
         $link.WorkingDirectory = {workdir}\n\
         if ({args} -ne '') {{ $link.Arguments = {args} }}\n\
         if ({icon} -ne '' -and (Test-Path -LiteralPath {icon})) {{ $link.IconLocation = {icon} }}\n\
         $link.Save()",
        path = crate::shim::ps_single_quoted(&lnk_path.to_string_lossy()),
        target = crate::shim::ps_single_quoted(&target.to_string_lossy()),
        workdir = crate::shim::ps_single_quoted(
            &target.parent().unwrap_or(Path::new("")).to_string_lossy()
        ),
        args = crate::shim::ps_single_quoted(&expanded_args),
        icon = crate::shim::ps_single_quoted(&icon_path),
    );

    let output = run_powershell_with_bypass(&script)?;
    if !output.success {
        let msg = decode_powershell_output(&output.output);
        return if msg.is_empty() {
            Err(format!("create shortcut {label}: {}", output.error))
        } else {
            Err(format!("create shortcut {label}: {}\n{msg}", output.error))
        };
    }
    Ok(())
}

/// Result of a PowerShell invocation.
struct PsOutput {
    success: bool,
    output: Vec<u8>,
    error: String,
}

/// Runs `powershell.exe` with the given script and captures combined output.
fn run_powershell_with_bypass(script: &str) -> Result<PsOutput, String> {
    let mut cmd = std::process::Command::new("powershell.exe");
    cmd.args([
        "-NoProfile",
        "-NonInteractive",
        "-ExecutionPolicy",
        "Bypass",
        "-Command",
        script,
    ]);
    procutil::hide_window(&mut cmd);
    match cmd.output() {
        Ok(out) => {
            let mut buf = out.stdout;
            buf.extend_from_slice(&out.stderr);
            Ok(PsOutput {
                success: out.status.success(),
                output: buf,
                error: format!("powershell exited with {}", out.status),
            })
        }
        Err(err) => Err(format!("run powershell: {err}")),
    }
}

/// Runs a simple PowerShell script, returning stdout bytes.
fn run_powershell(script: &str) -> Result<Vec<u8>, String> {
    let mut cmd = std::process::Command::new("powershell.exe");
    cmd.args(["-NoProfile", "-NonInteractive", "-Command", script]);
    procutil::hide_window(&mut cmd);
    cmd.output()
        .map(|out| out.stdout)
        .map_err(|err| format!("resolve start menu folder: {err}"))
}

/// Decodes PowerShell output: UTF-8 when valid, otherwise a lossy pass so a
/// GBK-encoded console message still surfaces.
pub fn decode_powershell_output(bytes: &[u8]) -> String {
    if bytes.is_empty() {
        return String::new();
    }
    let text = String::from_utf8_lossy(bytes);
    text.trim().to_string()
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::manifest::Manifest;

    fn manifest_from(json: &str) -> Manifest {
        crate::manifest::parse_str(json).expect("manifest")
    }

    #[test]
    fn expand_manifest_env_value_replaces_dir() {
        assert_eq!(
            expand_manifest_env_value("$dir\\bin;$dir\\sbin", r"C:\glue\apps\demo"),
            r"C:\glue\apps\demo\bin;C:\glue\apps\demo\sbin"
        );
        assert_eq!(expand_manifest_env_value("plain", r"C:\x"), "plain");
    }

    #[test]
    fn shim_env_for_package_expands_values() {
        let m = manifest_from(
            r#"{"version":"1","url":"https://e/x.zip","hash":"h",
                "env":{"DEMO_HOME":"$dir","DEMO_PLAIN":"static"}}"#,
        );
        let env = shim_env_for_package(&m, r"C:\glue\apps\demo", "");
        assert_eq!(
            env.get("DEMO_HOME").map(String::as_str),
            Some(r"C:\glue\apps\demo")
        );
        assert_eq!(env.get("DEMO_PLAIN").map(String::as_str), Some("static"));
    }

    #[test]
    fn expand_manifest_env_value_leaves_non_env_untouched() {
        let m = manifest_from(r#"{"version":"1","url":"https://e/x.zip","hash":"h"}"#);
        let env = shim_env_for_package(&m, r"C:\glue\apps\demo", "");
        assert!(env.is_empty());
    }

    #[test]
    fn apply_package_env_set_empty_is_ok() {
        let m = manifest_from(r#"{"version":"1","url":"https://e/x.zip","hash":"h"}"#);
        assert!(apply_package_env_set(&m, r"C:\glue\apps\demo", "").is_ok());
    }

    #[test]
    fn remove_user_env_empty_is_ok() {
        assert!(remove_user_env(&[]).is_ok());
    }

    #[test]
    fn shortcut_link_path_nests_labels() {
        let folder = Path::new(r"C:\Start\Programs\Glue Apps");
        let flat = shortcut_link_path(folder, "Demo");
        assert!(flat.ends_with("Demo.lnk"));
        let nested = shortcut_link_path(folder, "Tools/Demo");
        assert!(nested.to_string_lossy().ends_with(r"Tools\Demo.lnk"));
    }

    #[test]
    fn create_package_shortcuts_no_entries_is_ok() {
        let m = manifest_from(r#"{"version":"1","url":"https://e/x.zip","hash":"h"}"#);
        assert!(create_package_shortcuts(Path::new(r"C:\x"), &m, "").is_ok());
        assert!(remove_package_shortcuts(&m, "").is_ok());
    }

    #[test]
    fn decode_powershell_output_trims() {
        assert_eq!(decode_powershell_output(b"  hi  \r\n"), "hi");
        assert_eq!(decode_powershell_output(b""), "");
    }
}
