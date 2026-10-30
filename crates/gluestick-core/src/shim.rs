//! Scoop-style PATH shims: `shims-meta/<name>.json` config plus a copy of the
//! prebuilt `shim.exe` runner under `shims/<name>.exe`.
//!
//! The `shim.exe` runner is a prebuilt stub (`github.com/gluestick-sh/shim`),
//! copied into place rather than rebuilt, falling back to a `.bat` forwarder
//! when no stub can be found.
//!
//! Layout under the glue root:
//!   - `shims-meta/<name>.json` — [`Config`] (target path, default args, env)
//!   - `shims/<name>.exe`       — the runner binary (or `<name>.bat` fallback)

use std::collections::BTreeMap;
use std::fs;
use std::io;
use std::path::{Path, PathBuf};

/// The prebuilt shim runner filename.
const SHIM_EXE: &str = "shim.exe";

/// Rejects shim names that could escape the shim directories or break the
/// batch fallback: path separators, drive qualifiers, traversal segments,
/// Win32-forbidden and cmd-metacharacters, trailing dots/spaces, and reserved
/// device names.
///
/// Intentional divergence from (security hardening): shim manager
/// does not validate the name, so a malicious manifest `bin` alias naming an
/// absolute path or a `..` segment makes `Path.join` write (and later delete)
/// arbitrary `.exe`/`.json`/`.bat` files outside `shims/`.
pub fn validate_name(name: &str) -> io::Result<()> {
    fn invalid(reason: &str) -> io::Error {
        io::Error::new(
            io::ErrorKind::InvalidInput,
            format!("invalid shim name: {reason}"),
        )
    }
    if name.is_empty() || name == "." || name == ".." {
        return Err(invalid("empty or a traversal segment"));
    }
    if name.len() > 64 {
        return Err(invalid("longer than 64 characters"));
    }
    // Win32 strips trailing dots/spaces when creating the file, which would
    // desync create/remove from the on-disk name.
    if name.ends_with('.') || name.ends_with(' ') {
        return Err(invalid("trailing dot or space"));
    }
    // Win32-forbidden characters plus cmd metacharacters (`&`, `^`, `%`): the
    // `.bat` fallback embeds the name into a batch line unquoted.
    const FORBIDDEN: &[char] = &['/', '\\', ':', '*', '?', '"', '<', '>', '|', '&', '^', '%'];
    if name.chars().any(|c| c < ' ' || FORBIDDEN.contains(&c)) {
        return Err(invalid("contains a path separator or shell metacharacter"));
    }
    // Reserved device names match on the stem before the first dot (`NUL.json`
    // still resolves to the device).
    if is_reserved_device_name(name) {
        return Err(invalid("reserved device name"));
    }
    Ok(())
}

/// Whether `name`'s stem (before the first dot) is a reserved Win32 device
/// name — `NUL.json` still resolves to the NUL device. Shared with the
/// workspace registry, whose names also become directory names.
pub(crate) fn is_reserved_device_name(name: &str) -> bool {
    const RESERVED: &[&str] = &[
        "con", "prn", "aux", "nul", "com1", "com2", "com3", "com4", "com5", "com6", "com7", "com8",
        "com9", "lpt1", "lpt2", "lpt3", "lpt4", "lpt5", "lpt6", "lpt7", "lpt8", "lpt9",
    ];
    let stem = name.split('.').next().unwrap_or(name).to_ascii_lowercase();
    RESERVED.contains(&stem.as_str())
}

/// Per-executable shim configuration.
///
/// `command` and `path` carry the same target in the implementation; both
/// are written for `shim.exe` compatibility with existing installs.
#[derive(Debug, Clone, Default, PartialEq, Eq, serde::Deserialize, serde::Serialize)]
pub struct Config {
    /// Display name.
    pub name: String,
    /// Actual command to run.
    pub command: String,
    /// Default arguments.
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub args: Vec<String>,
    /// Package env vars applied at launch.
    #[serde(default, skip_serializing_if = "BTreeMap::is_empty")]
    pub env: BTreeMap<String, String>,
    /// Path to the executable.
    pub path: String,
}

/// Optional settings when creating a shim.
#[derive(Debug, Clone, Default)]
pub struct CreateOpts {
    pub args: Vec<String>,
    pub env: BTreeMap<String, String>,
}

/// Creates and manages shims under one glue root.
#[derive(Debug, Clone)]
pub struct Manager {
    root_dir: PathBuf,
    shims_dir: PathBuf,
    bin_dir: PathBuf,
}

