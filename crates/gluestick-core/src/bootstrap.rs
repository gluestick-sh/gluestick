//! On-demand bootstrap of external helper tools.
//!
//! Two Scoop hook helpers are not shipped with gluestick but are needed by a
//! handful of packages:
//!
//! * `dark.exe` + `wix.dll` (WiX toolset) — `Expand-DarkArchive` for WiX Burn
//!   bundles. Installed into `bin/wix`.
//! * `innounp.exe` — `Expand-InnoArchive` for Inno Setup installers.
//!   Installed into `bin/innounp`.
//!
//! The archive helpers are joined by the one dependency that is not a package
//! tool at all: `git`, which every bucket operation shells out to. It is a
//! runtime dependency, so a machine can lack it entirely — and a dev machine
//! never does, which is why its absence only ever shows up on a fresh install.
//! MinGit is bootstrapped into `bin/git` on first need and picked up by
//! [`crate::git::executable`].
//!
//! All of these ship as plain zips from fixed URLs. shells out to `7z.exe`
//! to unpack them; this implementation uses the built-in ZIP reader
//! ([`crate::extractor`]), so the bootstrap works even when 7-Zip itself is
//! missing — which is exactly the situation the bootstrap exists for.
//!
//! **Intentional divergence:** bootstrap is not serialised with a file lock.
//! An atomic directory swap is used instead (staging dir -> `bin/<tool>`, with
//! the previous directory renamed aside first and restored on failure), so two
//! racing processes may both
//! download but can never observe a half-populated tool directory — and a
//! failed swap never deletes the previous, working tool. The cost of the race
//! is a duplicate download, not corruption.

use std::path::{Path, PathBuf};

use crate::downloader::{Progress, Task};
use crate::engine::Services;

/// Byte-level download progress: `(transferred, total)`. `total` is `0` when
/// the server did not report a content length.
pub type ByteProgress<'a> = Option<&'a (dyn Fn(u64, u64) + Send + Sync)>;

/// A helper tool that may need bootstrapping.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum HelperTool {
    SevenZip,
    Innounp,
    Wix,
    /// MinGit — the fallback for a machine with no `git` at all.
    Git,
}

impl HelperTool {
    /// Name used in progress and warning lines.
    pub fn label(self) -> &'static str {
        match self {
            HelperTool::SevenZip => "7-Zip",
            HelperTool::Innounp => "innounp",
            HelperTool::Wix => "WiX toolset",
            HelperTool::Git => "git",
        }
    }

    /// Whether the tool is usable for `glue_root` right now.
    ///
    /// `git` is the one entry whose absence is not purely local: a `git` on
    /// `PATH` is just as good as a bootstrapped MinGit, so it only counts as
    /// missing when there is neither. The check is a filename scan rather than
    /// a run, because this runs at the start of every command (see
    /// [`crate::git::on_path`]).
    pub fn ready(self, glue_root: &Path) -> bool {
        match self {
            HelperTool::SevenZip => seven_zip_ready(glue_root),
            HelperTool::Innounp => INNOUNP.ready(glue_root),
            HelperTool::Wix => DARK.ready(glue_root),
            HelperTool::Git => GIT.ready(glue_root) || crate::git::on_path(),
        }
    }
}

/// Every helper tool this build bootstraps, in the order the environment
/// doctor lists them.
///
/// The list is fixed rather than derived from `bin/`: a data directory left
/// behind by the version carries the larger helper set that build needed
/// (and older Rust builds may carry extras), and those are none of this
/// build's business.
pub const ALL_HELPERS: [HelperTool; 4] = [
    HelperTool::SevenZip,
    HelperTool::Innounp,
    HelperTool::Wix,
    HelperTool::Git,
];

/// Readiness of every helper tool — the environment doctor's dependency row.
pub fn helper_status(glue_root: &Path) -> Vec<HelperStatus> {
    ALL_HELPERS
        .iter()
        .map(|&tool| HelperStatus {
            tool,
            ready: tool.ready(glue_root),
        })
        .collect()
}

/// A helper tool and whether it is usable, as reported by [`helper_status`].
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct HelperStatus {
    pub tool: HelperTool,
    pub ready: bool,
}

/// The helper tools that are not installed under `<glue_root>/bin` yet.
///
/// Uses the same readiness rule as [`HelperStatus`], so a tool the doctor
/// reports ready is never downloaded again and vice versa.
pub fn missing_helpers(glue_root: &Path) -> Vec<HelperTool> {
    ALL_HELPERS
        .iter()
        .copied()
        .filter(|tool| !tool.ready(glue_root))
        .collect()
}

/// A 7-Zip console build needs `7z.dll` beside it: NSIS/MSI/`#/dl.7z`
/// extracts use the full codec set, and a lone `7z.exe` fails on those.
fn seven_zip_ready(glue_root: &Path) -> bool {
    let bin = glue_root.join("bin");
    bin.join("7z.exe").is_file() && bin.join("7z.dll").is_file()
}

