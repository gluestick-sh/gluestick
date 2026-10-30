//! Install layout helpers under the glue data root: `apps/<pkg>/<version>`
//! directories plus the `current` junction that marks the active version.
//!
//! Covers what the cache index/rebuild/GC stage and the install pipeline need:
//! `PkgRoot`, `ReadCurrent`/`LinkCurrent` (the `current` junction),
//! `ActiveInstallDir`, version picking and the per-version install record
//! (`install.json`).

use std::path::{Path, PathBuf};
use std::process::Command;

/// The directory junction/symlink alias for the active version.
pub const CURRENT_LINK_NAME: &str = "current";

/// The per-version install record filename.
pub const INSTALL_RECORD_FILE: &str = "install.json";

/// Per-version install record, saved for `glue reset`.
///
/// The manifest is kept as raw JSON so the record is a lossless snapshot: for
/// packages with `extract_dir` every arch block is preserved, whereas
/// [`crate::manifest::Manifest`] resolves arch blocks while parsing.
#[derive(Debug, Clone, Default, PartialEq, serde::Deserialize, serde::Serialize)]
pub struct InstallRecord {
    pub version: String,
    pub bucket: String,
    /// Architecture the install used (`arm64` / `64bit` / `32bit`).
    ///
    /// Empty for records written before the field existed. `glue reset`
    /// rebuilds the shims, `env_set` values and shortcuts of the version it
    /// switches to, and it has no `--arch` of its own; without this the rebuild
    /// would silently fall back to the host architecture for a package that was
    /// installed with an explicit override.
    #[serde(default)]
    pub arch: String,
    pub manifest: Option<serde_json::Value>,
}

/// Saves the install record (`install.json`) into the version directory.
///
/// `manifest_json` is the raw manifest as parsed from disk; pass `None` only
/// when the caller genuinely has no JSON (the field is written as `null`).
pub fn save_install_record(
    install_dir: &Path,
    bucket: &str,
    version: &str,
    manifest_json: Option<serde_json::Value>,
) -> std::io::Result<()> {
    save_install_record_with_arch(install_dir, bucket, version, "", manifest_json)
}

/// [`save_install_record`] with the architecture the install actually used.
///
/// The install pipeline passes the arch it resolved (including a `--arch`
/// override), so a later `glue reset` can rebuild the version-pinned side
/// effects exactly as they were created.
pub fn save_install_record_with_arch(
    install_dir: &Path,
    bucket: &str,
    version: &str,
    arch: &str,
    manifest_json: Option<serde_json::Value>,
) -> std::io::Result<()> {
    let record = InstallRecord {
        version: version.to_string(),
        bucket: bucket.to_string(),
        arch: arch.to_string(),
        manifest: manifest_json,
    };
    let data = serde_json::to_vec_pretty(&record)?;
    crate::atomic::write_atomic(&install_dir.join(INSTALL_RECORD_FILE), &data)
}

/// Reads `install.json` from a version directory.
pub fn load_install_record(install_dir: &Path) -> std::io::Result<InstallRecord> {
    let data = std::fs::read(install_dir.join(INSTALL_RECORD_FILE))?;
    let record = serde_json::from_slice(&data)?;
    Ok(record)
}

/// PkgRoot returns `apps/<pkgName>` under the glue root.
pub fn pkg_root(glue_root: &Path, pkg_name: &str) -> PathBuf {
    glue_root.join("apps").join(pkg_name)
}

/// LinkCurrent points `apps/<pkg>/current` at `apps/<pkg>/<version>` via a
/// directory junction (`cmd /C mklink /J`, hidden window).
pub fn link_current(pkg_root: &Path, version: &str) -> std::io::Result<()> {
    // `mklink` needs absolute paths, but a caller-supplied pkg_root is
    // already absolute in practice; only fall back to the CWD when it is
    // genuinely relative (avoids a per-call getcwd on the common path).
    let pkg_root = absolutize_if_relative(pkg_root);
    let target = pkg_root.join(version);
    let meta = std::fs::metadata(&target)?;
    if !meta.is_dir() {
        return Err(std::io::Error::new(
            std::io::ErrorKind::NotFound,
            format!("version directory not found: {}", target.display()),
        ));
    }
    let current = pkg_root.join(CURRENT_LINK_NAME);
    std::fs::create_dir_all(&pkg_root)?;
    // A leftover `current` must go, otherwise `mklink /J` fails with "Cannot
    // create a file when that file already exists". A stale junction is the
    // expected case; a real directory is not (see `remove_current_link`), and
    // the failure to clear it must not be swallowed into that cryptic error.
    remove_current_link(&current).map_err(|err| {
        std::io::Error::new(
            err.kind(),
            format!("remove existing {}: {err}", current.display()),
        )
    })?;
    let mut cmd = Command::new("cmd");
    cmd.args(["/C", "mklink", "/J"])
        .arg(crate::procutil::cmd_path_arg(&current))
        .arg(crate::procutil::cmd_path_arg(&target));
    crate::procutil::hide_window(&mut cmd);
    let out = cmd.output()?;
    if !out.status.success() {
        return Err(std::io::Error::other(format!(
            "mklink /J: {}",
            String::from_utf8_lossy(&out.stderr).trim()
        )));
    }
    Ok(())
}

