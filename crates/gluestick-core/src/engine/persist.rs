//! `persist` data handling.
//!
//! Scoop's `persist` entries keep user data across reinstall/update: the data
//! lives in `persist/<pkg>/…` while the install directory holds either a
//! **junction** (directory entries) or a **copy** (file entries).
//!
//! Three moments matter:
//!
//! * **install** — [`restore_persist_on_install`] links/copies the stored data
//!   back into the freshly extracted version directory, and the two
//!   `prepare_*` helpers clear stale placeholders so Scoop `pre_install` /
//!   `post_install` `New-Item` calls can run again.
//! * **update** — the old version's data is saved out *before* its directory is
//!   removed ([`save_persist_on_uninstall`]), then restored into the new one.
//! * **uninstall** — same save step; junctions are *dropped*, never followed,
//!   so the stored data survives.
//!
//! Junctions (not symlinks) are used deliberately: creating a directory symlink
//! on Windows needs SeCreateSymbolicLinkPrivilege, which a normal user account
//! does not have, while a junction needs none.

use std::io;
use std::path::{Path, PathBuf};
use std::process::Command;

use crate::manifest::PersistEntry;
use crate::procutil;

/// A persist-handling failure. Wrapped by callers into
/// [`InstallError::App`](crate::engine::install::InstallError::App) so the CLI
/// prints it verbatim.
#[derive(Debug, thiserror::Error)]
pub enum PersistError {
    #[error("{0}")]
    Io(String),
}

impl From<io::Error> for PersistError {
    fn from(err: io::Error) -> Self {
        PersistError::Io(err.to_string())
    }
}

type Result<T> = std::result::Result<T, PersistError>;

/// Link `link` to `target` with a directory junction
///
/// Both paths must be absolute — `mklink /J` resolves relative paths against the
/// process CWD, which is not necessarily the parent of `link`.
pub fn link_directory_junction(link: &Path, target: &Path) -> Result<()> {
    let meta = std::fs::metadata(target)?;
    if !meta.is_dir() {
        return Err(PersistError::Io(format!(
            "junction target not a directory: {}",
            target.display()
        )));
    }
    if let Some(parent) = link.parent() {
        std::fs::create_dir_all(parent)?;
    }
    let _ = remove_directory_link(link);
    let mut cmd = Command::new("cmd");
    cmd.args(["/C", "mklink", "/J"])
        .arg(crate::procutil::cmd_path_arg(link))
        .arg(crate::procutil::cmd_path_arg(target));
    procutil::hide_window(&mut cmd);
    let out = cmd.output()?;
    if !out.status.success() {
        return Err(PersistError::Io(format!(
            "mklink /J {} {}: {}",
            crate::procutil::cmd_path_arg(link),
            crate::procutil::cmd_path_arg(target),
            String::from_utf8_lossy(&out.stderr).trim()
        )));
    }
    Ok(())
}

/// The junction/symlink target of `path`, or `(_, false, _)` when `path` is not
/// a link or does not exist.
///
/// A plain file or directory yields `(String::new(), false)`. `read_link` is
/// the only reliable probe, but its error is the localized
/// `ERROR_NOT_A_REPARSE_POINT`, so the answer is confirmed against the file
/// attribute instead of the message text.
pub fn directory_link_target(path: &Path) -> Result<(String, bool)> {
    let err = match std::fs::read_link(path) {
        Ok(target) => return Ok((target.to_string_lossy().into_owned(), true)),
        Err(err) if err.kind() == io::ErrorKind::NotFound => return Ok((String::new(), false)),
        Err(err) => err,
    };
    // `read_link` fails with ERROR_NOT_A_REPARSE_POINT on a plain file or
    // directory. Decide by the file attribute, not by the error text: the OS
    // message is localized, so matching "reparse point" worked only on English
    // Windows and turned an ordinary directory into a hard install failure on
    // others (os error 4390).
    if !is_reparse_point(path) {
        return Ok((String::new(), false));
    }
    Err(PersistError::Io(err.to_string()))
}