/// Bootstrap one helper tool, forwarding byte progress to `progress`.
pub fn ensure_helper(
    tool: HelperTool,
    services: &Services,
    progress: ByteProgress<'_>,
) -> Result<PathBuf, String> {
    match tool {
        HelperTool::SevenZip => ensure_7zip(services, progress),
        HelperTool::Innounp => ensure_innounp(services, progress),
        HelperTool::Wix => ensure_dark(services, progress),
        HelperTool::Git => ensure_git(services, progress),
    }
}

/// WiX 3 toolset (`dark.exe` + `wix.dll`) —
pub const DARK_URL: &str =
    "https://raw.githubusercontent.com/ScoopInstaller/Binary/master/dark/dark-3.14.1.zip";
/// Inno Setup unpacker —
///
/// Pinned to a commit, not to `refs/heads/master`. The author publishes each
/// innounp build by replacing `innounp-2.zip` on `master`, which changed the
/// digest underneath this constant and broke every Inno Setup install with no
/// warning (2.71.3 landed on 2026-10-31). A commit URL serves one immutable file
/// forever, so the digest below can only change when this constant is edited.
/// Upgrading is deliberate: pick the new commit, download it, update the digest.
pub const INNOUNP_URL: &str = "https://raw.githubusercontent.com/jrathlev/InnoUnpacker-Windows-GUI/75ded58da3a81c029db4f73ab803b9b4154400e9/innounp-2/bin/innounp-2.zip";

/// 7-Zip release bootstrapped on demand.
pub const SEVEN_ZIP_VERSION: &str = "2601";
/// The full-codec x64 installer (a 7z self-extracting archive).
pub const SEVEN_ZIP_ASSET: &str = "7z2601-x64.exe";
/// Upstream hosts, in preference order.
pub const SEVEN_ZIP_URLS: [&str; 2] = [
    "https://www.7-zip.org/a/7z2601-x64.exe",
    "https://mirror.7-zip.org/a/7z2601-x64.exe",
];

/// Pinned SHA-256 digests of the bootstrap assets, captured from the upstream
/// URLs above on 2026-10-21. The bootstrap downloads executables that are run
/// during installs, so "download over TLS and hope" is not enough: a poisoned
/// or hijacked source must fail the digest check instead of being executed.
///
/// The 7-Zip asset is an immutable versioned release (the mirror serves the
/// same file). `dark-3.14.1.zip` is versioned in its path. `innounp-2.zip` is
/// pinned through a commit URL, so it is immutable too — see [`INNOUNP_URL`].
const SEVEN_ZIP_SHA256: &str = "d64a0468f5b5b0b0fc5b2188450bcd655b70809d97b1c4535f2884635094377d";
const DARK_SHA256: &str = "cad986d43d59ed27c711b15113ea118c3a600c7e625130201ba946bcc70f5905";
/// innounp 2.71.3, built 2026-10-31 (PE timestamp matches the archive entries).
const INNOUNP_SHA256: &str = "f8e45c9676bebc7d7570108ab572673b541fcf7a7d3d24a04319ac6b5315b4e8";

/// MinGit version bootstrapped when the machine has no `git` of its own.
pub const MINIGIT_VERSION: &str = "2.56.0.2";
/// Release asset name at [`MINIGIT_URL`].
pub const MINIGIT_ASSET: &str = "MinGit-2.56.0.2-64-bit.zip";
/// Release asset URL.
///
/// MinGit is git-for-windows' minimal, self-contained build: a plain zip (so
/// this bootstrap needs no 7-Zip) carrying the HTTPS transport and a bundled CA
/// store — everything `clone`/`fetch`/`pull` over https need. It ships no
/// credential helper, which costs nothing here because [`crate::git`] disables
/// interactive credential prompts anyway.
///
/// x64 only, matching the 7-Zip bootstrap above: an ARM64 host is expected to
/// have a `git` of its own, and falls back to `PATH`.
pub const MINIGIT_URL: &str =
    "https://github.com/git-for-windows/git/releases/download/v2.56.0.windows.2/MinGit-2.56.0.2-64-bit.zip";
/// Pinned SHA-256 of [`MINIGIT_URL`]. Cross-checked against the digest GitHub
/// publishes for the asset itself, and against a downloaded copy.
const MINIGIT_SHA256: &str = "da35e72aa21c005a5a0d298cfbae110bc1609a815730ea0dde84b01a1b3cd3be";

/// A tool shipped as a zip that is unpacked into its own subdirectory of `bin`.
struct ZipTool {
    /// Progress label.
    name: &'static str,
    /// Download URL before GitHub-proxy expansion.
    url: &'static str,
    /// Pinned SHA-256 of the archive at `url` (see the digest note above).
    sha256: &'static str,
    /// Archive filename, used for format detection during extraction.
    archive: &'static str,
    /// Subdirectory of `bin/` the tool is unpacked into.
    dest: &'static str,
    /// Files that must exist afterwards; a partial unpack must not be adopted.
    required: &'static [&'static str],
    /// Entry-point executable, relative to the destination directory.
    exe: &'static str,
}