/// ReadCurrent returns the active version name from the current junction.
///
/// (lexical) then.
/// Only the components are inspected, so no filesystem access is needed
/// beyond resolving the link itself.
pub fn read_current(pkg_root: &Path) -> Option<String> {
    let current = pkg_root.join(CURRENT_LINK_NAME);
    let target = std::fs::read_link(&current).ok()?;
    // A junction target is normally absolute; joining the (absolute) pkg_root
    // for a relative target keeps the result absolute without touching the
    // process CWD, which `absolutize` would query on every call.
    let target = if target.is_absolute() {
        target
    } else {
        pkg_root.join(target)
    };
    normalize_lexically(&target)
        .file_name()
        .map(|n| n.to_string_lossy().into_owned())
}

/// RemoveCurrent deletes the current junction/symlink without removing the
/// target version directory (`cmd /C rmdir` removes a junction only).
///
/// A *plain* directory named `current` is removed too. Manifest installer
/// scripts call `appdir <pkg>` for a dependency that is not installed yet, so
/// `New-Item -ItemType Junction` creates the missing parents — `86box-roms`
/// leaves a real `apps/86box/current/` holding its `roms` junction. That
/// directory then blocks the next `link_current` for `86box` itself.
pub fn remove_current_link(current: &Path) -> std::io::Result<()> {
    // std::fs::symlink_metadata distinguishes a missing entry from other
    // errors (lstat + IsNotExist).
    let meta = match std::fs::symlink_metadata(current) {
        Ok(meta) => meta,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(()),
        Err(e) => return Err(e),
    };
    // `read_link` only succeeds on a reparse point (junction or symlink), and
    // those must be *unlinked*, never deleted through: the target is another
    // package's install directory.
    let is_link = std::fs::read_link(current).is_ok();
    if !is_link {
        if meta.is_dir() {
            // `remove_dir_all` unlinks nested reparse points instead of
            // recursing into them, so a manifest-made link inside survives as
            // a link removal, not as a deletion of its target.
            return std::fs::remove_dir_all(current);
        }
        // A stray file named `current` — `rmdir` would not remove it.
        return std::fs::remove_file(current);
    }
    let mut cmd = Command::new("cmd");
    cmd.args(["/C", "rmdir"])
        .arg(crate::procutil::cmd_path_arg(current));
    crate::procutil::hide_window(&mut cmd);
    let out = cmd.output()?;
    if !out.status.success() {
        return Err(std::io::Error::other(format!(
            "rmdir current: {}",
            String::from_utf8_lossy(&out.stderr).trim()
        )));
    }
    Ok(())
}

/// ListVersions returns installed version directory names, excluding current
/// and empty directories.
///
/// Sorted by name, which is what gives — callers depend on
/// that order (e.g. `Engine::list_installed` walks the list backwards to find
/// the newest version, and `std::fs::read_dir` on Windows yields directory
/// order instead).
pub fn list_versions(pkg_root: &Path) -> Vec<String> {
    let Ok(entries) = std::fs::read_dir(pkg_root) else {
        return Vec::new();
    };
    let mut versions = Vec::new();
    for entry in entries.flatten() {
        // DirEntry::file_type is a cheap cached stat on Windows (no extra
        // syscall after read_dir); Junctions report is_dir, and
        // the name check below still filters `current`.
        let Ok(file_type) = entry.file_type() else {
            continue;
        };
        if !file_type.is_dir() {
            continue;
        }
        let name = entry.file_name().to_string_lossy().into_owned();
        if name == CURRENT_LINK_NAME {
            continue;
        }
        if dir_is_empty(&pkg_root.join(&name)) {
            continue;
        }
        versions.push(name);
    }
    versions.sort();
    versions
}