impl Manager {
    /// Opens (creating if needed) the shim directories for `root_dir`.
    ///
    /// Creates `shims/` and `shims-meta/`, then caches a resolved `shim.exe`
    /// stub next to the glue root so later installs do not depend on the
    /// process working directory.
    pub fn new(root_dir: impl AsRef<Path>) -> io::Result<Manager> {
        let root_dir = root_dir.as_ref().to_path_buf();
        let bin_dir = root_dir.join("shims");
        let shims_dir = root_dir.join("shims-meta");

        fs::create_dir_all(&bin_dir)
            .map_err(|err| io::Error::new(err.kind(), format!("create shims dir: {err}")))?;
        fs::create_dir_all(&shims_dir)
            .map_err(|err| io::Error::new(err.kind(), format!("create shims-meta dir: {err}")))?;

        let manager = Manager {
            root_dir: root_dir.clone(),
            shims_dir,
            bin_dir,
        };
        if let Some(stub) = resolve_shim_stub_path(&root_dir) {
            // Use the same (canonicalised) directory the resolver used; a raw
            // current_exe().parent() can differ by the \\?\ prefix on Windows.
            let exe_dir = executable_dir();
            if should_cache_stub(&root_dir, &stub, exe_dir.as_deref()) {
                // Best effort: a failed cache copy just means every install re-resolves.
                let _ = cache_shim_stub(&root_dir, &stub);
            }
        }
        Ok(manager)
    }

    /// The directory that should be on `PATH`.
    pub fn bin_dir(&self) -> &Path {
        &self.bin_dir
    }

    /// The `shims-meta` directory holding the JSON configs.
    pub fn shims_meta_dir(&self) -> &Path {
        &self.shims_dir
    }

    /// Creates a shim: writes the config, then the runner beside it.
    pub fn create(&self, name: &str, target_path: &str, opts: &CreateOpts) -> io::Result<()> {
        validate_name(name)?;
        let config = Config {
            name: name.to_string(),
            command: target_path.to_string(),
            args: opts.args.clone(),
            env: opts.env.clone(),
            path: target_path.to_string(),
        };
        let data = serde_json::to_vec_pretty(&config)
            .map_err(|err| io::Error::other(format!("marshal config: {err}")))?;
        let config_path = self.shims_dir.join(format!("{name}.json"));
        crate::atomic::write_atomic(&config_path, &data)
            .map_err(|err| io::Error::new(err.kind(), format!("write config: {err}")))?;

        let shim_path = self.bin_dir.join(format!("{name}.exe"));
        self.create_runner(&shim_path, name)
            .map_err(|err| io::Error::new(err.kind(), format!("create shim: {err}")))
    }

    /// Removes a shim's config and runner; missing files are not an error.
    pub fn remove(&self, name: &str) -> io::Result<()> {
        validate_name(name)?;
        let _ = fs::remove_file(self.shims_dir.join(format!("{name}.json")));
        let _ = fs::remove_file(self.bin_dir.join(format!("{name}.exe")));
        let _ = fs::remove_file(self.bin_dir.join(format!("{name}.bat")));
        Ok(())
    }

    /// removes every shim whose config
    /// `Path` belongs to `pkg_name`, returning the removed names.
    ///
    /// Scans the shims-meta directory by ownership instead of re-deriving
    /// names from manifest bins: the install-time name drops executable
    /// extensions (`Zoom.exe` → shim `Zoom`), and `env_add_path` shims have
    /// no bin entry at all — a name-derivation pass would orphan both.
    pub fn remove_shims_for_package(&self, pkg_name: &str) -> Vec<String> {
        let mut removed = Vec::new();
        let entries = match fs::read_dir(&self.shims_dir) {
            Ok(entries) => entries,
            Err(_) => return removed,
        };
        for entry in entries.flatten() {
            if entry.path().is_dir() {
                continue;
            }
            let name = entry.file_name();
            let name = name.to_string_lossy();
            if !name.ends_with(".json") {
                continue;
            }
            let Ok(data) = fs::read(entry.path()) else {
                continue;
            };
            let Ok(cfg) = serde_json::from_slice::<Config>(&data) else {
                continue;
            };
            if belongs_to_package(&cfg.path, pkg_name) {
                let shim_name = name.trim_end_matches(".json").to_string();
                if self.remove(&shim_name).is_ok() {
                    removed.push(shim_name);
                }
            }
        }
        removed
    }

    /// Lists every installed shim config (unreadable entries are skipped).
    pub fn list(&self) -> io::Result<Vec<Config>> {
        let entries = fs::read_dir(&self.shims_dir)?;
        let mut configs = Vec::new();
        for entry in entries.flatten() {
            let name = entry.file_name();
            let name = name.to_string_lossy();
            if !name.ends_with(".json") {
                continue;
            }
            let Ok(data) = fs::read(entry.path()) else {
                continue;
            };
            if let Ok(cfg) = serde_json::from_slice::<Config>(&data) {
                configs.push(cfg);
            }
        }
        Ok(configs)
    }