/// Whether `path` carries `FILE_ATTRIBUTE_REPARSE_POINT`, i.e. is a junction,
/// symlink or other reparse point. Does not follow the link itself.
fn is_reparse_point(path: &Path) -> bool {
    #[cfg(windows)]
    {
        use std::os::windows::fs::MetadataExt;
        const FILE_ATTRIBUTE_REPARSE_POINT: u32 = 0x400;
        std::fs::symlink_metadata(path)
            .map(|meta| meta.file_attributes() & FILE_ATTRIBUTE_REPARSE_POINT != 0)
            .unwrap_or(false)
    }
    #[cfg(not(windows))]
    {
        std::fs::symlink_metadata(path)
            .map(|meta| meta.file_type().is_symlink())
            .unwrap_or(false)
    }
}

/// Remove a directory junction/symlink without touching its target
/// (: `cmd /C rmdir`).
///
/// A *plain* (non-empty) directory is left alone: `rmdir` fails on it, which is
/// the intended guard — callers that mean "delete the tree" must say so.
pub fn remove_directory_link(path: &Path) -> Result<()> {
    match std::fs::symlink_metadata(path) {
        Ok(_) => {}
        Err(err) if err.kind() == io::ErrorKind::NotFound => return Ok(()),
        Err(err) => return Err(PersistError::Io(err.to_string())),
    }
    let mut cmd = Command::new("cmd");
    cmd.args(["/C", "rmdir"]).arg(procutil::cmd_path_arg(path));
    procutil::hide_window(&mut cmd);
    let out = cmd.output()?;
    if !out.status.success() {
        // Swallowed exactlyan unremovable empty link is not worth
        // aborting an uninstall over. (The caller can still see the path on
        // disk afterwards — that is observable behaviour too.)
        let _ = out;
    }
    Ok(())
}

/// Decide file vs directory for a persist entry.
///
/// The live install path wins when it exists (the archive may ship a dotfile
/// such as `.portable`, or a dotted directory), then the stored persist data,
/// and only then the name heuristic — which cannot tell `.portable` from
/// `.config` and used to create a directory where the installer script writes
/// a file.
fn resolve_persist_entry_is_file(
    install_path: &Path,
    persist_path: &Path,
    entry: &PersistEntry,
) -> bool {
    // `metadata` follows junctions, so a persisted directory that is already
    // linked still reads as a directory.
    if let Ok(info) = std::fs::metadata(install_path) {
        return !info.is_dir();
    }
    if let Ok(info) = std::fs::metadata(persist_path) {
        return !info.is_dir();
    }
    entry.looks_like_file()
}

/// `PrepareInstallDirForPreInstallHooks`: drop stale persist placeholders a
/// failed install left behind so Scoop-style `New-Item` calls can run again.
///
/// Non-empty directories from a fresh extract are kept (e.g. obs-studio `data/`).
pub fn prepare_install_dir_for_pre_install_hooks(
    install_dir: &Path,
    persist_dir: &Path,
    persist: &[PersistEntry],
) -> Result<()> {
    if persist.is_empty() {
        return Ok(());
    }
    std::fs::create_dir_all(persist_dir)?;
    for item in persist {
        let persist_path = persist_dir.join(item.data_name());
        if persist_path.exists() {
            continue;
        }
        let install_path = install_dir.join(item.install_name());
        let is_file = resolve_persist_entry_is_file(&install_path, &persist_path, item);
        remove_stale_persist_install_path(&install_path, is_file)?;
    }
    Ok(())
}

/// `PrepareInstallDirForPostInstallHooks`: make sure persist install paths are
/// *directories* (not stale files) when no data is stored under `persist/`.
pub fn prepare_install_dir_for_post_install_hooks(
    install_dir: &Path,
    persist_dir: &Path,
    persist: &[PersistEntry],
) -> Result<()> {
    if persist.is_empty() {
        return Ok(());
    }
    for item in persist {
        let persist_path = persist_dir.join(item.data_name());
        if persist_path.exists() {
            continue;
        }
        let install_path = install_dir.join(item.install_name());
        // A directory entry whose install path is a stale *file* is exactly
        // what this function must replace, so the name heuristic decides
        // first. Only a file-looking entry defers to the live shape — dotfiles
        // such as `.portable` can legitimately be either.
        let is_file = item.looks_like_file()
            && resolve_persist_entry_is_file(&install_path, &persist_path, item);
        if is_file {
            continue;
        }
        ensure_install_persist_directory(&install_path)?;
    }
    Ok(())
}