/// dirIsEmpty checks if a directory is empty or doesn't exist.
fn dir_is_empty(dir: &Path) -> bool {
    match std::fs::read_dir(dir) {
        Ok(mut entries) => entries.next().is_none(),
        Err(_) => true,
    }
}

/// PickDefaultVersion chooses the best version when no current link exists
/// (migration): the lexicographically greatest version directory name
/// (Scoop-style).
pub fn pick_default_version(pkg_root: &Path) -> Option<String> {
    list_versions(pkg_root).into_iter().max()
}

/// ActiveInstallDir returns the install directory and version to scan for
/// on-disk refs: the `current` link when valid, else the default version.
///
/// The current link wins only when it resolves
/// to a non-empty directory; otherwise fall back to the lexicographically
/// greatest non-empty version.
pub fn active_install_dir(pkg_root: &Path) -> Option<(PathBuf, String)> {
    // Read the version list once: on slow/virtualized filesystems every
    // `read_dir` matters, and the fallback path needs the same data the
    // validity check does.
    let versions = list_versions(pkg_root);
    if let Some(ver) = read_current(pkg_root) {
        let dir = pkg_root.join(&ver);
        let valid = versions.iter().any(|v| v == &ver)
            && std::fs::metadata(&dir).map(|m| m.is_dir()).unwrap_or(false);
        if valid {
            return Some((dir, ver));
        }
    }
    let ver = versions.into_iter().max()?;
    Some((pkg_root.join(&ver), ver))
}

/// Absolutize a path lexically (semantics) only when it is
/// relative. Callers in this module always hold an absolute glue root, so the
/// `getcwd` syscall is skipped on the hot path; `..`/`.` segments are still
/// normalized away.
fn absolutize_if_relative(path: &Path) -> PathBuf {
    if path.is_absolute() {
        return normalize_lexically(path);
    }
    match std::env::current_dir() {
        Ok(cwd) => normalize_lexically(&cwd.join(path)),
        Err(_) => normalize_lexically(path),
    }
}