    /// Copies the prebuilt runner, falling back to a batch forwarder.
    ///
    /// Fallback forwards to `glue shim-run <name> %*`; the implementation emits the
    /// same line so an existing `glue.exe` can serve it.
    fn create_runner(&self, shim_path: &Path, name: &str) -> io::Result<()> {
        let Some(stub_path) = resolve_shim_stub_path(&self.root_dir) else {
            return self.create_batch_runner(shim_path, name);
        };
        let src = fs::read(&stub_path)
            .map_err(|err| io::Error::new(err.kind(), format!("read shim stub: {err}")))?;
        let mut opts = fs::OpenOptions::new();
        opts.write(true).create(true).truncate(true);
        use std::io::Write;
        let mut file = opts
            .open(shim_path)
            .map_err(|err| io::Error::new(err.kind(), format!("write shim: {err}")))?;
        file.write_all(&src)
            .map_err(|err| io::Error::new(err.kind(), format!("write shim: {err}")))?;
        Ok(())
    }

    /// Writes the `.bat` fallback forwarder next to where the `.exe` would go.
    fn create_batch_runner(&self, shim_path: &Path, name: &str) -> io::Result<()> {
        let batch_path = shim_path.with_extension("bat");
        let runner = std::env::current_exe()
            .ok()
            .filter(|p| {
                let base = p
                    .file_name()
                    .map(|n| n.to_string_lossy().to_lowercase())
                    .unwrap_or_default();
                base.starts_with("gluestick") || base.starts_with("glue")
            })
            .map(|p| p.to_string_lossy().into_owned())
            .unwrap_or_else(|| "glue".to_string());
        let content = format!("@echo off\r\n\"{runner}\" shim-run {name} %*\r\n");
        crate::atomic::write_atomic(&batch_path, content.as_bytes())
            .map_err(|err| io::Error::new(err.kind(), format!("write batch shim: {err}")))
    }
}

/// Reports whether `shims-meta` still references `pkg_name`.
pub fn has_shims_for_package(shims_meta_dir: &Path, pkg_name: &str) -> bool {
    let Ok(entries) = fs::read_dir(shims_meta_dir) else {
        return false;
    };
    for entry in entries.flatten() {
        let name = entry.file_name();
        if !name.to_string_lossy().ends_with(".json") {
            continue;
        }
        let Ok(data) = fs::read(entry.path()) else {
            continue;
        };
        let Ok(cfg) = serde_json::from_slice::<Config>(&data) else {
            continue;
        };
        if belongs_to_package(&cfg.path, pkg_name) {
            return true;
        }
    }
    false
}

/// Reports whether a shim target path points into `pkg_name`'s apps directory.
pub fn belongs_to_package(cfg_path: &str, pkg_name: &str) -> bool {
    package_name_from_apps_path(cfg_path) == pkg_name
}

/// Reports whether the named shim already belongs to a *different* package.
///
/// A non-empty owner with `conflict == true` means the shim must not be
/// re-created by `pkg_name` (the shims directory is a flat namespace).
pub fn owned_by_other_package(
    shims_meta_dir: &Path,
    shim_name: &str,
    pkg_name: &str,
) -> Option<String> {
    let owner = shim_owner_package(shims_meta_dir, shim_name);
    if owner.is_empty() || owner == pkg_name {
        None
    } else {
        Some(owner)
    }
}

/// Reads the owning package name out of a shim config, `""` when unknown.
fn shim_owner_package(shims_meta_dir: &Path, shim_name: &str) -> String {
    let config_path = shims_meta_dir.join(format!("{shim_name}.json"));
    let Ok(data) = fs::read(config_path) else {
        return String::new();
    };
    let Ok(cfg) = serde_json::from_slice::<Config>(&data) else {
        return String::new();
    };
    package_name_from_apps_path(&cfg.path)
}

/// Extracts the package name from a glue apps path (`…/apps/<pkg>/…`).
///
/// Returns `""` when the path is not inside an apps directory.
pub fn package_name_from_apps_path(cfg_path: &str) -> String {
    let normalized = cfg_path.replace('\\', "/");
    let Some(idx) = normalized.find("/apps/") else {
        return String::new();
    };
    let rest = &normalized[idx + "/apps/".len()..];
    match rest.find('/') {
        Some(end) => rest[..end].to_string(),
        None => String::new(),
    }
}

/// Removes every shim whose target points into `pkg_name`'s apps directory.
///
/// Returns the removed shim names. A missing `shims-meta` directory is not an
/// error (nothing to remove).
pub fn remove_shims_for_package(
    manager: &Manager,
    shims_meta_dir: &Path,
    pkg_name: &str,
) -> io::Result<Vec<String>> {
    let entries = match fs::read_dir(shims_meta_dir) {
        Ok(entries) => entries,
        Err(err) if err.kind() == io::ErrorKind::NotFound => return Ok(Vec::new()),
        Err(err) => return Err(err),
    };

    let mut removed = Vec::new();
    for entry in entries.flatten() {
        let file_name = entry.file_name();
        let file_name = file_name.to_string_lossy();
        if !file_name.ends_with(".json") {
            continue;
        }
        let Ok(data) = fs::read(entry.path()) else {
            continue;
        };
        let Ok(cfg) = serde_json::from_slice::<Config>(&data) else {
            continue;
        };
        if belongs_to_package(&cfg.path, pkg_name) {
            let shim_name = file_name.trim_end_matches(".json").to_string();
            if manager.remove(&shim_name).is_ok() {
                removed.push(shim_name);
            }
        }
    }
    Ok(removed)
}