/// `RestorePersistOnInstall`: link install persist paths to `persist/` when
/// data exists, otherwise ensure empty directories for Scoop `post_install`.
///
/// Directory entries become **junctions**; file entries are copied (Scoop's own
/// behaviour — a linked file cannot be replaced atomically by an updater).
pub fn restore_persist_on_install(
    install_dir: &Path,
    persist_dir: &Path,
    persist: &[PersistEntry],
) -> Result<()> {
    if persist.is_empty() {
        return Ok(());
    }
    // Always creates `persist/<pkg>`, even when the package has no data yet.
    std::fs::create_dir_all(persist_dir)?;
    for item in persist {
        let install_path = install_dir.join(item.install_name());
        let persist_path = persist_dir.join(item.data_name());
        let is_file = resolve_persist_entry_is_file(&install_path, &persist_path, item);
        if persist_path.exists() {
            if is_file {
                normalize_persist_file_store(&persist_path)?;
                if persist_path.exists() {
                    copy_persist_file_to_install(&persist_path, &install_path)?;
                }
            } else {
                // The archive may ship the persisted path as a real directory
                // (tor-browser's default profile): merge it into the stored
                // data first, otherwise `rmdir` refuses the non-empty
                // directory and `mklink /J` fails with "Cannot create a file
                // when that file already exists".
                merge_fresh_install_dir_into_persist(&install_path, &persist_path)?;
                link_directory_junction(&install_path, &persist_path)?;
            }
            continue;
        }
        if is_file {
            // Deliberately leaves a missing *file* entry alone: `pre_install`
            // / `post_install` is expected to create it, and an empty
            // placeholder would make the app think its config is empty.
            continue;
        }
        ensure_install_persist_directory(&install_path)?;
    }
    Ok(())
}

/// Merge a freshly extracted (non-link) directory into the stored persist
/// directory so a junction can replace it.
///
/// Anything already under `persist/` wins — it is the user's data, and the
/// fresh copy is only the archive's default. Missing entries are moved over
/// (rename, with a copy fallback across volumes); the emptied install path is
/// then removed so the caller can create the junction.
fn merge_fresh_install_dir_into_persist(install_path: &Path, persist_path: &Path) -> Result<()> {
    let info = match std::fs::symlink_metadata(install_path) {
        Ok(info) => info,
        Err(err) if err.kind() == io::ErrorKind::NotFound => return Ok(()),
        Err(err) => return Err(PersistError::Io(err.to_string())),
    };
    // A file where a directory is expected is the caller's junction attempt to
    // report; an existing link already points at the stored data.
    if !info.is_dir() || directory_link_target(install_path)?.1 {
        return Ok(());
    }
    for entry in std::fs::read_dir(install_path)? {
        let entry = entry?;
        let src = entry.path();
        let dst = persist_path.join(entry.file_name());
        if std::fs::symlink_metadata(&dst).is_ok() {
            continue; // stored data wins
        }
        match std::fs::rename(&src, &dst) {
            Ok(()) => {}
            // Cross-volume rename: copy instead (persist may be elsewhere).
            Err(err) if err.kind() == io::ErrorKind::CrossesDevices => {
                if std::fs::symlink_metadata(&src)?.is_dir() {
                    copy_dir_all(&src, &dst)?;
                } else {
                    std::fs::copy(&src, &dst)?;
                }
            }
            Err(err) => return Err(PersistError::Io(err.to_string())),
        }
    }
    remove_install_persist_path(install_path)
}