const DARK: ZipTool = ZipTool {
    name: "WiX toolset",
    url: DARK_URL,
    sha256: DARK_SHA256,
    archive: "dark.zip",
    dest: "wix",
    required: &["dark.exe", "wix.dll"],
    exe: "dark.exe",
};

const INNOUNP: ZipTool = ZipTool {
    name: "innounp",
    url: INNOUNP_URL,
    sha256: INNOUNP_SHA256,
    archive: "innounp.zip",
    dest: "innounp",
    required: &["innounp.exe"],
    exe: "innounp.exe",
};

/// MinGit.
///
/// `cmd/git.exe` is git-for-windows' launcher, not the git binary: the real
/// `git.exe` and its `git-core` helpers live under `ucrt64/`, and the launcher
/// derives that prefix from its own path. That is what makes it usable when
/// [`crate::git`] invokes it by absolute path from an arbitrary working
/// directory, and why `ucrt64/bin/git.exe` is checked alongside it — a
/// half-unpacked tree with only the launcher could not run.
const GIT: ZipTool = ZipTool {
    name: "git",
    url: MINIGIT_URL,
    sha256: MINIGIT_SHA256,
    archive: "mingit.zip",
    dest: "git",
    required: &["cmd/git.exe", "ucrt64/bin/git.exe"],
    exe: "cmd/git.exe",
};

/// Ensure the WiX toolset is available under `bin/wix`; returns the `dark.exe`
/// path.
pub fn ensure_dark(services: &Services, progress: ByteProgress<'_>) -> Result<PathBuf, String> {
    ensure(&DARK, services, progress)
}

/// Ensure `innounp.exe` is available under `bin/innounp`; returns its path.
pub fn ensure_innounp(services: &Services, progress: ByteProgress<'_>) -> Result<PathBuf, String> {
    ensure(&INNOUNP, services, progress)
}

impl ZipTool {
    fn dest_dir(&self, glue_root: &Path) -> PathBuf {
        glue_root.join("bin").join(self.dest)
    }

    fn exe_path(&self, glue_root: &Path) -> PathBuf {
        self.dest_dir(glue_root).join(self.exe)
    }

    /// Whether the tool is already usable. `dark.exe` alone cannot run, so WiX
    /// additionally requires its `wix.dll` sidecar.
    fn ready(&self, glue_root: &Path) -> bool {
        self.required
            .iter()
            .all(|name| self.dest_dir(glue_root).join(name).is_file())
    }
}

fn ensure(
    tool: &ZipTool,
    services: &Services,
    progress: ByteProgress<'_>,
) -> Result<PathBuf, String> {
    let root = services.root_dir();
    let dest_dir = tool.dest_dir(root);
    let exe = tool.exe_path(root);
    if tool.ready(root) {
        return Ok(exe);
    }

    // Older gluestick versions unpacked `dark.exe` straight into `bin`; a lone
    // copy there shadows nothing but is dead weight (removes it too).
    if tool.dest == "wix" {
        let legacy = root.join("bin").join("dark.exe");
        if legacy.is_file() {
            let _ = std::fs::remove_file(&legacy);
        }
    }

    println!("  -> Bootstrapping {} (first use)...", tool.name);
    let archive = download(tool, services, progress)?;
    let staging = root
        .join("bin")
        .join(format!(".bootstrap-{}-{}", tool.dest, std::process::id()));
    let _ = std::fs::remove_dir_all(&staging);
    std::fs::create_dir_all(&staging).map_err(|e| format!("create {}: {e}", staging.display()))?;

    let extracted = services
        .extractor()
        .extract_to_dir(&archive, &staging, tool.archive);
    if let Err(e) = extracted {
        let _ = std::fs::remove_dir_all(&staging);
        return Err(format!("extract {}: {e}", tool.name));
    }

    for name in tool.required {
        if !staging.join(name).is_file() {
            let _ = std::fs::remove_dir_all(&staging);
            return Err(format!("{name} missing after extracting {}", tool.name));
        }
    }

    if let Err(e) = replace_dir(&staging, &dest_dir) {
        let _ = std::fs::remove_dir_all(&staging);
        return Err(format!("install {}: {e}", tool.name));
    }

    if !exe.is_file() {
        return Err(format!(
            "{} missing after bootstrap ({})",
            tool.exe,
            exe.display()
        ));
    }
    // With a byte-progress sink the caller owns the live line and prints the
    // tool's completion itself; an extra line here would land mid-redraw.
    if progress.is_none() {
        println!("    installed {}", exe.display());
    }
    Ok(exe)
}