/// Resolves the prebuilt `shim.exe` stub, or `None` when nothing matches.
///
/// Candidate order: the glue root, the running
/// executable's directory and its ancestors' dev paths, the CWD dev paths, then
/// `%PROGRAMDATA%\glue`. Duplicates and empty entries are skipped.
///
/// In an *installed* tree the executable's directory is checked first: the
/// stub sits next to `glue.exe` (outside the data root), so upgrades replace it
/// there, while a cached `<data>\shim.exe` from an older build would be stale.
pub fn resolve_shim_stub_path(glue_root_dir: &Path) -> Option<PathBuf> {
    let exec_dir = executable_dir();
    let cwd = std::env::current_dir().ok();
    let program_data = std::env::var("PROGRAMDATA").ok();
    resolve_shim_stub_path_in(
        glue_root_dir,
        &EnvContext {
            exec_dir: exec_dir.as_deref(),
            installed: crate::paths::installed_data_root().is_some(),
            cwd: cwd.as_deref(),
            program_data: program_data
                .as_deref()
                .map(Path::new)
                .filter(|path| !path.as_os_str().is_empty()),
        },
    )
}

/// Everything [`resolve_shim_stub_path`] reads from the process environment.
///
/// Split out so the candidate order is testable without depending on where the
/// test binary happens to live. A checkout that has been installed once already
/// looks like an installed tree to `installed_data_root` (`target\debug\glue.exe`
/// beside `target\debug\.glue-install`), which would otherwise flip the order
/// under a test and hide a real regression in either branch.
struct EnvContext<'a> {
    /// Directory holding the running executable, symlinks resolved.
    exec_dir: Option<&'a Path>,
    /// The executable lives in an installation layout.
    installed: bool,
    cwd: Option<&'a Path>,
    program_data: Option<&'a Path>,
}

/// The resolver proper: same order, with every environment read passed in.
fn resolve_shim_stub_path_in(glue_root_dir: &Path, env: &EnvContext<'_>) -> Option<PathBuf> {
    let mut candidates: Vec<PathBuf> = Vec::new();

    let mut exe_candidates: Vec<PathBuf> = Vec::new();
    if let Some(exec_dir) = env.exec_dir {
        exe_candidates.push(exec_dir.join(SHIM_EXE));
        exe_candidates.extend(walk_up_shim_stub(exec_dir));
    }

    if env.installed {
        candidates.extend(exe_candidates);
        candidates.push(glue_root_dir.join(SHIM_EXE));
    } else {
        candidates.push(glue_root_dir.join(SHIM_EXE));
        candidates.extend(exe_candidates);
    }

    if let Some(cwd) = env.cwd {
        candidates.extend(dev_shim_stub_candidates(cwd));
    }
    if let Some(program_data) = env.program_data {
        candidates.push(program_data.join("glue").join(SHIM_EXE));
    }

    first_existing_path(&candidates)
}

/// The directory holding the running executable, symlinks resolved.
fn executable_dir() -> Option<PathBuf> {
    let exec_path = std::env::current_exe().ok()?;
    let resolved = fs::canonicalize(&exec_path).unwrap_or(exec_path);
    resolved.parent().map(|p| p.to_path_buf())
}

/// The first candidate that exists on disk, skipping empty entries and duplicates.
fn first_existing_path(candidates: &[PathBuf]) -> Option<PathBuf> {
    let mut seen: Vec<PathBuf> = Vec::new();
    for candidate in candidates {
        let cleaned = normalize_path(candidate);
        if seen.contains(&cleaned) {
            continue;
        }
        seen.push(cleaned.clone());
        if cleaned.exists() {
            return Some(cleaned);
        }
    }
    None
}

/// Lexically normalizes a path (approximation).
fn normalize_path(path: &Path) -> PathBuf {
    use std::path::Component;
    let mut out = PathBuf::new();
    for component in path.components() {
        match component {
            Component::CurDir => {}
            Component::ParentDir => {
                if !out.pop() {
                    out.push("..");
                }
            }
            other => out.push(other.as_os_str()),
        }
    }
    if out.as_os_str().is_empty() {
        out.push(".");
    }
    out
}

/// Dev-tree candidates for a prebuilt runner under `dir`.
fn dev_shim_stub_candidates(dir: &Path) -> Vec<PathBuf> {
    vec![dir.join("shim").join(SHIM_EXE), dir.join(SHIM_EXE)]
}

/// Walks up to 8 ancestors collecting dev-tree candidates.
fn walk_up_shim_stub(start_dir: &Path) -> Vec<PathBuf> {
    let mut out = Vec::new();
    let mut dir = start_dir.to_path_buf();
    for _ in 0..8 {
        out.extend(dev_shim_stub_candidates(&dir));
        let Some(parent) = dir.parent().map(|p| p.to_path_buf()) else {
            break;
        };
        if parent == dir {
            break;
        }
        dir = parent;
    }
    out
}