/// `SavePersistOnUninstall`: move version-local persist data into `persist/`
/// before the version directory is removed.
///
/// Junctions into `persist/` are dropped *without* deleting their data.
pub fn save_persist_on_uninstall(
    install_dir: &Path,
    persist_dir: &Path,
    persist: &[PersistEntry],
) -> Result<()> {
    if persist.is_empty() {
        return Ok(());
    }
    std::fs::create_dir_all(persist_dir)?;
    for item in persist {
        let install_path = install_dir.join(item.install_name());
        let persist_path = persist_dir.join(item.data_name());
        match std::fs::symlink_metadata(&install_path) {
            Ok(_) => {}
            Err(err) if err.kind() == io::ErrorKind::NotFound => continue,
            Err(err) => return Err(PersistError::Io(err.to_string())),
        }
        if let (target, true) = directory_link_target(&install_path)? {
            let abs_target = absolutize(Path::new(&target));
            let abs_persist = absolutize(&persist_path);
            if same_path_ci(&abs_target, &abs_persist) {
                remove_directory_link(&install_path)?;
                continue;
            }
        }
        move_install_persist_to_store(&install_path, &persist_path)?;
    }
    Ok(())
}

fn move_install_persist_to_store(install_path: &Path, persist_path: &Path) -> Result<()> {
    let install_info = match std::fs::symlink_metadata(install_path) {
        Ok(info) => info,
        Err(err) if err.kind() == io::ErrorKind::NotFound => return Ok(()),
        Err(err) => return Err(PersistError::Io(err.to_string())),
    };
    if !install_info.is_dir() {
        return merge_install_file_into_persist(install_path, persist_path);
    }
    if std::fs::symlink_metadata(persist_path).is_err() {
        if let Some(parent) = persist_path.parent() {
            std::fs::create_dir_all(parent)?;
        }
        return match std::fs::rename(install_path, persist_path) {
            Ok(()) => Ok(()),
            // Cross-device rename (persist on another volume): fall back to a
            // copy, which is what would fail on.
            Err(err) if err.kind() == io::ErrorKind::CrossesDevices => {
                copy_dir_all(install_path, persist_path)?;
                remove_install_persist_path(install_path)
            }
            Err(err) => Err(PersistError::Io(err.to_string())),
        };
    }
    let persist_info =
        std::fs::symlink_metadata(persist_path).map_err(|e| PersistError::Io(e.to_string()))?;
    if !persist_info.is_dir() {
        return merge_install_file_into_persist(install_path, persist_path);
    }
    for entry in std::fs::read_dir(install_path)? {
        let entry = entry?;
        let src = entry.path();
        let dst = persist_path.join(entry.file_name());
        merge_install_entry_into_persist(&src, &dst)?;
    }
    remove_install_persist_path(install_path)
}

fn merge_install_file_into_persist(install_path: &Path, persist_path: &Path) -> Result<()> {
    if let Some(parent) = persist_path.parent() {
        std::fs::create_dir_all(parent)?;
    }
    let install_info = match std::fs::symlink_metadata(install_path) {
        Ok(info) => info,
        Err(err) if err.kind() == io::ErrorKind::NotFound => return Ok(()),
        Err(err) => return Err(PersistError::Io(err.to_string())),
    };
    if install_info.is_dir() {
        return Err(PersistError::Io(format!(
            "expected file at {}",
            install_path.display()
        )));
    }
    match std::fs::symlink_metadata(persist_path) {
        Ok(persist_info) => {
            if persist_info.is_dir() {
                remove_dir_all(persist_path)?;
                return match std::fs::rename(install_path, persist_path) {
                    Ok(()) => Ok(()),
                    Err(err) if err.kind() == io::ErrorKind::CrossesDevices => {
                        std::fs::copy(install_path, persist_path)?;
                        remove_install_file(install_path)
                    }
                    Err(err) => Err(PersistError::Io(err.to_string())),
                };
            }
            // Both are files: the stored one only wins wholesale, rewrites it.
            std::fs::copy(install_path, persist_path)?;
            remove_install_file(install_path)
        }
        Err(err) if err.kind() == io::ErrorKind::NotFound => {
            match std::fs::rename(install_path, persist_path) {
                Ok(()) => Ok(()),
                Err(err) if err.kind() == io::ErrorKind::CrossesDevices => {
                    std::fs::copy(install_path, persist_path)?;
                    remove_install_file(install_path)
                }
                Err(err) => Err(PersistError::Io(err.to_string())),
            }
        }
        Err(err) => Err(PersistError::Io(err.to_string())),
    }
}