/// Ensure a usable `git` exists, bootstrapping MinGit into `<root>/bin/git`
/// when the machine has none; returns the executable to run.
///
/// Unlike the archive helpers this is a no-op on any machine that already has
/// `git` on `PATH`: the download is only for a machine that genuinely cannot
/// run a bucket operation, which is the fresh install this bootstrap exists
/// for. Because `git` is what fetches the buckets, this has to run before any
/// bucket step — see `ensure_environment` in the CLI.
///
/// Both checks ahead of the download are cheap: a pair of stats for the
/// bootstrapped MinGit, a filename scan for `PATH`. Bucket operations call
/// this each time round, so spawning git to answer the question would be the
/// bulk of their cost.
pub fn ensure_git(services: &Services, progress: ByteProgress<'_>) -> Result<PathBuf, String> {
    let root = services.root_dir();
    if GIT.ready(root) {
        return Ok(GIT.exe_path(root));
    }
    if crate::git::on_path() {
        return Ok(crate::git::executable(root));
    }
    ensure(&GIT, services, progress)
}

/// A directory with a full-codec `7z.exe` and `7z.dll` already on this
/// machine, in preference order.
///
/// The bootstrap download is hash-pinned; this adoption is not, but a local
/// 7-Zip is what the extractor already trusts from `PATH`/`<root>/bin`.
fn find_system_7zip() -> Option<PathBuf> {
    fn ready(dir: &Path) -> bool {
        dir.join("7z.exe").is_file() && dir.join("7z.dll").is_file()
    }

    let mut candidates = Vec::new();
    if let Some(home) = std::env::var_os("USERPROFILE").or_else(|| std::env::var_os("HOME")) {
        let home = PathBuf::from(home);
        candidates.push(home.join("scoop").join("apps").join("7zip").join("current"));
        candidates.push(home.join(".glue").join("bin"));
    }
    for var in ["ProgramFiles", "ProgramFiles(x86)", "LOCALAPPDATA"] {
        if let Some(dir) = std::env::var_os(var) {
            let dir = PathBuf::from(dir);
            candidates.push(dir.join("7-Zip"));
            candidates.push(dir.join("Programs").join("7-Zip"));
        }
    }
    if let Some(path) = std::env::var_os("PATH") {
        candidates.extend(std::env::split_paths(&path));
    }
    candidates.into_iter().find(|dir| ready(dir))
}

/// Ensure a full-codec 7-Zip (`7z.exe` + `7z.dll`) is available in `<root>/bin`.
///
/// The 7-Zip installer is a 7z self-extracting
/// archive, so this implementation unpacks it in-process (no `7zr.exe` seed needed) and
/// installs the DLL before the EXE — a half-upgraded `7z.exe` without its
/// codecs fails later with a much less obvious error.
pub fn ensure_7zip(services: &Services, progress: ByteProgress<'_>) -> Result<PathBuf, String> {
    let root = services.root_dir();
    let bin = root.join("bin");
    let exe = bin.join("7z.exe");
    let dll = bin.join("7z.dll");
    if seven_zip_ready(root) {
        return Ok(exe);
    }

    std::fs::create_dir_all(&bin).map_err(|e| format!("create {}: {e}", bin.display()))?;

    // A machine usually already has full-codec 7-Zip (Scoop, the old Glue
    // workspace, or Program Files). Copying it avoids the download entirely —
    // important where 7-zip.org is blocked or slow.
    if let Some(source) = find_system_7zip() {
        let adopted = (|| -> Result<(), String> {
            copy_atomic(&source.join("7z.dll"), &dll)?;
            copy_atomic(&source.join("7z.exe"), &exe)?;
            copy_codec_dlls(&source.join("codecs"), &bin.join("codecs"));
            Ok(())
        })();
        match adopted {
            Ok(()) => {
                println!("  -> Using installed 7-Zip");
                return Ok(exe);
            }
            Err(error) => eprintln!(
                "warning: could not use the installed 7-Zip at {}: {error}",
                source.display()
            ),
        }
    }

    println!("  -> Bootstrapping 7-Zip (first use)...");
    let blob = download_any(
        services,
        &SEVEN_ZIP_URLS,
        SEVEN_ZIP_SHA256,
        "7z-installer.exe",
        "7-Zip",
        progress,
    )?;

    let staging = bin.join(format!(".bootstrap-7zip-{}", std::process::id()));
    let _ = std::fs::remove_dir_all(&staging);
    std::fs::create_dir_all(&staging).map_err(|e| format!("create {}: {e}", staging.display()))?;

    let extracted = services
        .extractor()
        .extract_to_dir(&blob, &staging, "7z-installer.exe");
    if let Err(err) = extracted {
        let _ = std::fs::remove_dir_all(&staging);
        return Err(format!("extract 7-Zip installer: {err}"));
    }

    let found_exe = match find_named_file(&staging, "7z.exe", 0) {
        Some(path) => path,
        None => {
            let _ = std::fs::remove_dir_all(&staging);
            return Err("7z.exe not found in the extracted 7-Zip installer".to_string());
        }
    };
    let payload_dir = found_exe
        .parent()
        .map(Path::to_path_buf)
        .unwrap_or_default();
    let found_dll = payload_dir.join("7z.dll");
    if !found_dll.is_file() {
        let _ = std::fs::remove_dir_all(&staging);
        return Err("7z.dll not found in the extracted 7-Zip installer".to_string());
    }

    let result = (|| -> Result<(), String> {
        copy_atomic(&found_dll, &dll)?;
        copy_atomic(&found_exe, &exe)?;
        copy_codec_dlls(&payload_dir.join("codecs"), &bin.join("codecs"));
        Ok(())
    })();
    let _ = std::fs::remove_dir_all(&staging);
    result?;

    if !exe.is_file() {
        return Err(format!(
            "7z.exe missing after bootstrap ({})",
            exe.display()
        ));
    }
    if progress.is_none() {
        println!("    installed {}", exe.display());
    }
    Ok(exe)
}