/// Path form for identity checks: canonicalised when possible, so the
/// `\\?\`-prefixed result of `fs::canonicalize` compares equal to a plain path.
fn comparable_path(path: &Path) -> PathBuf {
    fs::canonicalize(path)
        .map(|resolved| normalize_path(&resolved))
        .unwrap_or_else(|_| normalize_path(path))
}

/// Whether the stub should be copied into the glue root as a cache.
///
/// An installed tree keeps `shim.exe` next to `glue.exe`, both in the install
/// directory, which is also the data root; copying it to `<root>\shim.exe` would
/// be the same file. The cache still applies when the stub was discovered
/// somewhere else (PATH, a dev tree), where a pinned copy keeps later installs
/// independent of the working directory.
fn should_cache_stub(glue_root_dir: &Path, stub_path: &Path, exe_dir: Option<&Path>) -> bool {
    if comparable_path(&glue_root_dir.join(SHIM_EXE)) == comparable_path(stub_path) {
        return false;
    }
    match exe_dir {
        Some(dir) => comparable_path(&dir.join(SHIM_EXE)) != comparable_path(stub_path),
        None => true,
    }
}

/// Copies the stub to `glue_root/shim.exe` so later installs are cwd-independent.
///
/// The copy is staged beside the destination and renamed into place, so an
/// interrupted copy never leaves a truncated runner behind.
pub fn cache_shim_stub(glue_root_dir: &Path, stub_path: &Path) -> io::Result<()> {
    let dest = glue_root_dir.join(SHIM_EXE);
    if comparable_path(&dest) == comparable_path(stub_path) {
        return Ok(());
    }
    copy_file_atomic(stub_path, &dest)
}

/// Copies the current stub over existing `<name>.exe` shims whose bytes differ.
///
/// A shim is a byte copy of `shim.exe` made when its package was installed, so
/// an upgraded stub (installer update) only reaches already-installed commands
/// through this pass. A copy without a matching `<name>.json` is not a live
/// shim and is left alone, as are `.bat` fallbacks. A file that cannot be
/// replaced (a running shim) is skipped; the count reports the replacements.
pub fn refresh_shim_copies(glue_root: &Path) -> io::Result<usize> {
    let Some(stub) = resolve_shim_stub_path(glue_root) else {
        return Ok(0);
    };
    let stub_bytes = fs::read(&stub)?;
    let meta_dir = glue_root.join("shims-meta");
    let entries = match fs::read_dir(glue_root.join("shims")) {
        Ok(entries) => entries,
        Err(err) if err.kind() == io::ErrorKind::NotFound => return Ok(0),
        Err(err) => return Err(err),
    };

    let mut replaced = 0;
    for entry in entries.flatten() {
        let path = entry.path();
        if !path
            .extension()
            .is_some_and(|ext| ext.eq_ignore_ascii_case("exe"))
        {
            continue;
        }
        let Some(stem) = path.file_stem().and_then(|stem| stem.to_str()) else {
            continue;
        };
        if !meta_dir.join(format!("{stem}.json")).is_file() {
            continue;
        }
        let current = fs::read(&path).unwrap_or_default();
        if current == stub_bytes {
            continue;
        }
        if copy_file_atomic(&stub, &path).is_ok() {
            replaced += 1;
        }
    }
    Ok(replaced)
}

/// Stages `src` beside `dst` and renames it into place.
///
/// Windows refuses the rename while a process is running `dst`, so a failed
/// rename drops `dst` and retries once.
fn copy_file_atomic(src: &Path, dst: &Path) -> io::Result<()> {
    if let Some(parent) = dst.parent() {
        fs::create_dir_all(parent)?;
    }
    let tmp_path = dst.with_extension(format!("tmp-{}", std::process::id()));
    fs::copy(src, &tmp_path)?;
    match fs::rename(&tmp_path, dst) {
        Ok(()) => Ok(()),
        Err(_) => {
            let _ = fs::remove_file(dst);
            let result = fs::rename(&tmp_path, dst);
            if result.is_err() {
                let _ = fs::remove_file(&tmp_path);
            }
            result
        }
    }
}