fn merge_install_entry_into_persist(src: &Path, dst: &Path) -> Result<()> {
    let src_info = match std::fs::symlink_metadata(src) {
        Ok(info) => info,
        Err(err) if err.kind() == io::ErrorKind::NotFound => return Ok(()),
        Err(err) => return Err(PersistError::Io(err.to_string())),
    };
    if std::fs::symlink_metadata(dst).is_err() {
        return match std::fs::rename(src, dst) {
            Ok(()) => Ok(()),
            Err(err) if err.kind() == io::ErrorKind::CrossesDevices => {
                if src_info.is_dir() {
                    copy_dir_all(src, dst)?;
                    remove_dir_all(src)
                } else {
                    std::fs::copy(src, dst)?;
                    remove_install_file(src)
                }
            }
            Err(err) => Err(PersistError::Io(err.to_string())),
        };
    }
    if src_info.is_dir() {
        for entry in std::fs::read_dir(src)? {
            let entry = entry?;
            let child_src = entry.path();
            let child_dst = dst.join(entry.file_name());
            merge_install_entry_into_persist(&child_src, &child_dst)?;
        }
        return remove_install_persist_path(src);
    }
    match std::fs::rename(src, dst) {
        Ok(()) => Ok(()),
        Err(err) if err.kind() == io::ErrorKind::CrossesDevices => {
            std::fs::copy(src, dst)?;
            remove_install_file(src)
        }
        Err(err) => Err(PersistError::Io(err.to_string())),
    }
}

/// the path must be a real directory and
/// never a junction or a stale file afterwards.
fn ensure_install_persist_directory(path: &Path) -> Result<()> {
    let info = match std::fs::symlink_metadata(path) {
        Ok(info) => info,
        Err(err) if err.kind() == io::ErrorKind::NotFound => {
            return std::fs::create_dir_all(path).map_err(PersistError::from);
        }
        Err(err) => return Err(PersistError::Io(err.to_string())),
    };
    if let (_, true) = directory_link_target(path)? {
        return Ok(());
    }
    if !info.is_dir() {
        remove_install_file(path)?;
        return std::fs::create_dir_all(path).map_err(PersistError::from);
    }
    Ok(())
}

/// `RemoveAll` for real trees, link removal for
/// junctions (removing a junction with `RemoveAll` would delete the data it
/// points at — the classic persist-data-loss bug).
fn remove_install_persist_path(path: &Path) -> Result<()> {
    if let (_, true) = directory_link_target(path)? {
        return remove_directory_link(path);
    }
    remove_dir_all(path)
}

/// clear empty placeholders from a failed
/// install. Non-empty directories are normal extract output and are kept.
fn remove_stale_persist_install_path(path: &Path, file_entry: bool) -> Result<()> {
    let info = match std::fs::symlink_metadata(path) {
        Ok(info) => info,
        Err(err) if err.kind() == io::ErrorKind::NotFound => return Ok(()),
        Err(err) => return Err(PersistError::Io(err.to_string())),
    };
    if let (_, true) = directory_link_target(path)? {
        return remove_directory_link(path);
    }
    if !info.is_dir() {
        return remove_install_file(path);
    }
    if file_entry {
        return remove_dir_all(path);
    }
    if dir_is_empty(path) {
        return std::fs::remove_dir(path).map_err(PersistError::from);
    }
    Ok(())
}

/// a file entry must not be a directory (an
/// older bug could leave `persist/<pkg>/<name>` as a folder).
fn normalize_persist_file_store(persist_path: &Path) -> Result<()> {
    let info = match std::fs::symlink_metadata(persist_path) {
        Ok(info) => info,
        Err(err) if err.kind() == io::ErrorKind::NotFound => return Ok(()),
        Err(err) => return Err(PersistError::Io(err.to_string())),
    };
    if info.is_dir() {
        return remove_dir_all(persist_path);
    }
    Ok(())
}

fn copy_persist_file_to_install(persist_path: &Path, install_path: &Path) -> Result<()> {
    if let Some(parent) = install_path.parent() {
        std::fs::create_dir_all(parent)?;
    }
    let data = std::fs::read(persist_path)?;
    remove_install_file(install_path)?;
    std::fs::write(install_path, data)?;
    Ok(())
}