/// First file named `name` (case-insensitive) under `dir`, depth-limited.
fn find_named_file(dir: &Path, name: &str, depth: usize) -> Option<PathBuf> {
    if depth > 4 {
        return None;
    }
    let mut subdirs = Vec::new();
    for entry in std::fs::read_dir(dir).ok()?.flatten() {
        let path = entry.path();
        if path.is_file()
            && path
                .file_name()
                .and_then(|n| n.to_str())
                .is_some_and(|n| n.eq_ignore_ascii_case(name))
        {
            return Some(path);
        }
        if path.is_dir() {
            subdirs.push(path);
        }
    }
    subdirs
        .into_iter()
        .find_map(|dir| find_named_file(&dir, name, depth + 1))
}

/// Copy `src` over `dst` atomically (temp file + rename).
fn copy_atomic(src: &Path, dst: &Path) -> Result<(), String> {
    let bytes = std::fs::read(src).map_err(|e| format!("read {}: {e}", src.display()))?;
    crate::atomic::write_atomic(dst, &bytes).map_err(|e| format!("write {}: {e}", dst.display()))
}

/// Older 7-Zip layouts keep codec DLLs in a `codecs/` directory beside `7z.exe`.
fn copy_codec_dlls(src: &Path, dst: &Path) {
    if !src.is_dir() || std::fs::create_dir_all(dst).is_err() {
        return;
    }
    let mut copied = 0;
    if let Ok(entries) = std::fs::read_dir(src) {
        for entry in entries.flatten() {
            let path = entry.path();
            let is_dll = path
                .extension()
                .and_then(|e| e.to_str())
                .is_some_and(|e| e.eq_ignore_ascii_case("dll"));
            if !path.is_file() || !is_dll {
                continue;
            }
            if let Some(name) = path.file_name() {
                if copy_atomic(&path, &dst.join(name)).is_ok() {
                    copied += 1;
                }
            }
        }
    }
    if copied == 0 {
        let _ = std::fs::remove_dir(dst);
    }
}

/// Download the tool archive and return the path of the CAS blob.
///
/// Mirrors are tried in order (GitHub proxies from config first when enabled).
fn download(
    tool: &ZipTool,
    services: &Services,
    progress: ByteProgress<'_>,
) -> Result<PathBuf, String> {
    download_any(
        services,
        &[tool.url],
        tool.sha256,
        tool.archive,
        tool.name,
        progress,
    )
}

/// Download any of the base URLs (each expanded through the configured mirrors)
/// and return the CAS blob path. The archive must match the pinned `sha256`;
/// a mismatch is a hard error for that URL and the next mirror is tried.
fn download_any(
    services: &Services,
    base_urls: &[&str],
    sha256: &str,
    archive: &str,
    label: &str,
    progress: ByteProgress<'_>,
) -> Result<PathBuf, String> {
    let downloader = services.downloader();
    let mut last = String::new();
    for base in base_urls {
        for url in downloader.resolve_download_urls(base) {
            println!("    downloading {url}");
            let task = Task {
                url: url.clone(),
                filename: archive.to_string(),
                checksum: String::new(),
                hash_algo: "sha256".to_string(),
                hash_value: sha256.to_string(),
                cancel: None,
            };
            // Report through the downloader's throttle so a large archive does
            // not redraw the caller's line on every 128 KiB read.
            let mut result = match progress {
                Some(cb) => {
                    let sink = |p: &Progress| cb(p.offset, p.total);
                    let throttled = crate::downloader::ThrottledProgress::new(&sink);
                    let report = |p: &Progress| throttled.report(p);
                    let result = downloader.download(&task, Some(&report));
                    throttled.flush();
                    result
                }
                None => downloader.download(&task, None),
            };
            if let Some(err) = result.error.take() {
                last = format!("{url}: {err}");
                continue;
            }
            if result.hash.is_empty() {
                last = format!("{url}: empty download");
                continue;
            }
            let blob = services.store().object_path(&result.hash);
            if !blob.is_file() {
                last = format!("{url}: blob missing from store");
                continue;
            }
            return Ok(blob);
        }
    }
    Err(format!("download {label} failed ({last})"))
}