/// Wraps `s` in single quotes for PowerShell, doubling embedded quotes
pub fn ps_single_quoted(s: &str) -> String {
    format!("'{}'", s.replace('\'', "''"))
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::testutil::TempDir;

    #[test]
    fn validate_name_accepts_ordinary_shim_names() {
        for name in ["app", "7z", "my-tool.v2", "g++", "Some_Cmd"] {
            assert!(validate_name(name).is_ok(), "{name} must be accepted");
        }
    }

    #[test]
    fn validate_name_rejects_escapes_and_shell_tricks() {
        for name in [
            "",
            ".",
            "..",
            r"C:\Windows\Temp\poc",
            "C:/Windows/Temp/poc",
            r"\\server\share\poc",
            r"..\outside",
            "a/b",
            "poc.",
            "poc ",
            "nul",
            "NUL.json",
            "com1",
            "aux.exe",
            "a&b",
            "a^b",
            "%PATH%",
            "a|b",
            "a\"b",
        ] {
            assert!(validate_name(name).is_err(), "{name:?} must be rejected");
        }
    }

    #[test]
    fn create_with_traversal_name_writes_nothing() {
        let root = TempDir::new("glue-shim-traversal");
        let manager = Manager::new(root.path()).unwrap();
        let err = manager
            .create(r"..\poc", "C:/app/tool.exe", &CreateOpts::default())
            .expect_err("traversal name must fail");
        assert_eq!(err.kind(), io::ErrorKind::InvalidInput);
        // Neither the config nor a runner may exist outside the shim dirs.
        assert!(!root.path().join("poc.json").exists());
        assert!(!root.path().join("poc.exe").exists());
        assert!(!root.path().join("poc.bat").exists());
    }

    #[test]
    fn remove_with_absolute_name_deletes_nothing_outside() {
        let root = TempDir::new("glue-shim-abs-remove");
        let manager = Manager::new(root.path()).unwrap();
        let victim = root.path().join("victim.exe");
        std::fs::write(&victim, b"keep").unwrap();
        let err = manager
            .remove(victim.to_str().unwrap())
            .expect_err("absolute name must fail");
        assert_eq!(err.kind(), io::ErrorKind::InvalidInput);
        assert_eq!(std::fs::read(&victim).unwrap(), b"keep");
    }

    #[test]
    fn stub_beside_the_executable_is_not_cached() {
        let root = TempDir::new("glue-shim-stub-cache-root");
        let exe_dir = TempDir::new("glue-shim-stub-cache-exe");
        let stub = exe_dir.path().join(SHIM_EXE);
        std::fs::write(&stub, b"MZ").unwrap();

        assert!(
            !should_cache_stub(root.path(), &stub, Some(exe_dir.path())),
            "a stub next to glue.exe must not be duplicated into the data root"
        );

        let elsewhere = TempDir::new("glue-shim-stub-cache-other");
        let other_stub = elsewhere.path().join(SHIM_EXE);
        std::fs::write(&other_stub, b"MZ").unwrap();
        assert!(
            should_cache_stub(root.path(), &other_stub, Some(exe_dir.path())),
            "a stub found elsewhere is cached at the data root"
        );
    }

    #[test]
    fn package_name_from_apps_path_forms() {
        assert_eq!(
            package_name_from_apps_path(r"C:\glue\apps\git\current\bin\git.exe"),
            "git"
        );
        assert_eq!(
            package_name_from_apps_path("/glue/apps/7zip/1.0/7z.exe"),
            "7zip"
        );
        assert_eq!(package_name_from_apps_path(r"C:\glue\bin\git.exe"), "");
        assert_eq!(package_name_from_apps_path("/glue/apps/git"), "");
    }

    #[test]
    fn belongs_and_has_shims_for_package() {
        let root = TempDir::new("glue-shim-own");
        let meta = root.join("shims-meta");
        fs::create_dir_all(&meta).unwrap();
        let cfg = Config {
            name: "git".into(),
            command: r"C:\glue\apps\git\current\bin\git.exe".into(),
            path: r"C:\glue\apps\git\current\bin\git.exe".into(),
            ..Default::default()
        };
        fs::write(meta.join("git.json"), serde_json::to_vec(&cfg).unwrap()).unwrap();

        assert!(has_shims_for_package(&meta, "git"));
        assert!(!has_shims_for_package(&meta, "curl"));
        assert!(belongs_to_package(&cfg.path, "git"));
        assert!(!belongs_to_package(&cfg.path, "curl"));
    }

    #[test]
    fn owned_by_other_package_reports_conflict() {
        let root = TempDir::new("glue-shim-conflict");
        let meta = root.join("shims-meta");
        fs::create_dir_all(&meta).unwrap();
        let cfg = Config {
            name: "node".into(),
            command: r"C:\glue\apps\nodejs\current\node.exe".into(),
            path: r"C:\glue\apps\nodejs\current\node.exe".into(),
            ..Default::default()
        };
        fs::write(meta.join("node.json"), serde_json::to_vec(&cfg).unwrap()).unwrap();

        // Same owner → no conflict.
        assert_eq!(owned_by_other_package(&meta, "node", "nodejs"), None);
        // Different owner → conflict with the owning package name.
        assert_eq!(
            owned_by_other_package(&meta, "node", "other"),
            Some("nodejs".to_string())
        );
        // No config at all → no conflict.
        assert_eq!(owned_by_other_package(&meta, "missing", "other"), None);
    }

    // `TestRemoveShimsForPackage`: ownership scan removes every shim whose
    // config Path belongs to the package — including the extension-dropped
    // name (`Zoom.exe` bin → shim `Zoom`) and env_add_path shims that have no
    // manifest bin entry at all, both of which a bin-name derivation misses.
    #[test]
    fn remove_shims_for_package_uses_ownership_scan() {
        let root = TempDir::new("glue-shim-remove-pkg");
        let meta = root.join("shims-meta");
        let bins = root.join("shims");
        fs::create_dir_all(&meta).unwrap();
        fs::create_dir_all(&bins).unwrap();

        let write_cfg = |name: &str, path: &str| {
            let cfg = Config {
                name: name.into(),
                command: path.into(),
                path: path.into(),
                ..Default::default()
            };
            fs::write(
                meta.join(format!("{name}.json")),
                serde_json::to_vec(&cfg).unwrap(),
            )
            .unwrap();
            fs::write(bins.join(format!("{name}.exe")), b"stub").unwrap();
        };
        write_cfg("Zoom", r"C:\glue\apps\zoom\current\Zoom.exe");
        write_cfg("zoomtool", r"C:\glue\apps\zoom\current\bin\zoomtool.exe"); // env_add_path shim
        write_cfg("node", r"C:\glue\apps\nodejs\current\node.exe"); // another package

        let mgr = Manager {
            root_dir: root.to_path_buf(),
            shims_dir: meta.clone(),
            bin_dir: bins.clone(),
        };
        let removed = mgr.remove_shims_for_package("zoom");
        assert_eq!(removed, vec!["Zoom", "zoomtool"]);
        assert!(!meta.join("Zoom.json").exists());
        assert!(!meta.join("zoomtool.json").exists());
        assert!(!bins.join("Zoom.exe").exists());
        // Another package's shim is untouched.
        assert!(meta.join("node.json").exists());
        assert!(bins.join("node.exe").exists());
    }

    #[test]
    fn remove_shims_for_package_removes_only_owned() {
        let root = TempDir::new("glue-shim-remove");
        let manager = Manager::new(root.path()).unwrap();
        let meta = manager.shims_meta_dir().to_path_buf();

        let mine = Config {
            name: "a".into(),
            command: r"C:\glue\apps\demo\current\a.exe".into(),
            path: r"C:\glue\apps\demo\current\a.exe".into(),
            ..Default::default()
        };
        let other = Config {
            name: "b".into(),
            command: r"C:\glue\apps\other\current\b.exe".into(),
            path: r"C:\glue\apps\other\current\b.exe".into(),
            ..Default::default()
        };
        fs::write(meta.join("a.json"), serde_json::to_vec(&mine).unwrap()).unwrap();
        fs::write(meta.join("b.json"), serde_json::to_vec(&other).unwrap()).unwrap();

        let removed = remove_shims_for_package(&manager, &meta, "demo").unwrap();
        assert_eq!(removed, vec!["a".to_string()]);
        assert!(!meta.join("a.json").exists());
        assert!(meta.join("b.json").exists());
    }

    #[test]
    fn remove_shims_for_package_missing_dir_is_ok() {
        let root = TempDir::new("glue-shim-remove-missing");
        let manager = Manager::new(root.path()).unwrap();
        let removed = remove_shims_for_package(&manager, &root.join("nope"), "demo").unwrap();
        assert!(removed.is_empty());
    }

    #[test]
    fn manager_create_writes_config() {
        let root = TempDir::new("glue-shim-create");
        let manager = Manager::new(root.path()).unwrap();

        let mut env = BTreeMap::new();
        env.insert(
            "DEMO_HOME".to_string(),
            r"C:\glue\apps\demo\current".to_string(),
        );
        let opts = CreateOpts {
            args: vec!["--flag".to_string()],
            env: env.clone(),
        };
        manager
            .create("demo", r"C:\glue\apps\demo\current\demo.exe", &opts)
            .unwrap();

        let cfg_path = manager.shims_meta_dir().join("demo.json");
        assert!(cfg_path.exists());
        let cfg: Config = serde_json::from_slice(&fs::read(&cfg_path).unwrap()).unwrap();
        assert_eq!(cfg.name, "demo");
        assert_eq!(cfg.args, vec!["--flag".to_string()]);
        assert_eq!(cfg.env, env);

        // The runner lands in the bin dir (stub copy or .bat fallback).
        let bin = manager.bin_dir();
        assert!(bin.join("demo.exe").exists() || bin.join("demo.bat").exists());

        manager.remove("demo").unwrap();
        assert!(!cfg_path.exists());
    }

    #[test]
    fn manager_list_skips_unreadable_entries() {
        let root = TempDir::new("glue-shim-list");
        let manager = Manager::new(root.path()).unwrap();
        let meta = manager.shims_meta_dir().to_path_buf();
        fs::write(meta.join("broken.json"), b"{not json").unwrap();

        let cfg = Config {
            name: "ok".into(),
            command: "ok.exe".into(),
            path: "ok.exe".into(),
            ..Default::default()
        };
        fs::write(meta.join("ok.json"), serde_json::to_vec(&cfg).unwrap()).unwrap();

        let list = manager.list().unwrap();
        assert_eq!(list.len(), 1);
        assert_eq!(list[0].name, "ok");
    }

    #[test]
    fn ps_single_quoted_doubles_quotes() {
        assert_eq!(ps_single_quoted("plain"), "'plain'");
        assert_eq!(ps_single_quoted(r"C:\it's\a.exe"), r"'C:\it''s\a.exe'");
    }

    #[test]
    fn resolve_shim_stub_prefers_glue_root_outside_an_installation() {
        let root = TempDir::new("glue-shim-stub");
        let stub = root.join("shim.exe");
        fs::write(&stub, b"stub").unwrap();

        // A dev-tree stub beside the "executable" exists too: an uninstalled
        // tree must still prefer the copy in the data root, which is what
        // `Manager::new` caches into.
        let exe_dir = TempDir::new("glue-shim-stub-exe");
        fs::write(exe_dir.join("shim.exe"), b"dev-tree").unwrap();

        let env = EnvContext {
            exec_dir: Some(exe_dir.path()),
            installed: false,
            cwd: None,
            program_data: None,
        };
        assert_eq!(resolve_shim_stub_path_in(root.path(), &env), Some(stub));
    }

    #[test]
    fn resolve_shim_stub_prefers_the_exe_dir_inside_an_installation() {
        // An installed tree keeps the stub next to `glue.exe`; a cached copy in
        // the data root may belong to an older build and must lose.
        let root = TempDir::new("glue-shim-installed-root");
        fs::write(root.join("shim.exe"), b"cached").unwrap();

        let exe_dir = TempDir::new("glue-shim-installed-exe");
        let installed_stub = exe_dir.join("shim.exe");
        fs::write(&installed_stub, b"fresh").unwrap();

        let env = EnvContext {
            exec_dir: Some(exe_dir.path()),
            installed: true,
            cwd: None,
            program_data: None,
        };
        assert_eq!(
            resolve_shim_stub_path_in(root.path(), &env),
            Some(installed_stub)
        );
    }

    #[test]
    fn resolve_shim_stub_falls_back_to_program_data() {
        // Neither the data root nor the executable's tree has a stub: the
        // machine-wide copy is the last resort before giving up.
        let root = TempDir::new("glue-shim-fallback-root");
        let exe_dir = TempDir::new("glue-shim-fallback-exe");
        let program_data = TempDir::new("glue-shim-fallback-pd");
        let machine_stub = program_data.join("glue").join("shim.exe");
        fs::create_dir_all(machine_stub.parent().unwrap()).unwrap();
        fs::write(&machine_stub, b"machine").unwrap();

        let env = EnvContext {
            exec_dir: Some(exe_dir.path()),
            installed: true,
            cwd: None,
            program_data: Some(program_data.path()),
        };
        assert_eq!(
            resolve_shim_stub_path_in(root.path(), &env),
            Some(machine_stub)
        );
    }

    #[test]
    fn cache_shim_stub_copies_and_skips_self() {
        let root = TempDir::new("glue-shim-cache");
        let src = TempDir::new("glue-shim-src");
        let stub = src.join("shim.exe");
        fs::write(&stub, b"runner-bytes").unwrap();

        cache_shim_stub(root.path(), &stub).unwrap();
        assert_eq!(fs::read(root.join("shim.exe")).unwrap(), b"runner-bytes");

        // Caching onto itself is a no-op, not an error.
        cache_shim_stub(root.path(), &root.join("shim.exe")).unwrap();
        assert_eq!(fs::read(root.join("shim.exe")).unwrap(), b"runner-bytes");
    }

    /// An upgraded stub reaches already-installed commands only through this
    /// pass; a file without a config is not a live shim and stays untouched.
    #[test]
    fn refresh_replaces_stale_shim_copies_only() {
        let root = TempDir::new("glue-shim-refresh");
        fs::write(root.path().join(SHIM_EXE), b"new-stub").unwrap();
        fs::create_dir_all(root.path().join("shims")).unwrap();
        fs::create_dir_all(root.path().join("shims-meta")).unwrap();
        fs::write(root.path().join("shims/app.exe"), b"old-stub").unwrap();
        fs::write(root.path().join("shims-meta/app.json"), b"{}").unwrap();
        fs::write(root.path().join("shims/stray.exe"), b"old-stub").unwrap();
        fs::write(root.path().join("shims/legacy.bat"), b"@echo off").unwrap();

        assert_eq!(refresh_shim_copies(root.path()).unwrap(), 1);
        assert_eq!(
            fs::read(root.path().join("shims/app.exe")).unwrap(),
            b"new-stub"
        );
        assert_eq!(
            fs::read(root.path().join("shims/stray.exe")).unwrap(),
            b"old-stub"
        );
        assert_eq!(
            fs::read(root.path().join("shims/legacy.bat")).unwrap(),
            b"@echo off"
        );
        assert_eq!(
            refresh_shim_copies(root.path()).unwrap(),
            0,
            "a second pass has nothing to replace"
        );
    }
}