/// Replace `.` and `..` segments without touching the filesystem.
fn normalize_lexically(path: &Path) -> PathBuf {
    use std::path::Component;
    let mut out = PathBuf::new();
    for comp in path.components() {
        match comp {
            Component::CurDir => {}
            Component::ParentDir => {
                if !out.pop() {
                    out.push("..");
                }
            }
            other => out.push(other.as_os_str()),
        }
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::testutil::TempDir;
    use std::fs;

    #[test]
    fn pkg_root_layout() {
        assert_eq!(
            pkg_root(Path::new("/glue"), "git"),
            PathBuf::from("/glue/apps/git")
        );
    }

    #[test]
    fn active_install_dir_prefers_current() {
        let root = TempDir::new("glue-apps-preferred");
        let pkg = root.join("demo");
        for ver in ["1.0.0", "2.0.0"] {
            let dir = pkg.join(ver);
            fs::create_dir_all(&dir).unwrap();
            fs::write(dir.join("app.exe"), b"x").unwrap();
        }
        link_current(&pkg, "1.0.0").unwrap();

        let (install_dir, version) = active_install_dir(&pkg).unwrap();
        assert_eq!(version, "1.0.0");
        assert_eq!(install_dir, pkg.join("1.0.0"));
    }

    /// Creates a directory junction, the way `link_current` does.
    fn mklink_junction(link: &Path, target: &Path) {
        let mut cmd = std::process::Command::new("cmd");
        cmd.args(["/C", "mklink", "/J"])
            .arg(crate::procutil::cmd_path_arg(link))
            .arg(crate::procutil::cmd_path_arg(target));
        let out = cmd.output().unwrap();
        assert!(out.status.success(), "mklink failed: {out:?}");
    }

    /// A manifest installer can leave a real `current` directory (it calls
    /// `appdir <pkg>` for a dependency that is not installed yet, and
    /// `New-Item -ItemType Junction` creates the missing parents).
    /// `86box-roms` leaves `apps/86box/current/roms` that way, and the stale
    /// directory used to fail the next `link_current` for `86box` with
    /// `mklink /J: Cannot create a file when that file already exists`.
    /// Clearing it must unlink the nested junction, never delete through it.
    #[test]
    fn link_current_replaces_a_plain_current_directory() {
        let root = TempDir::new("glue-apps-plain-current");
        let pkg = root.join("pkg");
        let install_dir = pkg.join("1.0.0");
        fs::create_dir_all(&install_dir).unwrap();
        fs::write(install_dir.join("app.exe"), b"x").unwrap();

        // Another package's install dir, linked from inside the stray
        // `current` — the `86box-roms` shape.
        let linked = root.join("other/6.0");
        fs::create_dir_all(&linked).unwrap();
        fs::write(linked.join("bios.bin"), b"y").unwrap();
        let stray = pkg.join(CURRENT_LINK_NAME);
        fs::create_dir_all(&stray).unwrap();
        mklink_junction(&stray.join("roms"), &linked);

        link_current(&pkg, "1.0.0").unwrap();

        assert_eq!(read_current(&pkg).as_deref(), Some("1.0.0"));
        assert!(
            linked.join("bios.bin").is_file(),
            "the junction inside `current` must be unlinked, not deleted through"
        );
    }

    #[test]
    fn active_install_dir_skips_empty_version_dir() {
        let root = TempDir::new("glue-apps-empty-ver");
        let pkg = root.join("demo");
        fs::create_dir_all(pkg.join("1.0.0")).unwrap();
        assert!(active_install_dir(&pkg).is_none());
    }

    #[test]
    fn active_install_dir_falls_back_to_latest_version() {
        let root = TempDir::new("glue-apps-fallback");
        let pkg = root.join("demo");
        for ver in ["1.0.0", "2.1.0", "1.9.9"] {
            let dir = pkg.join(ver);
            fs::create_dir_all(&dir).unwrap();
            fs::write(dir.join("app.exe"), b"x").unwrap();
        }
        // No current link → lexicographically greatest version wins.
        let (_, version) = active_install_dir(&pkg).unwrap();
        assert_eq!(version, "2.1.0");
    }

    #[test]
    fn read_current_missing_link_is_none() {
        let root = TempDir::new("glue-apps-no-current");
        assert!(read_current(root.path()).is_none());
    }

    #[test]
    fn install_record_round_trip() {
        let dir = TempDir::new("glue-apps-record");
        let manifest_json: serde_json::Value = serde_json::from_str(
            r#"{"version":"2.0.1","url":"https://example.com/app.zip","hash":"deadbeef"}"#,
        )
        .unwrap();
        save_install_record(dir.path(), "extras", "2.0.1", Some(manifest_json)).unwrap();

        let rec = load_install_record(dir.path()).unwrap();
        assert_eq!(rec.version, "2.0.1");
        assert_eq!(rec.bucket, "extras");
        let m = rec.manifest.expect("manifest");
        assert_eq!(m["version"], "2.0.1");
        assert_eq!(m["url"], "https://example.com/app.zip");
    }

    #[test]
    fn load_install_record_missing_file_errors() {
        let dir = TempDir::new("glue-apps-record-missing");
        assert!(load_install_record(dir.path()).is_err());
    }

    #[test]
    fn install_record_preserves_arch_blocks() {
        // A manifest with `extract_dir` resolves arch blocks during parse;
        // storing raw JSON keeps them intact for reset.
        let dir = TempDir::new("glue-apps-record-arch");
        let raw: serde_json::Value = serde_json::from_str(
            r#"{"version":"1.0","architecture":{"64bit":{"url":"https://example.com/x64.zip"}}}"#,
        )
        .unwrap();
        save_install_record(dir.path(), "main", "1.0", Some(raw)).unwrap();
        let rec = load_install_record(dir.path()).unwrap();
        assert_eq!(
            rec.manifest.unwrap()["architecture"]["64bit"]["url"],
            "https://example.com/x64.zip"
        );
    }

    /// `glue reset` reads the arch back to rebuild the version's shims; records
    /// written before the field existed must still load (as "").
    #[test]
    fn install_record_arch_round_trips_and_defaults_empty() {
        let dir = TempDir::new("glue-apps-record-arch-field");
        save_install_record_with_arch(dir.path(), "main", "1.0", "32bit", None).unwrap();
        assert_eq!(load_install_record(dir.path()).unwrap().arch, "32bit");

        // An older record has no `arch` key at all.
        fs::write(
            dir.join(INSTALL_RECORD_FILE),
            br#"{"version":"1.0","bucket":"main","manifest":null}"#,
        )
        .unwrap();
        assert_eq!(load_install_record(dir.path()).unwrap().arch, "");
    }
}