/// Atomically swap `staging` into `dest`: the previous directory is renamed
/// aside first, the new one moved into place, and the backup deleted — a
/// failed or interrupted swap leaves the old tool (or a restorable backup),
/// never a missing or half-populated tool directory.
fn replace_dir(staging: &Path, dest: &Path) -> std::io::Result<()> {
    if let Some(parent) = dest.parent() {
        std::fs::create_dir_all(parent)?;
        clean_stale_backups(parent, dest);
    }
    if !dest.exists() {
        return std::fs::rename(staging, dest);
    }
    let backup = backup_path(dest);
    std::fs::rename(dest, &backup)?;
    match std::fs::rename(staging, dest) {
        Ok(()) => {
            let _ = std::fs::remove_dir_all(&backup);
            Ok(())
        }
        Err(e) => {
            // Roll back: the previous tool is more useful than none.
            let _ = std::fs::rename(&backup, dest);
            Err(e)
        }
    }
}

/// The aside-name for the directory being replaced, unique per process.
fn backup_path(dest: &Path) -> PathBuf {
    let name = dest.file_name().unwrap_or_default().to_string_lossy();
    dest.with_file_name(format!(".old-{name}-{}", std::process::id()))
}

/// Remove backups left behind by swaps interrupted between the two renames.
fn clean_stale_backups(parent: &Path, dest: &Path) {
    let Some(name) = dest.file_name().map(|n| n.to_string_lossy().into_owned()) else {
        return;
    };
    let prefix = format!(".old-{name}-");
    if let Ok(entries) = std::fs::read_dir(parent) {
        for entry in entries.flatten() {
            if entry.file_name().to_string_lossy().starts_with(&prefix) {
                let _ = std::fs::remove_dir_all(entry.path());
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::testutil::TempDir;

    fn write(path: &Path, bytes: &[u8]) {
        if let Some(parent) = path.parent() {
            std::fs::create_dir_all(parent).unwrap();
        }
        std::fs::write(path, bytes).unwrap();
    }

    #[test]
    fn pinned_digests_are_sha256_hex() {
        for (name, digest) in [
            ("7-Zip", SEVEN_ZIP_SHA256),
            ("dark", DARK_SHA256),
            ("innounp", INNOUNP_SHA256),
            ("MinGit", MINIGIT_SHA256),
        ] {
            assert_eq!(digest.len(), 64, "{name} digest length");
            assert!(
                digest
                    .chars()
                    .all(|c| c.is_ascii_hexdigit() && !c.is_ascii_uppercase()),
                "{name} digest must be lowercase hex"
            );
        }
    }

    /// Every asset a pinned digest refers to, with the name used in messages.
    fn pinned_assets() -> Vec<(&'static str, &'static str, &'static str)> {
        let mut assets = vec![
            ("7-Zip", SEVEN_ZIP_URLS[0], SEVEN_ZIP_SHA256),
            ("dark", DARK_URL, DARK_SHA256),
            ("innounp", INNOUNP_URL, INNOUNP_SHA256),
            ("MinGit", MINIGIT_URL, MINIGIT_SHA256),
        ];
        // Every mirror of an asset serves the same bytes, so they all have to be
        // immutable for the digest to keep matching.
        for mirror in &SEVEN_ZIP_URLS[1..] {
            assets.push(("7-Zip mirror", mirror, SEVEN_ZIP_SHA256));
        }
        assets
    }

    /// A URL whose bytes cannot change under a pinned digest.
    ///
    /// This is the regression test for innounp: the asset was fetched through
    /// `refs/heads/master` and *named without a version* (`innounp-2.zip`), so
    /// when the author shipped 2.71.3 on 2026-10-31 they overwrote the file in
    /// place. The pinned digest stopped matching and every Inno Setup install
    /// failed with a checksum error that named no cause.
    ///
    /// A digest is only a security control if the URL under it is immutable. A
    /// mutable URL plus a pinned digest is a time bomb, so a mutable URL is
    /// rejected here rather than discovered by a user mid-install.
    #[test]
    fn bootstrap_urls_are_immutable() {
        for (name, url, _) in pinned_assets() {
            assert_immutable_url(name, url);
        }
    }

    fn assert_immutable_url(name: &str, url: &str) {
        let rest = url
            .strip_prefix("https://")
            .unwrap_or_else(|| panic!("{name}: {url} must be https"));

        // Release downloads are keyed by an immutable tag and asset name.
        if let Some(rest) = rest.strip_prefix("github.com/") {
            assert!(
                rest.contains("/releases/download/"),
                "{name}: {url} must be a release download; a blob or branch URL \
                 can change under a pinned digest"
            );
            return;
        }

        // `raw.githubusercontent.com/<owner>/<repo>/<ref>/<path— `.
        let reference = rest
            .strip_prefix("raw.githubusercontent.com/")
            .and_then(|rest| rest.split('/').nth(2));

        if let Some(reference) = reference {
            // A commit sha cannot be rewritten. A branch can — and did.
            if is_commit_sha(reference) {
                return;
            }
            assert_ne!(
                reference, "refs/heads/master",
                "{name}: {url} tracks the upstream branch by name, so an upstream \
                 release changes the bytes under the pinned digest. Pin to the \
                 commit sha of the file instead."
            );
        }

        // Reaching here means the URL is not pinned by commit, so it may only be
        // accepted when the *filename* carries a version: a new upstream release
        // then lands on a different name instead of overwriting this one.
        // `dark-3.14.1.zip` qualifies; `innounp-2.zip` does not.
        let file = url.rsplit('/').next().unwrap_or_default();
        assert!(
            has_version_in_name(file),
            "{name}: {url} is neither pinned to a commit sha nor versioned in its \
             filename, so an upstream release can replace the bytes under the \
             pinned digest. Pin to a commit sha, or use a versioned asset name."
        );
    }

    fn is_commit_sha(text: &str) -> bool {
        text.len() == 40 && text.chars().all(|c| c.is_ascii_hexdigit())
    }

    /// Whether a filename carries a version, e.g. `dark-3.14.1.zip` or
    /// `7z2601-x64.exe`.
    ///
    /// A run of digits counts once it totals three or more (`2601`, `3.14.1`);
    /// the lone `2` in `innounp-2.zip` is a major-version marker the author
    /// overwrites, not a release identity.
    fn has_version_in_name(file: &str) -> bool {
        file.split(|c: char| !c.is_ascii_digit())
            .map(str::len)
            .sum::<usize>()
            >= 3
    }

    /// The digest constants must belong to a tool that actually downloads them.
    ///
    /// A constant that no [`ZipTool`] references is a stale pin: the tool it was
    /// meant for now downloads something else, and the next person to update the
    /// asset has no way to tell which value is live.
    #[test]
    fn every_digest_constant_belongs_to_a_tool() {
        let used = [SEVEN_ZIP_SHA256, DARK.sha256, INNOUNP.sha256, GIT.sha256];
        for (name, _, digest) in pinned_assets() {
            if name == "7-Zip mirror" {
                continue;
            }
            assert!(
                used.contains(&digest),
                "{name}: digest {digest} is not referenced by any tool"
            );
        }
        // Each tool's URL and digest must be the pair that was reviewed
        // together; swapping one without the other is the other half of the
        // innounp failure mode.
        assert_eq!(DARK.url, DARK_URL);
        assert_eq!(INNOUNP.url, INNOUNP_URL);
        assert_eq!(GIT.url, MINIGIT_URL);
    }

    /// The GitHub-mirror probe must use an asset that will still be there.
    ///
    /// Every candidate mirror is timed against the same URL. If that URL starts
    /// returning 404 because upstream moved the file, every mirror looks equally
    /// broken and the proxy feature quietly picks "direct" forever.
    #[test]
    fn the_proxy_probe_uses_a_stable_asset() {
        let probe = crate::config::PROXY_PROBE_URL;
        assert_immutable_url("proxy probe", probe);
        assert!(
            pinned_assets().iter().any(|(_, url, _)| *url == probe),
            "the probe should reuse a bootstrap asset, not add its own URL to keep alive"
        );
    }

    #[test]
    fn ready_requires_every_required_file() {
        let root = TempDir::new("glue-bootstrap-ready");
        assert!(!DARK.ready(root.path()));
        // A lone dark.exe is not usable — it needs wix.dll.
        write(&root.path().join("bin/wix/dark.exe"), b"x");
        assert!(!DARK.ready(root.path()));
        write(&root.path().join("bin/wix/wix.dll"), b"x");
        assert!(DARK.ready(root.path()));
        assert_eq!(
            DARK.exe_path(root.path()),
            root.path().join("bin/wix/dark.exe")
        );
    }

    #[test]
    fn innounp_ready_needs_only_the_exe() {
        let root = TempDir::new("glue-bootstrap-innounp");
        assert!(!INNOUNP.ready(root.path()));
        write(&root.path().join("bin/innounp/innounp.exe"), b"x");
        assert!(INNOUNP.ready(root.path()));
    }

    /// The archive helpers are asserted without `git`: whether that one is
    /// missing depends on the machine's `PATH`, which these tests must not.
    fn archive_helpers(root: &Path) -> Vec<HelperTool> {
        missing_helpers(root)
            .into_iter()
            .filter(|tool| *tool != HelperTool::Git)
            .collect()
    }

    #[test]
    fn helper_status_matches_missing_helpers() {
        let root = TempDir::new("glue-bootstrap-status");
        let status = helper_status(root.path());
        assert_eq!(
            status.iter().map(|s| s.tool).collect::<Vec<_>>(),
            ALL_HELPERS.to_vec()
        );
        let unready: Vec<HelperTool> = status.iter().filter(|s| !s.ready).map(|s| s.tool).collect();
        assert_eq!(unready, missing_helpers(root.path()));
        // The doctor must not re-download what it just called ready, or vice
        // versa: installing everything clears both views.
        write(&root.path().join("bin/7z.exe"), b"x");
        write(&root.path().join("bin/7z.dll"), b"x");
        write(&root.path().join("bin/innounp/innounp.exe"), b"x");
        write(&root.path().join("bin/wix/dark.exe"), b"x");
        write(&root.path().join("bin/wix/wix.dll"), b"x");
        write(&root.path().join("bin/git/cmd/git.exe"), b"x");
        write(&root.path().join("bin/git/ucrt64/bin/git.exe"), b"x");
        assert!(helper_status(root.path()).iter().all(|s| s.ready));
        assert!(missing_helpers(root.path()).is_empty());
    }

    #[test]
    fn missing_helpers_lists_absent_tools_and_clears_when_installed() {
        let root = TempDir::new("glue-bootstrap-missing");
        assert_eq!(
            archive_helpers(root.path()),
            vec![HelperTool::SevenZip, HelperTool::Innounp, HelperTool::Wix]
        );
        // 7-Zip needs both the exe and its codec DLL to count as installed.
        write(&root.path().join("bin/7z.exe"), b"x");
        assert_eq!(
            archive_helpers(root.path()),
            vec![HelperTool::SevenZip, HelperTool::Innounp, HelperTool::Wix]
        );
        write(&root.path().join("bin/7z.dll"), b"x");
        write(&root.path().join("bin/innounp/innounp.exe"), b"x");
        write(&root.path().join("bin/wix/dark.exe"), b"x");
        write(&root.path().join("bin/wix/wix.dll"), b"x");
        assert!(archive_helpers(root.path()).is_empty());
    }

    /// A bootstrapped MinGit settles `git` on its own, whatever the machine's
    /// `PATH` says — so the assertion holds on a host that has git and on one
    /// that does not.
    #[test]
    fn a_bootstrapped_mingit_is_never_reported_missing() {
        let root = TempDir::new("glue-bootstrap-git");
        write(&root.path().join("bin/git/cmd/git.exe"), b"x");
        write(&root.path().join("bin/git/ucrt64/bin/git.exe"), b"x");
        assert!(!missing_helpers(root.path()).contains(&HelperTool::Git));
    }

    /// `cmd/git.exe` alone cannot run: it launches the tree under `ucrt64/`,
    /// so adopting a half-unpacked directory would break every bucket command
    /// with a much less obvious error.
    #[test]
    fn minigit_readiness_needs_the_tree_not_just_the_launcher() {
        let root = TempDir::new("glue-bootstrap-git-partial");
        assert!(!GIT.ready(root.path()));
        write(&root.path().join("bin/git/cmd/git.exe"), b"x");
        assert!(!GIT.ready(root.path()));
        write(&root.path().join("bin/git/ucrt64/bin/git.exe"), b"x");
        assert!(GIT.ready(root.path()));
    }

    /// `bootstrap` unpacks MinGit to the location `git::executable` looks in;
    /// if the two ever drift the bootstrap is silently never used, and every
    /// bucket operation goes back to failing on a machine without git.
    #[test]
    fn minigit_unpacks_where_git_looks_for_it() {
        assert_eq!(
            format!("bin/{}/{}", GIT.dest, GIT.exe),
            crate::git::MINIGIT_EXE_RELATIVE
        );
    }

    #[test]
    fn replace_dir_swaps_contents() {
        let root = TempDir::new("glue-bootstrap-replace");
        let staging = root.path().join("bin/.staging");
        let dest = root.path().join("bin/wix");
        write(&staging.join("dark.exe"), b"new");
        write(&dest.join("stale.exe"), b"old");

        replace_dir(&staging, &dest).unwrap();
        assert!(dest.join("dark.exe").is_file());
        assert!(!dest.join("stale.exe").exists());
        assert!(!staging.exists());
    }

    #[test]
    fn replace_dir_creates_destination_parent() {
        let root = TempDir::new("glue-bootstrap-mkdir");
        let staging = root.path().join("staging");
        let dest = root.path().join("a/b/c");
        write(&staging.join("f"), b"x");
        replace_dir(&staging, &dest).unwrap();
        assert!(dest.join("f").is_file());
    }

    #[test]
    fn replace_dir_removes_backup_and_stale_leftovers() {
        let root = TempDir::new("glue-bootstrap-backup");
        let staging = root.path().join("staging");
        let dest = root.path().join("bin/wix");
        write(&staging.join("dark.exe"), b"new");
        write(&dest.join("dark.exe"), b"old");
        // Left behind by a swap interrupted between the two renames.
        write(&root.path().join("bin/.old-wix-9999/wix.dll"), b"stale");

        replace_dir(&staging, &dest).unwrap();
        assert_eq!(std::fs::read(dest.join("dark.exe")).unwrap(), b"new");
        assert!(!root.path().join("bin/.old-wix-9999").exists());
        // The swap's own backup is deleted once the new directory is in place.
        let leftovers: Vec<_> = std::fs::read_dir(root.path().join("bin"))
            .unwrap()
            .flatten()
            .filter(|e| e.file_name().to_string_lossy().starts_with(".old-"))
            .collect();
        assert!(leftovers.is_empty(), "leftovers: {leftovers:?}");
    }
}