/// the file may be read-only, so clear that bit first.
fn remove_install_file(path: &Path) -> Result<()> {
    let info = match std::fs::symlink_metadata(path) {
        Ok(info) => info,
        Err(err) if err.kind() == io::ErrorKind::NotFound => return Ok(()),
        Err(err) => return Err(PersistError::Io(err.to_string())),
    };
    if info.permissions().readonly() {
        let mut perms = info.permissions();
        #[allow(clippy::permissions_set_readonly_false)]
        perms.set_readonly(false);
        let _ = std::fs::set_permissions(path, perms);
    }
    match std::fs::remove_file(path) {
        Ok(()) => Ok(()),
        Err(err) if err.kind() == io::ErrorKind::NotFound => Ok(()),
        Err(err) => Err(PersistError::Io(err.to_string())),
    }
}

fn remove_dir_all(path: &Path) -> Result<()> {
    std::fs::remove_dir_all(path).map_err(PersistError::from)
}

/// `dir_is_empty` reports whether `dir` has no entries (missing counts as empty).
fn dir_is_empty(dir: &Path) -> bool {
    match std::fs::read_dir(dir) {
        Ok(mut entries) => entries.next().is_none(),
        Err(_) => true,
    }
}

/// Recursive copy used by the cross-device fallbacks.
fn copy_dir_all(src: &Path, dst: &Path) -> Result<()> {
    std::fs::create_dir_all(dst)?;
    for entry in std::fs::read_dir(src)? {
        let entry = entry?;
        let child_src = entry.path();
        let child_dst = dst.join(entry.file_name());
        let meta = std::fs::symlink_metadata(&child_src)?;
        if meta.is_dir() {
            copy_dir_all(&child_src, &child_dst)?;
        } else {
            std::fs::copy(&child_src, &child_dst)?;
        }
    }
    Ok(())
}

/// Compare two paths the way Windows does for junction targets
/// case-insensitively.
fn same_path_ci(a: &Path, b: &Path) -> bool {
    a.to_string_lossy()
        .eq_ignore_ascii_case(&b.to_string_lossy())
}

/// Lexically absolutize `path`
fn absolutize(path: &Path) -> PathBuf {
    if path.is_absolute() {
        normalize_lexically(path)
    } else {
        let cwd = std::env::current_dir().unwrap_or_else(|_| PathBuf::from("."));
        normalize_lexically(&cwd.join(path))
    }
}

/// Drop the NT object-namespace prefix from a drive-letter path.
///
/// `fs::read_link` returns a junction's stored substitute name verbatim, and on
/// some systems that is `\??\Global\C:\…` or `\\?\C:\…` rather than the plain
/// DOS path. All of them name the same file, but a leftover prefix defeats
/// [`same_path_ci`] — and that comparison is what decides between "drop the
/// junction, keep the data" and "treat it as a real directory". A false
/// mismatch therefore deletes the persisted data.
fn strip_nt_prefix(path: &Path) -> PathBuf {
    let raw = path.to_string_lossy();
    for prefix in [r"\\?\Global\", r"\\?\", r"\??\Global\", r"\??\"] {
        if let Some(rest) = raw.strip_prefix(prefix) {
            // Drive-letter paths only: `\\?\UNC\…` is a different namespace and
            // must keep its prefix.
            if rest.len() >= 2 && rest.as_bytes()[1] == b':' {
                return PathBuf::from(rest);
            }
        }
    }
    path.to_path_buf()
}

/// Cheap lexical normalisation: resolve `.` and `..` components without touching
/// the filesystem (a junction may have been removed by the time we compare).
fn normalize_lexically(path: &Path) -> PathBuf {
    use std::path::Component;
    let mut out = PathBuf::new();
    let path = strip_nt_prefix(path);
    for comp in path.components() {
        match comp {
            Component::CurDir => {}
            Component::ParentDir => {
                out.pop();
            }
            other => out.push(other.as_os_str()),
        }
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::manifest::PersistEntry;
    use crate::testutil::TempDir;

    /// `PersistEntry` is built by the manifest parser, not deserialised, so
    /// construct it directly the way the parser does for `{"install": "data"}`.
    fn entry(install: &str, data: &str) -> PersistEntry {
        PersistEntry {
            install: install.to_string(),
            data: data.to_string(),
        }
    }

    #[test]
    fn restore_merges_extracted_dir_into_existing_persist_data() {
        let root = TempDir::new("glue-persist-merge");
        let install_dir = root.path().join("apps/pkg/1.0.0");
        let persist_dir = root.path().join("persist/pkg");
        // Stored user data from a previous install.
        std::fs::create_dir_all(persist_dir.join("data")).unwrap();
        std::fs::write(persist_dir.join("data/user.ini"), b"mine").unwrap();
        // The archive ships the same path as a real (non-empty) directory.
        std::fs::create_dir_all(install_dir.join("data")).unwrap();
        std::fs::write(install_dir.join("data/user.ini"), b"default").unwrap();
        std::fs::write(install_dir.join("data/readme.txt"), b"hello").unwrap();

        let items = vec![entry("data", "data")];
        restore_persist_on_install(&install_dir, &persist_dir, &items)
            .expect("a non-empty extracted persist dir must merge, not fail");

        let (_, linked) = directory_link_target(&install_dir.join("data")).unwrap();
        assert!(linked, "data must be a junction after the merge");
        // Stored data wins; the archive's file fills the gap.
        assert_eq!(
            std::fs::read(persist_dir.join("data/user.ini")).unwrap(),
            b"mine"
        );
        assert_eq!(
            std::fs::read(persist_dir.join("data/readme.txt")).unwrap(),
            b"hello"
        );
    }

    #[test]
    fn restore_links_directories_and_copies_files() {
        let root = TempDir::new("glue-persist-restore");
        let install_dir = root.path().join("apps/pkg/1.0.0");
        let persist_dir = root.path().join("persist/pkg");
        std::fs::create_dir_all(persist_dir.join("data")).unwrap();
        std::fs::write(persist_dir.join("data/config.ini"), b"[x]").unwrap();
        std::fs::write(persist_dir.join("settings.json"), b"{}").unwrap();
        std::fs::create_dir_all(&install_dir).unwrap();

        let items = vec![entry("data", "data"), entry("config.json", "settings.json")];
        restore_persist_on_install(&install_dir, &persist_dir, &items).unwrap();

        // A junction is not a real directory from lstat's point of view.
        let (target, linked) = directory_link_target(&install_dir.join("data")).unwrap();
        assert!(linked, "data must be a junction");
        assert!(target.contains("persist"));

        // File entries are copied, never linked.
        let (_, file_linked) = directory_link_target(&install_dir.join("config.json")).unwrap();
        assert!(!file_linked);
        assert_eq!(
            std::fs::read(install_dir.join("config.json")).unwrap(),
            b"{}"
        );
    }

    #[test]
    fn restore_ensures_empty_directories_when_no_data() {
        let root = TempDir::new("glue-persist-empty");
        let install_dir = root.path().join("apps/pkg/1.0.0");
        let persist_dir = root.path().join("persist/pkg");
        std::fs::create_dir_all(&install_dir).unwrap();

        restore_persist_on_install(&install_dir, &persist_dir, &[entry("data", "data")]).unwrap();

        assert!(persist_dir.is_dir());
        assert!(install_dir.join("data").is_dir());
        assert!(!directory_link_target(&install_dir.join("data")).unwrap().1);
    }

    #[test]
    fn a_plain_directory_is_never_a_link() {
        // The OS text for ERROR_NOT_A_REPARSE_POINT is localized (zh-CN
        // says "此文件或目录不是一个重分析点"), so the decision must come
        // from the file attribute and not from a message match.
        let root = TempDir::new("glue-persist-plain");
        let dir = root.path().join("plain");
        std::fs::create_dir_all(&dir).unwrap();
        assert!(!is_reparse_point(&dir));
        assert!(!directory_link_target(&dir).unwrap().1);
    }

    #[test]
    fn restore_leaves_missing_file_entries_alone() {
        let root = TempDir::new("glue-persist-nofile");
        let install_dir = root.path().join("apps/pkg/1.0.0");
        let persist_dir = root.path().join("persist/pkg");
        std::fs::create_dir_all(&install_dir).unwrap();

        restore_persist_on_install(
            &install_dir,
            &persist_dir,
            &[entry("config.json", "config.json")],
        )
        .unwrap();

        // No placeholder: pre_install/post_install is expected to create it.
        assert!(!install_dir.join("config.json").exists());
        assert!(persist_dir.is_dir());
    }

    #[test]
    fn save_drops_junction_without_deleting_data() {
        let root = TempDir::new("glue-persist-save-link");
        let install_dir = root.path().join("apps/pkg/1.0.0");
        let persist_dir = root.path().join("persist/pkg");
        std::fs::create_dir_all(persist_dir.join("data")).unwrap();
        std::fs::write(persist_dir.join("data/kept.txt"), b"keep").unwrap();
        std::fs::create_dir_all(&install_dir).unwrap();

        link_directory_junction(&install_dir.join("data"), &persist_dir.join("data")).unwrap();
        save_persist_on_uninstall(&install_dir, &persist_dir, &[entry("data", "data")]).unwrap();

        assert!(!install_dir.join("data").exists());
        assert_eq!(
            std::fs::read(persist_dir.join("data/kept.txt")).unwrap(),
            b"keep"
        );
    }

    #[test]
    fn save_moves_unlinked_directory_into_persist() {
        let root = TempDir::new("glue-persist-save-move");
        let install_dir = root.path().join("apps/pkg/1.0.0");
        let persist_dir = root.path().join("persist/pkg");
        std::fs::create_dir_all(install_dir.join("data/nested")).unwrap();
        std::fs::write(install_dir.join("data/nested/a.txt"), b"a").unwrap();

        save_persist_on_uninstall(&install_dir, &persist_dir, &[entry("data", "data")]).unwrap();

        assert!(!install_dir.join("data").exists());
        assert_eq!(
            std::fs::read(persist_dir.join("data/nested/a.txt")).unwrap(),
            b"a"
        );
    }

    #[test]
    fn save_merges_into_existing_file() {
        let root = TempDir::new("glue-persist-save-merge");
        let install_dir = root.path().join("apps/pkg/1.0.0");
        let persist_dir = root.path().join("persist/pkg");
        std::fs::create_dir_all(&install_dir).unwrap();
        std::fs::write(install_dir.join("config.json"), b"new").unwrap();

        save_persist_on_uninstall(
            &install_dir,
            &persist_dir,
            &[entry("config.json", "config.json")],
        )
        .unwrap();

        assert_eq!(
            std::fs::read(persist_dir.join("config.json")).unwrap(),
            b"new"
        );
        assert!(!install_dir.join("config.json").exists());
    }

    #[test]
    fn prepare_pre_install_clears_stale_placeholder() {
        let root = TempDir::new("glue-persist-prepare");
        let install_dir = root.path().join("apps/pkg/1.0.0");
        let persist_dir = root.path().join("persist/pkg");
        std::fs::create_dir_all(install_dir.join("data")).unwrap();
        std::fs::write(install_dir.join("data/leftover"), b"x").unwrap();

        // Non-empty extract output is kept.
        prepare_install_dir_for_pre_install_hooks(
            &install_dir,
            &persist_dir,
            &[entry("data", "data")],
        )
        .unwrap();
        assert!(install_dir.join("data/leftover").exists());

        std::fs::remove_file(install_dir.join("data/leftover")).unwrap();
        prepare_install_dir_for_pre_install_hooks(
            &install_dir,
            &persist_dir,
            &[entry("data", "data")],
        )
        .unwrap();
        // Now empty and with no stored data: the placeholder goes away.
        assert!(!install_dir.join("data").exists());
        assert!(persist_dir.is_dir());
    }

    #[test]
    fn prepare_post_install_turns_stale_file_into_directory() {
        let root = TempDir::new("glue-persist-prepare-post");
        let install_dir = root.path().join("apps/pkg/1.0.0");
        let persist_dir = root.path().join("persist/pkg");
        std::fs::create_dir_all(&install_dir).unwrap();
        std::fs::write(install_dir.join("data"), b"stale").unwrap();

        prepare_install_dir_for_post_install_hooks(
            &install_dir,
            &persist_dir,
            &[entry("data", "data")],
        )
        .unwrap();

        assert!(install_dir.join("data").is_dir());
        assert!(!install_dir.join("data").is_file());
    }
}
