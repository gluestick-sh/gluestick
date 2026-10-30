//! Install pipeline: resolve → fetch → deploy → finalize.
//!
//! [`InstallState`] holds *only* phase data and each phase takes
//! the collaborators it needs. Every phase takes `&Services` — the bucket
//! registry and search index are internally synchronized, so resolve no
//! longer needs a mutable borrow and several installs can overlap. The
//! manifest is moved out of the state after resolve so phases can hold
//! `&mut st` and `&Manifest` at once.
//!
//! * [`InstallOptions::cancel`] is polled at every phase boundary and inside
//!   the download loops, so a UI can stop a run without losing the bytes that
//!   already landed (store blobs are kept, a resumable `.part` file stays).
//! * Progress plumbing collapses onto the engine's [`ProgressEvent`]
//!   callback: download progress is coarse (the downloader's cache-aware entry
//!   point owns the byte-level reporting).
//! * Per-phase timing instrumentation is reported through `GLUE_TIMINGS`.
//!
//! # Scope
//!
//! Covers: ref parsing, bucket discovery + manifest lookup, the
//! already-installed fast paths and version-lock guard, cache reuse, download
//! with mirror/arch fallback + hash verification, ZIP member-index linking,
//! extract + member-level CAS adoption (with member-index write-back), tar /
//! 7z / MSI / `msi_` / portable-exe / SFX / `innosetup: true` / plain-file
//! routing, multi-artifact installs, `extract_dir` flatten + manifest bin
//! validation, persist restore, pre/post_install and `installer.script` hooks
//! (with uninstall-side `pre_uninstall`/`uninstaller.script`/`post_uninstall`
//! runners), bootstrapping 7-Zip, WiX `dark` and `innounp` into `<root>/bin` on
//! demand, `current` linking, shims and shortcuts, `env_set`, install records
//! and cache index updates.
//!
//! `innosetup: true` unpacks the installer instead of running it (many GUI-only
//! Inno packages have no silent switch), through the same `Expand-InnoArchive`
//! helper a manifest hook calls, so `innounp` stays the only extractor involved.
//!
//! [`InstallOptions::interactive`] sets `$interactive = $true` for installer
//! scripts and hooks — Scoop's `install --interactive` — so a manifest can skip
//! its silent switches. It is opt-in per command: background updates never set
//! it, because a wizard on an unseen window would hang the run.
//!
//! `pkg@version` has two routes. The bucket's own git history
//! ([`crate::bucket_history`]) serves any version it published: one
//! path-limited `git log` plus a batched blob read, entirely offline. A shallow
//! clone answers instantly but has no older revisions, so that error names
//! `glue bucket deepen` as the one-time fix. When the history does not hold the
//! version — or cannot be searched at all — the manifest's `autoupdate` block
//! is expanded into it ([`crate::autoupdate`], Scoop `Manifest.ForVersion`),
//! and each generated URL is fetched once so its hash describes the bytes that
//! URL actually serves.
//!
//! **Known gaps:**
//!
//! * `autoupdate` templates built from `$match…` values, which only the
//!   package's `checkver` regex can produce. Those pins fail with the variable
//!   named instead of downloading a literal `$matchBuild` URL.
//! * The full Scoop hook helper preamble. Hooks run (pre/post_install,
//!   `installer.script`) with `$dir`/`$persist`/`$app`, `$ErrorActionPreference
//!   = 'Stop'`, the generic helpers (`abort`/`error`/`info`/`warn`/`ensure`/
//!   `fname`/`strip_ext`/`get_config`, `Copy-Tree`/`movedir`),
//!   `Invoke-ExternalCommand`, `Expand-7zipArchive`, `Expand-MsiArchive`,
//!   `Expand-DarkArchive`, `Expand-InnoArchive`, the shortcut/shim helpers and
//!   the PATH helpers. Hooks that call anything outside that set fail loudly
//!   (Stop preference) rather than half-succeeding.
//! * Local-version activation/repair — a version directory that is not active
//!   or not cache-indexed degrades to a clean reinstall.

use std::collections::{BTreeMap, BTreeSet};
use std::error::Error;
use std::fmt;
use std::fs;
use std::io;
use std::path::{Path, PathBuf};
use std::process::Command;
use std::sync::atomic::{AtomicI64, AtomicUsize, Ordering};
use std::sync::Mutex;
use std::thread;

use serde_json::{Map, Value};

use crate::apps;
use crate::archmember;
use crate::bucket::BucketError;
use crate::cache::{self, IndexError, PackageEntry};
use crate::cancel::CancelToken;
use crate::catalog as core_catalog;
use crate::downloader::{verify_archive_object, DownloadError, DownloadResult, Task};
use crate::engine::progress::{ProgressEvent, ProgressFn};
use crate::engine::services::{Services, ServicesError};
use crate::engine::timings::{Bucket, Timings};
use crate::extractor::ExtractError;
use crate::manifest::{self, Manifest, ManifestError};
use crate::safepath;
use crate::store::{Store, StoreError};

use super::catalog as engine_catalog;

// ---------------------------------------------------------------------------
// Options / outcome
// ---------------------------------------------------------------------------

/// Options controlling an install run.
///
/// Not `Clone`/`Debug`: the progress callback is a boxed `Fn`.
#[derive(Default)]
pub struct InstallOptions {
    /// Package ref: `pkg`, `bucket/pkg`, or `pkg@version`.
    pub package_ref: String,
    /// Version pin; when set it behaves exactly like the `@version` suffix
    /// (and wins over a suffix on `package_ref`).
    pub version: String,
    /// Architecture override (`""`, `"64bit"`, `"arm64"`, `"32bit"`); empty
    /// selects the host-preferred manifest block.
    pub arch: String,
    /// Force reinstall: skip the already-installed fast paths, ignore the
    /// content cache and clear leftovers.
    pub force: bool,
    /// Reuse the content cache even when `force` is set.
    ///
    /// `update` forces a fresh deploy (so a version switch really relinks
    /// `current`) but must not re-download bytes that are already in the
    /// store, keyed by the manifest hash it is about to verify anyway.
    pub reuse_cache: bool,
    /// Emit a per-phase `GLUE_TIMINGS ...` summary after the install
    pub timings: bool,
    /// Run installer scripts and hooks with `$interactive = $true` (Scoop's
    /// `install --interactive`), so a manifest can skip its silent switches
    /// and show the real wizard. Only explicit user commands set this:
    /// background updates and scheduled refreshes must leave it `false`, or
    /// a dialog appears on a window nobody is watching.
    pub interactive: bool,
    /// Stop signal: when set and flipped, the pipeline returns
    /// [`InstallError::Cancelled`] at the next phase boundary or download
    /// read. Artifacts already in the store are kept, and a resumable
    /// `.part` file is left for the next attempt.
    pub cancel: Option<CancelToken>,
    /// Progress callback (no-op when `None`).
    pub progress: Option<ProgressFn>,
    /// Do not install the manifest's `depends` first (Scoop's `--independent`).
    ///
    /// `update` sets this: an upgrade must not pull in packages the user never
    /// asked for. Dependency installs themselves set it too, so the recursive
    /// expansion happens once, up front.
    pub independent: bool,
}

/// Result of a successful install.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct InstallOutcome {
    pub package: String,
    pub version: String,
    pub install_dir: PathBuf,
    /// Bucket the manifest came from.
    pub bucket: String,
    /// Total install bytes recorded in the cache index.
    pub size: i64,
    /// Number of installed files recorded in the cache index.
    pub files: usize,
    /// Whether the artifact was reused from the content cache.
    pub from_cache: bool,
    /// `true` when the target version was already installed (no work done).
    pub already_installed: bool,
    /// Manifest description (may be empty).
    pub description: String,
    /// Shim names created for this package (empty when it ships no bins).
    pub shims: Vec<String>,
    /// Files linked/adopted into the install dir (excludes `install.json`).
    pub linked_files: usize,
    /// Optional `suggest` entries from the manifest. Never installed
    /// automatically; the CLI only prints them after a successful install.
    pub suggestions: Vec<crate::manifest::ManifestSuggestion>,
}

// ---------------------------------------------------------------------------
// Errors
// ---------------------------------------------------------------------------

/// A plain-text error, used to reproduce compositions
/// (phase cause + hint, `%q`-quoted member paths, …) without an enum variant
/// per combination.
#[derive(Debug)]
pub struct TextError(pub String);

impl fmt::Display for TextError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(&self.0)
    }
}

impl Error for TextError {}

/// Errors produced by the install pipeline.
#[derive(Debug, thiserror::Error)]
pub enum InstallError {
    /// A composed -parity message (phase cause + hint, text substitutions).
    #[error("{0}")]
    App(Box<dyn Error + Send + Sync>),
    #[error(transparent)]
    Download(#[from] DownloadError),
    #[error(transparent)]
    Extract(#[from] ExtractError),
    #[error(transparent)]
    Store(#[from] StoreError),
    #[error(transparent)]
    Manifest(#[from] ManifestError),
    #[error(transparent)]
    Bucket(#[from] BucketError),
    #[error(transparent)]
    Index(#[from] IndexError),
    #[error(transparent)]
    Io(#[from] io::Error),
    #[error("parse download URL: {0}")]
    Url(#[from] url::ParseError),
    /// `"{cause}: {err}\n\n{hint}"` (hint may be empty).
    ///
    /// A `Phase` is always collapsed to an [`InstallError::App`] carrying the
    /// rendered message (see [`InstallError::with_source`]); the variant exists
    /// so the composition reads.
    #[error("{cause}")]
    Phase { cause: String, hint: String },
    #[error(
        "{pkg} is version-locked at {locked_version}; remove the lock or install with @version"
    )]
    VersionLocked { pkg: String, locked_version: String },
    /// no URLs for the selected architecture.
    #[error("manifest has no download URLs")]
    NoDownloadUrls,
    #[error("install directory is empty after cache install (try glue install --force {pkg})")]
    EmptyInstallDir { pkg: String },
    /// `pkg@version` where the bucket has no such version and the manifest
    /// has no `autoupdate` to rebuild one. Reached only after the git-history
    /// search in [`resolve_pinned_version`] came back empty, so the version
    /// was never published (or the pin is a typo).
    #[error(
        "{package}@{version} is not in the bucket, and its manifest has no autoupdate for building one; check the version or install without @version"
    )]
    NoAutoupdate { package: String, version: String },
    /// `autoupdate` exists but declares no URL for the selected architecture,
    /// so there is nothing to download for this pin.
    #[error(
        "{package}@{version} is not in the bucket, and its autoupdate declares no URL for architecture '{arch}'; install without @version"
    )]
    AutoupdateNoUrl {
        package: String,
        version: String,
        arch: String,
    },
    /// `autoupdate` needs a `$match…` value that only the package's `checkver`
    /// regex produces. Named rather than downloaded as a literal `$matchBuild`
    /// URL, which would surface as an unrelated 404.
    #[error(
        "{package}@{version} is not in the bucket, and its autoupdate builds the URL from {variable}, which needs the package's checkver script (not run here); install without @version"
    )]
    AutoupdateUnresolved {
        package: String,
        version: String,
        variable: String,
    },
    /// The bucket cannot produce the pinned version locally — either its
    /// history is missing (shallow clone) or the version was never published.
    #[error("{package}@{version} is not available from the bucket: {reason}\n\n{hint}")]
    PinnedVersionUnavailable {
        package: String,
        version: String,
        reason: String,
        hint: String,
    },
    #[error("expected binary missing after install: {bin} (installer may be incomplete; try force reinstall)")]
    MissingBin { bin: String },

    /// failure before download.
    #[error("ensure 7z: {0}")]
    Ensure7Zip(ExtractError),
    /// The caller cancelled the run ([`InstallOptions::cancel`]). Not a
    /// failure: the UI reports it as "paused" and the persisted download state
    /// is left in place for the retry.
    #[error("cancelled")]
    Cancelled,
}

impl InstallError {
    /// `phaseError(cause, err, hint)`.
    fn phase(
        cause: impl Into<String>,
        err: &InstallError,
        hint: impl Into<String>,
    ) -> InstallError {
        InstallError::Phase {
            cause: cause.into(),
            hint: hint.into(),
        }
        .with_source(err)
    }

    /// Keep the wrapped error as the `source` of a `Phase` error so callers can
    /// still inspect it (wraps with `%w`).
    fn with_source(self, err: &InstallError) -> InstallError {
        // `Phase` renders "cause: source\n\nhint" when a source is attached;
        // without one it renders "cause\n\nhint"
        match (&self, err) {
            (InstallError::Phase { cause, hint }, _) => {
                let mut msg = format!("{cause}: {err}");
                if !hint.trim().is_empty() {
                    msg.push_str("\n\n");
                    msg.push_str(hint);
                }
                InstallError::App(Box::new(TextError(msg)))
            }
            _ => self,
        }
    }
}

fn text_err(msg: impl Into<String>) -> InstallError {
    InstallError::App(Box::new(TextError(msg.into())))
}

fn download_phase_error(url_count: usize, err: &InstallError) -> InstallError {
    // A cancelled download must stay recognizable as such: the caller turns it
    // into a paused task, and the "check your network" hint would mislead.
    if err.is_cancelled() {
        return InstallError::Cancelled;
    }
    let hint = if is_http_not_found(err) {
        "The download URL does not exist. If you pinned @version, that release may be source-only or missing this architecture."
    } else if is_http_forbidden(err) {
        "HTTP 403 usually means the download host refused this network location or proxy exit. Set \"proxy\" to your proxy URL in config.json and add the package to \"proxy_packages\" so only it uses the proxy (github_proxy only mirrors GitHub URLs and cannot fetch other hosts), or switch your proxy/VPN to another region (for example US, Japan or Singapore) and retry."
    } else {
        "Check your network connection or configure a GitHub mirror, then retry"
    };
    InstallError::phase(
        format!("download failed after trying {url_count} URL(s)"),
        err,
        hint,
    )
}

fn extract_phase_error(err: &InstallError) -> InstallError {
    if err.is_cancelled() {
        return InstallError::Cancelled;
    }
    InstallError::phase(
        "extraction failed",
        err,
        "Run glue install 7zip to install 7-Zip, or reinstall the package",
    )
}

/// a filesystem permission failure against `dir`.
fn permission_phase_error(cause: &str, err: &io::Error, dir: &str) -> InstallError {
    let err = text_err(format!("{cause}: {err} ({dir})"));
    InstallError::phase(
        cause,
        &err,
        "Check folder permissions or run as administrator",
    )
}

/// `phaseError(cause, err, hint)` — the general composed-error helper.
/// Renders `"{cause}: {err}"` followed by `"\n\n{hint}"` when the hint is set.
fn phase_error(cause: &str, err: impl fmt::Display, hint: &str) -> InstallError {
    let src = text_err(err.to_string());
    InstallError::phase(cause, &src, hint)
}

fn is_http_not_found(err: &InstallError) -> bool {
    match err {
        InstallError::Download(DownloadError::Http { status, .. })
        | InstallError::Download(DownloadError::HttpAt { status, .. }) => *status == 404,
        _ => {
            let msg = err.to_string();
            msg.contains("HTTP 404") || msg.contains("404 Not Found")
        }
    }
}

/// Whether the download failed with HTTP 403.
///
/// A 403 here is usually a location/proxy block rather than a missing file, so
/// it gets its own hint instead of the generic "check your network".
fn is_http_forbidden(err: &InstallError) -> bool {
    match err {
        InstallError::Download(DownloadError::Http { status, .. })
        | InstallError::Download(DownloadError::HttpAt { status, .. })
        | InstallError::Download(DownloadError::ChunkHttp { status, .. }) => *status == 403,
        _ => {
            let msg = err.to_string();
            msg.contains("HTTP 403") || msg.contains("403 Forbidden")
        }
    }
}

impl InstallError {
    /// Whether this error means "the caller asked us to stop" rather than
    /// "the install failed". A UI uses it to mark the work paused and keep it
    /// queued instead of showing a failure.
    pub fn is_cancelled(&self) -> bool {
        match self {
            InstallError::Cancelled => true,
            InstallError::Download(DownloadError::Cancelled) => true,
            // A cancelled download is re-wrapped by the phase reporters.
            InstallError::App(_) | InstallError::Phase { .. } => false,
            _ => false,
        }
    }
}

// ---------------------------------------------------------------------------
// Install state
// ---------------------------------------------------------------------------

/// One URL + its hash pair.
#[derive(Debug, Clone)]
struct UrlHashPair {
    url: String,
    hash_algo: String,
    hash_value: String,
}

/// All mutable state for one install run. Fields are grouped by the phase that
/// populates them; the manifest is moved out after resolve (see `install`).
struct InstallState {
    // Shared context
    pkg_ref: String,
    pkg_name: String,
    pin_version: String,
    lookup_ref: String,
    /// Force flag from [`InstallOptions`].
    force: bool,
    /// Reuse the content cache even when `force` is set (update path).
    reuse_cache: bool,
    /// Stop signal copied from [`InstallOptions`] and handed to every download
    /// this run starts.
    cancel: Option<CancelToken>,
    /// Run installer scripts and hooks interactively
    /// ([`InstallOptions::interactive`]).
    interactive: bool,

    // Phase 1: resolve output
    manifest_path: Option<PathBuf>,
    /// Bucket the manifest lookup landed in (the ref may be a bare name that
    /// resolves across buckets).
    bucket_name: String,
    manifest: Option<Manifest>,
    /// Manifest description, kept even when `check_existing_install` short
    /// circuits before `manifest` is stored (so `Resolved` can still carry it).
    description: String,
    target_version: String,
    install_arch: String,
    arch_explicit: bool,
    clear_install_dir: bool,

    // Phase 2: fetch output
    url_hash_pairs: Vec<UrlHashPair>,
    download_results: Vec<DownloadResult>,
    from_cache: bool,
    cached_entry: Option<PackageEntry>,
    file_ext: String,
    download_name: String,
    /// CAS hash of the freshly downloaded artifact (single-artifact path only).
    archive_hash: String,
    multi_artifact: bool,
    first_hash_algo: String,
    first_hash_value: String,

    // Phase 3: deploy output
    install_dir: PathBuf,
    installed_files: BTreeMap<String, String>,
    total_size: i64,

    // Outcome flags (set by resolve/deploy phases)
    done: bool,
    install_succeeded: bool,
    /// Shim names created during finalize (reported in the install summary).
    created_shims: Vec<String>,
    /// Files linked/adopted before finalize (the "Linked N file(s)" line).
    linked_files: usize,
    /// Optional `suggest` entries from the (possibly overridden) manifest,
    /// carried into [`InstallOutcome`] so the CLI can print them.
    suggestions: Vec<crate::manifest::ManifestSuggestion>,

    /// Per-phase timings; disabled unless `--timings` was requested.
    timings: Timings,
}

impl InstallState {
    fn new(pkg_ref: &str) -> InstallState {
        let (pkg_name, pin_version) = parse_pkg_ref(pkg_ref);
        InstallState {
            pkg_ref: pkg_ref.to_string(),
            pkg_name,
            pin_version,
            lookup_ref: core_catalog::manifest_lookup_ref(pkg_ref),
            force: false,
            reuse_cache: false,
            cancel: None,
            interactive: false,
            manifest_path: None,
            bucket_name: String::new(),
            manifest: None,
            description: String::new(),
            target_version: String::new(),
            install_arch: String::new(),
            arch_explicit: false,
            clear_install_dir: false,
            url_hash_pairs: Vec::new(),
            download_results: Vec::new(),
            from_cache: false,
            cached_entry: None,
            file_ext: String::new(),
            download_name: String::new(),
            archive_hash: String::new(),
            multi_artifact: false,
            first_hash_algo: String::new(),
            first_hash_value: String::new(),
            install_dir: PathBuf::new(),
            installed_files: BTreeMap::new(),
            total_size: 0,
            done: false,
            install_succeeded: false,
            created_shims: Vec::new(),
            linked_files: 0,
            suggestions: Vec::new(),
            timings: Timings::off(),
        }
    }

    /// A blank state for tests that only exercise one phase.
    #[cfg(test)]
    fn default_for_test() -> InstallState {
        InstallState::new("dummy")
    }

    fn reset_fetch_attempt(&mut self) {
        self.url_hash_pairs.clear();
        self.download_results.clear();
        self.from_cache = false;
        self.cached_entry = None;
        self.file_ext.clear();
        self.download_name.clear();
        self.archive_hash.clear();
        self.multi_artifact = false;
        self.first_hash_algo.clear();
        self.first_hash_value.clear();
    }
}

/// split `pkg@version` / `bucket/pkg@version`.
fn parse_pkg_ref(pkg_ref: &str) -> (String, String) {
    let (name, pin) = match pkg_ref.trim().rsplit_once('@') {
        Some((name, version)) => (name, version.to_string()),
        None => (pkg_ref.trim(), String::new()),
    };
    (core_catalog::package_base_name(name), pin)
}

/// +.
fn install_architecture(arch_override: &str, m: &Manifest) -> String {
    if arch_override.trim().is_empty() {
        m.selected_architecture().to_string()
    } else {
        m.architecture_for_install(arch_override).to_string()
    }
}

// ---------------------------------------------------------------------------
// Entry point
// ---------------------------------------------------------------------------

/// Install a package through the four-phase pipeline.
///
/// `Engine.Install` parity: the ref is validated across buckets first
/// then [`resolve`](resolve_install_phase) →
/// [`fetch`](fetch_install_phase) → [`deploy`](deploy_install_phase) →
/// [`finalize`](finalize_package_install) run.
pub fn install(
    services: &Services,
    options: &InstallOptions,
    lock: &crate::lock::MutationLock,
) -> Result<InstallOutcome, InstallError> {
    // Marks the install as in flight for the whole run: the fetch phase gives
    // the data-root hold up, and this marker is what keeps a concurrent cache
    // GC from collecting blobs this install has just ingested.
    let _inflight = crate::cache::inflight::InFlightGuard::begin(services.root_dir());
    let emit = |event: ProgressEvent| {
        if let Some(cb) = &options.progress {
            cb(event);
        }
    };

    let outcome = match install_missing_dependencies(services, options, &emit, Some(lock)) {
        Ok(()) => run_install(services, options, &emit, Some(lock)),
        Err(err) => Err(err),
    };
    match &outcome {
        Ok(o) => emit(ProgressEvent::Completed {
            package: o.package.clone(),
            version: o.version.clone(),
        }),
        // A cancelled run is not a failure: its caller paused it on purpose,
        // and a `Failed` line would show up as an install error.
        Err(e) if e.is_cancelled() => {}
        Err(e) => emit(ProgressEvent::Failed {
            package: options.package_ref.clone(),
            error: e.to_string(),
        }),
    }
    outcome
}

/// Install the manifest `depends` that are missing, dependencies first.
///
/// Scoop installs a package's `depends` before the package itself; gluestick
/// did not, and that is what broke `86box-roms`: its installer script links
/// into `apps/86box/current`, which did not exist, so `New-Item -ItemType
/// Junction` created a bare `current` skeleton — and that skeleton later
/// blocked installing `extras/86box` itself.
///
/// `lock` is the caller's data-root hold ([`run_install`] gives it up around
/// each download window).
fn install_missing_dependencies(
    services: &Services,
    options: &InstallOptions,
    emit: &(dyn Fn(ProgressEvent) + Sync),
    lock: Option<&crate::lock::MutationLock>,
) -> Result<(), InstallError> {
    if options.independent {
        return Ok(());
    }
    // A pre-cancelled run must not do resolution work for dependencies either:
    // the phase-boundary check in `resolve_step` would have stopped before the
    // manifest lookup, and expanding here would turn a pause into a
    // manifest-not-found error.
    if options.cancel.as_ref().is_some_and(|t| t.is_cancelled()) {
        return Err(InstallError::Cancelled);
    }
    // A no-op install (the package is already there) must not pull new
    // dependencies in as a side effect; only an install that will actually
    // write does. `--force` is a real reinstall, so it still expands.
    let name = crate::catalog::package_base_name(&options.package_ref);
    if !options.force && services.cache().get_installed(&name).is_some() {
        return Ok(());
    }
    for dep in missing_dependencies(services, &options.package_ref)? {
        let dep_options = InstallOptions {
            package_ref: dep,
            // A dependency resolves its own current version, and
            // `install --force` must not reinstall deps that are present.
            version: String::new(),
            arch: options.arch.clone(),
            force: false,
            reuse_cache: options.reuse_cache,
            timings: options.timings,
            interactive: options.interactive,
            cancel: options.cancel.clone(),
            progress: None,
            // The closure was expanded in one go above; recursing again would
            // re-resolve the same graph per level.
            independent: true,
        };
        run_install(services, &dep_options, emit, lock)?;
    }
    Ok(())
}

/// `depends` of `pkg_ref` that are not installed yet, dependencies first.
///
/// Depth-first post-order: every dependency is emitted before the package that
/// needs it, so a manifest installer script can rely on the other package's
/// install directory existing.
fn missing_dependencies(services: &Services, pkg_ref: &str) -> Result<Vec<String>, InstallError> {
    let mut ordered = Vec::new();
    let mut done = std::collections::BTreeSet::new();
    let mut stack: Vec<String> = Vec::new();
    // `is_root` keeps the requested package out of the returned list: it is the
    // caller's own install, and listing it here would install it once as a
    // "dependency" and then short-circuit the real install as
    // already-installed.
    collect_missing_dependencies(services, pkg_ref, true, &mut ordered, &mut done, &mut stack)
        .map_err(text_err)?;
    Ok(ordered)
}

/// The user-facing text for a failed dependency resolution.
///
/// The root ref keeps the engine's own context (the CLI strips the
/// `resolve package:` wrapper); a dependency is prefixed with its ref, since
/// the failure belongs to a package the user did not name on the command line.
fn dependency_error(pkg_ref: &str, is_root: bool, err: ServicesError) -> String {
    if is_root {
        return err.to_string();
    }
    let text = match &err {
        ServicesError::Resolve(inner) => inner.to_string(),
        other => other.to_string(),
    };
    format!("dependency {pkg_ref}: {text}")
}

fn collect_missing_dependencies(
    services: &Services,
    pkg_ref: &str,
    is_root: bool,
    ordered: &mut Vec<String>,
    done: &mut std::collections::BTreeSet<String>,
    stack: &mut Vec<String>,
) -> Result<(), String> {
    let resolved = crate::engine::catalog::resolve_install_ref(services, pkg_ref)
        .map_err(|err| dependency_error(pkg_ref, is_root, err))?;
    // Cycle check before the visited check: `done` is only filled after the
    // recursion returns, so an `a -> b -> a` chain still reports itself.
    if stack.iter().any(|r| r == &resolved) {
        let mut cycle = stack.clone();
        cycle.push(resolved);
        return Err(format!("dependency cycle: {}", cycle.join(" -> ")));
    }
    if done.contains(&resolved) {
        return Ok(());
    }
    stack.push(resolved.clone());
    let deps: Vec<String> = {
        let registry = services.registry();
        let (_bucket, _path, manifest) = registry.find_manifest(&resolved).map_err(|err| {
            if is_root {
                format!("{resolved}: {err}")
            } else {
                format!("dependency {resolved}: {err}")
            }
        })?;
        manifest.depends().to_vec()
    };
    for dep in deps {
        collect_missing_dependencies(services, &dep, false, ordered, done, stack)?;
    }
    stack.pop();
    done.insert(resolved.clone());
    // Scoop skips a dependency that is already installed; it never upgrades it
    // as a side effect of installing something else.
    let name = crate::catalog::package_base_name(&resolved);
    if !is_root && services.cache().get_installed(&name).is_none() {
        ordered.push(resolved);
    }
    Ok(())
}

/// Install several packages, running their fetch + deploy (download, extract,
/// link, shims, index) **concurrently** while resolve stays sequential.
///
/// Divergence from (documented, deliberate): installs
/// each package **sequentially**. This implementation overlaps the per-package pipelines
/// so one package's network-bound download is not stalled behind another's
/// extract/link. Resolve runs first (sequentially) — it can lazily clone a
/// bucket and reload the registry, which is a shared write — and the rest
/// ([`fetch_and_deploy`]) takes `&Services` and runs under a `thread::scope`,
/// each worker owning its own `InstallState`.
///
/// Shared-write safety: the CAS store uses `.tmp-{pid}-{atomic}` temp names
/// with rename dedup (concurrent writes safe); the cache index is a SQLite
/// `Mutex<Connection>` (writes serialized); each package deploys into its own
/// `apps/<name>` dir and its own extraction temp. The one shared FS surface is
/// the flat `shims/` namespace: two packages in the **same** batch providing
/// the identical shim/bin name race on that file
/// Known edge case of concurrent installs.
///
/// Non-fail-fast: every package is attempted; each result is
/// reported independently, in input order.
pub fn install_many(
    services: &Services,
    options_list: Vec<InstallOptions>,
    progress: Option<ProgressFn>,
) -> Vec<Result<InstallOutcome, InstallError>> {
    // Temporary lifetime extension: `emit` borrows `progress`, which outlives
    // this function, so the `&dyn` stays valid for every scoped worker below.
    #[allow(clippy::redundant_closure)]
    let emit: &(dyn Fn(ProgressEvent) + Sync) = &|event: ProgressEvent| {
        if let Some(cb) = &progress {
            cb(event);
        }
    };

    let n = options_list.len();
    // One slot per input package, filled in input order
    let mut slots: Vec<Option<Result<InstallOutcome, InstallError>>> =
        (0..n).map(|_| None).collect();
    // Packages that resolved and still need fetch + deploy: (input index,
    // install state, manifest). Consumed by the scoped workers.
    let mut ready: Vec<(usize, InstallState, Manifest)> = Vec::new();

    // Pass 1: resolve sequentially. This is the only `&mut Services` phase, so
    // it cannot overlap; it is also cheap (no network), so nothing is lost.
    for (i, options) in options_list.iter().enumerate() {
        let pkg_ref = options.package_ref.trim().to_string();
        let mut st = InstallState::new(&pkg_ref);
        if !options.version.trim().is_empty() {
            // An explicit version option behaves like the `@version` suffix.
            st.pin_version = options.version.trim().to_string();
        }
        st.arch_explicit = !options.arch.trim().is_empty();
        st.force = options.force;
        st.reuse_cache = options.reuse_cache;
        st.cancel = options.cancel.clone();
        st.interactive = options.interactive;
        st.timings = if options.timings {
            Timings::on()
        } else {
            Timings::off()
        };

        if pkg_ref.is_empty() {
            let err = text_err("empty package name");
            emit(ProgressEvent::Failed {
                package: pkg_ref,
                error: err.to_string(),
            });
            slots[i] = Some(Err(err));
            continue;
        }

        emit(ProgressEvent::Started {
            package: pkg_ref.clone(),
        });

        // Engine.Install → catalog.ValidateInstallTarget: resolve unqualified
        // names across buckets (ambiguity / not-found surface here).
        match engine_catalog::validate_install_target(services, &pkg_ref, &[])
            .map_err(|e| text_err(e.to_string()))
        {
            Ok(resolved) => {
                // Keep the resolved `bucket/name`: `Registry::find_manifest`
                // defaults a bare name to `main`, so `load_manifest` would
                // otherwise report "not found" for every package that lives in
                // another bucket (`glue install 86box-roms` → extras).
                st.lookup_ref = core_catalog::manifest_lookup_ref(&resolved);
            }
            Err(err) => {
                emit(ProgressEvent::Failed {
                    package: pkg_ref,
                    error: err.to_string(),
                });
                slots[i] = Some(Err(err));
                continue;
            }
        }

        // `depends` first (Scoop does). Installed sequentially here, before the
        // concurrent fetch/deploy below, because a dependency's install
        // directory has to exist before the dependent's installer script runs.
        if let Err(err) = install_missing_dependencies(services, options, emit, None) {
            emit(ProgressEvent::Failed {
                package: pkg_ref,
                error: err.to_string(),
            });
            slots[i] = Some(Err(err));
            continue;
        }

        match resolve_step(services, &mut st, options, emit) {
            Ok(Resolved::Done(outcome)) => {
                emit_resolved(&st, emit);
                emit(ProgressEvent::Completed {
                    package: outcome.package.clone(),
                    version: outcome.version.clone(),
                });
                emit_timings(&st, emit);
                slots[i] = Some(Ok(*outcome));
            }
            Ok(Resolved::Ready) => {
                emit_resolved(&st, emit);
                match st.manifest.clone() {
                    Some(manifest) => ready.push((i, st, manifest)),
                    None => {
                        let err =
                            text_err(format!("manifest is nil for {pkg_ref} (internal error)"));
                        emit(ProgressEvent::Failed {
                            package: pkg_ref,
                            error: err.to_string(),
                        });
                        slots[i] = Some(Err(err));
                    }
                }
            }
            Err(err) => {
                if !st.install_succeeded && !st.done {
                    cleanup_incomplete_install(services, &st.pkg_name, &st.target_version);
                }
                emit_timings(&st, emit);
                if !err.is_cancelled() {
                    emit(ProgressEvent::Failed {
                        package: pkg_ref,
                        error: err.to_string(),
                    });
                }
                slots[i] = Some(Err(err));
            }
        }
    }

    // Pass 2: fetch + deploy concurrently. Shared `&Services` (immutable);
    // each worker owns its `InstallState` and `Manifest`. A sibling failure
    // must not abort the others, so errors are collected, not propagated.
    if !ready.is_empty() {
        let services_ref: &Services = services;
        let produced: Vec<(usize, Result<InstallOutcome, InstallError>)> =
            std::thread::scope(|scope| {
                let mut handles = Vec::with_capacity(ready.len());
                for (i, st, manifest) in ready {
                    handles.push(scope.spawn(move || {
                        let mut st = st;
                        // No lock: a batch keeps the data-root hold for the
                        // whole run (its fetches and deploys are interleaved,
                        // so a release window could not be made safe here).
                        let result = fetch_and_deploy(services_ref, &mut st, &manifest, emit, None);
                        if result.is_err() && !st.install_succeeded && !st.done {
                            // Removes partial artifacts of a failed attempt.
                            cleanup_incomplete_install(
                                services_ref,
                                &st.pkg_name,
                                &st.target_version,
                            );
                        }
                        emit_timings(&st, emit);
                        (i, result)
                    }));
                }
                handles
                    .into_iter()
                    .map(|h| h.join().expect("install worker thread panicked"))
                    .collect()
            });

        for (i, result) in produced {
            match &result {
                Ok(o) => emit(ProgressEvent::Completed {
                    package: o.package.clone(),
                    version: o.version.clone(),
                }),
                Err(e) if e.is_cancelled() => {}
                Err(e) => emit(ProgressEvent::Failed {
                    package: options_list[i].package_ref.clone(),
                    error: e.to_string(),
                }),
            }
            slots[i] = Some(result);
        }
    }

    slots
        .into_iter()
        .map(|slot| {
            slot.unwrap_or_else(|| {
                Err(text_err(
                    "install did not reach a terminal state (internal error)",
                ))
            })
        })
        .collect()
}

/// Emit the per-phase timings line for `st` when `--timings` is on, matching
/// `run_install`'s deferred emit (fires on success and failure alike).
fn emit_timings(st: &InstallState, emit: &(dyn Fn(ProgressEvent) + Sync)) {
    if let Some(line) = st.timings.line() {
        emit(ProgressEvent::Timings { line });
    }
}

/// Emit the resolved manifest's display metadata (version/bucket/description)
/// once per package, before any download starts.
fn emit_resolved(st: &InstallState, emit: &(dyn Fn(ProgressEvent) + Sync)) {
    emit(ProgressEvent::Resolved {
        package: st.pkg_name.clone(),
        version: st.target_version.clone(),
        bucket: resolved_bucket(st),
        description: st.description.clone(),
        arch: st.install_arch.clone(),
    });
}

/// Bucket this run actually resolved to.
///
/// `load_manifest` records it; before that phase the typed ref is the only
/// source, and it names a bucket only when the user qualified it —
/// [`core_catalog::package_bucket_name`] reads a bare name as `main`, which is
/// wrong for every package that lives elsewhere.
fn resolved_bucket(st: &InstallState) -> String {
    if st.bucket_name.is_empty() {
        core_catalog::package_bucket_name(&st.pkg_ref)
    } else {
        st.bucket_name.clone()
    }
}

fn run_install(
    services: &Services,
    options: &InstallOptions,
    emit: &(dyn Fn(ProgressEvent) + Sync),
    lock: Option<&crate::lock::MutationLock>,
) -> Result<InstallOutcome, InstallError> {
    let pkg_ref = options.package_ref.trim();
    if pkg_ref.is_empty() {
        return Err(text_err("empty package name"));
    }

    let mut st = InstallState::new(pkg_ref);
    if !options.version.trim().is_empty() {
        // An explicit version option behaves like the `@version` suffix.
        st.pin_version = options.version.trim().to_string();
    }
    st.arch_explicit = !options.arch.trim().is_empty();
    st.force = options.force;
    st.reuse_cache = options.reuse_cache;
    st.cancel = options.cancel.clone();
    st.interactive = options.interactive;
    st.timings = if options.timings {
        Timings::on()
    } else {
        Timings::off()
    };

    // Already stopped before we started (a task paused while queued).
    check_cancel(&st)?;

    // Engine.Install → catalog.ValidateInstallTarget: resolve unqualified
    // names across buckets (ambiguity / not-found surface here).
    emit(ProgressEvent::Started {
        package: pkg_ref.to_string(),
    });

    let resolved = engine_catalog::validate_install_target(services, pkg_ref, &[])
        .map_err(|err| text_err(err.to_string()))?;
    // Same as the `install_many` pass: the manifest lookup needs the resolved
    // bucket, not the bare name (which `find_manifest` sends to `main`).
    st.lookup_ref = core_catalog::manifest_lookup_ref(&resolved);

    // Remove partial artifacts of a failed attempt.
    let outcome = run_phases(services, &mut st, options, emit, lock);
    if outcome.is_err() && !st.install_succeeded && !st.done {
        cleanup_incomplete_install(services, &st.pkg_name, &st.target_version);
    }
    // The cleanup emits the timings line from a deferred block, so it appears
    // on failure too and before the caller's result line.
    if let Some(line) = st.timings.line() {
        emit(ProgressEvent::Timings { line });
    }
    outcome
}

/// Phase-boundary cancellation check: `Err(Cancelled)` once the token is set.
fn check_cancel(st: &InstallState) -> Result<(), InstallError> {
    match &st.cancel {
        Some(token) if token.is_cancelled() => Err(InstallError::Cancelled),
        _ => Ok(()),
    }
}

/// Outcome of the resolve step for a package.
///
/// `Done` short-circuits install (already installed or satisfied by cache) with
/// the same outcome `run_phases` used to return inline; `Ready` means the
/// manifest was resolved and the package is ready for fetch + deploy. Splitting
/// this out lets [`install_many`] resolve every package sequentially (the only
/// phase needing `&mut Services`) before running fetch + deploy concurrently.
enum Resolved {
    // Boxed: `InstallOutcome` is the only payload and `Ready` carries none;
    // keeping it inline made every `Resolved` value as large as the outcome.
    Done(Box<InstallOutcome>),
    Ready,
}

/// Phase 1 (resolve) as a standalone step
/// Manifest load may lazily clone a bucket
/// and realign the registry/search index, which `Services` serializes
/// internally, so this needs only `&Services`.
fn resolve_step(
    services: &Services,
    st: &mut InstallState,
    options: &InstallOptions,
    emit: &(dyn Fn(ProgressEvent) + Sync),
) -> Result<Resolved, InstallError> {
    check_cancel(st)?;
    resolve_install_phase(services, st, options, emit)?;
    if st.done {
        return Ok(Resolved::Done(Box::new(InstallOutcome {
            package: st.pkg_name.clone(),
            version: st.target_version.clone(),
            install_dir: apps::pkg_root(services.root_dir(), &st.pkg_name).join(&st.target_version),
            bucket: resolved_bucket(st),
            size: 0,
            files: 0,
            from_cache: true,
            already_installed: true,
            description: st.description.clone(),
            shims: Vec::new(),
            linked_files: 0,
            suggestions: st.suggestions.clone(),
        })));
    }
    Ok(Resolved::Ready)
}

/// Phases 2–3 (fetch + deploy + finalize) as a standalone step. Takes
/// `&Services` (no mutation) so multiple packages can run it concurrently in
/// `std::thread::scope`, each owning its own `st`. `manifest` is the clone the
/// caller pulled out of `st.manifest` so `&mut st` and `&Manifest` coexist.
fn fetch_and_deploy(
    services: &Services,
    st: &mut InstallState,
    manifest: &Manifest,
    emit: &(dyn Fn(ProgressEvent) + Sync),
    lock: Option<&crate::lock::MutationLock>,
) -> Result<InstallOutcome, InstallError> {
    // Phase 2: fetch. A download can take minutes, touches only the cache
    // store, and is what queues every other operation behind the install — so
    // give the data-root hold up for it. Blobs ingested here are kept out of a
    // concurrent GC by the install's in-flight marker.
    if let Some(lock) = lock {
        lock.release_for_fetch();
    }
    let fetched = fetch_install_phase(services, st, manifest, emit);
    let relocked = match lock {
        Some(lock) => lock
            .restore_after_fetch()
            .map_err(|e| text_err(e.to_string())),
        None => Ok(()),
    };
    // The fetch result is the operation's real outcome; a failed re-lock only
    // matters if the fetch itself was fine.
    fetched?;
    relocked?;
    // The fetch succeeded but the user may have paused while the last bytes
    // landed; do not start deploying (which would have to be cleaned up).
    check_cancel(st)?;
    st.timings
        .absorb_download(st.download_results.iter().map(|r| r.timing));

    // Phase 3: deploy (includes finalize steps via each deploy path).
    check_cancel(st)?;
    deploy_install_phase(services, st, manifest, emit)?;

    st.install_succeeded = true;
    Ok(InstallOutcome {
        package: st.pkg_name.clone(),
        version: manifest.version.clone(),
        install_dir: st.install_dir.clone(),
        bucket: core_catalog::package_bucket_name(&st.pkg_ref),
        size: st.total_size,
        files: st.installed_files.len(),
        from_cache: st.from_cache,
        already_installed: false,
        description: st.description.clone(),
        shims: st.created_shims.clone(),
        linked_files: st.linked_files,
        suggestions: st.suggestions.clone(),
    })
}

fn run_phases(
    services: &Services,
    st: &mut InstallState,
    options: &InstallOptions,
    emit: &(dyn Fn(ProgressEvent) + Sync),
    lock: Option<&crate::lock::MutationLock>,
) -> Result<InstallOutcome, InstallError> {
    // Phase 1: resolve.
    match resolve_step(services, st, options, emit)? {
        Resolved::Done(outcome) => {
            emit_resolved(st, emit);
            return Ok(*outcome);
        }
        Resolved::Ready => emit_resolved(st, emit),
    }

    // The deploy helpers (`extract_adopt`, `finalize_package_install`,
    // `is_portable_exe_install`, MSI routing, ...) all read `st.manifest`, so
    // it must stay in the state. The phases get a clone so `&mut st` and
    // `&Manifest` can coexist without aliasing.
    let manifest = st.manifest.clone().ok_or_else(|| {
        text_err(format!(
            "manifest is nil for {} (internal error)",
            st.pkg_ref
        ))
    })?;

    // Phases 2–3: fetch + deploy.
    fetch_and_deploy(services, st, &manifest, emit, lock)
}

/// (minus shim removal, which needs the shim
/// manager this implementation does not have yet).
fn cleanup_incomplete_install(services: &Services, pkg_name: &str, version: &str) {
    if pkg_name.is_empty() || version.is_empty() {
        return;
    }
    if let Some(inst) = services.cache().get_installed(pkg_name) {
        if inst.version == version {
            return;
        }
    }

    let root = services.root_dir();
    let pkg_root = apps::pkg_root(root, pkg_name);
    let install_dir = pkg_root.join(version);

    if apps::read_current(&pkg_root).as_deref() == Some(version) {
        let _ = apps::remove_current_link(&pkg_root.join(apps::CURRENT_LINK_NAME));
    }

    if let Some(entry) = services.cache().get(pkg_name) {
        if entry.version == version {
            let _ = services.cache().remove(pkg_name);
        }
    }

    if version_dir_installed(root, pkg_name, version) {
        let _ = remove_dir_all(&install_dir);
    }

    if fs::read_dir(&pkg_root)
        .map(|mut e| e.next().is_none())
        .unwrap_or(false)
    {
        let _ = fs::remove_dir(&pkg_root);
    }
}

/// Remove a directory tree, tolerating a missing target and read-only files
/// (parity).
fn remove_dir_all(path: &Path) -> io::Result<()> {
    match fs::remove_dir_all(path) {
        Ok(()) => Ok(()),
        Err(e) if e.kind() == io::ErrorKind::NotFound => Ok(()),
        Err(_) => {
            clear_readonly_recursive(path);
            fs::remove_dir_all(path)
        }
    }
}

/// Clear the read-only bit on `path` and everything below it.
///
/// Used before removing a tree and before running hook scripts, which write
/// into their own install directory.
fn clear_readonly_recursive(path: &Path) {
    let Ok(meta) = fs::symlink_metadata(path) else {
        return;
    };
    let mut perms = meta.permissions();
    if perms.readonly() {
        #[allow(clippy::permissions_set_readonly_false)]
        perms.set_readonly(false);
        let _ = fs::set_permissions(path, perms);
    }
    if meta.is_dir() {
        if let Ok(entries) = fs::read_dir(path) {
            for entry in entries.flatten() {
                clear_readonly_recursive(&entry.path());
            }
        }
    }
}

// ---------------------------------------------------------------------------
// Phase 1: resolve
// ---------------------------------------------------------------------------

fn resolve_install_phase(
    services: &Services,
    st: &mut InstallState,
    options: &InstallOptions,
    emit: &(dyn Fn(ProgressEvent) + Sync),
) -> Result<(), InstallError> {
    check_cancel(st)?;
    // Phase 1.1: ensure bucket + find manifest.
    let m = load_manifest(services, st)?;

    // Phase 1.2: target version + existing-install fast paths.
    //
    // Ahead of pin resolution on purpose: resolving `pkg@old` from bucket
    // history is the only slow step in this command (`git log` over the
    // bucket's ancestry), and a pin that is already installed needs no
    // manifest at all. Putting the fast path first keeps `glue install
    // pkg@1.0` on an installed 1.0 at ~0 ms instead of a second of git.
    st.target_version = if st.pin_version.is_empty() {
        m.version.clone()
    } else {
        st.pin_version.clone()
    };
    st.description = m.description.clone();
    st.suggestions = m.suggestions();
    check_existing_install(services, st, options)?;
    if st.done {
        return Ok(());
    }

    // Phase 1.3: version pinning (bucket history for `pkg@old`, or a manifest
    // built from `autoupdate`). The architecture is picked from the checked-out
    // manifest first: generating an `autoupdate` URL for a never-published
    // version has to know which block's template to expand, and that block is
    // the same one Phase 1.6 selects below.
    let pin_arch = install_architecture(&options.arch, &m);
    let m = resolve_pinned_version(services, st, m, &pin_arch, emit)?;
    st.description = m.description.clone();
    st.suggestions = m.suggestions();

    // Phase 1.6: architecture selection.
    st.install_arch = install_architecture(&options.arch, &m);
    st.manifest = Some(m);
    Ok(())
}

fn load_manifest(services: &Services, st: &mut InstallState) -> Result<Manifest, InstallError> {
    let lookup_ref = st.lookup_ref.clone();
    let buckets_dir = services.buckets_dir();
    {
        let (mut registry, mut index) = services.registry_and_index_mut();
        core_catalog::ensure_bucket_for_install(
            &mut registry,
            Some(&mut index),
            &buckets_dir,
            &lookup_ref,
            &[],
        )
        .map_err(InstallError::App)?;
    }

    // One read guard each, reused for the not-found notice: taking a second
    // read guard on the same lock from this thread could deadlock against a
    // resolve running on another thread.
    let registry = services.registry();
    let index = services.search_index();
    let (bucket_name, manifest_path, m) = registry.find_manifest(&lookup_ref).map_err(|e| {
        InstallError::App(core_catalog::wrap_manifest_not_found(
            &registry,
            Some(&index),
            &st.pkg_ref,
            Box::new(e),
        ))
    })?;
    drop(index);
    drop(registry);
    st.manifest_path = Some(manifest_path);
    st.bucket_name = bucket_name;
    Ok(m)
}

fn resolve_pinned_version(
    services: &Services,
    st: &InstallState,
    m: Manifest,
    arch: &str,
    emit: &(dyn Fn(ProgressEvent) + Sync),
) -> Result<Manifest, InstallError> {
    if st.pin_version.is_empty() || st.pin_version == m.version {
        return Ok(m);
    }

    // Route A: the version the bucket used to publish. Its manifests live in
    // the bucket's git checkout, so an older one is a `git log` away; the
    // lookup is local and runs no network at all.
    //
    // A history that cannot be searched is not fatal: `autoupdate` can still
    // build the version, so the gap is remembered and only reported if
    // generation has nothing to offer either. That keeps a fresh
    // `--depth 1` bucket (or a bucket added without git) from failing a pin
    // that autoupdate could have satisfied.
    let mut history_gap = None;
    if let Some(path) = st.manifest_path.clone() {
        match crate::bucket_history::find_version(services.root_dir(), &path, &st.pin_version) {
            crate::bucket_history::LookupOutcome::Found(manifest) => return Ok(*manifest),
            crate::bucket_history::LookupOutcome::NoHistory(gap) => history_gap = Some(gap),
            crate::bucket_history::LookupOutcome::NotFound => {}
        }
    }

    // Route B: build the manifest from `autoupdate` (`Manifest.ForVersion`).
    // This is the only route left when the bucket's history does not hold the
    // version, and it covers a version the bucket never committed at all.
    match generate_pinned_manifest(services, st, &m, arch, emit) {
        Ok(manifest) => Ok(manifest),
        Err(err) => {
            // "Cannot build it" is answered by the history hint when there is
            // one: a shallow clone may still hold the published manifest, and
            // deepening is the one fix that may make the pin work. A download
            // failure is a different story and keeps its own diagnosis.
            let cannot_build = matches!(
                err,
                InstallError::NoAutoupdate { .. }
                    | InstallError::AutoupdateNoUrl { .. }
                    | InstallError::AutoupdateUnresolved { .. }
            );
            Err(match history_gap {
                Some(gap) if cannot_build => pinned_version_gap_error(st, gap),
                _ => err,
            })
        }
    }
}

/// Route B: rewrite the checked-out manifest into `st.pin_version`.
///
/// [`crate::autoupdate::for_version`] produces the URLs; the hashes have to be
/// observed, so each generated URL is fetched once and the store keeps the
/// bytes. The install that follows finds them under that hash and skips the
/// network — which is why the extra fetch is not a second download.
fn generate_pinned_manifest(
    services: &Services,
    st: &InstallState,
    m: &Manifest,
    arch: &str,
    emit: &(dyn Fn(ProgressEvent) + Sync),
) -> Result<Manifest, InstallError> {
    use crate::autoupdate::GenerateError;

    let package = st.pkg_name.clone();
    let version = st.pin_version.clone();
    let mut generated =
        crate::autoupdate::for_version(m, &version, arch).map_err(|err| match err {
            GenerateError::NoAutoupdate => InstallError::NoAutoupdate {
                package: package.clone(),
                version: version.clone(),
            },
            GenerateError::NoUrl { arch } => InstallError::AutoupdateNoUrl {
                package: package.clone(),
                version: version.clone(),
                arch,
            },
            GenerateError::Unresolved { variable, .. } => InstallError::AutoupdateUnresolved {
                package: package.clone(),
                version: version.clone(),
                variable,
            },
        })?;

    let mut hashes = Vec::with_capacity(generated.urls().len());
    for url in generated.urls() {
        check_cancel(st)?;
        let pair = UrlHashPair {
            url: url.clone(),
            hash_algo: "sha256".to_string(),
            hash_value: String::new(),
        };
        let result = download_manifest_artifact(
            services,
            &pair,
            false,
            false,
            emit,
            &package,
            st.cancel.clone(),
        )
        .map_err(|err| download_phase_error(1, &err))?;
        hashes.push(format!("sha256:{}", result.hash));
    }
    generated.apply_hashes(&hashes);
    Ok(generated.into_manifest())
}

/// Turn "the bucket has no history to search" into an actionable error.
///
/// Every gap has a different fix, so each one names its own: deepen the clone,
/// re-add the bucket, or install git. A bare "version not found" would send
/// the user looking for a typo instead.
fn pinned_version_gap_error(
    st: &InstallState,
    gap: crate::bucket_history::NoHistory,
) -> InstallError {
    use crate::bucket_history::NoHistory;

    let bucket = crate::catalog::package_bucket_name(&st.pkg_ref);
    let (reason, hint) = match gap {
        NoHistory::Shallow => (
            format!(
                "the local copy of bucket '{bucket}' is a shallow clone, so older manifests were never fetched"
            ),
            format!("Run `glue bucket deepen {bucket}` once, then retry."),
        ),
        NoHistory::NotACheckout => (
            format!(
                "bucket '{bucket}' is not a git checkout, so there is no older history to search"
            ),
            format!("Re-add it with `glue bucket add {bucket}`, then retry."),
        ),
        NoHistory::GitUnavailable => (
            "git is not available, so the bucket history cannot be read".to_string(),
            "Install git (`glue install git`) or put it on PATH, then retry.".to_string(),
        ),
    };
    InstallError::PinnedVersionUnavailable {
        package: st.pkg_name.clone(),
        version: st.pin_version.clone(),
        reason,
        hint,
    }
}

fn check_existing_install(
    services: &Services,
    st: &mut InstallState,
    options: &InstallOptions,
) -> Result<(), InstallError> {
    if options.force {
        return Ok(());
    }
    guard_version_lock(services, st)?;
    if !version_dir_installed(services.root_dir(), &st.pkg_name, &st.target_version) {
        return Ok(());
    }
    // /; the repair and
    // local-activation paths are gaps, so a target dir that is not both active
    // and cache-registered degrades to a clean reinstall.
    let active = active_install_version(services.root_dir(), &st.pkg_name);
    if active.as_deref() == Some(st.target_version.as_str())
        && services.cache().get(&st.pkg_name).is_some()
    {
        st.done = true;
        return Ok(());
    }
    st.clear_install_dir = true;
    Ok(())
}

fn guard_version_lock(services: &Services, st: &InstallState) -> Result<(), InstallError> {
    if !st.pin_version.is_empty() {
        return Ok(());
    }
    let Some(installed_version) = installed_package_version(services.root_dir(), &st.pkg_name)
    else {
        return Ok(());
    };
    if is_package_version_locked(services, &st.pkg_name) && installed_version != st.target_version {
        return Err(InstallError::VersionLocked {
            pkg: st.pkg_name.clone(),
            locked_version: installed_version,
        });
    }
    Ok(())
}

/// + `IsPackageVersionLocked`.
pub(crate) fn is_package_version_locked(services: &Services, pkg_name: &str) -> bool {
    crate::engine::ops::version_locked(services, pkg_name)
}

/// the version dir exists and is non-empty.
pub(crate) fn version_dir_installed(root: &Path, pkg_name: &str, version: &str) -> bool {
    if version.is_empty() {
        return false;
    }
    let dir = apps::pkg_root(root, pkg_name).join(version);
    if !dir.is_dir() {
        return false;
    }
    !install_dir_is_empty(&dir)
}

fn install_dir_is_empty(dir: &Path) -> bool {
    match fs::read_dir(dir) {
        Ok(mut entries) => entries.next().is_none(),
        Err(_) => true,
    }
}

/// current link version, else default version.
fn installed_package_version(root: &Path, pkg_name: &str) -> Option<String> {
    let pkg_root = apps::pkg_root(root, pkg_name);
    if let Some(ver) = apps::read_current(&pkg_root) {
        if !ver.is_empty() && version_dir_installed(root, pkg_name, &ver) {
            return Some(ver);
        }
    }
    apps::pick_default_version(&pkg_root)
}

fn active_install_version(root: &Path, pkg_name: &str) -> Option<String> {
    let pkg_root = apps::pkg_root(root, pkg_name);
    if let Some(ver) = apps::read_current(&pkg_root) {
        if !ver.is_empty() && version_dir_installed(root, pkg_name, &ver) {
            return Some(ver);
        }
    }
    installed_package_version(root, pkg_name)
}

/// `humanize`-less worker helper:.
fn num_cpu() -> usize {
    thread::available_parallelism()
        .map(|n| n.get())
        .unwrap_or(1)
}

// ---------------------------------------------------------------------------
// Phase 2: fetch (install_fetch.go + install_multiurl.go + install_cache_reuse.go)
// ---------------------------------------------------------------------------

/// validate URLs/hashes, try the content cache,
/// ensure 7-Zip, and download. An HTTP 404 on a non-explicit architecture
/// walks the manifest's fallback chain and retries; the `tried` set bounds
/// the loop (re-checks the arch key at the top).
fn fetch_install_phase(
    services: &Services,
    st: &mut InstallState,
    m: &Manifest,
    emit: &(dyn Fn(ProgressEvent) + Sync),
) -> Result<(), InstallError> {
    let mut tried: BTreeSet<String> = BTreeSet::new();
    let mut last_err: Option<InstallError> = None;
    loop {
        check_cancel(st)?;
        let arch_key = if st.install_arch.is_empty() {
            "_".to_string()
        } else {
            st.install_arch.clone()
        };
        if tried.contains(&arch_key) {
            if let Some(e) = last_err {
                return Err(e);
            }
            return Err(InstallError::NoDownloadUrls);
        }
        tried.insert(arch_key);

        match fetch_install_attempt(services, st, m, emit) {
            Ok(()) => return Ok(()),
            Err(e) => {
                if st.arch_explicit || !is_http_not_found(&e) {
                    return Err(e);
                }
                let next = m.fallback_architecture(&st.install_arch);
                if next.is_empty() {
                    return Err(e);
                }
                st.reset_fetch_attempt();
                st.install_arch = next.to_string();
                last_err = Some(e);
            }
        }
    }
}

fn fetch_install_attempt(
    services: &Services,
    st: &mut InstallState,
    m: &Manifest,
    emit: &(dyn Fn(ProgressEvent) + Sync),
) -> Result<(), InstallError> {
    let install_arch = st.install_arch.clone();

    // Phase 2.1: validate manifest has download URLs.
    let urls = m.get_urls_for_install(&install_arch);
    let hashes = m.get_hashes_for_install(&install_arch);
    if urls.is_empty() {
        return Err(InstallError::NoDownloadUrls);
    }

    // Phase 2.2: build URL-hash pairs and determine download type.
    st.url_hash_pairs = build_url_hash_pairs(&urls, &hashes);
    st.multi_artifact = manifest_uses_multi_artifact_urls(&urls, &hashes);

    // Phase 2.3: parse the first URL for the file type.
    let parsed = manifest::parse_url(&st.url_hash_pairs[0].url)?;
    st.file_ext = parsed.extension;
    st.download_name = parsed.local_name;

    // Store hash info for later use (cache reuse + verification).
    st.first_hash_algo = st.url_hash_pairs[0].hash_algo.clone();
    st.first_hash_value = st.url_hash_pairs[0].hash_value.clone();

    // Phase 2.4: check cache for a reusable entry.
    //
    // `force` means "fresh deploy": skip the *full-artifact* store reuse, but
    // do not throw away a half-fetched `.part` — a cancelled or interrupted
    // run resumes from the bytes it already has.
    if !st.force || st.reuse_cache {
        if let Some(entry) = services.cache().get(&st.pkg_name) {
            if cache_reusable_for_install(
                services.store(),
                &entry,
                &m.version,
                &st.download_name,
                &st.first_hash_value,
            ) {
                st.from_cache = true;
                st.total_size = entry.size;
                st.cached_entry = Some(entry);
                return Ok(());
            }
        }
    }

    // Phase 2.5: ensure the 7z extractor if the route needs it.
    if install_needs_seven_zip(
        m,
        &st.download_name,
        &normalize_install_file_ext(&st.file_ext),
        &install_arch,
    ) {
        ensure_seven_zip(services)?;
    }

    // Phase 2.6: coarse progress; byte-level reporting lives inside the
    // downloader's cache-aware entry point.
    emit(ProgressEvent::Downloading {
        package: st.pkg_ref.clone(),
        file: st.download_name.clone(),
        bytes: 0,
        total: 0,
    });

    // Phase 2.7: execute download (multi-artifact or single with mirrors).
    if st.multi_artifact {
        download_multi_artifacts(services, st, emit)?;
    } else {
        download_single_artifact_with_mirrors(services, st, emit)?;
    }
    Ok(())
}

/// In-flight artifact downloads when one manifest carries multiple URLs.
///
/// (`install_multiurl.go`) is a plain
/// for-range: fully serial bandwidth time for arch splits / dark+wix
/// sidecars. The Rust port downloads the pairs concurrently — a deliberate
/// deviation with the same rationale as `install_many`: the pairs are
/// independent blobs, the downloader is `Sync`, and the result ordering is
/// preserved (see below). The cap keeps the fan-out bounded: an artifact
/// can itself use up to `workers` range connections, so the worst case is
/// `MULTI_ARTIFACT_WORKERS * workers` ≈ `MAX_PARALLEL_CONNECTIONS`-scale
/// connections to one mirror host.
const MULTI_ARTIFACT_WORKERS: usize = 4;

fn download_multi_artifacts(
    services: &Services,
    st: &mut InstallState,
    emit: &(dyn Fn(ProgressEvent) + Sync),
) -> Result<(), InstallError> {
    let pairs = st.url_hash_pairs.clone();
    let n = pairs.len();
    let force = st.force;
    let reuse_cache = st.reuse_cache;
    let pkg_ref = st.pkg_ref.clone();
    let cancel = st.cancel.clone();
    // Shared worker state declared *outside* the scope closure: references
    // to locals declared inside it do not satisfy `Scope::spawn`'s `'env`
    // bound (E0597), while references to function-level locals do.
    let first_error: Mutex<Option<InstallError>> = Mutex::new(None);
    let cursor = AtomicUsize::new(0);

    // Pass 2 of the multi-URL fetch, concurrent: workers steal pairs from a
    // shared cursor, each returns its own completed results, and the first
    // error stops the fan-out
    let (mut produced, first_err) = std::thread::scope(|scope| {
        let pairs = &pairs;
        let cursor = &cursor;
        let first_error = &first_error;
        let pkg_ref = &pkg_ref;
        let cancel = &cancel;
        // At least one worker so the cursor loop still drains (and reports
        // errors for) an empty pair list consistently with the serial loop.
        let workers = n.clamp(1, MULTI_ARTIFACT_WORKERS);
        let mut handles = Vec::with_capacity(workers);
        for _ in 0..workers {
            handles.push(scope.spawn(move || {
                let mut done: Vec<(usize, DownloadResult)> = Vec::new();
                loop {
                    if first_error.lock().unwrap().is_some() {
                        break;
                    }
                    let i = cursor.fetch_add(1, Ordering::Relaxed);
                    if i >= n {
                        break;
                    }
                    match download_manifest_artifact(
                        services,
                        &pairs[i],
                        force,
                        reuse_cache,
                        emit,
                        pkg_ref,
                        cancel.clone(),
                    ) {
                        Ok(result) => done.push((i, result)),
                        Err(e) => {
                            let mut slot = first_error.lock().unwrap();
                            if slot.is_none() {
                                *slot = Some(e);
                            }
                            break;
                        }
                    }
                }
                done
            }));
        }
        let mut produced: Vec<(usize, DownloadResult)> = Vec::with_capacity(n);
        for h in handles {
            produced.extend(h.join().expect("multi-artifact worker panicked"));
        }
        // Take through the Mutex: `into_inner` would conflict with the
        // still-typed borrows held by the (now joined) worker closures.
        let first_err = first_error.lock().unwrap().take();
        (produced, first_err)
    });
    if let Some(e) = first_err {
        return Err(download_phase_error(n, &e));
    }
    if produced.len() != n {
        return Err(download_phase_error(
            n,
            &text_err("incomplete multi-artifact download"),
        ));
    }

    // Reassemble in input-pair order so `record_file`, `st.download_results`
    // and the deploy phase see exactly the sequential loop's ordering.
    produced.sort_by_key(|(i, _)| *i);
    let mut total: u64 = 0;
    for (_, r) in &produced {
        total += r.size;
        record_file(st, services.store(), &r.hash, &r.task.filename);
    }
    st.download_results = produced.into_iter().map(|(_, r)| r).collect();
    emit(ProgressEvent::Downloading {
        package: st.pkg_ref.clone(),
        file: st.download_name.clone(),
        bytes: total,
        total,
    });
    Ok(())
}

/// URL a transfer starts from: the proxy-prefixed one when `github_proxy` is
/// configured, otherwise the canonical asset URL.
fn first_download_url(services: &Services, task: &Task) -> String {
    services
        .downloader()
        .resolve_download_urls(&task.url)
        .into_iter()
        .next()
        .unwrap_or_default()
}

fn download_manifest_artifact(
    services: &Services,
    pair: &UrlHashPair,
    force: bool,
    reuse_cache: bool,
    emit: &(dyn Fn(ProgressEvent) + Sync),
    package: &str,
    cancel: Option<CancelToken>,
) -> Result<DownloadResult, InstallError> {
    let parsed = manifest::parse_url(&pair.url)?;
    let task = Task {
        url: parsed.fetch_url,
        filename: parsed.local_name,
        hash_algo: pair.hash_algo.clone(),
        hash_value: pair.hash_value.clone(),
        cancel,
        ..Task::default()
    };
    if force && !reuse_cache {
        // `reuse_cache` (the update path) means "do not throw away bytes that
        // are already on disk", and an in-progress `.part` is exactly that: a
        // paused or interrupted update resumes instead of starting over.
        services.downloader().clear_partial(&task);
    }
    // Register the package's hosts only for a real transfer. A store hit does
    // not touch the network, so it must not trigger the missing-proxy note.
    let cached = !(force && !reuse_cache) && services.downloader().is_cached(&task);
    if !cached {
        let _ = services
            .downloader()
            .allow_proxy_for_package(package, std::slice::from_ref(&pair.url));
    }
    // Byte-level progress: `ProgressReader` fires on every 128 KiB read, so
    // throttle to 150 ms / percentage-change rule before touching the
    // consumer (the CLI redraws a terminal line per event).
    let file = task.filename.clone();
    let source_url = first_download_url(services, &task);
    // A store hit transfers nothing, so only a real transfer gets the
    // "Connecting to ..." line that covers the wait for the first byte.
    if !cached {
        emit(ProgressEvent::Connecting {
            package: package.to_string(),
            url: source_url.clone(),
        });
    }
    let source_reported = std::sync::atomic::AtomicBool::new(false);
    let sink = |p: &crate::downloader::Progress| {
        // The first byte proves a real transfer (a store hit never reports
        // bytes), so report the source once, before the bar starts moving.
        if !source_reported.swap(true, Ordering::Relaxed) {
            emit(ProgressEvent::DownloadSource {
                package: package.to_string(),
                url: source_url.clone(),
            });
        }
        emit(ProgressEvent::Downloading {
            package: package.to_string(),
            file: file.clone(),
            bytes: p.offset,
            total: p.total,
        });
    };
    let throttled = crate::downloader::ThrottledProgress::new(&sink);
    let progress = |p: &crate::downloader::Progress| throttled.report(p);
    let result = services.downloader().download_with_cache(
        task,
        &pair.hash_value,
        force && !reuse_cache,
        Some(&progress as &(dyn Fn(&crate::downloader::Progress) + Send + Sync)),
    );
    match result.error {
        Some(e) => Err(InstallError::Download(e)),
        None => {
            // Store hits were verified when first ingested.
            if !result.from_store {
                verify_archive_object(services.store(), &result.task, &result.hash)?;
                emit(ProgressEvent::DownloadFinished {
                    package: package.to_string(),
                    file: result.task.filename.clone(),
                    size: result.size,
                    network_ms: result.timing.network.as_millis() as u64,
                    store_ms: result.timing.store_ingest.as_millis() as u64,
                });
            }
            Ok(result)
        }
    }
}

/// try each URL in order (mirrors).
fn download_single_artifact_with_mirrors(
    services: &Services,
    st: &mut InstallState,
    emit: &(dyn Fn(ProgressEvent) + Sync),
) -> Result<(), InstallError> {
    let mut last_err: Option<InstallError> = None;
    let pairs = st.url_hash_pairs.clone();

    for pair in &pairs {
        check_cancel(st)?;
        let parsed = match manifest::parse_url(&pair.url) {
            Ok(p) => p,
            Err(e) => {
                last_err = Some(InstallError::Url(e));
                continue;
            }
        };
        let task = Task {
            url: parsed.fetch_url,
            filename: parsed.local_name,
            hash_algo: pair.hash_algo.clone(),
            hash_value: pair.hash_value.clone(),
            cancel: st.cancel.clone(),
            ..Task::default()
        };
        if st.force && !st.reuse_cache {
            // See `download_manifest_artifact`: a force install clears a stale
            // partial, but the update path keeps it so an interrupted download
            // resumes.
            services.downloader().clear_partial(&task);
        }
        let cached = !(st.force && !st.reuse_cache) && services.downloader().is_cached(&task);
        if !cached {
            let _ = services
                .downloader()
                .allow_proxy_for_package(&st.pkg_ref, std::slice::from_ref(&pair.url));
        }

        // Same throttled byte-level reporting as the multi-artifact path.
        let pkg_ref = st.pkg_ref.clone();
        let file = task.filename.clone();
        let source_url = first_download_url(services, &task);
        if !cached {
            emit(ProgressEvent::Connecting {
                package: pkg_ref.clone(),
                url: source_url.clone(),
            });
        }
        let source_reported = std::sync::atomic::AtomicBool::new(false);
        // Switch to the Verifying phase as soon as the last byte arrives: the
        // downloader still has to commit the object to the store (and Windows
        // AV may scan it), which otherwise looks like a silent stall at 100%.
        let early_verifying = std::sync::atomic::AtomicBool::new(false);
        let sink = |p: &crate::downloader::Progress| {
            if !source_reported.swap(true, Ordering::Relaxed) {
                emit(ProgressEvent::DownloadSource {
                    package: pkg_ref.clone(),
                    url: source_url.clone(),
                });
            }
            emit(ProgressEvent::Downloading {
                package: pkg_ref.clone(),
                file: file.clone(),
                bytes: p.offset,
                total: p.total,
            });
            if p.total > 0
                && p.offset >= p.total
                && !early_verifying.swap(true, std::sync::atomic::Ordering::Relaxed)
            {
                emit(ProgressEvent::Verifying {
                    package: pkg_ref.clone(),
                });
            }
        };
        let throttled = crate::downloader::ThrottledProgress::new(&sink);
        let progress = |p: &crate::downloader::Progress| throttled.report(p);
        let result = services.downloader().download_with_cache(
            task,
            &pair.hash_value,
            st.force && !st.reuse_cache,
            Some(&progress as &(dyn Fn(&crate::downloader::Progress) + Send + Sync)),
        );
        match result.error {
            Some(e) => {
                last_err = Some(InstallError::Download(e));
                continue; // Try next URL/mirror.
            }
            None => {
                if !result.from_store {
                    match verify_archive_object(services.store(), &result.task, &result.hash) {
                        Ok(()) => {}
                        Err(e) => {
                            last_err = Some(InstallError::Download(e));
                            continue; // Try next URL/mirror.
                        }
                    }
                }
                // Success — keep the single result and report it.
                let (hash, size, from_store) =
                    (result.hash.clone(), result.size, result.from_store);
                let file = result.task.filename.clone();
                let network_ms = result.timing.network.as_millis() as u64;
                let store_ms = result.timing.store_ingest.as_millis() as u64;
                st.archive_hash = hash.clone();
                st.download_results = vec![result];
                let _ = last_err.take();
                if !from_store {
                    emit(ProgressEvent::DownloadFinished {
                        package: st.pkg_ref.clone(),
                        file: file.clone(),
                        size,
                        network_ms,
                        store_ms,
                    });
                    if !early_verifying.load(std::sync::atomic::Ordering::Relaxed) {
                        emit(ProgressEvent::Verifying {
                            package: st.pkg_ref.clone(),
                        });
                    }
                } else {
                    // A store hit transfers nothing, so the downloader never
                    // reported bytes; this is the only signal that tells the
                    // consumer how large the cached artifact was. Fresh
                    // downloads already finished their bar at 100%.
                    emit(ProgressEvent::Downloading {
                        package: st.pkg_ref.clone(),
                        file: file.clone(),
                        bytes: size,
                        total: size,
                    });
                }
                let download_name = st.download_name.clone();
                record_file(st, services.store(), &hash, &download_name);
                return Ok(());
            }
        }
    }

    if let Some(e) = last_err {
        return Err(download_phase_error(pairs.len(), &e));
    }
    // Defensive tail (unreachable: `urls` was non-empty).
    Err(download_phase_error(1, &text_err("no download results")))
}

fn record_file(st: &mut InstallState, store: &Store, hash: &str, name: &str) {
    if hash.is_empty() || cache::is_hidden_install_path(name) {
        return;
    }
    if !st.installed_files.contains_key(hash) {
        if let Ok(info) = fs::metadata(store.object_path(hash)) {
            st.total_size += info.len() as i64;
        }
    }
    st.installed_files
        .insert(hash.to_string(), name.to_string());
}

fn build_url_hash_pairs(urls: &[String], hashes: &[String]) -> Vec<UrlHashPair> {
    let mut pairs = Vec::with_capacity(urls.len());
    for (i, url) in urls.iter().enumerate() {
        let hash_with_algo = if i < hashes.len() {
            hashes[i].as_str()
        } else if !hashes.is_empty() {
            hashes[0].as_str()
        } else {
            ""
        };
        let (hash_algo, hash_value) = parse_hash(hash_with_algo);
        pairs.push(UrlHashPair {
            url: url.clone(),
            hash_algo,
            hash_value,
        });
    }
    pairs
}

/// `algo:value` explicit prefixes win; bare digests infer the
/// algorithm by length (32 md5 / 40 sha1 / 64 sha256 / 128 sha512, default
/// sha256 for anything else).
fn parse_hash(hash: &str) -> (String, String) {
    let hash = hash.trim();
    if let Some((algo, value)) = hash.split_once(':') {
        return (algo.to_lowercase(), value.to_string());
    }
    let algo = match hash.len() {
        32 => "md5",
        40 => "sha1",
        64 => "sha256",
        128 => "sha512",
        _ => "sha256",
    };
    (algo.to_string(), hash.to_string())
}

/// each URL is its own artifact when the
/// counts match and every hash is non-empty and distinct; otherwise extra URLs
/// are mirrors.
fn manifest_uses_multi_artifact_urls(urls: &[String], hashes: &[String]) -> bool {
    if urls.len() <= 1 || hashes.len() != urls.len() {
        return false;
    }
    let mut seen: BTreeSet<String> = BTreeSet::new();
    for raw in hashes {
        let (_, value) = parse_hash(raw);
        let value = value.trim().to_lowercase();
        if value.is_empty() || !seen.insert(value) {
            return false;
        }
    }
    true
}

fn all_objects_present(store: &Store, entry: &PackageEntry) -> bool {
    if entry.files.is_empty() {
        return false;
    }
    entry.files.keys().all(|hash| store.has(hash))
}

fn cache_reusable_for_install(
    store: &Store,
    entry: &PackageEntry,
    version: &str,
    download_name: &str,
    manifest_hash: &str,
) -> bool {
    if entry.version != version || !all_objects_present(store, entry) {
        return false;
    }
    if download_name.is_empty() {
        return true;
    }
    if !find_cache_archive_hash(entry, download_name, manifest_hash).is_empty() {
        return true;
    }
    for rel in entry.files.values() {
        if rel == download_name {
            return true;
        }
        if cache_artifact_conflicts_with_download(rel, download_name) {
            return false;
        }
    }
    true
}

fn cache_artifact_conflicts_with_download(artifact_rel: &str, download_name: &str) -> bool {
    let dl = download_name.to_lowercase();
    let ar = artifact_rel.to_lowercase();
    if ar.ends_with(".zip") || ar.ends_with(".nupkg") {
        return ar != dl;
    }
    let dl_token = download_arch_token(&dl);
    if dl_token.is_empty() {
        return false;
    }
    let ar_token = artifact_arch_token(&ar);
    if ar_token.is_empty() {
        return false;
    }
    ar_token != dl_token
}

fn download_arch_token(download_name: &str) -> &'static str {
    let dl = download_name.to_lowercase();
    if dl.contains("windows_arm64") || dl.contains("_arm64") {
        return "arm64";
    }
    if dl.contains("win64") || dl.contains("x86_64") || dl.contains("amd64") {
        return "64bit";
    }
    if dl.contains("win32") || dl.contains("i386") || dl.contains("x86-") {
        return "32bit";
    }
    ""
}

/// (no `x86-` case, unlike).
fn artifact_arch_token(artifact: &str) -> &'static str {
    let ar = artifact.to_lowercase();
    if ar.contains("windows_arm64") || ar.contains("_arm64") {
        return "arm64";
    }
    if ar.contains("win64") || ar.contains("x86_64") || ar.contains("amd64") {
        return "64bit";
    }
    if ar.contains("win32") || ar.contains("i386") {
        return "32bit";
    }
    ""
}

/// three lookups — exact rel/hash match, the
/// manifest digest stored as filename (rebuild/legacy entries), and the
/// single-file entry with a mismatching name.
fn find_cache_archive_hash(
    entry: &PackageEntry,
    download_name: &str,
    expected_cas_hash: &str,
) -> String {
    for (hash, rel) in &entry.files {
        if rel == download_name || (!expected_cas_hash.is_empty() && hash == expected_cas_hash) {
            return hash.clone();
        }
    }
    // Index may store the manifest digest as filename (rebuild / legacy entries).
    if !expected_cas_hash.is_empty() {
        for (hash, rel) in &entry.files {
            if rel == expected_cas_hash {
                return hash.clone();
            }
        }
    }
    if entry.files.len() == 1 {
        for (hash, rel) in &entry.files {
            if !download_name.is_empty()
                && rel != download_name
                && (expected_cas_hash.is_empty()
                    || (hash != expected_cas_hash && rel != expected_cas_hash))
            {
                continue;
            }
            return hash.clone();
        }
    }
    String::new()
}

/// use a local full-codec 7-Zip, downloading one
/// into `<root>/bin` on demand (no setup bundling).
///
/// The extractor's configured path is updated so later extractions in the same
/// run (and the hook helpers) see the freshly bootstrapped binary.
fn ensure_seven_zip(services: &Services) -> Result<(), InstallError> {
    if services.extractor().uses_7z() {
        return Ok(());
    }
    match crate::bootstrap::ensure_7zip(services, None) {
        Ok(path) => {
            services
                .extractor()
                .set_7z_path(path.to_string_lossy().into_owned());
            Ok(())
        }
        Err(err) => Err(text_phase_error(&format!(
            "7z.exe not found and could not be bootstrapped: {err}"
        ))),
    }
}

// ---------------------------------------------------------------------------
// Deploy phase
// ---------------------------------------------------------------------------

///
/// Creates the install directory, cleans it when a previous attempt left files
/// behind, then routes to the multi-artifact / cache / download strategies.
/// Each leaf strategy finishes by calling [`finalize_package_install`], exactly
/// call).
fn deploy_install_phase(
    services: &Services,
    st: &mut InstallState,
    m: &Manifest,
    emit: &(dyn Fn(ProgressEvent) + Sync),
) -> Result<(), InstallError> {
    let install_dir = apps::pkg_root(services.root_dir(), &st.pkg_name).join(&m.version);
    fs::create_dir_all(&install_dir).map_err(|err| {
        permission_phase_error(
            "failed to create install directory",
            &err,
            &install_dir.to_string_lossy(),
        )
    })?;
    st.install_dir = install_dir.clone();

    // An install that replaces an installed copy is an uninstall followed by an
    // install, so let that copy unwind itself first. This is not optional for
    // manifests that keep state outside `$dir`: `86box-roms` links
    // `apps/86box/current/roms` at its version dir, and PowerShell's
    // `New-Item -ItemType Junction` cannot replace a junction whose target is
    // still populated (`cannot be removed because it is not empty`), so without
    // this the package could neither be reinstalled nor updated.
    //
    // This covers `--force` (same version rebuilt) and any install that lands on
    // a different version (`glue update`, `glue install pkg@version`). The one
    // case it must not touch is a cached relink of the version already active;
    // the helper skips that itself because only it knows the installed version.
    unwind_replaced_copy(services, st, m)?;

    if st.clear_install_dir {
        clean_install_dir(&install_dir)
            .map_err(|err| text_phase_error(&format!("clean install dir: {err}")))?;
    }

    st.file_ext = normalize_install_file_ext(&st.file_ext);

    if st.multi_artifact {
        return deploy_multi_artifacts(services, st, emit);
    }
    if st.from_cache {
        let entry = st.cached_entry.clone().ok_or_else(|| {
            text_phase_error("cached entry missing for cache deploy (internal error)")
        })?;
        return deploy_from_cache(services, st, &entry, emit);
    }
    deploy_from_download(services, st, emit)
}

/// Runs the installed copy's `pre_uninstall` + `uninstaller.script` before an
/// install replaces it.
///
/// Scoop does the same in `update` (and in `install --force` before that): it
/// uninstalls with `$old_manifest`, which is why the hooks come from the install
/// record rather than from the manifest the bucket ships now — they are the ones
/// that created the state being unwound. When the record is missing (installs
/// from an older gluestick) the target manifest is the best available
/// description of that state.
///
/// The old version stays on disk: unlike Scoop this does not move it aside, it
/// is left for `glue cleanup` (and `glue reset`) to deal with. `post_uninstall`
/// is therefore not replayed either — in Scoop it runs once the version
/// directory is gone, and here it is only replaced later by the deploy phase.
fn unwind_replaced_copy(
    services: &Services,
    st: &InstallState,
    target_manifest: &Manifest,
) -> Result<(), InstallError> {
    let pkg_root = apps::pkg_root(services.root_dir(), &st.pkg_name);
    let Some((installed_dir, installed_version)) = apps::active_install_dir(&pkg_root) else {
        return Ok(());
    };
    // `reuse_cache` is set by the update path: the artifact does not have to be
    // fetched again. On its own that is not an uninstall, but it is when the
    // update lands on a new version, so only the same-version relink is skipped.
    if st.reuse_cache && installed_version == target_manifest.version {
        return Ok(());
    }

    let record = apps::load_install_record(&installed_dir).ok();
    let arch = record
        .as_ref()
        .map(|r| r.arch.clone())
        .filter(|arch| !arch.is_empty())
        .unwrap_or_else(|| st.install_arch.clone());
    let manifest = record
        .and_then(|r| r.manifest)
        .and_then(|value| serde_json::from_value::<Manifest>(value).ok())
        .unwrap_or_else(|| target_manifest.clone());
    let pkg_ref = format!("{}/{}", resolved_bucket(st), st.pkg_name);

    run_manifest_uninstall_hooks(
        services,
        &manifest,
        &arch,
        &installed_dir,
        &st.pkg_name,
        &pkg_ref,
        &installed_version,
    )
}

fn deploy_multi_artifacts(
    services: &Services,
    st: &mut InstallState,
    emit: &(dyn Fn(ProgressEvent) + Sync),
) -> Result<(), InstallError> {
    let install_dir = st.install_dir.clone();
    clean_install_dir(&install_dir)
        .map_err(|err| text_phase_error(&format!("clean install dir: {err}")))?;

    report_deploy_start(emit, &st.pkg_name, 0);

    let link_timing = st.timings.start();
    if st.from_cache {
        let entry = st.cached_entry.clone().ok_or_else(|| {
            text_phase_error("cached entry missing for cache deploy (internal error)")
        })?;
        let names = expected_multi_artifact_names(&st.url_hash_pairs)?;
        link_multi_artifact_from_cache(services, st, &install_dir, &entry, &names)?;
    } else {
        let results = std::mem::take(&mut st.download_results);
        let linked = link_multi_artifact_from_results(services, st, &install_dir, &results)?;
        st.download_results = results;
        let _ = linked;
    }
    st.timings.add(Bucket::Link, link_timing);

    finalize_package_install(services, st, emit)
}

fn deploy_from_cache(
    services: &Services,
    st: &mut InstallState,
    entry: &PackageEntry,
    emit: &(dyn Fn(ProgressEvent) + Sync),
) -> Result<(), InstallError> {
    let download_name = st.download_name.clone();
    let archive_hash = find_cache_archive_hash(entry, &download_name, &st.first_hash_value);

    if is_portable_exe_install(services, st) && !archive_hash.is_empty() {
        return deploy_portable_exe_from_cache(services, st, entry, &archive_hash, emit);
    }
    if is_pre_install_7z_hook_install(st) && !archive_hash.is_empty() {
        return deploy_sfx_from_cache(services, st, entry, &archive_hash, emit);
    }
    if uses_inno_extraction(st) && !archive_hash.is_empty() {
        // A cached Inno package must not take the plain-link path: that would
        // drop the installer into the version dir and call it installed.
        let install_dir = st.install_dir.clone();
        clean_install_dir(&install_dir)
            .map_err(|err| text_phase_error(&format!("clean install dir: {err}")))?;
        let download_name = st.download_name.clone();
        let source_hash = store_hash_for_cached_artifact(entry, &archive_hash, &download_name);
        return deploy_inno_extract(services, st, &source_hash, emit);
    }
    deploy_archive_from_cache(services, st, entry, &archive_hash, emit)
}

fn deploy_portable_exe_from_cache(
    services: &Services,
    st: &mut InstallState,
    entry: &PackageEntry,
    archive_hash: &str,
    emit: &(dyn Fn(ProgressEvent) + Sync),
) -> Result<(), InstallError> {
    let install_dir = st.install_dir.clone();
    clean_install_dir(&install_dir)
        .map_err(|err| text_phase_error(&format!("clean install dir: {err}")))?;

    report_deploy_start(emit, &st.pkg_name, 0);

    let download_name = st.download_name.clone();
    let source_hash = store_hash_for_cached_artifact(entry, archive_hash, &download_name);
    link_blob(services, st, &source_hash, &download_name)?;

    validate_install_dir(&install_dir)?;
    let (files, size) = refresh_installed_files(services, &install_dir);
    st.installed_files = files;
    st.total_size = size;

    finalize_package_install(services, st, emit)
}

fn deploy_sfx_from_cache(
    services: &Services,
    st: &mut InstallState,
    entry: &PackageEntry,
    archive_hash: &str,
    emit: &(dyn Fn(ProgressEvent) + Sync),
) -> Result<(), InstallError> {
    let install_dir = st.install_dir.clone();
    clean_install_dir(&install_dir)
        .map_err(|err| text_phase_error(&format!("clean install dir: {err}")))?;

    report_deploy_start(emit, &st.pkg_name, 0);

    let download_name = st.download_name.clone();
    let source_hash = store_hash_for_cached_artifact(entry, archive_hash, &download_name);
    link_blob(services, st, &source_hash, &download_name)?;

    validate_install_dir(&install_dir)?;
    let (files, size) = refresh_installed_files(services, &install_dir);
    st.installed_files = files;
    st.total_size = size;

    finalize_package_install(services, st, emit)
}

fn deploy_archive_from_cache(
    services: &Services,
    st: &mut InstallState,
    entry: &PackageEntry,
    archive_hash: &str,
    emit: &(dyn Fn(ProgressEvent) + Sync),
) -> Result<(), InstallError> {
    let install_dir = st.install_dir.clone();

    if !st.first_hash_value.is_empty() && !archive_hash.is_empty() {
        let task = Task {
            hash_algo: st.first_hash_algo.clone(),
            hash_value: st.first_hash_value.clone(),
            ..Task::default()
        };
        verify_archive_object(services.store(), &task, archive_hash).map_err(|err| {
            phase_error(
                "file verification failed",
                err,
                "Reinstall the package or try a different mirror",
            )
        })?;
    }

    report_deploy_start(emit, &st.pkg_name, 0);

    let link_timing = st.timings.start();
    let linked = link_from_cache(services, &install_dir, entry)?;
    st.timings.add(Bucket::Link, link_timing);

    if linked == 0 && !archive_hash.is_empty() {
        return deploy_archive_from_cache_extract(services, st, archive_hash, emit);
    }

    validate_install_dir(&install_dir)?;
    let (files, size) = refresh_installed_files(services, &install_dir);
    st.installed_files = files;
    st.total_size = size;

    finalize_package_install(services, st, emit)
}

fn deploy_archive_from_cache_extract(
    services: &Services,
    st: &mut InstallState,
    archive_hash: &str,
    emit: &(dyn Fn(ProgressEvent) + Sync),
) -> Result<(), InstallError> {
    let file_ext = st.file_ext.clone();
    let cas_path = services.store().object_path(archive_hash);
    if !cas_path.exists() {
        return Err(phase_error(
            "archive not found in CAS",
            io::Error::new(io::ErrorKind::NotFound, "archive not found in CAS"),
            "Reinstall the package or try a different mirror",
        ));
    }

    match file_ext.as_str() {
        ".zip" | ".nupkg" => {
            deploy_zip_extract_and_adopt(services, st, &cas_path, archive_hash, emit)
        }
        ".tar" | ".tgz" => {
            deploy_tar_extract_and_adopt(services, st, &cas_path, archive_hash, emit)
        }
        ".7z" => deploy_7z_extract_and_adopt(services, st, &cas_path, archive_hash, emit),
        _ => Err(phase_error(
            &format!("unsupported archive format for extraction from cache: {file_ext}"),
            io::Error::other("unsupported archive format"),
            "Reinstall the package or try a different mirror",
        )),
    }
}

fn deploy_from_download(
    services: &Services,
    st: &mut InstallState,
    emit: &(dyn Fn(ProgressEvent) + Sync),
) -> Result<(), InstallError> {
    let Some(result) = st.download_results.first() else {
        return Err(phase_error(
            "no download results",
            io::Error::other("no download results"),
            "Reinstall the package or try a different mirror",
        ));
    };
    let hash = result.hash.clone();
    let cas_path = services.store().object_path(&hash);
    let file_ext = st.file_ext.clone();

    match file_ext.as_str() {
        ".zip" | ".nupkg" => deploy_zip_archive(services, st, &cas_path, &hash, emit),
        ".tar" | ".tgz" => deploy_tar_archive(services, st, &cas_path, &hash, emit),
        ".msi_" => deploy_msi_alias(services, st, &hash),
        ".msi" => deploy_msi(services, st, &cas_path, &hash, emit),
        ".exe" => deploy_exe(services, st, &cas_path, &hash, emit),
        ".7z" | ".7z.exe" => deploy_7z_archive(services, st, &cas_path, &hash, emit),
        _ => deploy_generic_file(services, st, &hash),
    }
}

// ---------------------------------------------------------------------------
// Archive deploy (zip / tar / 7z)
// ---------------------------------------------------------------------------

fn deploy_zip_archive(
    services: &Services,
    st: &mut InstallState,
    cas_path: &Path,
    archive_hash: &str,
    emit: &(dyn Fn(ProgressEvent) + Sync),
) -> Result<(), InstallError> {
    let (zip_files, zip_total) = services
        .downloader()
        .resolve_zip_member_index(archive_hash)
        .filter(|(files, _)| !files.is_empty())
        .map(|(files, total)| (files, total as i64))
        .unwrap_or_default();
    if zip_files.is_empty() {
        return deploy_zip_extract_and_adopt(services, st, cas_path, archive_hash, emit);
    }
    deploy_zip_from_index(services, st, archive_hash, &zip_files, zip_total, emit)
}

fn deploy_zip_extract_and_adopt(
    services: &Services,
    st: &mut InstallState,
    cas_path: &Path,
    archive_hash: &str,
    emit: &(dyn Fn(ProgressEvent) + Sync),
) -> Result<(), InstallError> {
    // No `ensure_seven_zip` here: this implementation extracts ZIP in-process
    // (see the divergence notes in [`crate::extractor`]), so `7z.exe` is
    // never required on the zip deploy path. calls `Ensure7Zip` because
    // its own ZIP extraction is an unimplemented stub that shells out to 7z.
    let install_dir = st.install_dir.clone();
    clean_install_dir(&install_dir)
        .map_err(|err| text_phase_error(&format!("clean install dir: {err}")))?;

    report_deploy_start(emit, &st.pkg_name, 0);

    let download_name = st.download_name.clone();
    let (mut zip_files, mut zip_total) =
        extract_adopt(services, st, cas_path, &download_name, emit)?;

    if let Ok(info) = fs::metadata(cas_path) {
        zip_total += info.len() as i64;
    }
    let _ = &mut zip_files;
    services
        .downloader()
        .save_zip_member_index(archive_hash, &zip_files, zip_total as u64)
        .map_err(|err| {
            phase_error(
                "save zip member index",
                err,
                "Reinstall the package or try a different mirror",
            )
        })?;

    let (indexed, zip_size) = index_zip_member_links(services, st, &zip_files, zip_total)?;
    for (hash, rel) in indexed {
        st.installed_files.insert(hash, rel);
    }
    st.total_size = zip_size;

    finalize_package_install(services, st, emit)
}

fn deploy_zip_from_index(
    services: &Services,
    st: &mut InstallState,
    archive_hash: &str,
    zip_files: &BTreeMap<String, String>,
    zip_total_bytes: i64,
    emit: &(dyn Fn(ProgressEvent) + Sync),
) -> Result<(), InstallError> {
    let install_dir = st.install_dir.clone();
    clean_install_dir(&install_dir)
        .map_err(|err| text_phase_error(&format!("clean install dir: {err}")))?;

    report_deploy_start(emit, &st.pkg_name, 0);

    let linked = link_extracted_files(services, st, &install_dir, zip_files, emit)?;
    if linked == 0 {
        return Err(text_phase_error("no files were linked from archive"));
    }

    let (indexed, zip_size) = index_zip_member_links(services, st, zip_files, zip_total_bytes)?;
    for (hash, rel) in indexed {
        st.installed_files.insert(hash, rel);
    }
    st.total_size = zip_size;
    let _ = archive_hash;

    finalize_package_install(services, st, emit)
}

fn deploy_tar_archive(
    services: &Services,
    st: &mut InstallState,
    cas_path: &Path,
    archive_hash: &str,
    emit: &(dyn Fn(ProgressEvent) + Sync),
) -> Result<(), InstallError> {
    let (tar_files, tar_total) = services
        .downloader()
        .resolve_zip_member_index(archive_hash)
        .filter(|(files, _)| !files.is_empty())
        .map(|(files, total)| (files, total as i64))
        .unwrap_or_default();
    if tar_files.is_empty() {
        return deploy_tar_extract_and_adopt(services, st, cas_path, archive_hash, emit);
    }
    deploy_tar_from_index(services, st, archive_hash, &tar_files, tar_total, emit)
}

/// a failed member-index write is a
/// warning here (logs and continues).
fn deploy_tar_extract_and_adopt(
    services: &Services,
    st: &mut InstallState,
    cas_path: &Path,
    archive_hash: &str,
    emit: &(dyn Fn(ProgressEvent) + Sync),
) -> Result<(), InstallError> {
    let install_dir = st.install_dir.clone();
    clean_install_dir(&install_dir)
        .map_err(|err| text_phase_error(&format!("clean install dir: {err}")))?;

    report_deploy_start(emit, &st.pkg_name, 0);

    let download_name = st.download_name.clone();
    let (tar_files, mut tar_total) = extract_adopt(services, st, cas_path, &download_name, emit)?;

    if let Ok(info) = fs::metadata(cas_path) {
        tar_total += info.len() as i64;
    }
    if let Err(err) =
        services
            .downloader()
            .save_zip_member_index(archive_hash, &tar_files, tar_total as u64)
    {
        eprintln!("warning: failed to save tar index: {err}");
    }

    let (files, size) = refresh_installed_files(services, &install_dir);
    st.installed_files = files;
    st.total_size = size;

    finalize_package_install(services, st, emit)
}

/// delegates to the tar variant.
fn deploy_7z_extract_and_adopt(
    services: &Services,
    st: &mut InstallState,
    cas_path: &Path,
    archive_hash: &str,
    emit: &(dyn Fn(ProgressEvent) + Sync),
) -> Result<(), InstallError> {
    deploy_tar_extract_and_adopt(services, st, cas_path, archive_hash, emit)
}

fn deploy_tar_from_index(
    services: &Services,
    st: &mut InstallState,
    _archive_hash: &str,
    tar_files: &BTreeMap<String, String>,
    _tar_total_bytes: i64,
    emit: &(dyn Fn(ProgressEvent) + Sync),
) -> Result<(), InstallError> {
    let install_dir = st.install_dir.clone();
    clean_install_dir(&install_dir)
        .map_err(|err| text_phase_error(&format!("clean install dir: {err}")))?;

    report_deploy_start(emit, &st.pkg_name, 0);

    let _linked = link_extracted_files(services, st, &install_dir, tar_files, emit)?;

    let (files, size) = refresh_installed_files(services, &install_dir);
    st.installed_files = files;
    st.total_size = size;

    finalize_package_install(services, st, emit)
}

fn deploy_7z_archive(
    services: &Services,
    st: &mut InstallState,
    cas_path: &Path,
    archive_hash: &str,
    emit: &(dyn Fn(ProgressEvent) + Sync),
) -> Result<(), InstallError> {
    ensure_seven_zip(services).map_err(|err| text_phase_error(&format!("ensure 7z: {err}")))?;

    report_deploy_start(
        emit,
        &st.pkg_name,
        if archive_member_index_ready(services, archive_hash) {
            1
        } else {
            0
        },
    );

    let install_dir = st.install_dir.clone();
    let download_name = st.download_name.clone();
    let count = install_archive_from_member_index(
        services,
        st,
        cas_path,
        &download_name,
        archive_hash,
        &install_dir,
        emit,
    )
    .map_err(extract_phase_error_map)?;

    if count == 0 {
        return Err(text_phase_error(&format!(
            "no files were extracted from {}",
            st.file_ext
        )));
    }

    finalize_package_install(services, st, emit)
}

/// (download path).
///
/// Tries the persisted member index first; on a miss it extracts into the
/// install dir, adopts the results into the CAS store, and writes the member
/// index back. Returns the number of adopted/linked members.
fn install_archive_from_member_index(
    services: &Services,
    st: &mut InstallState,
    cas_path: &Path,
    download_name: &str,
    archive_hash: &str,
    install_dir: &Path,
    emit: &(dyn Fn(ProgressEvent) + Sync),
) -> Result<usize, InstallError> {
    let (files, total) = services
        .downloader()
        .resolve_zip_member_index(archive_hash)
        .filter(|(files, _)| !files.is_empty())
        .map(|(files, total)| (files, total as i64))
        .unwrap_or_default();

    if !files.is_empty() {
        clean_install_dir(install_dir)
            .map_err(|err| text_phase_error(&format!("clean install dir: {err}")))?;
        let linked = link_extracted_files(services, st, install_dir, &files, emit)?;
        if linked == 0 {
            return Err(text_phase_error("no files were linked from archive"));
        }
        let (indexed, size) = index_zip_member_links(services, st, &files, total)?;
        for (hash, rel) in indexed {
            st.installed_files.insert(hash, rel);
        }
        st.total_size = size;
        return Ok(linked);
    }

    clean_install_dir(install_dir)
        .map_err(|err| text_phase_error(&format!("clean install dir: {err}")))?;

    let (member_files, mut member_total) =
        extract_adopt(services, st, cas_path, download_name, emit)?;
    if let Ok(info) = fs::metadata(cas_path) {
        member_total += info.len() as i64;
    }

    services
        .downloader()
        .save_zip_member_index(archive_hash, &member_files, member_total as u64)
        .map_err(|err| {
            phase_error(
                "save archive member index",
                err,
                "Reinstall the package or try a different mirror",
            )
        })?;

    let (indexed, size) = index_zip_member_links(services, st, &member_files, member_total)
        .map_err(|err| {
            phase_error(
                "index adopted files",
                err,
                "Reinstall the package or try a different mirror",
            )
        })?;
    for (hash, rel) in indexed {
        st.installed_files.insert(hash, rel);
    }
    st.total_size = size;

    Ok(member_files.len())
}

// ---------------------------------------------------------------------------
// MSI / exe / plain-file deploy
// ---------------------------------------------------------------------------

fn deploy_msi(
    services: &Services,
    st: &mut InstallState,
    cas_path: &Path,
    hash: &str,
    emit: &(dyn Fn(ProgressEvent) + Sync),
) -> Result<(), InstallError> {
    if msi_needs_administrative_extract(st) {
        return deploy_msi_admin(services, st, cas_path, hash, emit);
    }
    deploy_msi_standard(services, st, cas_path, hash, emit)
}

/// links the MSI, then shells out to
/// `msiexec /a` to extract it into a temp dir, ingests the members and links
/// them into the install dir.
fn deploy_msi_admin(
    services: &Services,
    st: &mut InstallState,
    _cas_path: &Path,
    hash: &str,
    emit: &(dyn Fn(ProgressEvent) + Sync),
) -> Result<(), InstallError> {
    let install_dir = st.install_dir.clone();
    report_deploy_start(emit, &st.pkg_name, 0);

    let download_name = st.download_name.clone();
    let target = install_dir.join(&download_name);
    services.store().link(hash, &target).map_err(|err| {
        phase_error(
            &format!("link {download_name}"),
            text_err(err.to_string()),
            "Reinstall the package or try a different mirror",
        )
    })?;
    record_file(st, services.store(), hash, &download_name);

    // Administrative install: msiexec /a <msi> /qn TARGETDIR=<tmp>
    let tmp = msi_temp_dir(services, &download_name);
    let _ = fs::remove_dir_all(&tmp);
    fs::create_dir_all(&tmp)
        .map_err(|err| text_phase_error(&format!("create msi temp dir: {err}")))?;

    let msi_path = install_dir.join(&download_name);
    let mut cmd = Command::new("msiexec.exe");
    cmd.args([
        "/a",
        &msi_path.to_string_lossy(),
        "/qn",
        &format!("TARGETDIR={}", tmp.display()),
    ]);
    crate::procutil::hide_window(&mut cmd);
    let out = cmd
        .output()
        .map_err(|err| text_phase_error(&format!("run msiexec /a: {err}")))?;
    if !out.status.success() {
        let mut msg = String::from_utf8_lossy(&out.stdout).into_owned();
        msg.push_str(&String::from_utf8_lossy(&out.stderr));
        let _ = fs::remove_dir_all(&tmp);
        return Err(text_phase_error(&format!(
            "msiexec /a failed: {}",
            msg.trim()
        )));
    }

    // Ingest extracted members and link them into the install dir.
    let files = services
        .extractor()
        .ingest_extracted_dir(&tmp)
        .map_err(|err| {
            let _ = fs::remove_dir_all(&tmp);
            extract_phase_error(&text_err(format!("ingest msi admin: {err}")))
        })?;
    let _ = fs::remove_dir_all(&tmp);

    let linked = link_extracted_files(services, st, &install_dir, &files, emit)?;
    if linked == 0 {
        return Err(text_phase_error("no files were linked from msi admin"));
    }

    let extract_dir = manifest_extract_dir(st);
    if !extract_dir.is_empty() {
        let _ = apply_extract_dir_layout(&install_dir, &extract_dir)
            .map_err(|err| text_phase_error(&format!("apply extract_dir: {err}")))?;
    }

    finalize_package_install(services, st, emit)
}

/// See the `deploy_msi_admin` comment for deployment direction.
fn deploy_msi_standard(
    services: &Services,
    st: &mut InstallState,
    cas_path: &Path,
    _hash: &str,
    emit: &(dyn Fn(ProgressEvent) + Sync),
) -> Result<(), InstallError> {
    let install_dir = st.install_dir.clone();
    report_deploy_start(emit, &st.pkg_name, 0);
    ensure_seven_zip(services).map_err(|err| text_phase_error(&format!("ensure 7z: {err}")))?;

    let download_name = st.download_name.clone();
    let tmp = msi_temp_dir(services, &download_name);
    let _ = fs::remove_dir_all(&tmp);
    let extract = services
        .extractor()
        .extract_to_dir(cas_path, &tmp, &download_name)
        .and_then(|_| services.extractor().ingest_extracted_dir(&tmp));
    let _ = fs::remove_dir_all(&tmp);
    let files = match extract {
        Ok(files) => files,
        Err(err) => {
            return Err(extract_phase_error(&text_err(format!(
                "extract MSI: {err}"
            ))))
        }
    };

    let linked = link_extracted_files(services, st, &install_dir, &files, emit)?;
    if linked == 0 {
        return Err(text_phase_error("no files were linked from msi"));
    }

    let extract_dir = manifest_extract_dir(st);
    if !extract_dir.is_empty() {
        let _ = apply_extract_dir_layout(&install_dir, &extract_dir)
            .map_err(|err| text_phase_error(&format!("apply extract_dir: {err}")))?;
    }

    finalize_package_install(services, st, emit)
}

fn deploy_msi_alias(
    services: &Services,
    st: &mut InstallState,
    hash: &str,
) -> Result<(), InstallError> {
    let install_dir = st.install_dir.clone();
    report_deploy_start(noop_emit(), &st.pkg_name, 0);
    let download_name = st.download_name.clone();
    link_blob(services, st, hash, &download_name)?;
    let _ = &install_dir;
    st.file_ext = ".msi_".to_string();

    // No emitter in this path: it hands finalize the no-op sink.
    finalize_package_install(services, st, noop_emit())
}

fn deploy_exe(
    services: &Services,
    st: &mut InstallState,
    cas_path: &Path,
    hash: &str,
    emit: &(dyn Fn(ProgressEvent) + Sync),
) -> Result<(), InstallError> {
    let file_ext = st.file_ext.clone();
    if file_ext == ".exe" {
        if is_portable_exe_install(services, st) {
            return deploy_exe_portable_like(services, st, cas_path, hash, emit);
        }
        if is_pre_install_7z_hook_install(st) {
            return deploy_exe_portable_like(services, st, cas_path, hash, emit);
        }
        if st
            .manifest
            .as_ref()
            .map(|m| m.has_installer_script_for_install(&st.install_arch))
            .unwrap_or(false)
        {
            // An `.exe` + `installer.script` package is installer-managed: the
            // script expands `$dir\$fname` itself (PowerToys unpacks its Burn
            // bundle with dark.exe), so all the deploy phase does is put the
            // download in place. `finalize_package_install` then runs the
            // script and rescans the tree.
            //
            // Checked before `innosetup` so a manifest that carries both keeps
            // its own script: Scoop prefers `installer.script` too.
            return deploy_exe_portable_like(services, st, cas_path, hash, emit);
        }
        if uses_inno_extraction(st) {
            return deploy_inno_extract(services, st, hash, emit);
        }
        return deploy_exe_as_archive(services, st, cas_path, hash, emit);
    }
    deploy_exe_as_archive(services, st, cas_path, hash, emit)
}

/// Shared portable/SFX exe deploy (no, no deploy progress —
/// goes straight to).
fn deploy_exe_portable_like(
    services: &Services,
    st: &mut InstallState,
    _cas_path: &Path,
    _hash: &str,
    emit: &(dyn Fn(ProgressEvent) + Sync),
) -> Result<(), InstallError> {
    let install_dir = st.install_dir.clone();
    report_deploy_start(emit, &st.pkg_name, 0);
    let download_name = st.download_name.clone();
    link_portable_exe(services, st, &install_dir, &download_name, _hash)?;
    validate_install_dir(&install_dir)?;
    let (files, size) = refresh_installed_files(services, &install_dir);
    st.installed_files = files;
    st.total_size = size;

    finalize_package_install(services, st, emit)
}

/// Whether this install is a bare Inno Setup installer that has to be unpacked
/// rather than run (`innosetup: true`).
///
/// Only an `.exe` download qualifies, and only when the manifest has no
/// `installer.script` of its own: a script is a hand-written replacement for the
/// extraction, and running both would unpack twice.
fn uses_inno_extraction(st: &InstallState) -> bool {
    st.file_ext == ".exe"
        && st
            .manifest
            .as_ref()
            .map(|m| m.innosetup && !m.has_installer_script_for_install(&st.install_arch))
            .unwrap_or(false)
}

/// `innosetup: true`: take the payload out of the installer instead of running
/// it.
///
/// Many vendors ship a GUI-only Inno Setup installer and nothing else, with no
/// silent switch to pass, so a package manager's only option is to unpack it.
/// The extraction goes through the same PowerShell helper a manifest hook would
/// call (`Expand-InnoArchive`), which keeps the `innounp` switch set identical
/// for both paths.
///
/// `-Removal` deletes the installer once it has been unpacked — the same thing
/// `Expand-InnoArchive -Removal` does in a hook — so the version directory holds
/// only the payload, and the download is not mistaken for a program file.
fn deploy_inno_extract(
    services: &Services,
    st: &mut InstallState,
    source_hash: &str,
    emit: &(dyn Fn(ProgressEvent) + Sync),
) -> Result<(), InstallError> {
    let install_dir = st.install_dir.clone();
    let download_name = st.download_name.clone();
    report_deploy_start(emit, &st.pkg_name, 0);
    // `innounp` reads the installer from disk, so it has to be in place first.
    link_portable_exe(services, st, &install_dir, &download_name, source_hash)?;

    run_inno_extraction(services, st, &install_dir, &download_name)?;

    // Innounp exits successfully on files it cannot parse, so an empty tree is
    // the only signal that the download was not an Inno Setup installer after
    // all (a renamed zip, a truncated download). Say that instead of the generic
    // "install directory is empty".
    if install_dir_is_empty(&install_dir) {
        return Err(text_phase_error(&format!(
            "no files were extracted from {download_name}: it does not look like an Inno Setup installer"
        )));
    }
    validate_install_dir(&install_dir)?;
    let (files, size) = refresh_installed_files(services, &install_dir);
    st.installed_files = files;
    st.total_size = size;

    finalize_package_install(services, st, emit)
}

/// The one hook line Scoop's `innosetup` path executes. Kept as a constant so the
/// script test and the real run cannot disagree about the arguments.
const INNO_EXTRACT_HOOK: &str =
    r#"Expand-InnoArchive -Path "$dir\$fname" -DestinationPath "$dir" -Removal"#;

/// Run [`INNO_EXTRACT_HOOK`] through the normal hook runner.
///
/// Reusing [`HookScriptEnv`] means `$dir`, `$fname`, `$persist_dir` and the
/// helper preamble are set exactly as in a manifest hook, and the innounp
/// bootstrap path ([`ensure_innounp_for_extract`]) is shared with them.
fn run_inno_extraction(
    services: &Services,
    st: &mut InstallState,
    install_dir: &Path,
    download_name: &str,
) -> Result<(), InstallError> {
    let innounp = ensure_innounp_for_extract(services, &st.pkg_name, &mut st.timings)?;
    let root = services.root_dir().to_path_buf();
    let persist_dir = root.join("persist").join(&st.pkg_name);
    let version = st
        .manifest
        .as_ref()
        .map(|m| m.version.clone())
        .unwrap_or_default();
    let hooks = vec![INNO_EXTRACT_HOOK.to_string()];
    let env = HookScriptEnv {
        install_dir,
        download_name,
        version: &version,
        app: &st.pkg_name,
        bucket: &crate::catalog::package_bucket_name(&st.pkg_ref),
        buckets_dir: &root.join("buckets"),
        persist_dir: &persist_dir,
        arch: &st.install_arch,
        glue_root: &root,
        // The helper needs innounp only. Bootstrapping 7-Zip here would pull a
        // download this package never uses.
        seven_zip: "",
        dark: "",
        innounp: &innounp,
        interactive: false,
        cmd: "",
        hooks: &hooks,
    };
    run_powershell_script(
        &env,
        "inno_extract",
        &format!("Inno Setup extraction for {}", st.pkg_name),
    )
}

/// an `.exe` that is really a 7z SFX.
fn deploy_exe_as_archive(
    services: &Services,
    st: &mut InstallState,
    cas_path: &Path,
    hash: &str,
    emit: &(dyn Fn(ProgressEvent) + Sync),
) -> Result<(), InstallError> {
    // Routes through installArchiveFromMemberIndex (exe-as-archive).
    ensure_seven_zip(services).map_err(|err| text_phase_error(&format!("ensure 7z: {err}")))?;
    let install_dir = st.install_dir.clone();
    let download_name = st.download_name.clone();
    let count = install_archive_from_member_index(
        services,
        st,
        cas_path,
        &download_name,
        hash,
        &install_dir,
        emit,
    )
    .map_err(extract_phase_error_map)?;
    if count == 0 {
        return Err(text_phase_error(&format!(
            "no files were extracted from {}",
            st.file_ext
        )));
    }
    finalize_package_install(services, st, emit)
}

/// Linking resolves the store object from
/// `hash`, so no CAS path is needed.
fn deploy_generic_file(
    services: &Services,
    st: &mut InstallState,
    hash: &str,
) -> Result<(), InstallError> {
    let download_name = st.download_name.clone();
    link_blob(services, st, hash, &download_name)?;

    // No emitter in this path: it hands finalize the no-op sink.
    finalize_package_install(services, st, noop_emit())
}

// ---------------------------------------------------------------------------
// Deploy helpers: progress, linking, adoption, flattening
// ---------------------------------------------------------------------------

/// No-op progress sink used where a signature requires an emitter but the call
/// site has none (passes `nil`).
fn noop_emit() -> &'static (dyn Fn(ProgressEvent) + Sync) {
    &|_| {}
}

/// the deploy progress ping. `files` is the number of
/// files this pass is about to link/adopt (0 when not yet known).
fn report_deploy_start(emit: &(dyn Fn(ProgressEvent) + Sync), package: &str, files: usize) {
    emit(ProgressEvent::Deploying {
        package: package.to_string(),
        files,
    });
}

/// Phase error whose cause is plain text (no `source`).
fn text_phase_error(message: &str) -> InstallError {
    let msg = text_err(message);
    InstallError::phase(message, &msg, "")
}

/// Adapter so the extract-phase wrapper can be used as a `map_err`.
fn extract_phase_error_map(err: InstallError) -> InstallError {
    extract_phase_error(&err)
}

/// Link the store object `hash` into the install dir as `download_name` and
/// record it. shares this shape between,
/// and.
///
/// Takes the hash instead of a source path on purpose: every caller knows it
/// already, and re-hashes the blob to recover it — a second
/// full SHA-256 pass over bytes the downloader had just hashed.
fn link_blob(
    services: &Services,
    st: &mut InstallState,
    hash: &str,
    download_name: &str,
) -> Result<(), InstallError> {
    let timing = st.timings.start();
    let install_dir = st.install_dir.clone();
    let target = install_dir.join(download_name);
    if let Some(parent) = target.parent() {
        fs::create_dir_all(parent).map_err(|err| {
            permission_phase_error(
                "failed to create install directory",
                &err,
                &parent.to_string_lossy(),
            )
        })?;
    }
    services.store().link(hash, &target).map_err(|err| {
        phase_error(
            &format!("link {download_name}"),
            err,
            "Reinstall the package or try a different mirror",
        )
    })?;
    record_file(st, services.store(), hash, download_name);
    st.timings.add(Bucket::Link, timing);
    Ok(())
}

fn link_portable_exe(
    services: &Services,
    st: &mut InstallState,
    install_dir: &Path,
    download_name: &str,
    hash: &str,
) -> Result<(), InstallError> {
    let target = install_dir.join(download_name);
    services.store().link(hash, &target).map_err(|err| {
        phase_error(
            &format!("link {download_name}"),
            err,
            "Reinstall the package or try a different mirror",
        )
    })?;
    record_file(st, services.store(), hash, download_name);
    Ok(())
}

/// Pick the CAS object path for a cache entry + archive hash, falling back to
/// `download_name`'s recorded file when the index stores a non-CAS path.
/// Store hash backing a cached artifact. The entry lookup can select a
/// *different* hash than `archive_hash` (a `rel == download_name` match wins
/// first), which is why callers must not just reuse `archive_hash`.
fn store_hash_for_cached_artifact(
    entry: &PackageEntry,
    archive_hash: &str,
    download_name: &str,
) -> String {
    for (hash, rel) in &entry.files {
        if hash == archive_hash || rel == download_name {
            return hash.clone();
        }
    }
    archive_hash.to_string()
}

/// sequential mkdir + link of the cached files
/// that are not skipped by the archive-blob rules.
fn link_from_cache(
    services: &Services,
    install_dir: &Path,
    entry: &PackageEntry,
) -> Result<usize, InstallError> {
    let mut linked = 0usize;
    for (hash, rel) in &entry.files {
        if cache::is_hidden_install_path(rel) || archmember::is_directory_placeholder_name(rel) {
            continue;
        }
        let target = safepath::join_under_base(install_dir, rel).map_err(|err| {
            phase_error(
                &format!("link {rel}"),
                err,
                "Reinstall the package or try a different mirror",
            )
        })?;
        if let Some(parent) = target.parent() {
            fs::create_dir_all(parent).map_err(|err| {
                permission_phase_error(
                    "failed to create install directory",
                    &err,
                    &parent.to_string_lossy(),
                )
            })?;
        }
        services.store().link(hash, &target).map_err(|err| {
            phase_error(
                &format!("link {rel}"),
                err,
                "Reinstall the package or try a different mirror",
            )
        })?;
        linked += 1;
    }
    Ok(linked)
}

/// `installArchive.go:linkMultiArtifactFromResults`.
fn link_multi_artifact_from_results(
    services: &Services,
    st: &mut InstallState,
    install_dir: &Path,
    results: &[DownloadResult],
) -> Result<usize, InstallError> {
    let mut linked = 0usize;
    for r in results {
        if let Some(e) = &r.error {
            return Err(phase_error(
                "multi-artifact download failed",
                text_err(e.to_string()),
                "Reinstall the package or try a different mirror",
            ));
        }
        let name = r.task.filename.trim().to_string();
        if name.is_empty() || r.hash.is_empty() {
            return Err(text_phase_error(
                "multi-artifact download missing filename or hash",
            ));
        }
        let target = install_dir.join(&name);
        if let Some(parent) = target.parent() {
            fs::create_dir_all(parent).map_err(|err| {
                permission_phase_error(
                    "failed to create install directory",
                    &err,
                    &parent.to_string_lossy(),
                )
            })?;
        }
        services.store().link(&r.hash, &target).map_err(|err| {
            phase_error(
                &format!("link {name}"),
                err,
                "Reinstall the package or try a different mirror",
            )
        })?;
        record_file(st, services.store(), &r.hash, &name);
        linked += 1;
    }
    Ok(linked)
}

/// `installArchive.go:linkMultiArtifactFromCache`.
fn link_multi_artifact_from_cache(
    services: &Services,
    st: &mut InstallState,
    install_dir: &Path,
    entry: &PackageEntry,
    names: &[String],
) -> Result<usize, InstallError> {
    let mut want: BTreeMap<String, String> = BTreeMap::new();
    for (hash, rel) in &entry.files {
        if cache::is_hidden_install_path(rel) {
            continue;
        }
        want.insert(go_to_slash(rel).to_lowercase(), hash.clone());
    }
    let mut linked = 0usize;
    for name in names {
        let key = name.trim().to_lowercase();
        let Some(hash) = want.get(&key) else {
            return Err(text_phase_error(&format!(
                "cache missing artifact {name:?} (reinstall with --force)"
            )));
        };
        let target = install_dir.join(name);
        if let Some(parent) = target.parent() {
            fs::create_dir_all(parent).map_err(|err| {
                permission_phase_error(
                    "failed to create install directory",
                    &err,
                    &parent.to_string_lossy(),
                )
            })?;
        }
        services.store().link(hash, &target).map_err(|err| {
            phase_error(
                &format!("link {name}"),
                err,
                "Reinstall the package or try a different mirror",
            )
        })?;
        record_file(st, services.store(), hash, name);
        linked += 1;
    }
    Ok(linked)
}

fn expected_multi_artifact_names(pairs: &[UrlHashPair]) -> Result<Vec<String>, InstallError> {
    let mut names = Vec::with_capacity(pairs.len());
    for pair in pairs {
        let parsed = manifest::parse_url(&pair.url)?;
        if parsed.local_name.is_empty() {
            return Err(text_phase_error(&format!(
                "multi-artifact URL missing filename: {}",
                pair.url
            )));
        }
        names.push(parsed.local_name);
    }
    Ok(names)
}

/// delete every child of `dir` (dir itself survives).
fn clean_install_dir(dir: &Path) -> io::Result<()> {
    let entries = match fs::read_dir(dir) {
        Ok(entries) => entries,
        Err(err) if err.kind() == io::ErrorKind::NotFound => return Ok(()),
        Err(err) => return Err(err),
    };
    for entry in entries {
        let entry = entry?;
        let path = entry.path();
        if entry.file_type()?.is_dir() {
            remove_dir_all(&path)?;
        } else {
            match fs::remove_file(&path) {
                Ok(()) => {}
                // A read-only file survives the delete with Access denied; the
                // same clear-then-retry the directory branch relies on fixes it.
                Err(err) if err.kind() == io::ErrorKind::PermissionDenied => {
                    clear_readonly_recursive(&path);
                    fs::remove_file(&path)?;
                }
                Err(err) => return Err(err),
            }
        }
    }
    Ok(())
}

/// resolve under the install dir.
fn install_extract_dest(install_dir: &Path, m: &Manifest, install_arch: &str) -> PathBuf {
    let extract_to = m.get_extract_to_for_install(install_arch);
    match safepath::join_under_base(install_dir, &extract_to) {
        Ok(p) => p,
        Err(_) => install_dir.to_path_buf(),
    }
}

/// rescan `install_dir` into `hash → relPath`
/// plus the store-payload-based total.
fn refresh_installed_files(
    services: &Services,
    install_dir: &Path,
) -> (BTreeMap<String, String>, i64) {
    cache::scan::scan_install_dir(install_dir, services.store(), None)
}

/// Files between two link-progress events. Linking is a local hardlink pass
/// (thousands of files for large packages), so a per-file event would swamp
/// the consumer while adding nothing visible.
const LINK_PROGRESS_STRIDE: usize = 32;

/// Report link progress: `done` of `total` member files are hardlinked.
fn report_link_progress(
    emit: &(dyn Fn(ProgressEvent) + Sync),
    package: &str,
    done: usize,
    total: usize,
) {
    emit(ProgressEvent::LinkingFiles {
        package: package.to_string(),
        done,
        total,
    });
}

/// hardlink member blobs (relPath → hash)
/// into `install_dir`, applying `extract_to` / `extract_dir`.
fn link_extracted_files(
    services: &Services,
    st: &mut InstallState,
    install_dir: &Path,
    files: &BTreeMap<String, String>,
    emit: &(dyn Fn(ProgressEvent) + Sync),
) -> Result<usize, InstallError> {
    let timing = st.timings.start();
    let result = link_extracted_files_inner(services, st, install_dir, files, emit);
    st.timings.add(Bucket::Link, timing);
    result
}

fn link_extracted_files_inner(
    services: &Services,
    st: &mut InstallState,
    install_dir: &Path,
    files: &BTreeMap<String, String>,
    emit: &(dyn Fn(ProgressEvent) + Sync),
) -> Result<usize, InstallError> {
    let extract_to = st
        .manifest
        .as_ref()
        .map(|m| m.get_extract_to_for_install(&st.install_arch))
        .unwrap_or_default();
    let extract_dir = st
        .manifest
        .as_ref()
        .map(|m| m.get_extract_dir_for_install(&st.install_arch))
        .unwrap_or_default();

    let mut items: Vec<(String, String)> = Vec::with_capacity(files.len());
    for (rel_path, hash) in files {
        if cache::is_hidden_install_path(rel_path)
            || archmember::is_directory_placeholder_name(rel_path)
        {
            continue;
        }
        items.push((archmember::normalize_member(rel_path), hash.clone()));
    }
    items.sort_by(|a, b| {
        let da = archmember::depth(&a.0);
        let db = archmember::depth(&b.0);
        da.cmp(&db).then_with(|| a.0.cmp(&b.0))
    });

    struct Job {
        rel_path: String,
        target_rel: String,
        target: PathBuf,
        hash: String,
    }

    let mut jobs: Vec<Job> = Vec::with_capacity(items.len());
    let mut dir_set: BTreeSet<PathBuf> = BTreeSet::new();
    for (rel_path, hash) in items {
        let target_rel = install_member_rel_path(&extract_to, &extract_dir, &rel_path)?;
        if target_rel.is_empty() {
            continue;
        }
        let target = safepath::join_under_base(install_dir, &target_rel)
            .map_err(|err| text_phase_error(&format!("link {rel_path}: {err}")))?;
        let mut dir = target.parent().map(|p| p.to_path_buf());
        while let Some(d) = dir {
            if d.as_os_str().is_empty() || d == Path::new(".") {
                break;
            }
            let parent = d.parent().map(|p| p.to_path_buf());
            dir_set.insert(d.clone());
            match parent {
                Some(p) if p != d => dir = Some(p),
                _ => break,
            }
        }
        jobs.push(Job {
            rel_path,
            target_rel,
            target,
            hash,
        });
    }
    if jobs.is_empty() {
        return Ok(0);
    }
    let total = jobs.len();
    report_link_progress(emit, &st.pkg_ref, 0, total);

    let mut dirs: Vec<PathBuf> = dir_set.into_iter().collect();
    dirs.sort_by(|a, b| {
        let da = archmember::depth(&go_to_slash(&a.to_string_lossy()));
        let db = archmember::depth(&go_to_slash(&b.to_string_lossy()));
        da.cmp(&db).then_with(|| a.cmp(b))
    });
    let mut created: BTreeSet<PathBuf> = BTreeSet::new();
    for dir in &dirs {
        ensure_link_parent_dir_cached(dir, &mut created)?;
    }

    let workers = link_workers(jobs.len());
    let store = services.store();
    if workers <= 1 {
        let mut linked = 0usize;
        for job in &jobs {
            store
                .link(&job.hash, &job.target)
                .map_err(|err| text_phase_error(&format!("link {}: {err}", job.rel_path)))?;
            st.installed_files
                .insert(job.hash.clone(), job.target_rel.clone());
            if !st.installed_files.contains_key(&job.hash) {
                st.total_size += fs::metadata(&job.target)
                    .map(|m| m.len() as i64)
                    .unwrap_or(0);
            }
            linked += 1;
            if linked.is_multiple_of(LINK_PROGRESS_STRIDE) || linked == total {
                report_link_progress(emit, &st.pkg_ref, linked, total);
            }
        }
        return Ok(linked);
    }

    let cursor = AtomicUsize::new(0);
    let linked = AtomicUsize::new(0);
    let first_err: Mutex<Option<InstallError>> = Mutex::new(None);
    let records: Mutex<Vec<(String, String)>> = Mutex::new(Vec::new());
    // Workers emit from several threads; the consumer redraws one line, so the
    // emissions themselves are serialised.
    let emit_lock = Mutex::new(());
    let package = st.pkg_ref.clone();
    thread::scope(|scope| {
        for _ in 0..workers {
            scope.spawn(|| loop {
                let i = cursor.fetch_add(1, Ordering::SeqCst);
                if i >= jobs.len() {
                    break;
                }
                let job = &jobs[i];
                if let Err(err) = store.link(&job.hash, &job.target) {
                    let mut slot = first_err.lock().unwrap();
                    if slot.is_none() {
                        *slot = Some(text_phase_error(&format!("link {}: {err}", job.rel_path)));
                    }
                    continue;
                }
                records
                    .lock()
                    .unwrap()
                    .push((job.hash.clone(), job.target_rel.clone()));
                let done = linked.fetch_add(1, Ordering::SeqCst) + 1;
                if done.is_multiple_of(LINK_PROGRESS_STRIDE) || done == total {
                    let _guard = emit_lock
                        .lock()
                        .unwrap_or_else(|poisoned| poisoned.into_inner());
                    report_link_progress(emit, &package, done, total);
                }
            });
        }
    });

    if let Some(err) = first_err.into_inner().unwrap() {
        return Err(err);
    }
    for (hash, rel) in records.into_inner().unwrap() {
        st.installed_files.insert(hash, rel);
    }
    Ok(linked.load(Ordering::SeqCst))
}

fn link_workers(item_count: usize) -> usize {
    if item_count <= 1 {
        return 1;
    }
    let mut n = num_cpu();
    if item_count > 5000 {
        n *= 4;
    } else if item_count > 1000 {
        n *= 2;
    }
    n.clamp(2, 32).min(item_count)
}

fn ensure_link_parent_dir_cached(
    dir: &Path,
    created: &mut BTreeSet<PathBuf>,
) -> Result<(), InstallError> {
    let dir = dir.to_path_buf();
    if dir.as_os_str().is_empty() || dir == Path::new(".") {
        return Ok(());
    }
    if created.contains(&dir) {
        return Ok(());
    }
    if let Some(parent) = dir.parent() {
        if parent != dir {
            ensure_link_parent_dir_cached(parent, created)?;
        }
    }
    ensure_link_parent_dir(&dir)?;
    created.insert(dir);
    Ok(())
}

/// mkdir, clearing zero-byte zip folder markers that
/// block mkdir on Windows.
fn ensure_link_parent_dir(dir: &Path) -> Result<(), InstallError> {
    if dir.as_os_str().is_empty() || dir == Path::new(".") {
        return Ok(());
    }
    if let Some(parent) = dir.parent() {
        if parent != dir {
            ensure_link_parent_dir(parent)?;
        }
    }
    match fs::metadata(dir) {
        Ok(info) => {
            if info.is_dir() {
                return Ok(());
            }
            if info.len() == 0 {
                fs::remove_file(dir).map_err(|err| text_phase_error(&err.to_string()))?;
            } else {
                return Err(text_phase_error(&format!(
                    "path exists as file: {}",
                    dir.display()
                )));
            }
        }
        Err(err) if err.kind() == io::ErrorKind::NotFound => {}
        Err(err) => return Err(text_phase_error(&err.to_string())),
    }
    fs::create_dir_all(dir).map_err(|err| text_phase_error(&err.to_string()))
}

// ---------------------------------------------------------------------------
// Adopt install dir into the CAS store
// ---------------------------------------------------------------------------

/// Move one extracted file into the CAS.
///
/// When the extractor recorded the file's digest while writing it (`known`),
/// the digest is reused and the bytes are never read again. The install-
/// relative path is tried first — it costs a map lookup — and the hardlink
/// identity second, for files a layout pass moved. `size` comes from the walk
/// that found the file, so the stale-key guard needs no extra `stat` on the
/// source.
fn adopt_file(
    store: &Store,
    path: &Path,
    rel: &str,
    size: u64,
    known: Option<&BTreeMap<String, String>>,
    by_rel: bool,
) -> Result<String, crate::store::StoreError> {
    if let Some(hashes) = known {
        // Path keys are only valid while the extraction layout is untouched:
        // a flatten pass can move one member onto another member's path, and
        // the identity key is what survives that.
        let by_path = if by_rel {
            hashes.get(&crate::extractor::rel_hash_key(rel)).cloned()
        } else {
            None
        };
        let recorded = by_path.or_else(|| {
            let key = crate::cache::fileid::file_ref_key(path)?;
            hashes.get(&key).cloned()
        });
        if let Some(hash) = recorded {
            // Guard the CAS invariant against a stale key (NTFS can reuse a
            // deleted file's index): trust the recorded digest only when the
            // object already in the store has the same size.
            let trust = match std::fs::metadata(store.object_path(&hash)) {
                Ok(object) => object.len() == size,
                Err(_) => true,
            };
            if trust {
                store.adopt_with_hash(path, &hash)?;
                return Ok(hash);
            }
        }
    }
    store.adopt(path)
}

fn adopt_install_dir_to_store(
    services: &Services,
    install_dir: &Path,
    package: &str,
    emit: &(dyn Fn(ProgressEvent) + Sync),
    known: Option<&BTreeMap<String, String>>,
    by_rel: bool,
) -> Result<(BTreeMap<String, String>, i64), InstallError> {
    struct Job {
        abs: PathBuf,
        rel: String,
        size: i64,
    }
    let mut jobs: Vec<Job> = Vec::new();
    walk_install_files(install_dir, &mut |path: &Path, meta: &fs::Metadata| {
        let Ok(rel) = path.strip_prefix(install_dir) else {
            return;
        };
        let rel_path = go_to_slash(&rel.to_string_lossy());
        if cache::is_hidden_install_path(&rel_path) {
            return;
        }
        jobs.push(Job {
            abs: path.to_path_buf(),
            rel: archmember::normalize_member(&rel_path),
            size: meta.len() as i64,
        });
    });

    if jobs.is_empty() {
        return Err(text_phase_error(&format!(
            "no files to adopt under {}",
            install_dir.display()
        )));
    }

    let total = jobs.len();
    let store = services.store();
    let mut files: BTreeMap<String, String> = BTreeMap::new();
    let mut adopted_bytes: i64 = 0;
    let workers = adopt_workers(jobs.len());
    // Workers emit from several threads; the consumer redraws one line, so the
    // emissions themselves are serialised.
    let emit_lock = Mutex::new(());
    report_link_progress(emit, package, 0, total);

    if workers <= 1 {
        for (i, job) in jobs.iter().enumerate() {
            let hash = adopt_file(store, &job.abs, &job.rel, job.size as u64, known, by_rel)
                .map_err(|err| text_phase_error(&err.to_string()))?;
            files.insert(job.rel.clone(), hash);
            adopted_bytes += job.size;
            let n = i + 1;
            if n.is_multiple_of(LINK_PROGRESS_STRIDE) || n == total {
                report_link_progress(emit, package, n, total);
            }
        }
        return Ok((files, adopted_bytes));
    }

    let cursor = AtomicUsize::new(0);
    let processed = AtomicUsize::new(0);
    let first_err: Mutex<Option<InstallError>> = Mutex::new(None);

    // Each worker keeps its own results: a shared Vec under a mutex put a lock
    // on the hot path for every single file.
    let (collected, bytes) = thread::scope(|scope| {
        let mut handles = Vec::with_capacity(workers);
        for _ in 0..workers {
            let cursor = &cursor;
            let processed = &processed;
            let first_err = &first_err;
            let emit_lock = &emit_lock;
            handles.push(scope.spawn(|| {
                let mut local: Vec<(String, String)> = Vec::new();
                let mut local_bytes: i64 = 0;
                loop {
                    let i = cursor.fetch_add(1, Ordering::Relaxed);
                    if i >= jobs.len() {
                        break;
                    }
                    let job = &jobs[i];
                    match adopt_file(store, &job.abs, &job.rel, job.size as u64, known, by_rel) {
                        Ok(hash) => {
                            local.push((job.rel.clone(), hash));
                            local_bytes += job.size;
                            let n = processed.fetch_add(1, Ordering::Relaxed) + 1;
                            if n.is_multiple_of(LINK_PROGRESS_STRIDE) || n == total {
                                let _guard = emit_lock
                                    .lock()
                                    .unwrap_or_else(|poisoned| poisoned.into_inner());
                                report_link_progress(emit, package, n, total);
                            }
                        }
                        Err(err) => {
                            let mut slot = first_err.lock().unwrap();
                            if slot.is_none() {
                                *slot =
                                    Some(text_phase_error(&format!("adopt {}: {err}", job.rel)));
                            }
                        }
                    }
                }
                (local, local_bytes)
            }));
        }
        let mut all: Vec<(String, String)> = Vec::new();
        let mut bytes: i64 = 0;
        for handle in handles {
            let (local, local_bytes) = handle.join().unwrap();
            all.extend(local);
            bytes += local_bytes;
        }
        (all, bytes)
    });

    if let Some(err) = first_err.into_inner().unwrap() {
        return Err(err);
    }
    for (rel, hash) in collected {
        files.insert(rel, hash);
    }
    Ok((files, bytes))
}

/// Adopt-and-return helper shared by the zip/tar extract paths.
fn extract_adopt(
    services: &Services,
    st: &mut InstallState,
    cas_path: &Path,
    download_name: &str,
    emit: &(dyn Fn(ProgressEvent) + Sync),
) -> Result<(BTreeMap<String, String>, i64), InstallError> {
    // covers the whole extract+adopt span as one bucket.
    let timing = st.timings.start();
    let result = extract_adopt_inner(services, st, cas_path, download_name, emit);
    st.timings.add(Bucket::Extract, timing);
    result
}

/// Fail fast when the data root's volume cannot hold the unpacked archive.
///
/// Without this the volume fills up mid-extraction: the partial install is
/// removed afterwards, but only after the download and the write cost have
/// been paid.
fn ensure_free_space(
    root: &Path,
    needed: u64,
    package: &str,
    archive: &str,
) -> Result<(), InstallError> {
    let Some(available) = crate::disk::available_space(root) else {
        return Ok(());
    };
    // Cluster slack, the install record and shims: a small share of the payload.
    let margin = (needed / 20).max(32 << 20);
    let required = needed.saturating_add(margin);
    if available >= required {
        return Ok(());
    }
    Err(phase_error(
        "insufficient disk space",
        format!(
            "unpacking {archive} for {package} needs about {}, but only {} is free on {}",
            format_bytes(required),
            format_bytes(available),
            root.display()
        ),
        "Free up space on that volume and retry: the downloaded archive is kept, so the retry does not download it again",
    ))
}

/// Human-readable byte size for preflight messages.
fn format_bytes(bytes: u64) -> String {
    const UNITS: [&str; 4] = ["B", "KiB", "MiB", "GiB"];
    let mut value = bytes as f64;
    let mut unit = 0;
    while value >= 1024.0 && unit < UNITS.len() - 1 {
        value /= 1024.0;
        unit += 1;
    }
    if unit == 0 {
        format!("{bytes} B")
    } else {
        format!("{value:.1} {}", UNITS[unit])
    }
}

fn extract_adopt_inner(
    services: &Services,
    st: &mut InstallState,
    cas_path: &Path,
    download_name: &str,
    emit: &(dyn Fn(ProgressEvent) + Sync),
) -> Result<(BTreeMap<String, String>, i64), InstallError> {
    let install_dir = st.install_dir.clone();
    let dest = install_extract_dest(
        &install_dir,
        st.manifest.as_ref().unwrap(),
        &st.install_arch,
    );
    // Estimate before touching the disk: ZIP and tar carry the unpacked sizes,
    // so a volume that cannot hold the result fails here instead of midway
    // through the write.
    if let Some(needed) = crate::extractor::estimate_extract_bytes(cas_path, download_name) {
        ensure_free_space(services.root_dir(), needed, &st.pkg_ref, download_name)?;
    }
    // Drop any counters left by an extraction that bailed before adopt.
    let _ = crate::extract_profile::take_last();
    let extract_start = std::time::Instant::now();
    let known_hashes = services
        .extractor()
        .extract_to_dir(cas_path, &dest, download_name)
        .map_err(|err| extract_phase_error(&text_err(err.to_string())))?;
    let extract_wall = extract_start.elapsed();

    let extract_dir = st
        .manifest
        .as_ref()
        .map(|m| m.get_extract_dir_for_install(&st.install_arch))
        .unwrap_or_default();
    let layout_start = std::time::Instant::now();
    let mut layout_applied = false;
    if !extract_dir.is_empty() {
        layout_applied = apply_extract_dir_layout(&install_dir, &extract_dir)
            .map_err(|err| text_phase_error(&format!("apply extract_dir: {err}")))?;
    }
    let layout_wall = layout_start.elapsed();

    let adopt_start = std::time::Instant::now();
    let result = adopt_install_dir_to_store(
        services,
        &install_dir,
        &st.pkg_ref,
        emit,
        Some(&known_hashes),
        !layout_applied,
    );
    let adopt_wall = adopt_start.elapsed();

    let (kind, counters) = match crate::extract_profile::take_last() {
        Some((kind, counters)) => (kind, Some(counters)),
        None => ("other", None),
    };
    crate::extract_profile::print_line(kind, counters, extract_wall, layout_wall, adopt_wall);
    result
}

fn adopt_workers(item_count: usize) -> usize {
    if item_count <= 1 {
        return 1;
    }
    let mut n = num_cpu();
    if item_count > 5000 {
        n *= 4;
    } else if item_count > 1000 {
        n *= 2;
    }
    n.clamp(2, 32).min(item_count)
}

// ---------------------------------------------------------------------------
// indexZipMemberLinks + extract_dir layout helpers
// ---------------------------------------------------------------------------

/// `zip_files` is `relPath → hash`; returns
/// `hash → targetRelPath` plus the resolved total size.
fn index_zip_member_links(
    services: &Services,
    st: &InstallState,
    zip_files: &BTreeMap<String, String>,
    zip_total_bytes: i64,
) -> Result<(BTreeMap<String, String>, i64), InstallError> {
    let extract_to = st
        .manifest
        .as_ref()
        .map(|m| m.get_extract_to_for_install(&st.install_arch))
        .unwrap_or_default();
    let extract_dir = st
        .manifest
        .as_ref()
        .map(|m| m.get_extract_dir_for_install(&st.install_arch))
        .unwrap_or_default();

    let mut installed: BTreeMap<String, String> = BTreeMap::new();
    for (rel_path, hash) in zip_files {
        if cache::is_hidden_install_path(rel_path)
            || archmember::is_directory_placeholder_name(rel_path)
        {
            continue;
        }
        let rel = archmember::normalize_member(rel_path);
        let target_rel = install_member_rel_path(&extract_to, &extract_dir, &rel)?;
        if target_rel.is_empty() {
            continue;
        }
        installed.insert(hash.clone(), target_rel);
    }
    if !st.archive_hash.is_empty() && !st.download_name.is_empty() {
        installed.insert(st.archive_hash.clone(), st.download_name.clone());
    }
    if zip_total_bytes > 0 {
        return Ok((installed, zip_total_bytes));
    }
    let total = sum_unique_store_object_sizes(services.store(), &installed)?;
    Ok((installed, total))
}

fn sum_unique_store_object_sizes(
    store: &Store,
    files: &BTreeMap<String, String>,
) -> Result<i64, InstallError> {
    let mut seen: BTreeSet<String> = BTreeSet::new();
    let mut unique: Vec<String> = Vec::new();
    for hash in files.keys() {
        if seen.insert(hash.clone()) {
            unique.push(hash.clone());
        }
    }
    if unique.is_empty() {
        return Ok(0);
    }
    let workers = num_cpu().clamp(2, 8).min(unique.len());
    let mut total: i64 = 0;
    if workers <= 1 {
        for hash in &unique {
            let info = fs::metadata(store.object_path(hash)).map_err(|err| {
                text_phase_error(&format!("stat {}: {err}", &hash[..hash.len().min(8)]))
            })?;
            total += info.len() as i64;
        }
        return Ok(total);
    }
    let cursor = AtomicUsize::new(0);
    let sum = AtomicI64::new(0);
    let first_err: Mutex<Option<InstallError>> = Mutex::new(None);
    thread::scope(|scope| {
        for _ in 0..workers {
            scope.spawn(|| loop {
                let i = cursor.fetch_add(1, Ordering::SeqCst);
                if i >= unique.len() {
                    break;
                }
                let hash = &unique[i];
                match fs::metadata(store.object_path(hash)) {
                    Ok(info) => {
                        sum.fetch_add(info.len() as i64, Ordering::SeqCst);
                    }
                    Err(err) => {
                        let mut slot = first_err.lock().unwrap();
                        if slot.is_none() {
                            *slot = Some(text_phase_error(&format!(
                                "stat {}: {err}",
                                &hash[..hash.len().min(8)]
                            )));
                        }
                    }
                }
            });
        }
    });
    if let Some(err) = first_err.into_inner().unwrap() {
        return Err(err);
    }
    Ok(sum.load(Ordering::SeqCst))
}

fn install_member_rel_path(
    extract_to: &str,
    extract_dir: &str,
    rel_path: &str,
) -> Result<String, InstallError> {
    let rel = rel_path_after_extract_dir(rel_path, extract_dir);
    if rel.is_empty() {
        return Ok(String::new());
    }
    let safe_rel = safepath::validate_manifest_rel_path(&rel)
        .map_err(|err| text_phase_error(&format!("archive member {rel_path:?}: {err}")))?;
    if safe_rel.is_empty() {
        return Ok(String::new());
    }
    let safe_extract_to = safepath::validate_manifest_rel_path(extract_to)
        .map_err(|err| text_phase_error(&err.to_string()))?;
    if safe_extract_to.is_empty() {
        return Ok(safe_rel);
    }
    let combined = format!("{safe_extract_to}/{safe_rel}");
    safepath::validate_manifest_rel_path(&combined)
        .map_err(|err| text_phase_error(&err.to_string()))
}

fn rel_path_after_extract_dir(rel_path: &str, extract_dir: &str) -> String {
    for prefix in extract_dir_lookup_paths(extract_dir) {
        if let Some(rest) = strip_extract_dir_prefix(rel_path, &prefix) {
            return rest;
        }
    }
    rel_path.to_string()
}

fn strip_extract_dir_prefix(rel_path: &str, prefix: &str) -> Option<String> {
    let rel = normalize_manifest_rel_path(rel_path);
    let prefix = normalize_manifest_rel_path(prefix);
    if prefix.is_empty() || prefix == "." {
        return None;
    }
    if !rel.starts_with(&prefix) {
        return None;
    }
    let rest = rel[prefix.len()..].trim_start_matches('/');
    Some(rest.to_string())
}

fn extract_dir_lookup_paths(extract_dir: &str) -> Vec<String> {
    let base = normalize_manifest_rel_path(extract_dir);
    if base.is_empty() || base == "." {
        return Vec::new();
    }
    let mut seen: BTreeSet<String> = BTreeSet::new();
    seen.insert(base.clone());
    let mut out = vec![base.clone()];
    let add = |p: String, out: &mut Vec<String>, seen: &mut BTreeSet<String>| {
        let p = normalize_manifest_rel_path(&p);
        if p.is_empty() || p == "." || !seen.insert(p.clone()) {
            return;
        }
        out.push(p);
    };
    if let Some(rest) = base.strip_prefix("PFiles64/") {
        add(format!("Program Files/{rest}"), &mut out, &mut seen);
    }
    if let Some(rest) = base.strip_prefix("PFiles/") {
        add(format!("Program Files (x86)/{rest}"), &mut out, &mut seen);
    }
    if let Some(i) = base.rfind('/') {
        add(base[i + 1..].to_string(), &mut out, &mut seen);
    }
    out
}

fn normalize_manifest_rel_path(p: &str) -> String {
    go_to_slash(p.trim_matches(|c| c == '/' || c == '\\'))
}

/// stage-rename then move children up (Windows
/// case-insensitive collision safety).
fn flatten_extract_dir(install_dir: &Path, extract_dir: &str) -> Result<(), InstallError> {
    let extract_dir = safepath::validate_manifest_rel_path(extract_dir)
        .map_err(|err| text_phase_error(&err.to_string()))?;
    if extract_dir.is_empty() {
        return Ok(());
    }
    let mut src = install_dir.to_path_buf();
    for seg in extract_dir.split('/') {
        src.push(seg);
    }
    if !src.exists() {
        return Err(text_phase_error(&format!(
            "read extract_dir {extract_dir}: {}",
            src.display()
        )));
    }
    let staging = PathBuf::from(format!("{}.glue-staging", src.display()));
    fs::rename(&src, &staging)
        .map_err(|err| text_phase_error(&format!("stage extract_dir {extract_dir}: {err}")))?;

    let entries = fs::read_dir(&staging)
        .map_err(|err| text_phase_error(&format!("read extract_dir {extract_dir}: {err}")))?;
    for entry in entries {
        let entry = entry.map_err(|err| text_phase_error(&err.to_string()))?;
        let from = entry.path();
        let to = install_dir.join(entry.file_name());
        if to.exists() {
            remove_dir_all(&to).map_err(|err| {
                text_phase_error(&format!(
                    "replace {}: {err}",
                    entry.file_name().to_string_lossy()
                ))
            })?;
        }
        fs::rename(&from, &to).map_err(|err| {
            text_phase_error(&format!(
                "move {}: {err}",
                entry.file_name().to_string_lossy()
            ))
        })?;
    }
    remove_dir_all(&staging).map_err(|err| text_phase_error(&err.to_string()))
}

fn find_extract_dir_parent(install_dir: &Path, extract_dir: &str) -> Option<(PathBuf, String)> {
    for dir in extract_dir_lookup_paths(extract_dir) {
        if let Some(parent) = find_extract_dir_parent_exact(install_dir, &dir) {
            return Some((parent, dir));
        }
    }
    None
}

fn find_extract_dir_parent_exact(install_dir: &Path, extract_dir: &str) -> Option<PathBuf> {
    let extract_dir = normalize_manifest_rel_path(extract_dir);
    if extract_dir.is_empty() || extract_dir == "." {
        return None;
    }
    let direct = install_dir.join(extract_dir.replace('/', std::path::MAIN_SEPARATOR_STR));
    if direct.is_dir() {
        return Some(install_dir.to_path_buf());
    }
    let entries = fs::read_dir(install_dir).ok()?;
    let mut nested: Option<PathBuf> = None;
    for entry in entries.flatten() {
        if !entry.path().is_dir() {
            continue;
        }
        let candidate = entry
            .path()
            .join(extract_dir.replace('/', std::path::MAIN_SEPARATOR_STR));
        if candidate.is_dir() {
            if nested.is_some() {
                return None;
            }
            nested = Some(entry.path());
        }
    }
    nested
}

/// flatten `extract_dir`, incl. under a single
/// top-level wrapper.
fn apply_extract_dir_layout(install_dir: &Path, extract_dir: &str) -> Result<bool, InstallError> {
    let safe = safepath::validate_manifest_rel_path(extract_dir)
        .map_err(|err| text_phase_error(&err.to_string()))?;
    if safe.is_empty() {
        return Ok(false);
    }
    let Some((parent, matched)) = find_extract_dir_parent(install_dir, &safe) else {
        return Ok(false);
    };
    flatten_extract_dir(&parent, &matched)?;
    if parent != install_dir {
        let base = parent
            .file_name()
            .map(|s| s.to_string_lossy().into_owned())
            .unwrap_or_default();
        flatten_extract_dir(install_dir, &base)?;
    }
    Ok(true)
}

// ---------------------------------------------------------------------------
// Bin layout / portable detection
// ---------------------------------------------------------------------------

/// (against the install state's manifest + download).
fn is_portable_exe_install(services: &Services, st: &InstallState) -> bool {
    let _ = services;
    let Some(m) = st.manifest.as_ref() else {
        return false;
    };
    if !go_path_ext(&st.download_name).eq_ignore_ascii_case(".exe") {
        return false;
    }
    if m.has_installer_script_for_install(&st.install_arch) || !m.installer.args.is_empty() {
        return false;
    }
    if m.innosetup {
        return false;
    }
    if !m.get_extract_dir_for_install(&st.install_arch).is_empty() {
        return false;
    }
    if pre_install_extracts_download_blob(m, &st.install_arch) {
        return false;
    }
    let dl_base = go_base(&st.download_name).to_lowercase();
    let bins = m.binaries();
    if bins.is_empty() {
        return true;
    }
    for pattern in &bins {
        let (bin_name, _) = parse_bin_pattern(pattern);
        if bin_name.is_empty() {
            continue;
        }
        if go_base(&bin_name).eq_ignore_ascii_case(&dl_base) {
            return true;
        }
    }
    bins.len() == 1
}

fn is_pre_install_7z_hook_install(st: &InstallState) -> bool {
    let Some(m) = st.manifest.as_ref() else {
        return false;
    };
    if !go_path_ext(&st.download_name).eq_ignore_ascii_case(".exe") {
        return false;
    }
    let hooks = m.pre_install_hooks_for_install(&st.install_arch);
    if hooks.is_empty() {
        return false;
    }
    if is_sevenzip_arm64_sfx_pre_install(&st.pkg_name, &st.install_arch, &hooks) {
        return false;
    }
    let body = hooks.join("\n").to_lowercase();
    if !body.contains("expand-7ziparchive") {
        return false;
    }
    hooks.iter().any(|h| {
        let h = h.to_lowercase();
        h.contains("expand-7ziparchive") && h.contains("$fname")
    })
}

fn pre_install_extracts_download_blob(m: &Manifest, install_arch: &str) -> bool {
    let hooks = m.pre_install_hooks_for_install(install_arch);
    if hooks.is_empty() {
        return false;
    }
    let body = hooks.join("\n").to_lowercase();
    [
        "expand-7ziparchive",
        "expand-msiarchive",
        "expand-innoarchive",
        "expand-darkarchive",
        "msiextract",
    ]
    .iter()
    .any(|marker| body.contains(marker))
}

fn is_sevenzip_arm64_sfx_pre_install(pkg_name: &str, install_arch: &str, hooks: &[String]) -> bool {
    if pkg_name != "7zip" || install_arch != "arm64" || hooks.is_empty() {
        return false;
    }
    let body = hooks.join("\n").to_lowercase();
    body.contains("7zr") && body.contains("invoke-externalcommand")
}

/// handles the arm64 7-Zip SFX layout
/// without running the PowerShell `pre_install` hook (not implemented here).
///
/// Extraction reuses the same extractor path as every other package, so the
/// external `7z.exe` fallback applies exactly as in.
fn apply_sevenzip_arm64_sfx_install(
    services: &Services,
    st: &mut InstallState,
    m: &Manifest,
) -> Result<(), InstallError> {
    if manifest_bin_exists_at_root(&st.install_dir, m) {
        cleanup_sevenzip_sfx_artifacts(&st.install_dir, &st.download_name);
        return Ok(());
    }

    let sfx_path = st.install_dir.join(&st.download_name);
    if !sfx_path.exists() {
        let archive_hash = archive_hash_for_download(&st.installed_files, &st.download_name);
        if archive_hash.is_empty() {
            return Err(text_phase_error(&format!(
                "7zip arm64 installer not found: {}",
                sfx_path.display()
            )));
        }
        materialize_installer_file(services, &archive_hash, &sfx_path)?;
    }

    ensure_seven_zip(services)
        .map_err(|err| text_phase_error(&format!("7zip arm64 sfx: {err}")))?;
    services
        .extractor()
        .extract_to_dir(&sfx_path, &st.install_dir, &st.download_name)
        .map_err(|err| text_phase_error(&format!("extract 7zip arm64 sfx: {err}")))?;

    cleanup_sevenzip_sfx_artifacts(&st.install_dir, &st.download_name);
    normalize_sevenzip_arm64_names(&st.install_dir)?;
    refresh_state_files(services, st);
    if !manifest_bin_exists_at_root(&st.install_dir, m) {
        return Err(text_phase_error(
            "7zip arm64 install incomplete after sfx extract",
        ));
    }
    Ok(())
}

/// Restores the downloaded SFX into the install dir from the CAS blob
fn materialize_installer_file(
    services: &Services,
    archive_hash: &str,
    target: &Path,
) -> Result<(), InstallError> {
    services
        .store()
        .link(archive_hash, target)
        .map_err(|err| text_phase_error(&format!("stage 7zip arm64 sfx: {err}")))?;
    Ok(())
}

/// Removes the SFX leftovers: `Uninstall.exe`, the download itself, and any
/// `*-arm64.exe` installer.
fn cleanup_sevenzip_sfx_artifacts(install_dir: &Path, download_name: &str) {
    let _ = fs::remove_file(install_dir.join("Uninstall.exe"));
    let _ = fs::remove_file(install_dir.join(download_name));
    let Ok(entries) = fs::read_dir(install_dir) else {
        return;
    };
    for entry in entries.flatten() {
        if entry.path().is_dir() {
            continue;
        }
        let name = entry.file_name();
        if name
            .to_string_lossy()
            .to_lowercase()
            .ends_with("-arm64.exe")
        {
            let _ = fs::remove_file(entry.path());
        }
    }
}

/// 7-Zip installer outputs renamed to the names the manifest expects.
///
/// Both the arm64 SFX and the x64/x86 MSI ship underscore-prefixed files
/// inside their archive.
const SEVENZIP_ARM64_RENAMES: &[(&str, &str)] = &[
    ("_7z.exe", "7z.exe"),
    ("_7zG.exe", "7zG.exe"),
    ("_7zFM.exe", "7zFM.exe"),
    ("_7z.dll", "7z.dll"),
    ("_7zip.dll", "7-zip.dll"),
    ("_7zip32.dll", "7zip32.dll"),
    ("_7zip.chm", "7-zip.chm"),
    ("_7-zip.chm", "7-zip.chm"),
];

/// Applies [`SEVENZIP_ARM64_RENAMES`], replacing an existing destination.
fn normalize_sevenzip_arm64_names(install_dir: &Path) -> Result<(), InstallError> {
    for (src, dst) in SEVENZIP_ARM64_RENAMES {
        let src_path = install_dir.join(src);
        if !src_path.exists() {
            continue;
        }
        let dst_path = install_dir.join(dst);
        if dst_path.exists() {
            fs::remove_file(&dst_path)
                .map_err(|err| text_phase_error(&format!("replace 7zip {dst}: {err}")))?;
        }
        fs::rename(&src_path, &dst_path)
            .map_err(|err| text_phase_error(&format!("rename 7zip {src} to {dst}: {err}")))?;
    }
    Ok(())
}

/// Repairs a leftover 7-Zip arm64 layout after the post-install hooks
fn repair_sevenzip_arm64_layout(install_dir: &Path) -> Result<(), InstallError> {
    if !install_dir.join("_7zip.dll").exists() {
        return Ok(());
    }
    normalize_sevenzip_arm64_names(install_dir)
}

/// Reports whether every manifest bin is present at the install root
fn manifest_bin_exists_at_root(install_dir: &Path, m: &Manifest) -> bool {
    for pattern in m.binaries() {
        let (bin_name, _) = parse_bin_pattern(&pattern);
        if bin_name.is_empty() {
            continue;
        }
        if !install_dir.join(&bin_name).exists() {
            return false;
        }
    }
    true
}

/// the CAS hash recorded for the download name.
///
/// Compares of the indexed rel path against the download name,
/// so the extension is kept (does not strip it here).
fn archive_hash_for_download(
    installed_files: &BTreeMap<String, String>,
    download_name: &str,
) -> String {
    installed_files
        .iter()
        .find(|(_, name)| go_file_name(name) == download_name)
        .map(|(hash, _)| hash.clone())
        .unwrap_or_default()
}

/// the last path element, extension included.
fn go_file_name(p: &str) -> String {
    let trimmed = p.trim_end_matches(['/', '\\']);
    trimmed
        .rsplit(['/', '\\'])
        .next()
        .unwrap_or(trimmed)
        .to_string()
}

fn msi_needs_administrative_extract(st: &InstallState) -> bool {
    st.manifest
        .as_ref()
        .map(|m| !m.get_extract_dir_for_install(&st.install_arch).is_empty())
        .unwrap_or(false)
}

/// against the install state.
fn manifest_extract_dir(st: &InstallState) -> String {
    st.manifest
        .as_ref()
        .map(|m| m.get_extract_dir_for_install(&st.install_arch))
        .unwrap_or_default()
}

/// Temp dir for MSI extraction.
fn msi_temp_dir(_services: &Services, download_name: &str) -> PathBuf {
    std::env::temp_dir().join(format!("glue-msi-{}-{download_name}", std::process::id()))
}

fn archive_member_index_ready(services: &Services, archive_hash: &str) -> bool {
    if archive_hash.is_empty() {
        return false;
    }
    services
        .downloader()
        .resolve_zip_member_index(archive_hash)
        .is_some()
}

fn validate_install_dir(install_dir: &Path) -> Result<(), InstallError> {
    if install_dir_is_empty(install_dir) {
        let pkg = install_dir
            .parent()
            .and_then(|p| p.file_name())
            .map(|s| s.to_string_lossy().into_owned())
            .unwrap_or_default();
        return Err(text_phase_error(&format!(
            "install directory is empty after cache install (try glue install --force {pkg})"
        )));
    }
    Ok(())
}

fn ensure_extract_dir_flattened(
    install_dir: &Path,
    m: &Manifest,
    install_arch: &str,
) -> Result<bool, InstallError> {
    if install_bin_layout_ready(install_dir, m) {
        return Ok(false);
    }
    let mut flattened = false;

    let extract_dir = m.get_extract_dir_for_install(install_arch);
    if !extract_dir.is_empty() {
        let applied = apply_extract_dir_layout(install_dir, &extract_dir)?;
        if applied {
            flattened = true;
            if install_bin_layout_ready(install_dir, m) {
                return Ok(flattened);
            }
        }
    }

    for _ in 0..4 {
        if install_bin_layout_ready(install_dir, m) {
            return Ok(flattened);
        }
        let Some(nested) = nested_dir_with_manifest_bin_layout(install_dir, m) else {
            return Ok(flattened);
        };
        flatten_extract_dir(install_dir, &nested)?;
        flattened = true;
    }
    Ok(flattened)
}

fn install_bin_layout_ready(install_dir: &Path, m: &Manifest) -> bool {
    for pattern in m.binaries() {
        let (bin_name, _) = parse_bin_pattern(&pattern);
        if bin_name.is_empty() {
            continue;
        }
        let found = install_root_bin_candidates(install_dir, &bin_name, "")
            .iter()
            .any(|c| c.exists());
        if !found {
            return false;
        }
    }
    true
}

fn nested_dir_with_manifest_bin_layout(install_dir: &Path, m: &Manifest) -> Option<String> {
    let entries = fs::read_dir(install_dir).ok()?;
    let mut matches: Vec<String> = Vec::new();
    for entry in entries.flatten() {
        if !entry.path().is_dir() {
            continue;
        }
        let name = entry.file_name().to_string_lossy().into_owned();
        if should_skip_portable_scan_dir(&name) {
            continue;
        }
        if install_bin_layout_ready(&entry.path(), m) {
            matches.push(name);
        }
    }
    if matches.len() != 1 {
        return None;
    }
    Some(matches.remove(0))
}

fn validate_manifest_bins(install_dir: &Path, m: &Manifest) -> Result<(), InstallError> {
    for pattern in m.binaries() {
        let (bin_name, _) = parse_bin_pattern(&pattern);
        if bin_name.is_empty() {
            continue;
        }
        if resolve_installed_bin_path(install_dir, &bin_name, "").is_none() {
            let tree = install_tree_summary(install_dir);
            return Err(text_phase_error(&format!(
                "expected binary missing after install: {bin_name} (installer may be incomplete; try force reinstall){tree}"
            )));
        }
    }
    Ok(())
}

/// What the install directory actually holds, appended to the missing-binary
/// error.
///
/// A bare "expected binary missing" cannot tell a flat tree with one absent
/// file from a tree that was never extracted or is still wrapped in its
/// archive folder, and those need different fixes. The summary is bounded so
/// the message stays readable in a dialog.
fn install_tree_summary(install_dir: &Path) -> String {
    const SHOWN: usize = 8;
    let Ok(entries) = fs::read_dir(install_dir) else {
        return "; install dir could not be read".to_owned();
    };
    let mut names: Vec<String> = Vec::new();
    for entry in entries.flatten() {
        let name = entry.file_name().to_string_lossy().into_owned();
        let is_dir = entry.path().is_dir();
        let name = if is_dir { format!("{name}/") } else { name };
        names.push(name);
        if names.len() >= SHOWN {
            break;
        }
    }
    if names.is_empty() {
        return "; install dir is empty".to_owned();
    }
    names.sort();
    format!("; install dir holds: {}", names.join(", "))
}

/// fall back to the single top-level `.exe`.
fn resolve_installed_bin_path(
    install_dir: &Path,
    bin_name: &str,
    download_name: &str,
) -> Option<(PathBuf, String)> {
    for candidate in resolve_bin_candidates(install_dir, bin_name, download_name) {
        if candidate.exists() {
            let rel = candidate
                .strip_prefix(install_dir)
                .map(|p| p.to_string_lossy().into_owned())
                .unwrap_or_else(|_| go_base(&candidate.to_string_lossy()));
            return Some((candidate, rel));
        }
    }
    let entries = fs::read_dir(install_dir).ok()?;
    let mut exes: Vec<String> = Vec::new();
    for entry in entries.flatten() {
        if entry.path().is_dir() {
            continue;
        }
        let name = entry.file_name().to_string_lossy().into_owned();
        if go_path_ext(&name).eq_ignore_ascii_case(".exe") {
            exes.push(name);
        }
    }
    if exes.len() == 1 {
        let rel = exes.remove(0);
        return Some((install_dir.join(&rel), rel));
    }
    None
}

// ---------------------------------------------------------------------------
// shim/bin.go helpers (no shim module in this implementation)
// ---------------------------------------------------------------------------

/// `(exe, alias, extraArgs)`.
///
/// The alias is `parts[1]` when present; everything from `parts[2]` on becomes
/// the shim's default arguments. The first element doubles as the exe, so a
/// two-element form is `exe,alias` (not `exe,arg`).
/// `(exe, alias, extraArgs)`.
///
/// Canonical shared parser: `engine::ops` delegates here instead of
/// keeping its own copy (the copies had diverged from parity).
pub(crate) fn parse_bin_pattern_full(pattern: &str) -> (String, String, Vec<String>) {
    let trimmed = pattern.trim();
    if trimmed.is_empty() {
        return (String::new(), String::new(), Vec::new());
    }
    let parts: Vec<String> = if trimmed.starts_with('[') {
        let inner = trimmed.trim_matches(['[', ']']);
        inner.split(',').map(trim_bin_token).collect()
    } else if trimmed.contains(',') {
        trimmed.split(',').map(trim_bin_token).collect()
    } else {
        return (trim_bin_token(trimmed), String::new(), Vec::new());
    };
    if parts.is_empty() || parts[0].is_empty() {
        return (String::new(), String::new(), Vec::new());
    }
    let exe = parts[0].clone();
    let alias = parts.get(1).cloned().unwrap_or_default();
    let extra_args = if parts.len() > 2 {
        parts[2..].to_vec()
    } else {
        Vec::new()
    };
    (exe, alias, extra_args)
}

/// `(exe, args)`.
fn parse_bin_pattern(pattern: &str) -> (String, Vec<String>) {
    let (name, args) = parse_bin_pattern_parts(pattern);
    (trim_bin_token(&name), args)
}

fn parse_bin_pattern_parts(pattern: &str) -> (String, Vec<String>) {
    let trimmed = pattern.trim();
    if trimmed.starts_with('[') && trimmed.ends_with(']') {
        let inner = &trimmed[1..trimmed.len() - 1];
        let parts: Vec<String> = inner.split(',').map(trim_bin_token).collect();
        if !parts.is_empty() {
            let name = parts[0].clone();
            let args = parts.into_iter().skip(1).collect();
            return (name, args);
        }
        return (String::new(), Vec::new());
    }
    let parts: Vec<String> = trimmed.split(',').map(trim_bin_token).collect();
    if parts.is_empty() {
        return (String::new(), Vec::new());
    }
    let name = parts[0].clone();
    let args = parts.into_iter().skip(1).collect();
    (name, args)
}

/// trim whitespace and paired quotes.
fn trim_bin_token(value: &str) -> String {
    let mut v = value.trim().to_string();
    if v.len() >= 2 {
        let bytes = v.as_bytes();
        let first = bytes[0];
        let last = bytes[v.len() - 1];
        if (first == b'"' && last == b'"') || (first == b'\'' && last == b'\'') {
            v = v[1..v.len() - 1].to_string();
        }
    }
    v.trim().to_string()
}

// ---------------------------------------------------------------------------
// install_shim.go — PATH shims for manifest bins and env_add_path dirs
// ---------------------------------------------------------------------------

/// Collects shim creation for one package, remembering only the first failure
/// so remaining entries are still attempted.
struct ShimCreator<'a> {
    manager: &'a crate::shim::Manager,
    shims_meta_dir: PathBuf,
    pkg_name: String,
    shim_env: BTreeMap<String, String>,
    first_err: Option<InstallError>,
    /// Shim names created during this pass, for progress reporting.
    created: Vec<String>,
}

impl ShimCreator<'_> {
    /// Records the first error only (later failures do not overwrite it).
    fn record_err(&mut self, err: InstallError) {
        if self.first_err.is_none() {
            self.first_err = Some(err);
        }
    }

    /// Creates one shim unless another package already owns the name.
    ///
    /// The shims directory is a flat namespace, so a name provided by a
    /// different package is skipped rather than replaced.
    fn try_create(&mut self, shim_name: &str, shim_target: &Path, label: &str) {
        if let Some(owner) =
            crate::shim::owned_by_other_package(&self.shims_meta_dir, shim_name, &self.pkg_name)
        {
            emit_shim_skipped(shim_name, &owner);
            return;
        }
        let opts = crate::shim::CreateOpts {
            args: Vec::new(),
            env: self.shim_env.clone(),
        };
        match self
            .manager
            .create(shim_name, &shim_target.to_string_lossy(), &opts)
        {
            Ok(()) => {
                emit_shim_created(shim_name, label);
                self.created.push(shim_name.to_string());
            }
            Err(err) => {
                let err = format!("shim {shim_name}: {err}");
                self.record_err(text_phase_error(&err));
            }
        }
    }
}

/// Creates PATH shims for `m`.
///
/// `shim_dir` empty falls back to `install_dir`
pub(crate) fn create_package_shims(
    manager: &crate::shim::Manager,
    shims_meta_dir: &Path,
    pkg_name: &str,
    install_dir: &Path,
    shim_dir: Option<&Path>,
    m: &Manifest,
    install_arch: &str,
) -> Result<Vec<String>, InstallError> {
    let owned;
    let shim_dir = match shim_dir {
        Some(dir) => dir,
        None => {
            owned = install_dir.to_path_buf();
            &owned
        }
    };
    let mut creator = ShimCreator {
        manager,
        shims_meta_dir: shims_meta_dir.to_path_buf(),
        pkg_name: pkg_name.to_string(),
        shim_env: crate::winshell::shim_env_for_package(
            m,
            &install_dir.to_string_lossy(),
            install_arch,
        ),
        first_err: None,
        created: Vec::new(),
    };
    create_manifest_bin_shims(&mut creator, install_dir, shim_dir, m);
    create_env_add_path_shims(&mut creator, install_dir, shim_dir, m);
    match creator.first_err {
        Some(err) => Err(err),
        None => Ok(creator.created),
    }
}

/// Creates shims for the manifest's `bin` entries.
fn create_manifest_bin_shims(
    creator: &mut ShimCreator<'_>,
    install_dir: &Path,
    shim_dir: &Path,
    m: &Manifest,
) {
    let bins = m.binaries();
    if bins.is_empty() {
        return;
    }
    for bin_pattern in &bins {
        let (bin_name, alias, _args) = parse_bin_pattern_full(bin_pattern);
        if bin_name.is_empty() {
            continue;
        }
        let rel_bin = bin_rel_path(install_dir, &bin_name);
        creator.try_create(
            &shim_name_for_bin(&bin_name, &alias),
            &shim_dir.join(&rel_bin),
            &bin_name,
        );
    }
}

/// Absolute paths of the programs a manifest declares as `bin` entries.
///
/// The launch menu uses this: a package's `env_add_path` directories can hold
/// dozens of helper executables (`python`'s `Scripts\pip.exe`, `f2py.exe`, …)
/// whose shims we create for `PATH`, but they are not the program a user opens.
pub(crate) fn manifest_bin_targets(install_dir: &Path, m: &Manifest) -> Vec<PathBuf> {
    m.launch_binaries()
        .iter()
        .filter_map(|pattern| {
            let (bin_name, _alias, _args) = parse_bin_pattern_full(pattern);
            if bin_name.is_empty() {
                return None;
            }
            let rel = bin_rel_path(install_dir, &bin_name);
            Some(install_dir.join(rel.replace('\\', "/")))
        })
        .collect()
}

/// Resolves a manifest bin entry to its path relative to the install dir.
///
/// Prefers the resolved on-disk location, then the Scoop `_` mirror, then the
/// raw entry.
fn bin_rel_path(install_dir: &Path, bin_name: &str) -> String {
    if let Some((_, rel)) = resolve_installed_bin_path(install_dir, bin_name, "") {
        return rel;
    }
    // Only probe the `_` mirror when the plain entry is genuinely absent.
    if !install_dir.join(bin_name).exists() && install_dir.join(format!("_{bin_name}")).exists() {
        return format!("_{bin_name}");
    }
    bin_name.to_string()
}

/// Creates a shim for every executable under the `env_add_path` directories.
fn create_env_add_path_shims(
    creator: &mut ShimCreator<'_>,
    install_dir: &Path,
    shim_dir: &Path,
    m: &Manifest,
) {
    for dir_path in m.env_add_paths() {
        let scan_dir = if dir_path == "." {
            install_dir.to_path_buf()
        } else {
            install_dir.join(dir_path.replace('/', "\\"))
        };
        let entries = match fs::read_dir(&scan_dir) {
            Ok(entries) => entries,
            Err(err) if err.kind() == io::ErrorKind::NotFound => continue,
            Err(err) => {
                creator.record_err(text_phase_error(&format!(
                    "read {}: {err}",
                    scan_dir.display()
                )));
                continue;
            }
        };
        create_shims_from_dir_entries(creator, entries, &scan_dir, install_dir, shim_dir);
    }
}

/// Creates a shim for each `.exe`/`.cmd` file of one scan pass.
fn create_shims_from_dir_entries(
    creator: &mut ShimCreator<'_>,
    entries: fs::ReadDir,
    _scan_dir: &Path,
    install_dir: &Path,
    shim_dir: &Path,
) {
    for entry in entries.flatten() {
        let path = entry.path();
        if path.is_dir() {
            continue;
        }
        let name = entry.file_name().to_string_lossy().into_owned();
        let lower = name.to_lowercase();
        if !lower.ends_with(".exe") && !lower.ends_with(".cmd") {
            continue;
        }
        let rel = path
            .strip_prefix(install_dir)
            .map(|p| p.to_string_lossy().into_owned())
            .unwrap_or_else(|_| name.clone());
        creator.try_create(&env_add_path_shim_name(&name), &shim_dir.join(&rel), &name);
    }
}

/// Strips the executable extension from a scanned file name
fn env_add_path_shim_name(name: &str) -> String {
    let without = name
        .strip_suffix(".exe")
        .or_else(|| name.strip_suffix(".cmd"))
        .or_else(|| name.strip_suffix(".EXE"))
        .or_else(|| name.strip_suffix(".CMD"))
        .unwrap_or(name);
    without.to_string()
}

/// The PATH command name for a manifest bin entry.
fn shim_name_for_bin(exe_path: &str, alias: &str) -> String {
    let normalized = normalize_shim_alias(alias);
    if !normalized.is_empty() {
        return normalized;
    }
    strip_shim_extensions(&go_base(exe_path))
}

/// Trims whitespace and a trailing executable extension from an alias
/// `""` when nothing remains.
fn normalize_shim_alias(alias: &str) -> String {
    let trimmed = alias.trim();
    if trimmed.is_empty() {
        return String::new();
    }
    strip_shim_extensions(trimmed)
}

/// Strips any of the known executable extensions, covering upper case too.
fn strip_shim_extensions(name: &str) -> String {
    let mut out = name.to_string();
    for ext in [".exe", ".bat", ".cmd", ".sh", ".EXE", ".BAT", ".CMD", ".SH"] {
        if let Some(stripped) = out.strip_suffix(ext) {
            out = stripped.to_string();
        }
    }
    out
}

/// the CAS hash recorded for the download name.
/// Reports a skipped shim (name already owned by another package).
fn emit_shim_skipped(shim_name: &str, owner: &str) {
    let _ = (shim_name, owner);
}

/// Reports a created shim.
fn emit_shim_created(shim_name: &str, label: &str) {
    let _ = (shim_name, label);
}

fn should_skip_portable_scan_dir(name: &str) -> bool {
    matches!(name, "node_modules" | ".git" | "__pycache__") || name.starts_with('.')
}

fn install_root_bin_candidates(
    install_dir: &Path,
    bin_name: &str,
    source_dir: &str,
) -> Vec<PathBuf> {
    let mut candidates: Vec<PathBuf> = Vec::new();
    let push = |p: PathBuf, candidates: &mut Vec<PathBuf>| {
        if p.exists() && !candidates.contains(&p) {
            candidates.push(p);
        }
    };
    push(install_dir.join(bin_name), &mut candidates);
    push(install_dir.join(format!("_{bin_name}")), &mut candidates);
    if !source_dir.is_empty() {
        push(
            install_dir
                .join(source_dir.replace('/', std::path::MAIN_SEPARATOR_STR))
                .join(bin_name),
            &mut candidates,
        );
    }
    candidates
}

/// plus one level of subdirectory probing.
fn resolve_bin_candidates(install_dir: &Path, bin_name: &str, download_name: &str) -> Vec<PathBuf> {
    let mut candidates = install_root_bin_candidates(install_dir, bin_name, "");
    if !download_name.is_empty() {
        let dl = install_dir.join(download_name);
        if !candidates.contains(&dl) {
            candidates.push(dl);
        }
    }
    if let Ok(entries) = fs::read_dir(install_dir) {
        for entry in entries.flatten() {
            if !entry.path().is_dir() {
                continue;
            }
            let sub = entry.path();
            let direct = sub.join(bin_name);
            if !candidates.contains(&direct) {
                candidates.push(direct);
            }
            let underscored = sub.join(format!("_{bin_name}"));
            if !candidates.contains(&underscored) {
                candidates.push(underscored);
            }
        }
    }
    candidates
}

// ---------------------------------------------------------------------------
// Finalize phase (in-scope subset)
// ---------------------------------------------------------------------------

/// minus the gaps documented at module level:
/// persist restore, hooks, installer scripts, install records, shims,
/// shortcuts and env_set are skipped. The in-scope steps are:
/// extract_dir flatten + bin validation, `current` linking, and the cache
/// index update.
fn finalize_package_install(
    services: &Services,
    st: &mut InstallState,
    emit: &(dyn Fn(ProgressEvent) + Sync),
) -> Result<(), InstallError> {
    let Some(m) = st.manifest.clone() else {
        return Err(text_phase_error(&format!(
            "manifest is nil for {} (internal error)",
            st.pkg_ref
        )));
    };
    let root = services.root_dir().to_path_buf();
    let install_dir = st.install_dir.clone();
    let install_arch = st.install_arch.clone();
    let pkg_name = st.pkg_name.clone();
    let download_name = st.download_name.clone();
    let file_ext = st.file_ext.clone();
    let persist_dir = root.join("persist").join(&pkg_name);
    // Files linked/adopted into the install dir before finalize; install.json
    // is written below, so it is not part of this count
    // The downloaded archive is indexed against the
    // package too, but it is not a file of the install tree — leave it out so
    // this matches the link-progress total.
    st.linked_files = st
        .installed_files
        .iter()
        .filter(|(hash, rel)| {
            hash.as_str() != st.archive_hash.as_str() || rel.as_str() != download_name.as_str()
        })
        .count();
    // Registered by `resolve_full_local_7z` at engine construction
    // empty means hooks get no `Expand-7zipArchive` helper.
    // Dark/innounp are resolved lazily per hook run.
    let _ = services.root_dir();

    // restorePersistData: link/copy data from `persist/<pkg>` into the new
    // install dir for each entry whose install target does not yet exist.
    if !m.persist_entries_for_install(&install_arch).is_empty()
        && !is_exe_installer_script_install(&file_ext, &m, &install_arch)
    {
        // `PrepareInstallDirForPreInstallHooks`: clear stale placeholders a
        // failed install left behind so Scoop `New-Item` calls can run again.
        prepare_install_dir_for_pre_install_hooks(&install_dir, &persist_dir, &m, &install_arch)?;
        restore_persist_data(&install_dir, &persist_dir, &m, &install_arch)?;
    }
    // runPreInstallHooks
    run_pre_install_hooks(services, st, &m, &persist_dir)?;
    // runInstallerScript. The `.exe` + `installer.script` layout is included:
    // deploy put `$fname` in the install dir and the script is what turns it
    // into the package (PowerToys, VS Build Tools).
    if m.has_installer_script_for_install(&install_arch) {
        let installer_hooks = m.installer_script_hooks_for_install(&install_arch);
        let helpers = hook_helper_paths(services, &installer_hooks, &pkg_name, &mut st.timings)?;
        let env = HookScriptEnv {
            install_dir: &install_dir,
            download_name: &download_name,
            version: &m.version,
            app: &pkg_name,
            bucket: &crate::catalog::package_bucket_name(&st.pkg_ref),
            buckets_dir: &root.join("buckets"),
            persist_dir: &persist_dir,
            arch: &install_arch,
            glue_root: &root,
            seven_zip: &helpers.seven_zip,
            dark: helpers.dark.as_deref().unwrap_or(""),
            innounp: helpers.innounp.as_deref().unwrap_or(""),
            interactive: st.interactive,
            cmd: "",
            hooks: &installer_hooks,
        };
        run_installer_script(&m, &install_arch, &env)?;
        if is_exe_installer_script_install(&file_ext, &m, &install_arch) {
            // The script populated the tree (and usually deleted `$fname`),
            // while the deploy phase only knew about the downloaded exe — the
            // cache index would otherwise list a file that no longer exists
            // and miss everything the script unpacked.
            refresh_state_files(services, st);
        }
    }

    // persist_data: `.exe` + `installer.script` packages skip the pre-hook
    // restore above and their installers write straight into `$persist_dir`
    // (rustup-msvc sets CARGO_HOME/RUSTUP_HOME there). Link those directories
    // into the install tree once the script has run, or `env_add_path`
    // targets such as `.cargo\bin` resolve to a path that does not exist.
    if is_exe_installer_script_install(&file_ext, &m, &install_arch)
        && !m.persist_entries_for_install(&install_arch).is_empty()
    {
        restore_persist_data(&install_dir, &persist_dir, &m, &install_arch)?;
    }

    // flattenAndValidateBins
    let flattened = ensure_extract_dir_flattened(&install_dir, &m, &install_arch)?;
    if flattened {
        refresh_state_files(services, st);
    }
    validate_manifest_bins(&install_dir, &m)?;

    // runPostInstallHooks: moveFlattenIfNeeded → post_install → refresh →
    // 7zip arm64 layout repair.
    move_flatten_if_needed(services, st, &m)?;
    if !m.post_install_hooks_for_install(&install_arch).is_empty() {
        let post_hooks = m.post_install_hooks_for_install(&install_arch);
        // `PrepareInstallDirForPostInstallHooks`: persist install paths must
        // be directories, not stale files, when no data is stored yet.
        prepare_install_dir_for_post_install_hooks(&install_dir, &persist_dir, &m, &install_arch)?;
        let helpers = hook_helper_paths(services, &post_hooks, &pkg_name, &mut st.timings)?;
        let env = HookScriptEnv {
            install_dir: &install_dir,
            download_name: &download_name,
            version: &m.version,
            app: &pkg_name,
            bucket: &crate::catalog::package_bucket_name(&st.pkg_ref),
            buckets_dir: &root.join("buckets"),
            persist_dir: &persist_dir,
            arch: &install_arch,
            glue_root: &root,
            seven_zip: &helpers.seven_zip,
            dark: helpers.dark.as_deref().unwrap_or(""),
            innounp: helpers.innounp.as_deref().unwrap_or(""),
            interactive: st.interactive,
            cmd: "",
            hooks: &post_hooks,
        };
        run_powershell_hooks("post_install", &env)?;
        // refreshes the index when hooks touch files
        // (zoom's post_install deletes `Install*` and registers the protocol).
        refresh_state_files(services, st);
    }
    repair_sevenzip_arm64_layout(&install_dir)?;

    // linkCurrentAndSaveRecord
    emit(ProgressEvent::Linking {
        package: pkg_name.clone(),
        version: m.version.clone(),
    });
    let bucket = crate::catalog::package_bucket_name(&st.pkg_ref);
    apps::link_current(&apps::pkg_root(&root, &pkg_name), &m.version)
        .map_err(|err| text_phase_error(&format!("link current: {err}")))?;
    save_install_record_for(&install_dir, &bucket, &m, st)?;

    // createShimsAndShortcuts
    emit(ProgressEvent::CreatingShims {
        package: pkg_name.clone(),
    });
    let shim_timing = st.timings.start();
    st.created_shims =
        create_shims_and_shortcuts(services, &m, &install_dir, &pkg_name, &install_arch)?;
    st.timings.add(Bucket::Shim, shim_timing);

    // updateCacheIndex (the archive release is cache work too)
    emit(ProgressEvent::Persisting {
        package: pkg_name.clone(),
    });
    let cache_timing = st.timings.start();
    release_downloaded_archives(services, st);
    services
        .cache()
        .add(&pkg_name, &m.version, &st.installed_files, st.total_size)
        .map_err(|err| text_phase_error(&format!("update cache index: {err}")))?;
    st.timings.add(Bucket::Cache, cache_timing);
    let metadata = preserve_installed_metadata(services, &pkg_name);
    services
        .cache()
        .add_installed(
            &pkg_name,
            &m.version,
            &install_dir.to_string_lossy(),
            st.total_size,
            &metadata,
        )
        .map_err(|err| text_phase_error(&format!("update cache index: {err}")))?;

    let _ = &download_name;
    st.done = true;
    st.install_succeeded = true;
    Ok(())
}

/// Drop the downloaded archive blobs once their contents live in the store and
/// are hardlinked into the install tree.
///
/// The archive is not a file of the install tree (except for direct-file
/// installs such as portable exes and multi-artifact bundles, where it is
/// skipped), and keeping it doubles the on-disk footprint of archive installs.
/// The cache entry written afterwards lists only the extracted members, so a
/// reinstall still links them from the store without downloading again; once
/// the entry is gone, the next install downloads the archive again.
///
/// Shared blobs are kept: another package's legacy cache entry may still point
/// at the same archive.
fn release_downloaded_archives(services: &Services, st: &mut InstallState) {
    let mut candidates: BTreeMap<String, String> = BTreeMap::new();
    for result in &st.download_results {
        let name = result.task.filename.trim();
        if !result.hash.is_empty() && !name.is_empty() {
            candidates.insert(result.hash.clone(), name.to_string());
        }
    }
    if !st.archive_hash.is_empty() && !st.download_name.is_empty() {
        candidates.insert(st.archive_hash.clone(), st.download_name.clone());
    }

    for (hash, name) in candidates {
        // Direct-file and multi-artifact installs link the artifact itself
        // into the tree; its blob is the installed file.
        if st.install_dir.join(&name).exists() {
            continue;
        }
        if !services
            .cache()
            .packages_sharing_hashes(std::slice::from_ref(&hash), &st.pkg_name)
            .is_empty()
        {
            continue;
        }
        let object_size = fs::metadata(services.store().object_path(&hash))
            .map(|info| info.len() as i64)
            .unwrap_or(0);
        if let Err(err) = services.store().delete(&hash) {
            eprintln!("warning: could not release archive {hash}: {err}");
            continue;
        }
        crate::zip::remove(services.store().path(), &hash);
        if st.installed_files.remove(&hash).is_some() {
            st.total_size = (st.total_size - object_size).max(0);
        }
    }
}

/// link (junction) directory persist
/// entries to `persist/<pkg>`, copy file entries, and ensure empty directories
/// when nothing is stored yet.
///
/// The whole implementation lives in [`crate::engine::persist`]; this wrapper
/// only maps its errors into the install-pipeline error type.
fn restore_persist_data(
    install_dir: &Path,
    persist_dir: &Path,
    m: &Manifest,
    install_arch: &str,
) -> Result<(), InstallError> {
    let entries = m.persist_entries_for_install(install_arch);
    if entries.is_empty() {
        return Ok(());
    }
    crate::engine::persist::restore_persist_on_install(install_dir, persist_dir, &entries)
        .map_err(|err| text_phase_error(&err.to_string()))
}

fn prepare_install_dir_for_pre_install_hooks(
    install_dir: &Path,
    persist_dir: &Path,
    m: &Manifest,
    install_arch: &str,
) -> Result<(), InstallError> {
    let entries = m.persist_entries_for_install(install_arch);
    if entries.is_empty() {
        return Ok(());
    }
    crate::engine::persist::prepare_install_dir_for_pre_install_hooks(
        install_dir,
        persist_dir,
        &entries,
    )
    .map_err(|err| text_phase_error(&err.to_string()))
}

fn prepare_install_dir_for_post_install_hooks(
    install_dir: &Path,
    persist_dir: &Path,
    m: &Manifest,
    install_arch: &str,
) -> Result<(), InstallError> {
    let entries = m.persist_entries_for_install(install_arch);
    if entries.is_empty() {
        return Ok(());
    }
    crate::engine::persist::prepare_install_dir_for_post_install_hooks(
        install_dir,
        persist_dir,
        &entries,
    )
    .map_err(|err| text_phase_error(&err.to_string()))
}

/// into the live [`InstallState`].
fn refresh_state_files(services: &Services, st: &mut InstallState) {
    let (files, size) = refresh_installed_files(services, &st.install_dir);
    st.installed_files = files;
    st.total_size = size;
}

/// executes the manifest's `installer.script` via
/// PowerShell with the full Scoop helper set and env vars
///
/// `env.hooks` must already hold the installer-script hooks. The need
/// pre-checks7z is always required;
/// innounp/dark only when the hooks reference `Expand-InnoArchive`/
/// `Expand-DarkArchive` — and their failure text is -verbatim.
fn run_installer_script(
    m: &Manifest,
    install_arch: &str,
    env: &HookScriptEnv,
) -> Result<(), InstallError> {
    let hooks = m.installer_script_hooks_for_install(install_arch);
    if hooks.is_empty() {
        return Ok(());
    }
    if env.seven_zip.is_empty() {
        return Err(text_phase_error(
            "installer.script requires 7z.exe (install 7zip first)",
        ));
    }
    if installer_script_needs_innounp(env.hooks) && env.innounp.is_empty() {
        return Err(text_phase_error(
            "innounp.exe not found\n\nInno Setup installer scripts need innounp in glue/bin",
        ));
    }
    if hook_helpers_need_dark(env.hooks) && env.dark.is_empty() {
        return Err(text_phase_error(
            "dark.exe or wix.exe not found\n\nWiX Burn installer scripts need dark or wix in glue/bin",
        ));
    }

    let script = build_hook_script(env);
    // Hooks and installer scripts edit their own install tree. A file that
    // arrived read-only (a `.portable` marker inside the archive, an MSIX
    // payload) makes those writes fail with ACCESS_DENIED, e.g. Windows
    // Terminal's `Add-Content "$dir\.portable"` when the marker already exists.
    clear_readonly_recursive(env.install_dir);

    // Write to a temp .ps1 file and execute with PowerShell.
    let tmp = std::env::temp_dir().join(format!("glue-installer-{}.ps1", env.app));
    fs::write(&tmp, script)
        .map_err(|err| text_phase_error(&format!("write installer script: {err}")))?;
    let result = run_powershell_file(&tmp);
    let _ = fs::remove_file(&tmp);
    result.map_err(|err| text_phase_error(&format!("installer script for {}: {err}", env.app)))?;
    Ok(())
}

/// Executes a PowerShell script file, hiding the console window on Windows.
fn run_powershell_file(script_path: &Path) -> Result<(), String> {
    let mut cmd = Command::new("powershell.exe");
    cmd.args([
        "-NoProfile",
        "-NonInteractive",
        "-ExecutionPolicy",
        "Bypass",
        "-File",
        script_path.to_str().ok_or("invalid script path")?,
    ]);
    crate::procutil::hide_window(&mut cmd);
    let out = cmd
        .output()
        .map_err(|err| format!("run powershell: {err}"))?;
    if !out.status.success() {
        let mut msg = String::from_utf8_lossy(&out.stdout).into_owned();
        msg.push_str(&String::from_utf8_lossy(&out.stderr));
        return Err(msg.trim().to_string());
    }
    Ok(())
}

/// the `.exe` + `installer.script` layout
/// manages its own files and skips most finalize side effects.
fn is_exe_installer_script_install(file_ext: &str, m: &Manifest, install_arch: &str) -> bool {
    file_ext == ".exe" && m.has_installer_script_for_install(install_arch)
}

///
/// Handles the Scoop move-flatten manifests and the 7-Zip arm64 SFX layout
/// repair; anything else is a `pre_install` hook script (not implemented here).
fn run_pre_install_hooks(
    services: &Services,
    st: &mut InstallState,
    m: &Manifest,
    persist_dir: &Path,
) -> Result<(), InstallError> {
    if is_exe_installer_script_install(&st.file_ext, m, &st.install_arch) {
        return Ok(());
    }
    let mut hooks = m.pre_install_hooks_for_install(&st.install_arch);
    if is_scoop_move_flatten_pre_install(&hooks) {
        ensure_scoop_move_flatten_install_dir(&st.install_dir, m)?;
        hooks.clear();
    }
    if is_sevenzip_arm64_sfx_pre_install(&st.pkg_name, &st.install_arch, &hooks) {
        return apply_sevenzip_arm64_sfx_install(services, st, m);
    }
    if !hooks.is_empty() {
        // `ResolveHookHelpers`: 7z from the extractor (resolved at engine
        // construction), dark/innounp resolved — and bootstrapped if missing.
        let helpers = hook_helper_paths(services, &hooks, &st.pkg_name, &mut st.timings)?;
        let root = services.root_dir().to_path_buf();
        let env = HookScriptEnv {
            install_dir: &st.install_dir,
            download_name: &st.download_name,
            version: &m.version,
            app: &st.pkg_name,
            bucket: &crate::catalog::package_bucket_name(&st.pkg_ref),
            buckets_dir: &root.join("buckets"),
            persist_dir,
            arch: &st.install_arch,
            glue_root: &root,
            seven_zip: &helpers.seven_zip,
            dark: helpers.dark.as_deref().unwrap_or(""),
            innounp: helpers.innounp.as_deref().unwrap_or(""),
            interactive: st.interactive,
            cmd: "",
            hooks: &hooks,
        };
        // fails fast when a needed 7z is missing.
        if hook_helpers_need_7z(&hooks) && env.seven_zip.is_empty() {
            return Err(text_phase_error(
                "hook requires 7z.exe (install 7zip first)",
            ));
        }
        run_powershell_hooks("pre_install", &env)?;
        // re-indexes after the hooks ran: an
        // `Expand-7zipArchive` pre_install can unpack a 180 MB `.cab` into
        // hundreds of files, and the recorded file count/size must match.
        refresh_state_files(services, st);
    }
    Ok(())
}

/// Executes a list of PowerShell hook lines with Scoop-style env vars injected.
/// (in-scope subset): the helper functions
/// Scoop/glue manifests call from `pre_install`/`post_install` hooks.
///
/// `Expand-7zipArchive` is the one hooks reference most (`#/dl.7z` SFX
/// packages like zoom unpack a `.cab` with it); it needs a registered 7z
/// path, so it is injected only when `seven_zip` is non-empty — exactly like
/// which skips the helper when `ResolveHookHelpers` finds nothing.
fn scoop_hook_helper_preamble(
    seven_zip: &str,
    dark: &str,
    innounp: &str,
    glue_root: &Path,
) -> String {
    let mut preamble = String::from(
        r#"function Publish-EnvVar {
  if (-not ('Win32.NativeMethods' -as [Type])) {
    Add-Type -Namespace Win32 -Name NativeMethods -MemberDefinition @'
[DllImport("user32.dll", SetLastError = true, CharSet = CharSet.Auto)]
public static extern IntPtr SendMessageTimeout(
  IntPtr hWnd, uint Msg, UIntPtr wParam, string lParam,
  uint fuFlags, uint uTimeout, out UIntPtr lpdwResult
);
'@
  }
  $HWND_BROADCAST = [IntPtr] 0xffff
  $WM_SETTINGCHANGE = 0x1a
  $result = [UIntPtr]::Zero
  [Win32.NativeMethods]::SendMessageTimeout($HWND_BROADCAST,
    $WM_SETTINGCHANGE,
    [UIntPtr]::Zero,
    'Environment',
    2,
    5000,
    [ref] $result
  ) | Out-Null
}
function Get-EnvVar {
  param(
    [string]$Name,
    [switch]$Global
  )
  $registerKey = if ($Global) {
    Get-Item -Path 'HKLM:\SYSTEM\CurrentControlSet\Control\Session Manager'
  } else {
    Get-Item -Path 'HKCU:'
  }
  $envRegisterKey = $registerKey.OpenSubKey('Environment')
  $registryValueOption = [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames
  $envRegisterKey.GetValue($Name, $null, $registryValueOption)
}
function Set-EnvVar {
  param(
    [string]$Name,
    [string]$Value,
    [switch]$Global
  )
  $registerKey = if ($Global) {
    Get-Item -Path 'HKLM:\SYSTEM\CurrentControlSet\Control\Session Manager'
  } else {
    Get-Item -Path 'HKCU:'
  }
  $envRegisterKey = $registerKey.OpenSubKey('Environment', $true)
  if ($null -eq $Value -or $Value -eq '') {
    if ($envRegisterKey.GetValue($Name)) {
      $envRegisterKey.DeleteValue($Name)
    }
  } else {
    $registryValueKind = if ($Value.Contains('%')) {
      [Microsoft.Win32.RegistryValueKind]::ExpandString
    } elseif ($envRegisterKey.GetValue($Name)) {
      $envRegisterKey.GetValueKind($Name)
    } else {
      [Microsoft.Win32.RegistryValueKind]::String
    }
    $envRegisterKey.SetValue($Name, $Value, $registryValueKind)
  }
  Publish-EnvVar
}
function Split-PathLikeEnvVar {
  param(
    [string[]]$Pattern,
    [string]$Path
  )
  if ($null -eq $Path -and $Path -eq '') {
    return $null, $null
  } else {
    $splitPattern = $Pattern.Split(';', [System.StringSplitOptions]::RemoveEmptyEntries)
    $splitPath = $Path.Split(';', [System.StringSplitOptions]::RemoveEmptyEntries)
    $inPath = @()
    foreach ($p in $splitPattern) {
      $inPath += $splitPath.Where({ $_ -like $p })
      $splitPath = $splitPath.Where({ $_ -notlike $p })
    }
    return ($inPath -join ';'), ($splitPath -join ';')
  }
}
function Add-Path {
  param(
    [string[]]$Path,
    [string]$TargetEnvVar = 'PATH',
    [switch]$Global,
    [switch]$Force,
    [switch]$Quiet
  )
  $inPath, $strippedPath = Split-PathLikeEnvVar $Path (Get-EnvVar -Name $TargetEnvVar -Global:$Global)
  if (!$inPath -or $Force) {
    if (!$Quiet) {
      $Path | ForEach-Object {
        Write-Host "Adding $(friendly_path $_) to $(if ($Global) {'global'} else {'your'}) path."
      }
    }
    Set-EnvVar -Name $TargetEnvVar -Value ((@($Path) + $strippedPath) -join ';') -Global:$Global
  }
  $inPath, $strippedPath = Split-PathLikeEnvVar $Path $env:PATH
  if (!$inPath -or $Force) {
    $env:PATH = (@($Path) + $strippedPath) -join ';'
  }
}
function Remove-Path {
  param(
    [string[]]$Path,
    [string]$TargetEnvVar = 'PATH',
    [switch]$Global,
    [switch]$Quiet,
    [switch]$PassThru
  )
  $inPath, $strippedPath = Split-PathLikeEnvVar $Path (Get-EnvVar -Name $TargetEnvVar -Global:$Global)
  if ($inPath) {
    if (!$Quiet) {
      $Path | ForEach-Object {
        Write-Host "Removing $(friendly_path $_) from $(if ($Global) {'global'} else {'your'}) path."
      }
    }
    Set-EnvVar -Name $TargetEnvVar -Value $strippedPath -Global:$Global
  }
  $inSessionPath, $strippedPath = Split-PathLikeEnvVar $Path $env:PATH
  if ($inSessionPath) {
    $env:PATH = $strippedPath
  }
  if ($PassThru) {
    return $inPath
  }
}
function is_admin {
  $admin = [Security.Principal.WindowsBuiltInRole]::Administrator
  $id = [Security.Principal.WindowsIdentity]::GetCurrent()
  ([Security.Principal.WindowsPrincipal]($id)).IsInRole($admin)
}
function Invoke-ExternalCommand {
  param(
    [Parameter(Mandatory=$true,Position=0)][Alias('Path')][string]$FilePath,
    [Parameter(Position=1)][Alias('Args')][string[]]$ArgumentList,
    [Alias('Log')][string]$LogPath,
    [Alias('Msg')][string]$Activity,
    [Alias('cec')][hashtable]$ContinueExitCodes,
    [switch]$RunAs,
    [switch]$Quiet
  )
  if ($Activity) { Write-Host "$Activity " -NoNewline }
  $args = $ArgumentList
  if ($null -eq $args) { $args = @() }
  elseif ($args -isnot [array]) { $args = @($args) }
  $exitOk = {
    param([int]$code)
    if ($code -eq 0) { return $true }
    if ($ContinueExitCodes -and $ContinueExitCodes.ContainsKey($code)) {
      warn $ContinueExitCodes[$code]
      return $true
    }
    # Scoop aborts here; hooks that pipe the result away must still fail.
    error "Exit code was $code!"
    return $false
  }
  if (-not (Test-Path -LiteralPath $FilePath)) {
    error "Could not find '$FilePath'"
    return $false
  }
  $FilePath = (Resolve-Path -LiteralPath $FilePath).Path
  if ($RunAs) {
    try {
      $windowStyle = if ($Quiet) { 'Hidden' } else { 'Normal' }
      $proc = Start-Process -FilePath $FilePath -ArgumentList $args -Wait -PassThru -Verb RunAs -WindowStyle $windowStyle
      return (& $exitOk $proc.ExitCode)
    } catch {
      error $_.Exception.Message
      return $false
    }
  }
  if ($LogPath) {
    try {
      & $FilePath @args *>&1 | Out-File -FilePath $LogPath -Encoding utf8
      return (& $exitOk $LASTEXITCODE)
    } catch {
      error $_.Exception.Message
      return $false
    }
  }
  $interactive = $false
  if (Get-Variable -Name glue_interactive -ErrorAction SilentlyContinue) {
    $interactive = [bool]$glue_interactive
  }
  try {
    if ($Quiet) {
      $proc = Start-Process -FilePath $FilePath -ArgumentList $args -Wait -PassThru -WindowStyle Hidden
    } elseif ($interactive) {
      $proc = Start-Process -FilePath $FilePath -ArgumentList $args -Wait -PassThru
    } else {
      $proc = Start-Process -FilePath $FilePath -ArgumentList $args -Wait -PassThru -NoNewWindow
    }
    return (& $exitOk $proc.ExitCode)
  } catch {
    error $_.Exception.Message
    return $false
  }
}
function abort([string]$msg) { throw $msg }
function error([string]$msg) { throw $msg }
function info([string]$msg) { Write-Host $msg }
function warn([string]$msg) { Write-Host $msg }
function get_config([string]$name, $default) {
  if ($name -eq 'USE_LESSMSI') { return $false }
  return $default
}
function ensure([string]$path) {
  if (!(Test-Path $path)) {
    New-Item -ItemType Directory -Path $path -Force | Out-Null
  }
  return (Resolve-Path $path).Path
}
function fname([string]$path) { return [System.IO.Path]::GetFileName($path) }
function strip_ext([string]$path) {
  $base = [System.IO.Path]::GetFileName($path)
  return [System.IO.Path]::GetFileNameWithoutExtension($base)
}
function Copy-Tree([string]$from, [string]$to) {
  if (!(Test-Path -LiteralPath $from)) { return }
  if ((Test-Path -LiteralPath $to) -and -not (Test-Path -LiteralPath $to -PathType Container)) {
    Remove-Item -LiteralPath $to -Force
  }
  ensure $to | Out-Null
  & robocopy.exe $from $to /E /COPY:DAT /R:1 /W:1 /NFL /NDL /NJH /NJS /NC /NS | Out-Null
  if ($LASTEXITCODE -ge 8) { throw "Copy-Tree failed (robocopy exit $LASTEXITCODE)" }
}
function movedir([string]$from, [string]$to) {
  if (!(Test-Path -LiteralPath $from)) { return }
  if (!(Test-Path -LiteralPath $to)) {
    New-Item -ItemType Directory -Path $to -Force | Out-Null
  }
  Get-ChildItem -Path $from -Force | ForEach-Object {
    $dest = Join-Path $to $_.Name
    if ($_.PSIsContainer -and (Test-Path -LiteralPath $dest -PathType Container)) {
      Copy-Tree $_.FullName $dest
      Remove-Item -LiteralPath $_.FullName -Recurse -Force -ErrorAction SilentlyContinue
    } else {
      Move-Item -LiteralPath $_.FullName -Destination $to -Force
    }
  }
}
"#,
    );
    // tail: path/shortcut/shim/appdir helpers
    // and the always-present `Expand-MsiArchive` (Scoop helper set).
    preamble.push_str(
        r#"
function friendly_path([string]$p) { return $p }
function new_issue_msg($app,$bucket,$type) { return '' }
"#,
    );
    preamble.push_str(&scoop_shortcut_shim_and_appdir_helpers(glue_root));
    preamble.push_str(scoop_appdir_helpers());
    // unconditional in both the
    // hook and installer-script preambles (`#/dl.msi` Scoop installs).
    preamble.push_str(SCOOP_EXPAND_MSI_ARCHIVE);
    // injected only when
    // dark/wix resolved; WiX Burn archives (`Expand-DarkArchive`) otherwise
    // abort with a clear message (non-terminating errors are Stopped).
    if !dark.is_empty() {
        preamble.push_str(&scoop_expand_dark_archive_helper(dark));
    }
    // innounp-backed `Expand-InnoArchive`
    // (13 manifests in the local corpus). Injected only when resolved; the
    // installer-script path pre-checks the need and fails with message.
    if !innounp.is_empty() {
        preamble.push_str(&scoop_expand_inno_archive_helper(innounp));
    }
    if !seven_zip.is_empty() {
        let escaped = seven_zip.replace('\'', "''");
        preamble.push_str(&format!(
            r#"$global:GLUE_7Z = '{escaped}'
function Expand-7zipArchive {{
  param(
    [Parameter(Mandatory=$true,Position=0,ValueFromPipeline=$true)][string]$Path,
    [Parameter(Position=1)][string]$DestinationPath = (Split-Path $Path),
    [string]$ExtractDir,
    [string]$Switches,
    [switch]$Removal
  )
  if (-not $global:GLUE_7Z -or -not (Test-Path -LiteralPath $global:GLUE_7Z)) {{
    abort '7z.exe not available; install 7zip first'
  }}
  if (-not (Test-Path -LiteralPath $DestinationPath)) {{
    New-Item -ItemType Directory -Path $DestinationPath -Force | Out-Null
  }}
  $DestinationPath = $DestinationPath.TrimEnd('\')
  $ArgList = @('x', $Path, "-o$DestinationPath", '-xr!*.nsis', '-y')
  $leaf = Split-Path $Path -Leaf
  $IsTar = ($leaf -match '\.tar$') -or ($Path -match '\.t[abgpx]z2?$')
  if (-not $IsTar -and $ExtractDir) {{ $ArgList += "-ir!$ExtractDir\*" }}
  if ($Switches) {{ $ArgList += (-split $Switches) }}
  & $global:GLUE_7Z @ArgList | Out-Null
  if ($LASTEXITCODE -ne 0) {{ abort "7z extract failed (exit $LASTEXITCODE)" }}
  if ($IsTar) {{
    $LogPath = Join-Path (Split-Path $Path) '7zip.log'
    & $global:GLUE_7Z @('l', $Path) *>&1 | Out-File -FilePath $LogPath -Encoding utf8
    $TarFile = (Select-String -Path $LogPath -Pattern '[^ ]*tar$').Matches.Value
    if ($TarFile) {{
      Expand-7zipArchive -Path "$DestinationPath\$TarFile" -DestinationPath $DestinationPath -ExtractDir $ExtractDir -Removal
    }}
    if (Test-Path $LogPath) {{ Remove-Item $LogPath -Force }}
  }}
  if (-not $IsTar -and $ExtractDir) {{
    movedir "$DestinationPath\$ExtractDir" $DestinationPath
    $ExtractDirTopPath = "$DestinationPath\$($ExtractDir -replace '[\\/].*','')"
    if ((Get-ChildItem -Path $ExtractDirTopPath -Force -ErrorAction SilentlyContinue | Measure-Object).Count -eq 0) {{
      Remove-Item -Path $ExtractDirTopPath -Recurse -Force -ErrorAction SilentlyContinue
    }}
  }}
  if ($Removal) {{ Remove-Item -LiteralPath $Path -Force -ErrorAction SilentlyContinue }}
}}
"#
        ));
    }
    preamble
}

// ---------------------------------------------------------------------------
// Hook helper resolution + script building.
// engine/internal/lifecycle/{hooks,env,dark}.go,
//      engine/internal/install/{install_script,install_innosetup}.go
// ---------------------------------------------------------------------------

fn hook_helpers_need_7z(hooks: &[String]) -> bool {
    hooks
        .join("\n")
        .to_lowercase()
        .contains("expand-7ziparchive")
}

fn hook_helpers_need_dark(hooks: &[String]) -> bool {
    hooks
        .join("\n")
        .to_lowercase()
        .contains("expand-darkarchive")
}

fn installer_script_needs_innounp(hooks: &[String]) -> bool {
    let body = hooks.join("\n").to_lowercase();
    body.contains("expand-innoarchive") || body.contains("innounp")
}

fn ps_single_quoted(value: &str) -> String {
    format!("'{}'", value.replace('\'', "''"))
}

/// First matching entry on `PATH`
fn lookup_on_path(name: &str) -> Option<PathBuf> {
    let paths = std::env::var_os("PATH")?;
    std::env::split_paths(&paths)
        .filter(|dir| !dir.as_os_str().is_empty())
        .map(|dir| dir.join(name))
        .find(|candidate| candidate.is_file())
}

/// `wix.exe` runs standalone; `dark.exe`
/// needs its `wix.dll` sidecar in the same directory.
fn dark_executable_ready(exe_path: &Path) -> bool {
    if !exe_path.is_file() {
        return false;
    }
    let is_wix = exe_path
        .file_name()
        .map(|n| n.eq_ignore_ascii_case("wix.exe"))
        .unwrap_or(false);
    if is_wix {
        return true;
    }
    exe_path
        .parent()
        .map(|dir| dir.join("wix.dll").is_file())
        .unwrap_or(false)
}

/// Version dir under `apps/<app>`: the `current` junction when present, else
/// the highest version (/ `ListVersions` + max).
fn latest_app_version_dir(app_root: &Path) -> Option<PathBuf> {
    if let Some(current) = apps::read_current(app_root) {
        let dir = app_root.join(&current);
        if dir.is_dir() {
            return Some(dir);
        }
    }
    let mut versions = apps::list_versions(app_root);
    versions.sort();
    versions.pop().map(|v| app_root.join(v))
}

/// `bin/wix` first, then shims/apps, PATH last;
/// every candidate must pass `dark_executable_ready`. Prefers WiX 4+ wix.exe.
fn resolve_dark(glue_root: &Path) -> Option<String> {
    let bin_wix = glue_root.join("bin").join("wix");
    let mut candidates = vec![bin_wix.join("wix.exe"), bin_wix.join("dark.exe")];
    for (app, exe) in [("wix", "wix.exe"), ("dark", "dark.exe")] {
        candidates.push(glue_root.join("shims").join(exe));
        if let Some(dir) = latest_app_version_dir(&glue_root.join("apps").join(app)) {
            candidates.push(dir.join(exe));
        }
    }
    for name in ["wix.exe", "dark.exe"] {
        if let Some(path) = lookup_on_path(name) {
            candidates.push(path);
        }
    }
    candidates
        .into_iter()
        .find(|c| dark_executable_ready(c))
        .map(|p| p.to_string_lossy().into_owned())
}

/// `bin/innounp` first, then shims/apps, PATH.
fn resolve_innounp(glue_root: &Path) -> Option<String> {
    let mut candidates = vec![
        glue_root.join("bin").join("innounp").join("innounp.exe"),
        glue_root.join("shims").join("innounp.exe"),
    ];
    if let Some(dir) = latest_app_version_dir(&glue_root.join("apps").join("innounp")) {
        candidates.push(dir.join("innounp.exe"));
    }
    if let Some(path) = lookup_on_path("innounp.exe") {
        candidates.push(path);
    }
    candidates
        .into_iter()
        .find(|c| c.is_file())
        .map(|p| p.to_string_lossy().into_owned())
}
/// normalize newlines, then patch each line.
fn patch_optional_hook_lines(body: &str) -> String {
    body.replace('\n', "\r\n")
        .split("\r\n")
        .map(patch_optional_hook_line)
        .collect::<Vec<_>>()
        .join("\r\n")
}

/// wrap `... 2>$null` statements so stderr noise
/// cannot terminate the install under `$ErrorActionPreference = 'Stop'`
/// (the Scoop optional-hook convention).
fn patch_optional_hook_line(line: &str) -> String {
    let trimmed = line.trim();
    if trimmed.is_empty() {
        return line.to_string();
    }
    let (hook, comment) = match trimmed.find(" #") {
        Some(idx) => (trimmed[..idx].trim(), &trimmed[idx..]),
        None => (trimmed, ""),
    };
    let Some(bare) = hook.strip_suffix("2>$null") else {
        return line.to_string();
    };
    format!(
        "$ErrorActionPreference = 'Continue'; {} 2>$null; $ErrorActionPreference = 'Stop'{}",
        bare.trim(),
        comment
    )
}

/// rewrites fragile `Copy-Item "...\*" "..." -Recurse`
/// into `Copy-Tree` (robocopy), which quotes paths with spaces correctly.
/// Hand-rolled matcher instead of a regex dependency (size red line).
fn patch_recurse_copy_item(body: &str) -> String {
    let mut out = String::with_capacity(body.len());
    let mut copied = 0usize;
    let mut search_from = 0usize;
    while search_from + 9 <= body.len() {
        let Some(rel) = find_ascii_ci(&body[search_from..], "copy-item") else {
            break;
        };
        let start = search_from + rel;
        match match_copy_item_recurse(body, start + 9) {
            Some((end, from, to)) => {
                out.push_str(&body[copied..start]);
                let src = from.strip_suffix('*').unwrap_or(from);
                let src = src.trim_end_matches('\\');
                out.push_str(&format!("Copy-Tree \"{src}\" \"{to}\""));
                copied = end;
                search_from = end;
            }
            None => search_from = start + 9,
        }
    }
    out.push_str(&body[copied..]);
    out
}

/// Case-insensitive ASCII substring search on char boundaries.
fn find_ascii_ci(haystack: &str, needle_lower: &str) -> Option<usize> {
    let n = needle_lower.len();
    for (i, _) in haystack.char_indices() {
        if let Some(window) = haystack.get(i..i + n) {
            if window.eq_ignore_ascii_case(needle_lower) {
                return Some(i);
            }
        }
    }
    None
}

/// Regex tail after `Copy-Item`: `\s+ "([^"]*)\*" \s+ "([^"]+)" \s+ -Recurse`.
/// Returns (end offset, from-group without the trailing `*`, to-group).
fn match_copy_item_recurse(s: &str, mut i: usize) -> Option<(usize, &str, &str)> {
    i = skip_ps_ws(s, i);
    let (after_from, from) = read_dquote(s, i)?;
    let from = from.strip_suffix('*')?;
    i = skip_ps_ws(s, after_from);
    let (after_to, to) = read_dquote(s, i)?;
    if to.is_empty() {
        return None;
    }
    i = skip_ps_ws(s, after_to);
    let rest = s.get(i..i + 8)?;
    if !rest.eq_ignore_ascii_case("-recurse") {
        return None;
    }
    Some((i + 8, from, to))
}

fn skip_ps_ws(s: &str, mut i: usize) -> usize {
    while let Some(c) = s[i..].chars().next() {
        if c.is_whitespace() {
            i += c.len_utf8();
        } else {
            break;
        }
    }
    i
}

/// Reads `"..."` at `i` (must start with a double quote; inner text may not
/// contain one). Returns (offset after the closing quote, inner text).
fn read_dquote(s: &str, i: usize) -> Option<(usize, &str)> {
    if s.as_bytes().get(i) != Some(&b'"') {
        return None;
    }
    let mut j = i + 1;
    while j < s.len() {
        if s.as_bytes()[j] == b'"' {
            return Some((j + 1, &s[i + 1..j]));
        }
        j += 1;
    }
    None
}
/// verbatim. Unconditional in
/// both the hook and installer-script preambles (Scoop `#/dl.msi` installs).
const SCOOP_EXPAND_MSI_ARCHIVE: &str = r#"
function Expand-MsiArchive {
  param(
    [Parameter(Mandatory=$true,Position=0,ValueFromPipeline=$true)][string]$Path,
    [Parameter(Position=1)][string]$DestinationPath = (Split-Path $Path),
    [string]$ExtractDir,
    [string]$Switches,
    [switch]$Removal
  )
  $DestinationPath = $DestinationPath.TrimEnd('\')
  if ($ExtractDir) {
    $OriDestinationPath = $DestinationPath
    $DestinationPath = "$DestinationPath\_tmp"
  }
  if (!(Test-Path $DestinationPath)) { New-Item -ItemType Directory -Path $DestinationPath -Force | Out-Null }
  $MsiPath = Join-Path $env:WINDIR 'System32\msiexec.exe'
  if (!(Test-Path -LiteralPath $MsiPath)) {
    $MsiPath = Join-Path $env:WINDIR 'Sysnative\msiexec.exe'
  }
  if (!(Test-Path -LiteralPath $MsiPath)) {
    $cmd = Get-Command msiexec.exe -ErrorAction SilentlyContinue
    if ($cmd) { $MsiPath = $cmd.Source }
  }
  if (!(Test-Path -LiteralPath $MsiPath)) { abort "Could not find msiexec.exe" }
  $ArgList = @('/a', $Path, '/qn', "TARGETDIR=$DestinationPath\SourceDir")
  $LogPath = Join-Path (Split-Path $Path) 'msi.log'
  if ($Switches) { $ArgList += (-split $Switches) }
  $ok = Invoke-ExternalCommand -FilePath $MsiPath -ArgumentList $ArgList -LogPath $LogPath
  if (-not $ok) { abort "Failed to extract files from $Path." }
  if ($ExtractDir -and (Test-Path "$DestinationPath\SourceDir")) {
    movedir "$DestinationPath\SourceDir\$ExtractDir" $OriDestinationPath
    Remove-Item $DestinationPath -Recurse -Force
  } elseif ($ExtractDir) {
    movedir "$DestinationPath\$ExtractDir" $OriDestinationPath
    Remove-Item $DestinationPath -Recurse -Force
  } elseif (Test-Path "$DestinationPath\SourceDir") {
    movedir "$DestinationPath\SourceDir" $DestinationPath
  }
  if ((Test-Path "$DestinationPath\SourceDir") -and -not (Get-ChildItem "$DestinationPath\SourceDir" -Force | Select-Object -First 1)) {
    Remove-Item "$DestinationPath\SourceDir" -Force -ErrorAction SilentlyContinue
  }
  if (($DestinationPath -ne (Split-Path $Path)) -and (Test-Path "$DestinationPath\$(fname $Path)")) {
    Remove-Item "$DestinationPath\$(fname $Path)" -Force
  }
  if (Test-Path $LogPath) { Remove-Item $LogPath -Force }
  if ($Removal) { Remove-Item -LiteralPath $Path -Force -ErrorAction SilentlyContinue }
}
"#;
/// body only; the
/// `$global:GLUE_DARK` assignment is prepended by the wrapper below.
const SCOOP_EXPAND_DARK_ARCHIVE_BODY: &str = r#"
function Expand-DarkArchive {
  param(
    [Parameter(Mandatory=$true,Position=0,ValueFromPipeline=$true)][string]$Path,
    [Parameter(Position=1)][string]$DestinationPath = (Split-Path $Path),
    [string]$Switches,
    [switch]$Removal
  )
  $LogPath = Join-Path (Split-Path $Path) 'dark.log'
  $DarkPath = $global:GLUE_DARK
  if (-not $DarkPath -or -not (Test-Path -LiteralPath $DarkPath)) {
    abort 'dark.exe or wix.exe not found; run: glue install dark'
  }
  if ((Split-Path $DarkPath -Leaf) -eq 'wix.exe') {
    $ArgList = @('burn', 'extract', $Path, '-out', $DestinationPath, '-outba', "$DestinationPath\UX")
  } else {
    $ArgList = @('-nologo', '-x', $DestinationPath, $Path)
  }
  if ($Switches) { $ArgList += (-split $Switches) }
  $ok = Invoke-ExternalCommand -FilePath $DarkPath -ArgumentList $ArgList -LogPath $LogPath
  if (-not $ok) { abort "Failed to extract files from $Path." }
  if (Test-Path "$DestinationPath\WixAttachedContainer") {
    Rename-Item "$DestinationPath\WixAttachedContainer" 'AttachedContainer' -ErrorAction Ignore
  } elseif (Test-Path "$DestinationPath\AttachedContainer\a0") {
    $Xml = [xml](Get-Content -Raw "$DestinationPath\UX\manifest.xml" -Encoding utf8)
    $Xml.BurnManifest.UX.Payload | ForEach-Object {
      Rename-Item "$DestinationPath\UX\$($_.SourcePath)" $_.FilePath -ErrorAction Ignore
    }
    $Xml.BurnManifest.Payload | ForEach-Object {
      Rename-Item "$DestinationPath\AttachedContainer\$($_.SourcePath)" $_.FilePath -ErrorAction Ignore
    }
  }
  if (Test-Path $LogPath) { Remove-Item $LogPath -Force }
  if ($Removal) { Remove-Item -LiteralPath $Path -Force -ErrorAction SilentlyContinue }
}
"#;

fn scoop_expand_dark_archive_helper(dark: &str) -> String {
    let mut out = String::from("$global:GLUE_DARK = ");
    out.push_str(&ps_single_quoted(dark));
    out.push('\n');
    out.push_str(SCOOP_EXPAND_DARK_ARCHIVE_BODY);
    out
}

/// tail — `Expand-InnoArchive`.
const SCOOP_EXPAND_INNO_ARCHIVE_BODY: &str = r#"
function Expand-InnoArchive {
  param(
    [Parameter(Mandatory=$true,Position=0,ValueFromPipeline=$true)][string]$Path,
    [Parameter(Position=1)][string]$DestinationPath = (Split-Path $Path),
    [string]$ExtractDir,
    [string]$Switches,
    [switch]$Removal
  )
  $DestinationPath = $DestinationPath.TrimEnd('\')
  if (-not (Test-Path $DestinationPath)) { New-Item -ItemType Directory -Path $DestinationPath | Out-Null }
  $LogPath = Join-Path (Split-Path $Path) 'innounp.log'
  $ArgList = @('-x', "-d$DestinationPath", $Path, '-y')
  if ($ExtractDir) {
    if ($ExtractDir -match '^[^{].*') { $ArgList += "-c{app}\$ExtractDir" }
    elseif ($ExtractDir -match '^{.*') { $ArgList += "-c$ExtractDir" }
    else { $ArgList += '-c{app}' }
  } else {
    $ArgList += '-c{app}'
  }
  if ($Switches) { $ArgList += (-split $Switches) }
  $innounp = Get-HelperPath -Helper Innounp
  if (-not $innounp) { throw 'innounp.exe not found in glue/bin' }
  $ok = Invoke-ExternalCommand -FilePath $innounp -ArgumentList $ArgList -LogPath $LogPath
  if (-not $ok) { throw "Failed to extract Inno archive: $Path" }
  if (Test-Path $LogPath) { Remove-Item $LogPath -Force }
  if ($Removal) { Remove-Item -LiteralPath $Path -Force -ErrorAction SilentlyContinue }
}
"#;

/// innounp half: `$global:GLUE_INNOUNP`,
/// `Get-HelperPath` and `Expand-InnoArchive`. Injected only when innounp
/// resolved — the installer-script path pre-checks the need and fails with
/// message otherwise.
fn scoop_expand_inno_archive_helper(innounp: &str) -> String {
    let mut out = String::from("$global:GLUE_INNOUNP = ");
    out.push_str(&ps_single_quoted(innounp));
    out.push_str(
        r#"
function Get-HelperPath {
  param(
    [Parameter(Mandatory=$true,Position=0)][ValidateSet('7zip','Innounp')][String]$Helper
  )
  switch ($Helper) {
    '7zip' { return $global:GLUE_7Z }
    'Innounp' { return $global:GLUE_INNOUNP }
  }
  return $null
}
"#,
    );
    out.push_str(SCOOP_EXPAND_INNO_ARCHIVE_BODY);
    out
}
/// +. Needs
/// `$glue_root`, so the text is built per call.
fn scoop_shortcut_shim_and_appdir_helpers(glue_root: &Path) -> String {
    format!(
        r#"
$glue_root = '{glue_root}'
function shortcut_folder($global) {{
  if ($global) {{ $startmenu = 'CommonStartMenu' }} else {{ $startmenu = 'StartMenu' }}
  return Convert-Path (ensure ([System.IO.Path]::Combine([Environment]::GetFolderPath($startmenu), 'Programs', 'Glue Apps')))
}}
function startmenu_shortcut([System.IO.FileInfo] $target, $shortcutName, $arguments, [System.IO.FileInfo]$icon, $global) {{
  if ($target -is [string]) {{ $target = [System.IO.FileInfo]$target }}
  if ($icon -is [string] -and $icon) {{ $icon = [System.IO.FileInfo]$icon }}
  if (!$target.Exists) {{
    Write-Host "Creating shortcut for $shortcutName ($(fname $target.FullName)) failed: Couldn't find $target" -ForegroundColor DarkRed
    return
  }}
  if ($icon -and !$icon.Exists) {{
    Write-Host "Creating shortcut for $shortcutName ($(fname $target.FullName)) failed: Couldn't find icon $icon" -ForegroundColor DarkRed
    return
  }}
  $scoop_startmenu_folder = shortcut_folder $global
  $subdirectory = [System.IO.Path]::GetDirectoryName($shortcutName)
  if ($subdirectory) {{
    $subdirectory = ensure $([System.IO.Path]::Combine($scoop_startmenu_folder, $subdirectory))
  }}
  $lnkPath = "$scoop_startmenu_folder\$shortcutName.lnk"
  if (Test-Path -LiteralPath $lnkPath) {{ Remove-Item -LiteralPath $lnkPath -Force }}
  $wsShell = New-Object -ComObject WScript.Shell
  $wsShell = $wsShell.CreateShortcut($lnkPath)
  $wsShell.TargetPath = $target.FullName
  $wsShell.WorkingDirectory = $target.DirectoryName
  if ($arguments) {{ $wsShell.Arguments = $arguments }}
  if ($icon -and $icon.Exists) {{ $wsShell.IconLocation = $icon.FullName }}
  $wsShell.Save()
  Write-Host "Shortcut: $shortcutName"
}}
function shimdir($global) {{ Join-Path $glue_root 'shims' }}
function shim($path, $global, $name, $arg) {{
  if (!(Test-Path -LiteralPath $path)) {{ abort "Can't shim '$(fname $path)': couldn't find '$path'." }}
  $abs_shimdir = ensure (shimdir $global)
  if (!$name) {{ $name = strip_ext (fname $path) }}
  $shimName = $name.ToLower()
  $resolved = (Resolve-Path -LiteralPath $path).Path
  $metaDir = Join-Path $glue_root 'shims-meta'
  ensure $metaDir | Out-Null
  $args = @()
  if ($arg) {{ $args = @($arg) }}
  $cfg = @{{ name = $shimName; command = $resolved; args = $args; path = $resolved }} | ConvertTo-Json -Compress
  Set-Content -LiteralPath (Join-Path $metaDir "$shimName.json") -Value $cfg -Encoding utf8
  $stub = Join-Path $glue_root 'shim.exe'
  if (!(Test-Path -LiteralPath $stub)) {{ abort "shim stub not found at $stub" }}
  Copy-Item -LiteralPath $stub -Destination (Join-Path $abs_shimdir "$shimName.exe") -Force
  Write-Host "    [OK] $shimName"
}}
function rm_shim($name, $shimdir, $app) {{
  $shimName = $name.ToLower()
  $cfg = Join-Path (Join-Path $glue_root 'shims-meta') "$shimName.json"
  $exe = Join-Path $shimdir "$shimName.exe"
  if (Test-Path -LiteralPath $cfg) {{ Remove-Item -LiteralPath $cfg -Force }}
  if (Test-Path -LiteralPath $exe) {{ Remove-Item -LiteralPath $exe -Force }}
}}
"#,
        glue_root = glue_root.display().to_string().replace('\'', "''")
    )
}

/// `appsdir`/`appdir`/`Select-CurrentVersion`/
/// `installed`, used by uninstaller scripts (`$global = installed $app $true`)
/// and by installer scripts that resolve another package (`86box-roms` calls
/// `$(appdir 86box $global)`).
///
/// The separator split must be written `'[/\\]'` — as a .NET regex the
/// shorter `'/|\'` ends on a lone backslash and PowerShell aborts the whole
/// script with `parsing "/|\" - Illegal \ at end of pattern`.
fn scoop_appdir_helpers() -> &'static str {
    r#"
function appsdir($global) {
  if ($global) { return $null }
  if (-not $glue_root) { return $null }
  return Join-Path $glue_root 'apps'
}
function appdir($app, $global) {
  $root = appsdir $global
  if (-not $root) { return $null }
  $app = ($app -split '[/\\]')[-1]
  return Join-Path $root $app
}
function Select-CurrentVersion {
  param(
    [Parameter(Mandatory=$true)][string]$AppName,
    [switch]$Global
  )
  $pkgRoot = appdir $AppName $Global
  if (-not $pkgRoot -or !(Test-Path -LiteralPath $pkgRoot)) { return $null }
  $current = Join-Path $pkgRoot 'current'
  if (Test-Path -LiteralPath $current) {
    try {
      $item = Get-Item -LiteralPath $current -Force
      if ($item.LinkType -eq 'Junction') {
        $target = $item.Target
        if ($target -is [array]) { $target = $target[0] }
        if ($target) { return [System.IO.Path]::GetFileName($target.TrimEnd('\')) }
      }
    } catch { }
  }
  $versions = Get-ChildItem -LiteralPath $pkgRoot -Directory -ErrorAction SilentlyContinue |
    Where-Object { $_.Name -ne 'current' } |
    Sort-Object Name -Descending
  if ($versions) { return $versions[0].Name }
  return $null
}
function installed($app, $global) {
  if ($null -eq $global) {
    return (installed $app $false) -or (installed $app $true)
  }
  $app = ($app -split '[/\\]')[-1]
  return $null -ne (Select-CurrentVersion -AppName $app -Global:([bool]$global))
}
"#
}
/// Resolved external-helper paths for a hook run —
/// (7z is always taken from the extractor; dark/innounp only when present).
struct HookHelperPaths {
    seven_zip: String,
    dark: Option<String>,
    innounp: Option<String>,
}

/// Download the WiX toolset when the hooks need dark but nothing resolves.
///
/// Refuse for the helper packages themselves
/// (installing `dark`/`wix` must not bootstrap the tool it provides), otherwise
/// fetch the vendor zip into `bin/wix`. Returns the resolved `dark.exe` path.
fn bootstrap_dark_for_hooks(services: &Services, pkg_name: &str) -> Result<String, String> {
    if is_dark_helper_package(pkg_name) {
        return Err("dark.exe or wix.exe not found".to_string());
    }
    crate::bootstrap::ensure_dark(services, None)
        .map(|p| p.to_string_lossy().into_owned())
        .map_err(|err| err.to_string())
}

/// `pre_uninstall`, then
/// `uninstaller.script`, before anything is deleted.
///
/// Both phases run with `$cmd = "uninstall"`, which suppresses
/// `$ErrorActionPreference = 'Stop'`
/// and `uninstaller.script` gets a
/// trailing `exit 0` because native commands routinely leave `$LASTEXITCODE`
/// non-zero without throwing.
///
/// Returns nil here when there is nothing to run; a real hook failure is
/// propagated so the uninstall stops rather than deleting files the hook was
/// supposed to unwind.
pub(crate) fn run_manifest_uninstall_hooks(
    services: &Services,
    manifest: &Manifest,
    install_arch: &str,
    install_dir: &Path,
    pkg_name: &str,
    pkg_ref: &str,
    version: &str,
) -> Result<(), InstallError> {
    let pre = manifest.pre_uninstall_hooks_for_install(install_arch);
    let uninstaller = manifest.uninstaller_script_hooks_for_install(install_arch);
    if pre.is_empty() && uninstaller.is_empty() {
        return Ok(());
    }

    // Resolves helpers for `preHooks + uninstallerHooks`; uninstall never
    // bootstraps innounp (that is install-only), so resolve dark and take the
    // already-resolved innounp as-is.
    let all: Vec<String> = pre.iter().chain(uninstaller.iter()).cloned().collect();
    let mut helpers = resolve_hook_helper_paths(services);
    if helpers.dark.is_none() && hook_helpers_need_dark(&all) {
        helpers.dark = Some(bootstrap_dark_for_hooks(services, pkg_name).map_err(|err| {
            text_phase_error(&format!(
                "dark.exe or wix.exe not found\n\nWiX Burn packages need dark.exe in glue/bin\n{err}"
            ))
        })?);
    }

    let download_name = download_name_from_manifest(manifest, install_arch);
    let root = services.root_dir().to_path_buf();
    let persist_dir = root.join("persist").join(pkg_name);

    let run = |kind: &str, hooks: &[String]| -> Result<(), InstallError> {
        if hooks.is_empty() {
            return Ok(());
        }
        let env = HookScriptEnv {
            install_dir,
            download_name: &download_name,
            version,
            app: pkg_name,
            bucket: &crate::catalog::package_bucket_name(pkg_ref),
            buckets_dir: &root.join("buckets"),
            persist_dir: &persist_dir,
            arch: install_arch,
            glue_root: &root,
            seven_zip: &helpers.seven_zip,
            dark: helpers.dark.as_deref().unwrap_or(""),
            innounp: helpers.innounp.as_deref().unwrap_or(""),
            interactive: false,
            cmd: "uninstall",
            hooks,
        };
        run_powershell_hooks(kind, &env)
    };

    run("pre_uninstall", &pre)?;
    let mut uninstaller_owned = uninstaller.clone();
    uninstaller_owned.push("exit 0".to_string());
    run("uninstaller.script", &uninstaller_owned)?;

    // a service/process-stop hook can leave
    // the binary still mapped for a moment, and removing it right after fails
    // with a sharing violation.
    if uninstall_hooks_need_settle_time(&all) {
        std::thread::sleep(std::time::Duration::from_millis(1500));
    }
    Ok(())
}

/// runs after the version
/// directory is gone. Failures are warnings there; the implementation reports them the
/// same way so a half-finished uninstall is never strung along by a hook.
pub(crate) fn run_manifest_post_uninstall_hooks(
    services: &Services,
    manifest: &Manifest,
    install_arch: &str,
    install_dir: &Path,
    pkg_name: &str,
    pkg_ref: &str,
    version: &str,
) -> Result<(), InstallError> {
    let hooks = manifest.post_uninstall_hooks_for_install(install_arch);
    if hooks.is_empty() {
        return Ok(());
    }
    let helpers = resolve_hook_helper_paths(services);
    let download_name = download_name_from_manifest(manifest, install_arch);
    let root = services.root_dir().to_path_buf();
    let env = HookScriptEnv {
        install_dir,
        download_name: &download_name,
        version,
        app: pkg_name,
        bucket: &crate::catalog::package_bucket_name(pkg_ref),
        buckets_dir: &root.join("buckets"),
        persist_dir: &root.join("persist").join(pkg_name),
        arch: install_arch,
        glue_root: &root,
        seven_zip: &helpers.seven_zip,
        dark: helpers.dark.as_deref().unwrap_or(""),
        innounp: helpers.innounp.as_deref().unwrap_or(""),
        interactive: false,
        cmd: "uninstall",
        hooks: &hooks,
    };
    run_powershell_hooks("post_uninstall", &env)
}

/// the archive filename of the first
/// install URL (Scoop `#/newname.ext` fragments already handled by `parse_url`).
fn download_name_from_manifest(manifest: &Manifest, install_arch: &str) -> String {
    match manifest.get_urls_for_install(install_arch).first() {
        Some(url) => manifest::parse_url(url)
            .map(|p| p.local_name)
            .unwrap_or_default(),
        None => String::new(),
    }
}

/// hooks that stop a service or kill a
/// process need a beat before the files they hold are removed.
fn uninstall_hooks_need_settle_time(hooks: &[String]) -> bool {
    let body = hooks.join("\n").to_lowercase();
    [
        "stop-service",
        "stop-process",
        "uninstall-system-daemon",
        "sc.exe stop",
    ]
    .iter()
    .any(|needle| body.contains(needle))
}

/// `bin/wix` / `bin/innounp` / shims / apps /
/// PATH, with no downloading. Used by the uninstall phases, where passes
/// `timer = nil` and never bootstraps innounp.
fn resolve_hook_helper_paths(services: &Services) -> HookHelperPaths {
    let root = services.root_dir();
    HookHelperPaths {
        seven_zip: services.extractor().seven_zip_path(),
        dark: resolve_dark(root),
        innounp: resolve_innounp(root),
    }
}

/// Resolve hook helpers, bootstrapping `dark.exe`/`innounp.exe` on demand.
///
/// +: when
/// the hooks need a helper that is not installed, the vendor zip is downloaded
/// into `bin/<tool>` and the resolution is retried. Skipped while installing
/// the helper package itself, which would otherwise recurse forever.
///
/// This replaces a plain "not found" error: before this, any WiX-Burn or Inno
/// package failed on a machine that had never installed the helper.
fn hook_helper_paths(
    services: &Services,
    hooks: &[String],
    pkg_name: &str,
    timings: &mut Timings,
) -> Result<HookHelperPaths, InstallError> {
    let mut helpers = resolve_hook_helper_paths(services);

    // Every hook/installer script runs with 7-Zip available (Scoop does the
    // same); bootstrap it into `<root>/bin` when the machine has none.
    if helpers.seven_zip.is_empty() && !hooks.is_empty() {
        let since = timings.start();
        let bootstrapped = crate::bootstrap::ensure_7zip(services, None);
        timings.add(Bucket::Bootstrap, since);
        match bootstrapped {
            Ok(path) => {
                let path = path.to_string_lossy().into_owned();
                services.extractor().set_7z_path(path.clone());
                helpers.seven_zip = path;
            }
            Err(err) => {
                return Err(text_phase_error(&format!(
                    "7z.exe not found\n\ninstaller scripts need 7z in glue/bin\n{err}"
                )))
            }
        }
    }
    if helpers.dark.is_none() && hook_helpers_need_dark(hooks) {
        // Wraps the bootstrap in so its download time
        // lands in `bootstrap_ms` instead of being invisible in the wall clock.
        let since = timings.start();
        let bootstrapped = bootstrap_dark_for_hooks(services, pkg_name);
        timings.add(Bucket::Bootstrap, since);
        match bootstrapped {
            Ok(path) => helpers.dark = Some(path),
            Err(err) => {
                return Err(text_phase_error(&format!(
                    "dark.exe or wix.exe not found\n\nWiX Burn packages need dark.exe in glue/bin\n{err}"
                )))
            }
        }
    }

    if helpers.innounp.is_none() && installer_script_needs_innounp(hooks) {
        helpers.innounp = Some(ensure_innounp_for_extract(services, pkg_name, timings)?);
    }

    Ok(helpers)
}

/// Resolve `innounp.exe`, bootstrapping it into `<root>/bin` when the machine
/// has none.
///
/// Shared by hook scripts and the manifest-level `innosetup: true` extraction so
/// both paths pick the same binary and report the same failure. Installing the
/// helper package itself must not recurse into downloading the helper — that is
/// the one case that fails with plain "not found".
fn ensure_innounp_for_extract(
    services: &Services,
    pkg_name: &str,
    timings: &mut Timings,
) -> Result<String, InstallError> {
    if let Some(path) = resolve_innounp(services.root_dir()) {
        return Ok(path);
    }
    if is_innounp_helper_package(pkg_name) {
        return Err(text_phase_error(
            "innounp.exe not found\n\nInno Setup packages need innounp in glue/bin",
        ));
    }
    let since = timings.start();
    let bootstrapped = crate::bootstrap::ensure_innounp(services, None);
    timings.add(Bucket::Bootstrap, since);
    match bootstrapped {
        Ok(path) => Ok(path.to_string_lossy().into_owned()),
        Err(err) => Err(text_phase_error(&format!(
            "innounp.exe not found\n\nInno Setup packages need innounp in glue/bin\n{err}"
        ))),
    }
}

/// installing `dark`/`wix` itself must not
/// try to bootstrap the tool it is about to provide.
fn is_dark_helper_package(pkg_name: &str) -> bool {
    matches!(
        pkg_name.trim().to_ascii_lowercase().as_str(),
        "dark" | "wix"
    )
}

fn is_innounp_helper_package(pkg_name: &str) -> bool {
    pkg_name.trim().eq_ignore_ascii_case("innounp")
}

/// the values exposed to hook scripts as
/// PowerShell variables, plus the resolved external-helper paths.
struct HookScriptEnv<'a> {
    install_dir: &'a Path,
    download_name: &'a str,
    version: &'a str,
    app: &'a str,
    bucket: &'a str,
    buckets_dir: &'a Path,
    persist_dir: &'a Path,
    arch: &'a str,
    glue_root: &'a Path,
    seven_zip: &'a str,
    dark: &'a str,
    innounp: &'a str,
    /// `glue install --interactive`: sets `$glue_interactive` and the
    /// Scoop-compatible `$interactive` in the generated script. `false`
    /// (the default everywhere) makes hooks skip their silent-switch guards.
    interactive: bool,
    /// `HookScriptEnv.Cmd`: empty for install hooks, `"uninstall"` for the
    /// three uninstall phases. When set it becomes `$cmd` in the script and
    /// **suppresses** `$ErrorActionPreference = 'Stop'` — rule, because a
    /// failing cleanup step must not abort a half-finished uninstall.
    cmd: &'a str,
    hooks: &'a [String],
}

/// Build the full hook script:
/// helper preamble, Scoop env vars, the patched hook body wrapped in
/// Push-Location/trap, and PATH restoration afterwards.
fn build_hook_script(env: &HookScriptEnv) -> String {
    let body = patch_recurse_copy_item(&patch_optional_hook_lines(&env.hooks.join("\r\n")));

    let mut prefix = String::new();
    prefix.push_str(&scoop_hook_helper_preamble(
        env.seven_zip,
        env.dark,
        env.innounp,
        env.glue_root,
    ));
    prefix.push_str("\r\n");
    // Parity-critical
    // without Stop, a missing helper is a
    // non-terminating error — PowerShell exits 0 and the install silently
    // "succeeds" with an incomplete directory.
    //
    // Uninstall hooks are the one place deliberately leaves it out, and this
    // port follows: cleanup is best-effort, and a hard failure mid-uninstall
    // would strand the package. See `run_uninstall_hooks`.
    if env.cmd != "uninstall" {
        prefix.push_str("$ErrorActionPreference = 'Stop'\r\n");
    }
    if !env.version.is_empty() {
        prefix.push_str(&format!("$version = {}; ", ps_single_quoted(env.version)));
    }
    if !env.app.is_empty() {
        prefix.push_str(&format!("$app = {}; ", ps_single_quoted(env.app)));
    }
    if !env.bucket.is_empty() {
        prefix.push_str(&format!("$bucket = {}; ", ps_single_quoted(env.bucket)));
    }
    if !env.buckets_dir.as_os_str().is_empty() {
        prefix.push_str(&format!(
            "$bucketsdir = {}; ",
            ps_single_quoted(&env.buckets_dir.display().to_string())
        ));
    }
    if !env.persist_dir.as_os_str().is_empty() {
        prefix.push_str(&format!(
            "$persist_dir = {}; ",
            ps_single_quoted(&env.persist_dir.display().to_string())
        ));
    }
    if !env.arch.is_empty() {
        prefix.push_str(&format!("$architecture = {}; ", ps_single_quoted(env.arch)));
    }
    if !env.cmd.is_empty() {
        prefix.push_str(&format!("$cmd = {}; ", ps_single_quoted(env.cmd)));
    }
    // `$glue_interactive` is the marker the helper preamble reads (inside a
    // function scope it cannot see the caller's `$interactive`); `$interactive`
    // is the Scoop-compatible name a manifest hook tests directly
    // (`if (!$interactive) { $args += '/S' }`).
    if env.interactive {
        prefix.push_str("$glue_interactive = $true; $interactive = $true; ");
    } else {
        prefix.push_str("$glue_interactive = $false; $interactive = $false; ");
    }

    let install_dir = ps_single_quoted(&env.install_dir.display().to_string());
    let fname = ps_single_quoted(env.download_name);
    format!(
        "{prefix}$global = $false; $original_dir = {install_dir}; $dir = {install_dir}; $fname = {fname}; $glue_old_path = $env:PATH; $env:PATH = \"$dir;$glue_old_path\"; Push-Location -LiteralPath $dir; trap {{ Pop-Location; $env:PATH = $glue_old_path; throw $_ }}\r\n{body}\r\nPop-Location; $env:PATH = $glue_old_path"
    )
}

/// Executes PowerShell hook lines (`pre_install`/`post_install`) —
/// / `RunPostInstallHooks`.
fn run_powershell_hooks(kind: &str, env: &HookScriptEnv) -> Result<(), InstallError> {
    run_powershell_script(env, kind, &format!("{kind} hooks for {}", env.app))
}

/// Write a single-purpose hook script to a temp file and run it.
///
/// `kind` names the script file (and keeps existing temp names stable); `what`
/// is the user-facing label a failure is reported under — hook phases say
/// "post_install hooks for 7zip", non-hook callers name the operation.
fn run_powershell_script(env: &HookScriptEnv, kind: &str, what: &str) -> Result<(), InstallError> {
    // Same reason as the installer script: pre/post-install hooks patch files
    // in the install tree, and a read-only file there fails them outright.
    clear_readonly_recursive(env.install_dir);
    let script = build_hook_script(env);
    let tmp = std::env::temp_dir().join(format!("glue-{kind}-{}.ps1", env.app));
    fs::write(&tmp, script)
        .map_err(|err| text_phase_error(&format!("write {what} script: {err}")))?;
    let result = run_powershell_file(&tmp);
    let _ = fs::remove_file(&tmp);
    result.map_err(|err| text_phase_error(&format!("{what}: {err}")))?;
    Ok(())
}

/// a hook that flattens a nested archive
/// directory with a Unix-style `mv */* .`.
///
/// Match the literal `mv` command, never PowerShell's `Move-Item` cmdlet: the
/// string `remove-item` *contains* `move-item`, so a cmdlet substring match
/// reclassifies every `Remove-Item` cleanup hook as a flatten — and both call
/// sites then clear the hook list, silently skipping the hook. That is exactly
/// how apngasm's `$PLUGINSDIR` / `Uninstall.exe` cleanup got lost.
fn is_scoop_move_flatten_pre_install(hooks: &[String]) -> bool {
    hooks.iter().any(|hook| {
        // Strings.Fields normalizes runs of whitespace (including
        // newlines) to single spaces before the substring check.
        let h = hook
            .to_lowercase()
            .split_whitespace()
            .collect::<Vec<_>>()
            .join(" ");
        h.contains("mv */*") || h.contains("mv *\\*")
    })
}

/// a single nested directory that holds the bins.
fn install_needs_move_flatten(install_dir: &Path) -> bool {
    let Ok(entries) = fs::read_dir(install_dir) else {
        return false;
    };
    let mut subdirs: Vec<PathBuf> = Vec::new();
    let mut loose_files = 0usize;
    for entry in entries.flatten() {
        let path = entry.path();
        if path.is_dir() {
            let name = entry.file_name().to_string_lossy().into_owned();
            if should_skip_portable_scan_dir(&name) {
                continue;
            }
            subdirs.push(path);
        } else {
            loose_files += 1;
        }
    }
    subdirs.len() == 1 && loose_files == 0
}

/// hoists the single nested directory's
/// children into `install_dir` and removes the now-empty container.
fn ensure_scoop_move_flatten_install_dir(
    install_dir: &Path,
    m: &Manifest,
) -> Result<(), InstallError> {
    let Some(nested) = nested_dir_with_manifest_bin_layout(install_dir, m) else {
        return Ok(());
    };
    let nested_path = install_dir.join(&nested);
    let mut result = Ok(());
    if let Ok(entries) = fs::read_dir(&nested_path) {
        for entry in entries.flatten() {
            let from = entry.path();
            let Some(name) = from.file_name() else {
                continue;
            };
            let to = install_dir.join(name);
            if to.exists() {
                continue;
            }
            if let Err(err) = fs::rename(&from, &to) {
                result = Err(err);
                break;
            }
        }
    }
    if result.is_ok() {
        let _ = fs::remove_dir(&nested_path);
    }
    result.map_err(|err| text_phase_error(&format!("move flatten install dir: {err}")))
}

fn move_flatten_if_needed(
    services: &Services,
    st: &mut InstallState,
    m: &Manifest,
) -> Result<(), InstallError> {
    let pre_hooks = m.pre_install_hooks_for_install(&st.install_arch);
    if !is_scoop_move_flatten_pre_install(&pre_hooks)
        && !install_needs_move_flatten(&st.install_dir)
    {
        return Ok(());
    }
    ensure_scoop_move_flatten_install_dir(&st.install_dir, m)?;
    refresh_state_files(services, st);
    Ok(())
}

/// (record half).
fn save_install_record_for(
    install_dir: &Path,
    bucket: &str,
    m: &Manifest,
    st: &InstallState,
) -> Result<(), InstallError> {
    // Prefer the raw manifest on disk so arch blocks survive.
    let manifest_json = st
        .manifest_path
        .as_ref()
        .and_then(|path| fs::read(path).ok())
        .and_then(|data| serde_json::from_slice::<Value>(&data).ok());
    apps::save_install_record_with_arch(
        install_dir,
        bucket,
        &m.version,
        &st.install_arch,
        manifest_json,
    )
    .map_err(|err| text_phase_error(&format!("save install record: {err}")))
}

/// (Re)creates every side effect of a package that is pinned to one version
/// directory: the `shims/` runners (whose `command` goes through `current`, but
/// whose `env` expands `$dir` to `install_dir`), the `env_set` values in the
/// user environment and the Start-menu shortcuts (`TargetPath` is absolute).
/// Rebuild the version-pinned side effects of an already installed version.
///
/// `glue reset` (and an update that switches to an already downloaded newer
/// version) only swaps the `current` junction, but a package's shims, `env_set`
/// values and Start-menu shortcuts all embed a *concrete* version directory:
/// `shims-meta/<pkg>.json` carries `$dir` expanded to the install dir, the
/// user-environment values do the same, and a `.lnk` stores an absolute
/// `TargetPath`. Left alone they keep pointing at the previously active
/// version, so a reset would switch `current` and still launch the old tree.
///
/// `install.json` is the source of truth: it holds the version's own manifest
/// (raw, so arch blocks survive). `arch` is the record's arch when present and
/// the manifest's host-selected block otherwise (records written before the
/// field existed).
pub(crate) fn rebuild_version_links(
    services: &Services,
    pkg_name: &str,
    install_dir: &Path,
) -> Result<(), InstallError> {
    let Ok(record) = apps::load_install_record(install_dir) else {
        // No record: nothing describes what this version's shims/shortcuts
        // should be. Whatever is on disk was created for this very version, so
        // it is already correct — and shim `command` resolves through `current`
        // either way.
        return Ok(());
    };
    let Some(m) = record
        .manifest
        .and_then(|value| serde_json::from_value::<Manifest>(value).ok())
    else {
        return Ok(());
    };
    let install_arch = if record.arch.trim().is_empty() {
        m.selected_architecture().to_string()
    } else {
        record.arch
    };
    create_shims_and_shortcuts(services, &m, install_dir, pkg_name, &install_arch)?;
    Ok(())
}

fn create_shims_and_shortcuts(
    services: &Services,
    m: &Manifest,
    install_dir: &Path,
    pkg_name: &str,
    install_arch: &str,
) -> Result<Vec<String>, InstallError> {
    let root = services.root_dir();
    let shims_meta_dir = root.join("shims-meta");
    let apps_dir = root.join("apps");
    let current_dir = apps::pkg_root(root, pkg_name).join(apps::CURRENT_LINK_NAME);

    let manager = crate::shim::Manager::new(root)
        .map_err(|err| text_phase_error(&format!("open shim manager: {err}")))?;

    crate::shim::remove_shims_for_package(&manager, &shims_meta_dir, pkg_name)
        .map_err(|err| text_phase_error(&format!("remove old shims: {err}")))?;
    let _ = apps_dir;

    let created = create_package_shims(
        &manager,
        &shims_meta_dir,
        pkg_name,
        install_dir,
        Some(&current_dir),
        m,
        install_arch,
    )
    .map_err(|err| text_phase_error(&format!("create shims: {err}")))?;

    crate::winshell::apply_package_env_set(m, &install_dir.to_string_lossy(), install_arch)
        .map_err(|err| text_phase_error(&format!("apply env_set: {err}")))?;

    crate::winshell::create_package_shortcuts(install_dir, m, install_arch)
        .map_err(|err| text_phase_error(&format!("create shortcuts: {err}")))?;
    Ok(created)
}

/// carry the previous install's metadata map
/// forward so `AddInstalled` does not drop it.
fn preserve_installed_metadata(services: &Services, pkg_name: &str) -> Map<String, Value> {
    services
        .cache()
        .get_installed(pkg_name)
        .map(|entry| entry.metadata)
        .unwrap_or_default()
}

/// (extension-stripped basename).
fn go_base(p: &str) -> String {
    let trimmed = p.trim_end_matches(['/', '\\']);
    let base = trimmed.rsplit(['/', '\\']).next().unwrap_or(trimmed);
    match base.rfind('.') {
        Some(i) if i > 0 => base[..i].to_string(),
        _ => base.to_string(),
    }
}

fn normalize_install_file_ext(ext: &str) -> String {
    match ext.to_lowercase().as_str() {
        ".tar.gz" | ".tar.bz2" | ".tar.xz" | ".tgz" => ".tar".to_string(),
        _ => ext.to_string(),
    }
}

fn is_archive_install_ext(ext: &str) -> bool {
    matches!(
        ext.to_lowercase().as_str(),
        ".tar" | ".tar.gz" | ".tar.bz2" | ".tar.xz" | ".tgz" | ".nupkg" | ".7z" | ".7z.exe"
    )
}

fn is_plain_file_ext(ext: &str) -> bool {
    matches!(
        ext.to_lowercase().as_str(),
        ".bat" | ".cmd" | ".ps1" | ".jar" | ".sh" | ".vbs" | ".py"
    )
}

/// `.tar.bz2` / `.tar.xz` (and the short `.tbz2` / `.txz`) downloads
/// normalize to `.tar` but have no in-process decoder — the 7z gate must
/// still demand the external tool for them.
fn is_bz2_xz_tar_name(download_name: &str) -> bool {
    let lower = download_name.to_ascii_lowercase();
    lower.ends_with(".tar.bz2")
        || lower.ends_with(".tbz2")
        || lower.ends_with(".tar.xz")
        || lower.ends_with(".txz")
}

/// with this implementation's documented in-process archive
/// divergences applied (see the module docs in [`crate::extractor`]): `.zip`,
/// `.nupkg` and the plain-tar family are read by `crate::ziparchive` /
/// `crate::tararchive` and never reach `7z.exe`, so they must not gate on
/// the external tool. returns `true` for every format because its own
/// ZIP/TAR extraction shells out to 7z.
fn install_needs_seven_zip(
    m: &Manifest,
    download_name: &str,
    file_ext: &str,
    install_arch: &str,
) -> bool {
    if is_plain_file_ext(file_ext) {
        return false;
    }
    match file_ext {
        ".zip" | ".nupkg" => false,
        ".tar" => {
            // Plain tar / tar.gz / tgz are read in-process (see
            // `crate::tararchive`); bz2/xz-compressed tarballs — which
            // collapses into ".tar" — still need 7z.
            is_bz2_xz_tar_name(download_name)
        }
        ".7z" | ".7z.exe" => true,
        // `!msiNeedsAdministrativeExtract`, i.e. no extract_dir → 7z extract.
        ".msi" => m.get_extract_dir_for_install(install_arch).is_empty(),
        ".msi_" => false,
        ".exe" => {
            if is_portable_exe_install_for(m, download_name, install_arch) {
                return false;
            }
            if m.innosetup {
                return false;
            }
            if m.has_installer_script_for_install(install_arch) {
                return false;
            }
            true
        }
        default => {
            if is_archive_install_ext(default) {
                return true;
            }
            !is_plain_file_ext(default)
        }
    }
}

/// against explicit inputs.
fn is_portable_exe_install_for(m: &Manifest, download_name: &str, install_arch: &str) -> bool {
    if !go_path_ext(download_name).eq_ignore_ascii_case(".exe") {
        return false;
    }
    if m.has_installer_script_for_install(install_arch) || !m.installer.args.is_empty() {
        return false;
    }
    if m.innosetup {
        return false;
    }
    if !m.get_extract_dir_for_install(install_arch).is_empty() {
        return false;
    }
    if pre_install_extracts_download_blob(m, install_arch) {
        return false;
    }
    let dl_base = go_base(download_name).to_lowercase();
    let bins = m.binaries();
    if bins.is_empty() {
        return true;
    }
    for pattern in &bins {
        let (bin_name, _) = parse_bin_pattern(pattern);
        if bin_name.is_empty() {
            continue;
        }
        if go_base(&bin_name).eq_ignore_ascii_case(&dl_base) {
            return true;
        }
    }
    bins.len() == 1
}

/// (including the dot; empty when no extension).
fn go_path_ext(p: &str) -> String {
    let base = p
        .trim_end_matches(['/', '\\'])
        .rsplit(['/', '\\'])
        .next()
        .unwrap_or(p);
    match base.rfind('.') {
        Some(i) if i > 0 => base[i..].to_string(),
        _ => String::new(),
    }
}

/// Backslash → forward slash (on Windows).
fn go_to_slash(s: &str) -> String {
    s.replace('\\', "/")
}

/// Walk every regular file under `dir`, invoking `on_file(path, metadata)`.
/// Unreadable entries and directories are skipped
fn walk_install_files<F>(dir: &Path, on_file: &mut F)
where
    F: FnMut(&Path, &fs::Metadata),
{
    let Ok(entries) = fs::read_dir(dir) else {
        return;
    };
    for entry in entries.flatten() {
        let path = entry.path();
        let Ok(file_type) = entry.file_type() else {
            continue;
        };
        // Regular entries: type and size come from the directory-enumeration
        // buffer, so a large tree is walked without a per-file open.
        if file_type.is_dir() {
            walk_install_files(&path, on_file);
            continue;
        }
        if file_type.is_file() {
            if let Ok(meta) = entry.metadata() {
                on_file(&path, &meta);
            }
            continue;
        }
        // A reparse point may resolve to a directory (junction); follow it
        // exactly as the previous unconditional `fs::metadata` did.
        let Ok(meta) = fs::metadata(&path) else {
            continue;
        };
        if meta.is_dir() {
            walk_install_files(&path, on_file);
        } else {
            on_file(&path, &meta);
        }
    }
}

// ---------------------------------------------------------------------------
// Tests (.go`)
// ---------------------------------------------------------------------------

#[cfg(test)]
mod tests {
    use super::*;
    use crate::testutil::TempDir;
    use serde_json::json;

    fn manifest_from_json(value: Value) -> Manifest {
        serde_json::from_value(value).expect("manifest json")
    }

    /// Parses a manifest from JSON text.
    fn manifest_from(json: &str) -> Manifest {
        crate::manifest::parse_str(json).expect("manifest json")
    }

    fn temp_dir(tag: &str) -> TempDir {
        TempDir::new(&format!("glue-install-test-{tag}"))
    }

    fn options_for(package: &str) -> InstallOptions {
        InstallOptions {
            package_ref: package.to_string(),
            version: String::new(),
            arch: String::new(),
            force: false,
            reuse_cache: false,
            timings: false,
            interactive: false,
            cancel: None,
            progress: None,
            independent: true,
        }
    }

    /// A token that is already flipped stops the pipeline before it touches
    /// the data root: no manifest lookup, no download, and the error stays
    /// recognizable as "cancelled" so a UI can park the task instead of
    /// reporting a failure.
    #[test]
    fn a_cancelled_token_stops_before_any_work() {
        let dir = temp_dir("cancel-before-start");
        let engine = crate::engine::Engine::new(dir.path()).expect("engine");
        let token = CancelToken::new();
        token.cancel();
        let options = InstallOptions {
            package_ref: "glue-cancel-demo".to_string(),
            cancel: Some(token),
            ..InstallOptions::default()
        };
        let err = engine
            .install(options, None)
            .expect_err("a cancelled install must not run");
        assert!(err.is_cancelled(), "{err:?}");
    }

    /// Phase wrapping must not turn a cancelled download into a network error:
    /// the retry hint would be wrong and the UI would show a failure.
    #[test]
    fn phase_wrapping_preserves_cancellation() {
        let cancelled = InstallError::Download(DownloadError::Cancelled);
        assert!(cancelled.is_cancelled());
        assert!(download_phase_error(3, &cancelled).is_cancelled());
        assert!(extract_phase_error(&cancelled).is_cancelled());
        let real = InstallError::Download(DownloadError::NoUrls);
        assert!(!real.is_cancelled());
        assert!(!download_phase_error(1, &real).is_cancelled());
    }

    /// A 403 is almost always a blocked location or proxy exit, so the hint
    /// names the route that actually gets the package installed.
    #[test]
    fn a_forbidden_download_explains_the_proxy_route() {
        let denied = InstallError::Download(DownloadError::Http {
            status: 403,
            status_text: "403 Forbidden".to_string(),
        });
        let rendered = download_phase_error(1, &denied).to_string();
        assert!(rendered.contains("403"), "{rendered}");
        assert!(rendered.contains("proxy"), "{rendered}");
        assert!(rendered.contains("proxy_packages"), "{rendered}");
        assert!(rendered.contains("github_proxy"), "{rendered}");
        // Installing the tool is this tool's job; never send users to npm.
        assert!(!rendered.contains("npm"), "{rendered}");

        // A 404 keeps its own, different hint.
        let missing = InstallError::Download(DownloadError::Http {
            status: 404,
            status_text: "404 Not Found".to_string(),
        });
        let rendered = download_phase_error(1, &missing).to_string();
        assert!(rendered.contains("does not exist"), "{rendered}");
    }

    // `install_many` must return exactly one slot per input, even for empty
    // input, and never touch the concurrent path with nothing to do.
    #[test]
    fn install_many_empty_input_yields_empty() {
        let dir = temp_dir("many-empty");
        let engine = crate::engine::Engine::new(dir.path()).expect("engine");
        let results = engine.install_many(Vec::new(), None::<crate::engine::progress::ProgressFn>);
        assert!(results.is_empty());
    }

    // is non-fail-fast: a missing package errors but does not
    // abort the others, and per-package results are reported in input order.
    // With no buckets present every ref fails at resolve, which exercises the
    // pass-1 slot assignment + ordering without the concurrent fetch/deploy.
    #[test]
    fn install_many_missing_packages_non_fatal_and_ordered() {
        let dir = temp_dir("many-missing");
        let engine = crate::engine::Engine::new(dir.path()).expect("engine");
        let results = engine
            .install_many(
                vec![
                    options_for("glue-does-not-exist-a"),
                    options_for("glue-does-not-exist-b"),
                    options_for("glue-does-not-exist-c"),
                ],
                None::<crate::engine::progress::ProgressFn>,
            )
            .into_iter()
            .map(|r| r.map(|o| o.package))
            .collect::<Vec<_>>();
        assert_eq!(results.len(), 3, "one result slot per input package");
        // Each unresolvable ref errors independently (non-fail-fast: the call
        // did not return early on the first failure).
        assert!(
            results.iter().all(|r| r.is_err()),
            "all missing packages must error, got {results:?}"
        );
    }

    fn write_file(path: &Path, bytes: &[u8]) {
        if let Some(parent) = path.parent() {
            fs::create_dir_all(parent).unwrap();
        }
        fs::write(path, bytes).unwrap();
    }

    fn hook_env<'a>(
        install_dir: &'a Path,
        persist_dir: &'a Path,
        hooks: &'a [String],
        seven_zip: &'a str,
        version: &'a str,
    ) -> HookScriptEnv<'a> {
        HookScriptEnv {
            install_dir,
            download_name: "artifact.exe",
            version,
            app: "tool",
            bucket: "main",
            buckets_dir: Path::new(""),
            persist_dir,
            arch: "64bit",
            glue_root: Path::new(""),
            seven_zip,
            dark: "",
            innounp: "",
            interactive: false,
            cmd: "",
            hooks,
        }
    }

    // `TestBuildInstallHookScript` (subset): the helper preamble must define
    // `Expand-7zipArchive` with the registered 7z path, and hook failures must
    // be terminating (`$ErrorActionPreference = 'Stop'`) — without it a missing
    // helper is a non-terminating error and the install "succeeds" with an
    // incomplete directory (the zoom `#/dl.7z` + `ZoomFull.cab` shape).
    #[test]
    fn build_hook_script_injects_sevenzip_helpers() {
        let dir = temp_dir("hookscript");
        let hooks = [r#"Expand-7zipArchive "$dir\ZoomFull.cab" -Removal"#.to_string()];
        let script = build_hook_script(&hook_env(
            &dir.join("apps/zoom/current"),
            &dir.join("persist/zoom"),
            &hooks,
            r"C:\glue\apps\7zip\current\7z.exe",
            "",
        ));
        assert!(script.contains("$ErrorActionPreference = 'Stop'"));
        assert!(script.contains(r#"$global:GLUE_7Z = 'C:\glue\apps\7zip\current\7z.exe'"#));
        assert!(script.contains("function Expand-7zipArchive"));
        assert!(script.contains("function abort"));
        assert!(script.contains("function ensure"));
        assert!(script.contains("function movedir"));
        assert!(script.contains("function Copy-Tree"));
        // `ScoopExpandMsiArchiveHelper` + `Invoke-ExternalCommand` are
        // unconditional; `Expand-InnoArchive` needs a resolved innounp.
        assert!(script.contains("function Expand-MsiArchive"));
        assert!(script.contains("function Invoke-ExternalCommand"));
        assert!(script.contains("function Add-Path"));
        assert!(!script.contains("Expand-InnoArchive"));
        let install_dir = dir.join("apps/zoom/current").display().to_string();
        assert!(script.contains(&format!("$dir = '{install_dir}'")));
        assert!(script.contains(r#"Expand-7zipArchive "$dir\ZoomFull.cab" -Removal"#));
    }

    #[test]
    fn build_hook_script_omits_sevenzip_helper_without_path() {
        let dir = temp_dir("hookscript-no7z");
        let hooks = ["Write-Host hi".to_string()];
        let script = build_hook_script(&hook_env(
            &dir.join("apps/tool/current"),
            &dir.join("persist/tool"),
            &hooks,
            "",
            "",
        ));
        assert!(!script.contains("Expand-7zipArchive"));
        assert!(!script.contains("GLUE_7Z"));
        // Base helpers are still injected — plain hooks use them too.
        assert!(script.contains("function abort"));
        assert!(script.contains("function ensure"));
        assert!(script.contains("$ErrorActionPreference = 'Stop'"));
    }

    #[test]
    fn build_hook_script_escapes_single_quotes_in_7z_path() {
        let dir = temp_dir("hookscript-escape");
        let hooks = ["Write-Host hi".to_string()];
        let script = build_hook_script(&hook_env(&dir, &dir, &hooks, r"C:\o'brien\7z.exe", ""));
        assert!(script.contains(r#"$global:GLUE_7Z = 'C:\o''brien\7z.exe'"#));
    }

    /// `glue install --interactive` reaches the hooks through
    /// `$glue_interactive`: a manifest skips its silent switches with
    /// `if (!$interactive) { $args += '/S' }`, and the preamble defaults that
    /// variable to `$false` when nothing injected it.
    #[test]
    fn build_hook_script_marks_interactive_hooks() {
        let dir = temp_dir("hookscript-interactive");
        let hooks = ["Write-Host hi".to_string()];
        let mut env = hook_env(&dir, &dir, &hooks, "", "");
        let unattended = build_hook_script(&env);
        assert!(unattended.contains("$glue_interactive = $false;"));
        assert!(unattended.contains("$interactive = $false;"));
        assert!(!unattended.contains("$glue_interactive = $true;"));

        env.interactive = true;
        let attended = build_hook_script(&env);
        assert!(attended.contains("$glue_interactive = $true;"));
        // A manifest tests `$interactive` directly, so it must exist at the
        // script scope — not only inside `Invoke-ExternalCommand`.
        assert!(attended.contains("$interactive = $true;"));
        // The preamble only upgrades `$interactive` when the variable exists.
        assert!(attended.contains("if (Get-Variable -Name glue_interactive"));
    }

    /// A pinned version the bucket cannot produce must say which one of the
    /// three situations it is in: each has a different one-line fix, and a
    /// bare "not found" would read like a typo.
    #[test]
    fn pinned_version_gap_names_the_fix() {
        use crate::bucket_history::NoHistory;

        let mut st = InstallState::new("main/demo");
        st.pin_version = "1.0.0".to_string();

        let shallow = pinned_version_gap_error(&st, NoHistory::Shallow).to_string();
        assert!(shallow.contains("shallow clone"), "{shallow}");
        assert!(shallow.contains("glue bucket deepen main"), "{shallow}");

        let plain = pinned_version_gap_error(&st, NoHistory::NotACheckout).to_string();
        assert!(plain.contains("glue bucket add main"), "{plain}");

        let no_git = pinned_version_gap_error(&st, NoHistory::GitUnavailable).to_string();
        assert!(no_git.contains("git is not available"), "{no_git}");
    }

    // `installer.script` runs through the same builder
    // so it must carry `$version` — and must not
    // emit an empty `$version = ''` when the caller has none.
    #[test]
    fn build_hook_script_injects_version_only_when_present() {
        let dir = temp_dir("hookscript-version");
        let hooks = ["Write-Host $version".to_string()];
        let with_version = build_hook_script(&hook_env(&dir, &dir, &hooks, "", "1.2.3"));
        assert!(with_version.contains("$version = '1.2.3'"));

        let plain = ["Write-Host hi".to_string()];
        let without = build_hook_script(&hook_env(&dir, &dir, &plain, "", ""));
        assert!(!without.contains("$version = "));
    }

    // dark/inno helpers appear only when
    // their tool resolved; the paths are single-quote-escaped in the globals.
    #[test]
    fn build_hook_script_injects_dark_and_innounp_only_when_resolved() {
        let dir = temp_dir("hookscript-dark-inno");
        let hooks = ["Expand-DarkArchive \"$dir\\setup.exe\"".to_string()];
        let mut env = hook_env(&dir, &dir, &hooks, r"C:\7z.exe", "1.0");
        env.dark = r"C:\glue\bin\wix\dark.exe";
        env.innounp = r"C:\o'brien\innounp.exe";
        let script = build_hook_script(&env);
        assert!(script.contains(r#"$global:GLUE_DARK = 'C:\glue\bin\wix\dark.exe'"#));
        assert!(script.contains("function Expand-DarkArchive"));
        assert!(script.contains(r#"$global:GLUE_INNOUNP = 'C:\o''brien\innounp.exe'"#));
        assert!(script.contains("function Get-HelperPath"));
        assert!(script.contains("function Expand-InnoArchive"));

        let plain = ["Write-Host hi".to_string()];
        let without = build_hook_script(&hook_env(&dir, &dir, &plain, "", ""));
        assert!(!without.contains("GLUE_DARK"));
        assert!(!without.contains("Expand-DarkArchive"));
        assert!(!without.contains("GLUE_INNOUNP"));
        assert!(!without.contains("Expand-InnoArchive"));
    }

    // `BuildInstallHookScript`: the Scoop env vars and the
    // Push-Location/trap wrapper around the hook body.
    #[test]
    fn build_hook_script_sets_scoop_env_vars() {
        let dir = temp_dir("hookscript-env");
        let hooks = ["Write-Host hi".to_string()];
        let install_dir = dir.join("apps/tool/1.0");
        let persist_dir = dir.join("persist/tool");
        let base = hook_env(&install_dir, &persist_dir, &hooks, "", "9.9");
        let env = HookScriptEnv {
            buckets_dir: &dir.join("buckets"),
            bucket: "extras",
            arch: "arm64",
            download_name: "setup.exe",
            ..base
        };
        let script = build_hook_script(&env);
        assert!(script.contains("$app = 'tool'"));
        assert!(script.contains("$bucket = 'extras'"));
        assert!(script.contains(&format!(
            "$bucketsdir = '{}'",
            dir.join("buckets").display()
        )));
        assert!(script.contains(&format!(
            "$persist_dir = '{}'",
            dir.join("persist/tool").display()
        )));
        assert!(script.contains("$architecture = 'arm64'"));
        assert!(script.contains("$fname = 'setup.exe'"));
        assert!(script.contains("$global = $false"));
        assert!(script.contains("$original_dir = "));
        assert!(script.contains("Push-Location -LiteralPath $dir"));
        assert!(script.contains("trap { Pop-Location; $env:PATH = $glue_old_path; throw $_ }"));
        assert!(script
            .trim_end()
            .ends_with("Pop-Location; $env:PATH = $glue_old_path"));
    }

    // `... 2>$null` gets the Continue/Stop
    // wrapper so stderr noise cannot abort under Stop.
    #[test]
    fn build_hook_script_patches_optional_error_lines() {
        let dir = temp_dir("hookscript-optional");
        let hooks = ["Get-Process zoom 2>$null # ignore missing".to_string()];
        let script = build_hook_script(&hook_env(&dir, &dir, &hooks, "", ""));
        assert!(script.contains(
            "$ErrorActionPreference = 'Continue'; Get-Process zoom 2>$null; $ErrorActionPreference = 'Stop' # ignore missing"
        ));
        // Lines without the suppression stay untouched.
        let plain = ["Write-Host hi".to_string()];
        let script = build_hook_script(&hook_env(&dir, &dir, &plain, "", ""));
        assert!(script.contains("Write-Host hi"));
        assert!(!script.contains("$ErrorActionPreference = 'Continue'"));
    }

    // fragile wildcard Copy-Item becomes Copy-Tree.
    #[test]
    fn build_hook_script_patches_recurse_copy_item() {
        let dir = temp_dir("hookscript-copytree");
        let hooks = [r#"Copy-Item "$dir\app\*" "$persist_dir\app" -Recurse"#.to_string()];
        let script = build_hook_script(&hook_env(&dir, &dir, &hooks, "", ""));
        assert!(script.contains(r#"Copy-Tree "$dir\app" "$persist_dir\app""#));
        assert!(!script.contains(r#"Copy-Item "$dir\app\*""#));
    }

    // The three uninstall phases share `$cmd = "uninstall"`.
    #[test]
    fn uninstall_hooks_are_grouped_and_gated() {
        use crate::manifest::Manifest;
        let m: Manifest = manifest::parse_str(
            r#"{
                "version": "1.0.0",
                "url": "https://example.com/a.zip",
                "hash": "h",
                "pre_uninstall": "Stop-Service foo",
                "uninstaller": {"script": ["taskkill /IM foo.exe", "reg delete HKCU\\Foo /f"]},
                "post_uninstall": "Remove-Item env:FOO"
            }"#,
        )
        .unwrap();
        let arch = "64bit";
        assert_eq!(m.pre_uninstall_hooks_for_install(arch).len(), 1);
        assert_eq!(m.uninstaller_script_hooks_for_install(arch).len(), 2);
        assert_eq!(m.post_uninstall_hooks_for_install(arch).len(), 1);
    }

    /// A `--force` reinstall of a version that is already on disk is an
    /// uninstall followed by an install, so the manifest's uninstaller has to
    /// run first. Manifests keep state outside `$dir` — `86box-roms` links
    /// `apps/86box/current/roms` at its version dir and PowerShell refuses to
    /// replace a junction whose target is still populated — so skipping this
    /// makes those packages impossible to reinstall.
    #[cfg(windows)]
    #[test]
    fn force_reinstall_runs_the_uninstaller_before_deploy() {
        let pkg = "glue-force-reinstall";
        let (root, services, marker) = seed_installer_state(pkg, "force-reinstall-uninstaller");
        let m = manifest_from(
            r#"{"version":"1.0.0","url":"https://e/x.zip","hash":"h",
                "uninstaller":{"script":"Remove-Item \"$dir\\installer-state.txt\" -Force"}}"#,
        );
        let mut st = InstallState {
            pkg_ref: format!("main/{pkg}"),
            pkg_name: pkg.to_string(),
            manifest: Some(m.clone()),
            install_arch: "64bit".into(),
            force: true,
            ..InstallState::default_for_test()
        };

        // Deploy itself fails afterwards (`no download results`); the point of
        // the assertion is which state the version dir is left in.
        let _ = deploy_install_phase(&services, &mut st, &m, &|_: ProgressEvent| {});
        assert!(
            !marker.exists(),
            "the uninstaller script must run before the version dir is redeployed"
        );
        assert!(root.path().exists());
    }

    /// An update whose version is unchanged just relinks the active version
    /// (`reuse_cache` + same version): there is no installed copy being
    /// replaced, so the hooks must not run.
    #[cfg(windows)]
    #[test]
    fn relink_of_the_same_version_does_not_run_the_uninstaller() {
        let pkg = "glue-force-update";
        let (root, services, marker) = seed_installer_state(pkg, "cached-force-update");
        let m = manifest_from(
            r#"{"version":"1.0.0","url":"https://e/x.zip","hash":"h",
                "uninstaller":{"script":"Remove-Item \"$dir\\installer-state.txt\" -Force"}}"#,
        );
        let mut st = InstallState {
            pkg_ref: format!("main/{pkg}"),
            pkg_name: pkg.to_string(),
            manifest: Some(m.clone()),
            install_arch: "64bit".into(),
            force: true,
            reuse_cache: true,
            ..InstallState::default_for_test()
        };

        let _ = deploy_install_phase(&services, &mut st, &m, &|_: ProgressEvent| {});
        assert!(
            marker.exists(),
            "an update must not unwind the version it is about to relink"
        );
        assert!(root.path().exists());
    }

    /// The update path sets `reuse_cache` too, and an update that lands on a new
    /// version does replace the installed copy: Scoop uninstalls the old version
    /// first for exactly the same reason.
    #[cfg(windows)]
    #[test]
    fn update_to_a_new_version_runs_the_installed_hooks() {
        let pkg = "glue-update-hooks";
        let (root, services, marker) = seed_installer_state(pkg, "update-new-version");
        let m = manifest_from(
            r#"{"version":"2.0.0","url":"https://e/x.zip","hash":"h",
                "uninstaller":{"script":"Remove-Item \"$dir\\installer-state.txt\" -Force"}}"#,
        );
        let mut st = InstallState {
            pkg_ref: format!("main/{pkg}"),
            pkg_name: pkg.to_string(),
            manifest: Some(m.clone()),
            install_arch: "64bit".into(),
            force: true,
            reuse_cache: true,
            ..InstallState::default_for_test()
        };

        let _ = deploy_install_phase(&services, &mut st, &m, &|_: ProgressEvent| {});
        assert!(
            !marker.exists(),
            "an update replaces the installed copy, so its hooks must run"
        );
        assert!(root.path().join("apps").join(pkg).join("2.0.0").is_dir());
    }

    /// The hooks come from the *installed* manifest (Scoop's `$old_manifest`),
    /// not from the version the bucket ships now: force-installing a new
    /// version has to unwind the state the old hooks created.
    #[cfg(windows)]
    #[test]
    fn force_install_over_another_version_uses_the_installed_hooks() {
        let pkg = "glue-force-bump";
        let root = temp_dir("force-install-old-hooks");
        let installed_dir = root.path().join("apps").join(pkg).join("1.0.0");
        fs::create_dir_all(&installed_dir).unwrap();
        let marker = installed_dir.join("installer-state.txt");
        fs::write(&marker, b"state left behind by the installer script").unwrap();
        apps::save_install_record_with_arch(
            &installed_dir,
            "main",
            "1.0.0",
            "64bit",
            Some(serde_json::json!({
                "version": "1.0.0",
                "url": "https://e/x.zip",
                "hash": "h",
                "uninstaller": {"script": "Remove-Item \"$dir\\installer-state.txt\" -Force"}
            })),
        )
        .unwrap();

        let store = Store::new(root.path().join("store")).unwrap();
        let services = test_services(root.path(), store);
        // The target manifest has no uninstaller, so only the installed
        // copy's hooks can remove the marker.
        let m = manifest_from(r#"{"version":"2.0.0","url":"https://e/x.zip","hash":"h"}"#);
        let mut st = InstallState {
            pkg_ref: format!("main/{pkg}"),
            pkg_name: pkg.to_string(),
            manifest: Some(m.clone()),
            install_arch: "64bit".into(),
            force: true,
            ..InstallState::default_for_test()
        };

        let _ = deploy_install_phase(&services, &mut st, &m, &|_: ProgressEvent| {});
        assert!(
            !marker.exists(),
            "the installed version's uninstaller must run, not the target manifest's"
        );
        assert!(root.path().join("apps").join(pkg).join("2.0.0").is_dir());
    }

    /// Pre-seeds `apps/<pkg>/1.0.0` with `installer-state.txt`, the file the
    /// uninstaller scripts above delete. Every test uses its own package name:
    /// the hook runner writes `%TEMP%\glue-<kind>-<app>.ps1`, so two tests
    /// running in parallel with the same name delete each other's script.
    /// Returns `(root, services, marker)`; the caller keeps the returned guard
    /// alive for the duration of the test.
    #[cfg(windows)]
    fn seed_installer_state(pkg: &str, tag: &str) -> (TempDir, Services, PathBuf) {
        let root = temp_dir(tag);
        let install_dir = root.path().join("apps").join(pkg).join("1.0.0");
        fs::create_dir_all(&install_dir).unwrap();
        let marker = install_dir.join("installer-state.txt");
        fs::write(&marker, b"state left behind by the installer script").unwrap();
        let store = Store::new(root.path().join("store")).unwrap();
        let services = test_services(root.path(), store);
        (root, services, marker)
    }

    /// Only hooks that stop a service or kill a process need the settle delay.
    #[test]
    fn settle_time_needs_patterns() {
        assert!(uninstall_hooks_need_settle_time(&[
            "Stop-Process foo".to_string()
        ]));
        assert!(uninstall_hooks_need_settle_time(&[
            "uninstall-system-daemon foo".to_string()
        ]));
        assert!(!uninstall_hooks_need_settle_time(&[
            "Remove-Item \"$dir\\x\"".to_string()
        ]));
        // Case-insensitive.ToLower` body.
        assert!(uninstall_hooks_need_settle_time(&[
            "SC.EXE STOP foo".to_string()
        ]));
    }

    /// `$fname` for the uninstall phases comes from the first install URL.
    #[test]
    fn download_name_from_manifest_uses_first_url() {
        let m = manifest::parse_str(
            r#"{
                "version": "1.0.0",
                "url": ["https://example.com/archive.zip#/renamed.7z", "https://mirror/a.zip"],
                "hash": ["h1", "h2"]
            }"#,
        )
        .unwrap();
        assert_eq!(download_name_from_manifest(&m, "64bit"), "renamed.7z");
    }

    /// `mklink` / `rmdir` treat `/` as an option prefix, so the command-line
    /// form must never carry forward slashes.
    #[test]
    fn cmd_path_arg_uses_backslashes_only() {
        assert_eq!(
            crate::procutil::cmd_path_arg(std::path::Path::new("C:/a b/pkg/1.0.0/data")),
            "C:\\a b\\pkg\\1.0.0\\data"
        );
        // Already-correct paths pass through unchanged.
        assert_eq!(
            crate::procutil::cmd_path_arg(std::path::Path::new("C:\\a\\b")),
            "C:\\a\\b"
        );
    }

    #[test]
    fn resolve_dark_prefers_bin_wix_and_requires_dll_for_dark_exe() {
        let root = temp_dir("resolve-dark");
        // Lone dark.exe (no wix.dll) is not usable.
        let lone = root.join("shims");
        std::fs::create_dir_all(&lone).unwrap();
        std::fs::write(lone.join("dark.exe"), b"MZ").unwrap();
        assert_eq!(resolve_dark(root.path()), None);
        // dark.exe + wix.dll beside it becomes usable.
        std::fs::write(lone.join("wix.dll"), b"DLL").unwrap();
        assert_eq!(
            resolve_dark(root.path()),
            Some(lone.join("dark.exe").to_string_lossy().into_owned())
        );
        // bin/wix/wix.exe outranks the shims candidate.
        let bin_wix = root.join("bin").join("wix");
        std::fs::create_dir_all(&bin_wix).unwrap();
        std::fs::write(bin_wix.join("wix.exe"), b"MZ").unwrap();
        assert_eq!(
            resolve_dark(root.path()),
            Some(bin_wix.join("wix.exe").to_string_lossy().into_owned())
        );
    }

    // bin/innounp first, then apps/<ver>.
    #[test]
    fn resolve_innounp_prefers_bin_then_apps_version() {
        let root = temp_dir("resolve-innounp");
        // apps/innounp/<version>/innounp.exe
        let app_dir = root.join("apps").join("innounp").join("2.0");
        std::fs::create_dir_all(&app_dir).unwrap();
        std::fs::write(app_dir.join("innounp.exe"), b"MZ").unwrap();
        assert_eq!(
            resolve_innounp(root.path()),
            Some(app_dir.join("innounp.exe").to_string_lossy().into_owned())
        );
        // bin/innounp/innounp.exe outranks it.
        let bin_dir = root.join("bin").join("innounp");
        std::fs::create_dir_all(&bin_dir).unwrap();
        std::fs::write(bin_dir.join("innounp.exe"), b"MZ").unwrap();
        assert_eq!(
            resolve_innounp(root.path()),
            Some(bin_dir.join("innounp.exe").to_string_lossy().into_owned())
        );
    }

    #[test]
    fn helper_need_detectors_match_helper_calls() {
        assert!(hook_helpers_need_dark(&[
            "Expand-DarkArchive \"$dir\\a.exe\"".to_string()
        ]));
        assert!(!hook_helpers_need_dark(&[
            "Expand-7zipArchive \"$dir\\a.exe\"".to_string()
        ]));
        assert!(installer_script_needs_innounp(&[
            "Expand-InnoArchive \"$dir\\a.exe\"".to_string()
        ]));
        assert!(!installer_script_needs_innounp(&[
            "Write-Host hi".to_string()
        ]));
        assert!(hook_helpers_need_7z(&[
            "expand-7ziparchive \"x\"".to_string()
        ]));
    }

    // `TestNormalizeInstallFileExt`.
    #[test]
    fn normalize_install_file_ext_cases() {
        let cases = [
            (".tar.gz", ".tar"),
            (".tar.bz2", ".tar"),
            (".tar.xz", ".tar"),
            (".tgz", ".tar"),
            (".tar", ".tar"),
            (".zip", ".zip"),
            (".exe", ".exe"),
        ];
        for (input, want) in cases {
            assert_eq!(normalize_install_file_ext(input), want, "input {input}");
        }
    }

    // `TestInstallNeedsSevenZip` — one deliberate divergence: the gate returns
    // `false` for zip, because ZIP is extracted in-process (see the notes in
    // `crate::extractor`); returning `true` would hard-require 7z for every zip
    // package.
    #[test]
    fn install_needs_seven_zip_cases() {
        let zip_m = manifest_from_json(json!({ "version": "1" }));
        assert!(!install_needs_seven_zip(&zip_m, "app.zip", ".zip", ""));
        assert!(!install_needs_seven_zip(&zip_m, "app.nupkg", ".nupkg", ""));

        let tar_m = manifest_from_json(json!({ "version": "1" }));
        assert!(!install_needs_seven_zip(&tar_m, "app.tar.gz", ".tar", ""));
        assert!(!install_needs_seven_zip(&tar_m, "app.tgz", ".tar", ""));
        assert!(!install_needs_seven_zip(&tar_m, "app.tar", ".tar", ""));
        // bz2/xz-compressed tarballs normalize to ".tar" but have no
        // in-process decoder — the gate must still demand 7z. The short
        // .tbz2/.txz spellings are recognized too.
        assert!(install_needs_seven_zip(&tar_m, "app.tar.bz2", ".tar", ""));
        assert!(install_needs_seven_zip(&tar_m, "app.tar.xz", ".tar", ""));
        assert!(install_needs_seven_zip(&tar_m, "app.tbz2", ".tar", ""));
        assert!(install_needs_seven_zip(&tar_m, "app.txz", ".tar", ""));

        let sevenz_m = manifest_from_json(json!({ "version": "1" }));
        assert!(install_needs_seven_zip(&sevenz_m, "app.7z", ".7z", ""));

        let portable = manifest_from_json(json!({ "version": "1", "bin": ["app.exe"] }));
        assert!(!install_needs_seven_zip(&portable, "app.exe", ".exe", ""));

        let inno = manifest_from_json(json!({ "version": "1", "innosetup": true }));
        assert!(!install_needs_seven_zip(&inno, "setup.exe", ".exe", ""));

        let msi_admin = manifest_from_json(json!({ "version": "1", "extract_dir": "files" }));
        assert!(!install_needs_seven_zip(
            &msi_admin,
            "setup.msi",
            ".msi",
            ""
        ));
    }

    /// `innosetup: true` is unpacked, not run — but only for an `.exe` download
    /// that has no `installer.script` of its own. A script is a hand-written
    /// replacement for the extraction, and Scoop prefers it too.
    #[test]
    fn uses_inno_extraction_cases() {
        let mut st = InstallState::default_for_test();
        st.file_ext = ".exe".to_string();
        st.manifest = Some(manifest_from_json(
            json!({ "version": "1", "innosetup": true }),
        ));
        assert!(uses_inno_extraction(&st));

        // Not flagged: an ordinary `.exe` keeps the portable / SFX routing.
        st.manifest = Some(manifest_from_json(json!({ "version": "1" })));
        assert!(!uses_inno_extraction(&st));

        st.manifest = Some(manifest_from_json(json!({
            "version": "1",
            "innosetup": true,
            "installer": { "script": ["Expand-7zipArchive \"$dir\\setup.exe\" \"$dir\""] }
        })));
        assert!(
            !uses_inno_extraction(&st),
            "an explicit installer.script replaces the built-in extraction"
        );

        // Only an `.exe` download is an Inno Setup installer.
        st.manifest = Some(manifest_from_json(
            json!({ "version": "1", "innosetup": true }),
        ));
        st.file_ext = ".zip".to_string();
        assert!(!uses_inno_extraction(&st));
    }

    /// `appdir`/`installed` strip a `bucket/name` prefix before building the
    /// path. The separator class must be `[/\\]`: a bare `'/|\'` is an
    /// *invalid* .NET regex, so PowerShell aborts the installer script with
    /// `parsing "/|\" - Illegal \ at end of pattern` (seen with
    /// `86box-roms`, whose `installer.script` calls `appdir`).
    #[test]
    fn appdir_helpers_split_on_both_path_separators() {
        let helpers = scoop_appdir_helpers();
        assert!(
            !helpers.contains(r"'/|\'"),
            "the separator class needs the escaped backslash"
        );
        assert_eq!(
            helpers.matches(r"($app -split '[/\\]')[-1]").count(),
            2,
            "both `appdir` and `installed` strip the bucket prefix"
        );
    }

    /// The extraction must run through the hook helper with the installer in
    /// place: same `Expand-InnoArchive -Removal` call a manifest would write, and
    /// no 7-Zip requirement — the inno route never unpacks anything else.
    #[test]
    fn inno_extract_script_uses_the_expand_inno_helper() {
        let dir = temp_dir("inno-script");
        let install_dir = dir.join("apps/demo/1.0.0");
        std::fs::create_dir_all(&install_dir).unwrap();
        let hooks = vec![INNO_EXTRACT_HOOK.to_string()];
        let env = HookScriptEnv {
            install_dir: &install_dir,
            download_name: "setup.exe",
            version: "1.0.0",
            app: "demo",
            bucket: "main",
            buckets_dir: &dir.join("buckets"),
            persist_dir: &dir.join("persist/demo"),
            arch: "64bit",
            glue_root: dir.path(),
            seven_zip: "",
            dark: "",
            innounp: r"C:\glue\bin\innounp\innounp.exe",
            interactive: false,
            cmd: "",
            hooks: &hooks,
        };
        let script = build_hook_script(&env);
        assert!(
            script.contains(
                r#"Expand-InnoArchive -Path "$dir\$fname" -DestinationPath "$dir" -Removal"#
            ),
            "the helper must target the linked installer and drop it afterwards"
        );
        assert!(script.contains("function Expand-InnoArchive"));
        assert!(script.contains(r"innounp\innounp.exe"));
        assert!(
            !script.contains("function Expand-7zipArchive"),
            "no 7-Zip helper for a package that only unpacks an Inno installer"
        );
        assert!(!script.contains("function Expand-DarkArchive"));
    }

    /// A resolved `innounp.exe` is used as-is: the inno route must not hit the
    /// network when the helper is already installed.
    #[test]
    fn ensure_innounp_for_extract_reuses_the_installed_helper() {
        let dir = temp_dir("inno-innounp");
        let bin = dir.join("bin").join("innounp").join("innounp.exe");
        std::fs::create_dir_all(bin.parent().unwrap()).unwrap();
        std::fs::write(&bin, b"MZ").unwrap();
        let services = Services::new(crate::engine::services::EngineConfig::new(dir.path()))
            .expect("services");
        let mut timings = Timings::off();
        assert_eq!(
            ensure_innounp_for_extract(&services, "demo", &mut timings).unwrap(),
            bin.to_string_lossy()
        );
    }

    /// Installing the helper itself must not recurse into downloading the helper.
    #[test]
    fn ensure_innounp_refuses_to_bootstrap_itself() {
        let dir = temp_dir("inno-self");
        let services = Services::new(crate::engine::services::EngineConfig::new(dir.path()))
            .expect("services");
        let mut timings = Timings::off();
        let err = ensure_innounp_for_extract(&services, "innounp", &mut timings).unwrap_err();
        assert!(err.to_string().contains("innounp.exe not found"), "{err}");
    }

    // `TestFindCacheArchiveHash`.
    #[test]
    fn find_cache_archive_hash_cases() {
        let hash = "9d590a7beb702d6d54678a3f1766e1d7cdc2b9faa577f507c26e76cd625a687d";
        let mut entry = PackageEntry {
            version: String::new(),
            installed: String::new(),
            files: BTreeMap::new(),
            size: 0,
        };
        entry
            .files
            .insert(hash.to_string(), "upm_1.0_windows_amd64.tar.gz".into());
        assert_eq!(
            find_cache_archive_hash(&entry, "upm_1.0_windows_amd64.tar.gz", hash),
            hash
        );

        let mut wrong_name = PackageEntry {
            version: String::new(),
            installed: String::new(),
            files: BTreeMap::new(),
            size: 0,
        };
        wrong_name.files.insert(
            hash.to_string(),
            "590a7beb702d6d54678a3f1766e1d7cdc2b9faa577f507c26e76cd625a687d".into(),
        );
        assert_eq!(
            find_cache_archive_hash(&wrong_name, "upm_1.0_windows_amd64.tar.gz", hash),
            hash
        );

        let digest = "6c6c52a4b2648e179f917bdaa7c57e793d18561b380a8bfa025f10cd1b9b2ad1";
        let mut by_digest = PackageEntry {
            version: String::new(),
            installed: String::new(),
            files: BTreeMap::new(),
            size: 0,
        };
        by_digest.files.insert("caskey".into(), digest.into());
        assert_eq!(
            find_cache_archive_hash(&by_digest, "yazi-x86_64-pc-windows-msvc.zip", digest),
            "caskey"
        );
    }

    // `TestInstallExtractDest`.
    #[test]
    fn install_extract_dest_cases() {
        let root = temp_dir("extract-dest");
        let m = manifest_from_json(json!({ "version": "1", "extract_to": "IDE" }));
        assert_eq!(
            install_extract_dest(root.path(), &m, ""),
            root.path().join("IDE")
        );

        let empty = manifest_from_json(json!({ "version": "1" }));
        assert_eq!(install_extract_dest(root.path(), &empty, ""), root.path());

        let escape = manifest_from_json(json!({ "version": "1", "extract_to": "../escape" }));
        // Unsafe extract_to falls back to the install dir (safepath rejects it).
        assert_eq!(install_extract_dest(root.path(), &escape, ""), root.path());
    }

    /// A preflight that cannot fit must fail before any extraction work.
    #[cfg(windows)]
    #[test]
    fn preflight_rejects_a_volume_that_cannot_hold_the_payload() {
        let root = temp_dir("disk-preflight");
        let err = ensure_free_space(root.path(), u64::MAX / 2, "demo", "demo.zip")
            .expect_err("an impossible payload must be rejected");
        let text = err.to_string();
        assert!(text.contains("insufficient disk space"), "{text}");
        assert!(text.contains("Free up space"), "{text}");
    }

    // `TestCleanInstallDir`.
    #[test]
    fn clean_install_dir_removes_children() {
        let dir = temp_dir("clean");
        write_file(&dir.path().join("stale"), b"x");
        clean_install_dir(dir.path()).unwrap();
        let count = fs::read_dir(dir.path()).unwrap().count();
        assert_eq!(count, 0);
    }

    /// A read-only file at the top of the version dir used to fail the clean
    /// with "Access is denied" (the subdirectory branch already retried after
    /// clearing the bit), which is how a version switch reported that error.
    #[test]
    fn clean_install_dir_clears_read_only_files() {
        let dir = temp_dir("clean-readonly");
        let file = dir.path().join("locked.exe");
        write_file(&file, b"x");
        let mut perms = fs::metadata(&file).unwrap().permissions();
        perms.set_readonly(true);
        fs::set_permissions(&file, perms).unwrap();
        clean_install_dir(dir.path()).unwrap();
        assert_eq!(fs::read_dir(dir.path()).unwrap().count(), 0);
    }

    // `TestFlattenExtractDir_backslashManifestPath`.
    #[test]
    fn flatten_extract_dir_backslash_manifest_path() {
        let root = temp_dir("flatten-backslash");
        let install_dir = root.path().join("apps").join("pdfsam").join("6.0.1");
        let nested = install_dir
            .join("pdfsam-basic-6.0.1-windows-x64")
            .join("pdfsam");
        write_file(&nested.join("pdfsam.exe"), b"exe");

        flatten_extract_dir(&install_dir, r"pdfsam-basic-6.0.1-windows-x64\pdfsam").unwrap();
        assert!(install_dir.join("pdfsam.exe").exists());
    }

    // `TestFlattenExtractDir_browserCaseCollision`.
    #[test]
    fn flatten_extract_dir_browser_case_collision() {
        let install_dir = temp_dir("flatten-case");
        let browser_dir = install_dir.path().join("Browser");
        let browser_sub = browser_dir.join("browser");
        write_file(&browser_dir.join("firefox.exe"), b"exe");
        write_file(&browser_sub.join("omni.ja"), b"ja");

        flatten_extract_dir(install_dir.path(), "Browser").unwrap();
        assert!(install_dir.path().join("firefox.exe").exists());
        assert!(install_dir.path().join("browser").join("omni.ja").exists());
    }

    // `TestApplyExtractDirLayout_torBrowserWrapper`.
    #[test]
    fn apply_extract_dir_layout_tor_browser_wrapper() {
        let install_dir = temp_dir("tor-browser");
        let wrapper = install_dir.path().join("Tor Browser");
        let browser = wrapper.join("Browser");
        let tor_data = wrapper.join("TorBrowser").join("Data");
        fs::create_dir_all(&browser).unwrap();
        fs::create_dir_all(&tor_data).unwrap();
        write_file(&browser.join("firefox.exe"), b"exe");
        write_file(&browser.join("browser").join("omni.ja"), b"ja");

        let applied = apply_extract_dir_layout(install_dir.path(), "Browser").unwrap();
        assert!(applied);
        assert!(install_dir.path().join("firefox.exe").exists());
        assert!(install_dir.path().join("TorBrowser").join("Data").exists());

        let m = manifest_from_json(json!({
            "version": "15.0.15",
            "extract_dir": "Browser",
            "bin": [["firefox.exe", "tor-browser"]],
        }));
        validate_manifest_bins(install_dir.path(), &m).unwrap();
    }

    // `TestValidateManifestBins_jetbrainsLayout`.
    #[test]
    fn validate_manifest_bins_jetbrains_layout() {
        let install_dir = temp_dir("jetbrains");
        write_file(
            &install_dir
                .path()
                .join("IDE")
                .join("bin")
                .join("pycharm64.exe"),
            b"exe",
        );
        let m = manifest_from_json(json!({
            "version": "2026.1.2",
            "bin": [[r"IDE\bin\pycharm64.exe", "pycharm"]],
        }));
        validate_manifest_bins(install_dir.path(), &m).unwrap();
    }

    // A missing bin must say what the tree actually contains: a flat tree
    // with one absent file and a tree still wrapped in its archive folder
    // need different fixes.
    #[test]
    fn validate_manifest_bins_reports_the_installed_tree() {
        let install_dir = temp_dir("missing-bin");
        write_file(&install_dir.path().join("AIMP").join("readme.txt"), b"x");
        write_file(&install_dir.path().join("unins000.dat"), b"x");
        let m = manifest_from_json(json!({ "version": "5.40.2730", "bin": ["AIMP.exe"] }));

        let err = validate_manifest_bins(install_dir.path(), &m).unwrap_err();
        let msg = err.to_string();
        assert!(msg.contains("AIMP.exe"), "{msg}");
        assert!(msg.contains("install dir holds:"), "{msg}");
        assert!(msg.contains("AIMP/"), "{msg}");
    }

    // `TestEnsureExtractDirFlattened_*` — nested bin layout gets flattened.
    #[test]
    fn ensure_extract_dir_flattened_nested_bin() {
        let install_dir = temp_dir("ensure-flatten");
        // Bin is nested one directory deep.
        write_file(&install_dir.path().join("app-1.0").join("app.exe"), b"exe");
        let m = manifest_from_json(json!({ "version": "1.0", "bin": ["app.exe"] }));

        let flattened = ensure_extract_dir_flattened(install_dir.path(), &m, "").unwrap();
        assert!(flattened);
        assert!(install_dir.path().join("app.exe").exists());
        validate_manifest_bins(install_dir.path(), &m).unwrap();
    }

    // `TestEnsureExtractDirFlattened_alreadyFlat`.
    #[test]
    fn ensure_extract_dir_flattened_already_flat() {
        let install_dir = temp_dir("ensure-flat");
        write_file(&install_dir.path().join("app.exe"), b"exe");
        let m = manifest_from_json(json!({ "version": "1.0", "bin": ["app.exe"] }));
        assert!(!ensure_extract_dir_flattened(install_dir.path(), &m, "").unwrap());
    }

    // `TestIsPortableExeInstall`.
    #[test]
    fn is_portable_exe_install_cases() {
        // No bins → portable.
        let bare = manifest_from_json(json!({ "version": "1" }));
        assert!(is_portable_exe_install_for(&bare, "app.exe", ""));

        // Bin matching the download base name → portable.
        let matching = manifest_from_json(json!({ "version": "1", "bin": ["app.exe"] }));
        assert!(is_portable_exe_install_for(&matching, "app.exe", ""));

        // Non-exe download → never portable.
        assert!(!is_portable_exe_install_for(&bare, "app.zip", ""));

        // extract_dir disqualifies portability.
        let with_extract = manifest_from_json(json!({ "version": "1", "extract_dir": "files" }));
        assert!(!is_portable_exe_install_for(&with_extract, "app.exe", ""));
    }

    // `TestLinkFromCache_*` semantics: archive blobs are linked verbatim.
    #[test]
    fn link_from_cache_links_plain_blob() {
        let root = temp_dir("link-cache");
        let store = Store::new(root.path().join("store")).unwrap();
        store.prereqs().unwrap();
        let services = test_services(root.path(), store.clone());

        let hash = store.write(&b"blob"[..]).unwrap();
        let install_dir = root.path().join("apps").join("pkg").join("1.0");
        fs::create_dir_all(&install_dir).unwrap();

        let mut entry = PackageEntry {
            version: "1.0".into(),
            installed: String::new(),
            files: BTreeMap::new(),
            size: 0,
        };
        entry.files.insert(hash.clone(), "tool.exe".into());

        let linked = link_from_cache(&services, &install_dir, &entry).unwrap();
        assert_eq!(linked, 1);
        assert!(install_dir.join("tool.exe").exists());
    }

    // `link_blob` trusts the caller's hash instead of re-hashing the blob.
    // The link must still materialize the right bytes and record the hash, and
    // it must be attributed to the Link timing bucket when enabled.
    #[test]
    fn link_blob_uses_caller_hash_and_times_the_link() {
        let root = temp_dir("link-blob");
        let store = Store::new(root.path().join("store")).unwrap();
        store.prereqs().unwrap();
        let services = test_services(root.path(), store.clone());

        let hash = store.write(&b"payload"[..]).unwrap();
        let install_dir = root.path().join("apps").join("pkg").join("1.0");
        fs::create_dir_all(&install_dir).unwrap();

        let mut st = InstallState::default_for_test();
        st.install_dir = install_dir.clone();
        st.timings = Timings::on();

        link_blob(&services, &mut st, &hash, "tool.bin").unwrap();

        assert_eq!(fs::read(install_dir.join("tool.bin")).unwrap(), b"payload");
        assert_eq!(
            st.installed_files.get(&hash).map(String::as_str),
            Some("tool.bin")
        );
        assert!(st.timings.phases().unwrap().link > std::time::Duration::ZERO);
    }

    // `TestAdoptInstallDirToStore`: hashing + hardlinking a directory yields
    // relPath→hash plus the adopted file bytes.
    #[test]
    fn adopt_install_dir_to_store_hashes_files() {
        let root = temp_dir("adopt");
        let store = Store::new(root.path().join("store")).unwrap();
        store.prereqs().unwrap();
        let services = test_services(root.path(), store.clone());

        let install_dir = root.path().join("apps").join("pkg").join("1.0");
        write_file(&install_dir.join("bin").join("a.exe"), b"aaa");
        write_file(&install_dir.join("b.txt"), b"bb");

        let (files, bytes) =
            adopt_install_dir_to_store(&services, &install_dir, "pkg", noop_emit(), None, false)
                .unwrap();
        assert_eq!(files.len(), 2);
        assert_eq!(bytes, 5);
        assert!(files.contains_key("bin/a.exe"));
        assert!(files.contains_key("b.txt"));
    }

    // / extract_dir prefix stripping.
    #[test]
    fn install_member_rel_path_strips_extract_dir() {
        assert_eq!(
            install_member_rel_path("", "Browser", "Browser/firefox.exe").unwrap(),
            "firefox.exe"
        );
        assert_eq!(
            install_member_rel_path("IDE", "", "bin/pycharm64.exe").unwrap(),
            "IDE/bin/pycharm64.exe"
        );
    }

    // MSI alias mapping.
    #[test]
    fn extract_dir_lookup_paths_aliases() {
        let paths = extract_dir_lookup_paths("PFiles64/Foo");
        assert!(paths.contains(&"PFiles64/Foo".to_string()));
        assert!(paths.contains(&"Program Files/Foo".to_string()));
        assert!(paths.contains(&"Foo".to_string()));
    }

    // (`[name, arg]` and comma forms).
    #[test]
    fn parse_bin_pattern_forms() {
        assert_eq!(parse_bin_pattern("app.exe").0, "app.exe");
        assert_eq!(parse_bin_pattern("[app.exe, --flag]").0, "app.exe");
        assert_eq!(parse_bin_pattern("\"quoted.exe\"").0, "quoted.exe");
    }

    // / clamping.
    #[test]
    fn workers_clamp_bounds() {
        assert_eq!(link_workers(0), 1);
        assert_eq!(link_workers(1), 1);
        assert!(link_workers(10) >= 2);
        assert!(link_workers(100_000) <= 32);
    }

    // de-duplicates by hash.
    #[test]
    fn sum_unique_store_object_sizes_dedups() {
        let root = temp_dir("sum-unique");
        let store = Store::new(root.path().join("store")).unwrap();
        store.prereqs().unwrap();
        let hash = store.write(&b"12345"[..]).unwrap();

        let mut files: BTreeMap<String, String> = BTreeMap::new();
        files.insert(hash.clone(), "a.txt".into());
        files.insert(hash.clone(), "b.txt".into());
        assert_eq!(sum_unique_store_object_sizes(&store, &files).unwrap(), 5);
    }

    // ---- test helper: a minimal `Services` rooted at `root` ----
    fn test_services(root: &Path, store: Store) -> Services {
        Services::for_test(root.to_path_buf(), store)
    }

    // ---- stage 2.5: shims / install records / move-flatten / persist ----

    /// `(exe, alias, extraArgs)`.
    #[test]
    fn parse_bin_pattern_full_separates_alias_from_args() {
        assert_eq!(
            parse_bin_pattern_full("app.exe"),
            ("app.exe".into(), String::new(), vec![])
        );
        assert_eq!(
            parse_bin_pattern_full("app.exe,myalias"),
            ("app.exe".into(), "myalias".into(), vec![])
        );
        assert_eq!(
            parse_bin_pattern_full("[app.exe,myalias,--flag,x]"),
            (
                "app.exe".into(),
                "myalias".into(),
                vec!["--flag".into(), "x".into()]
            )
        );
        // The second element is the alias, not an argument — even when it
        // looks like a flag. has no `-`/`/` heuristic here; this case
        // pins the parity (a divergent ops-local copy once treated
        // `--flag` as extra args, orphaning shims on uninstall).
        assert_eq!(
            parse_bin_pattern_full("app.exe,--flag"),
            ("app.exe".into(), "--flag".into(), vec![])
        );
    }

    /// / extension stripping.
    #[test]
    fn shim_name_for_bin_strips_extensions() {
        assert_eq!(shim_name_for_bin("app.exe", ""), "app");
        assert_eq!(shim_name_for_bin("sub/app.EXE", ""), "app");
        assert_eq!(shim_name_for_bin("run.bat", ""), "run");
        // An alias wins and is also stripped.
        assert_eq!(shim_name_for_bin("app.exe", "myalias"), "myalias");
        assert_eq!(shim_name_for_bin("app.exe", "myalias.exe"), "myalias");
        // An alias with only whitespace collapses to the derived name.
        assert_eq!(shim_name_for_bin("app.exe", "   "), "app");
    }

    #[test]
    fn env_add_path_shim_name_strips_extensions() {
        assert_eq!(env_add_path_shim_name("App.exe"), "App");
        assert_eq!(env_add_path_shim_name("App.cmd"), "App");
        assert_eq!(env_add_path_shim_name("App.EXE"), "App");
        assert_eq!(env_add_path_shim_name("App.CMD"), "App");
        // No known extension → unchanged.
        assert_eq!(env_add_path_shim_name("App.dll"), "App.dll");
    }

    /// resolution order: resolved path → `_` mirror → raw.
    #[test]
    fn bin_rel_path_prefers_resolved_then_underscore() {
        let dir = temp_dir("bin-rel");
        fs::create_dir_all(dir.path().join("bin")).unwrap();
        fs::write(dir.path().join("bin/app.exe"), b"x").unwrap();

        // Resolved on-disk location.
        assert_eq!(bin_rel_path(dir.path(), "bin/app.exe"), "bin/app.exe");
        // Only the `_` mirror exists.
        fs::create_dir_all(dir.path().join("other")).unwrap();
        fs::write(dir.path().join("other/_hidden.exe"), b"x").unwrap();
        assert_eq!(
            bin_rel_path(&dir.path().join("other"), "hidden.exe"),
            "_hidden.exe"
        );
        // Neither → raw entry.
        assert_eq!(bin_rel_path(dir.path(), "nowhere.exe"), "nowhere.exe");
    }

    /// bin entries and env_add_path both shim.
    #[test]
    fn create_package_shims_bins_and_env_add_path() {
        let root = temp_dir("shim-pipeline");
        let store = Store::new(root.path().join("store")).unwrap();
        let _ = test_services(root.path(), store.clone());
        let install_dir = root.path().join("app");
        fs::create_dir_all(install_dir.join("sub")).unwrap();
        fs::write(install_dir.join("app.exe"), b"bin").unwrap();
        fs::write(install_dir.join("sub/helper.exe"), b"bin").unwrap();
        fs::write(install_dir.join("sub/skip.txt"), b"txt").unwrap();

        let manager = crate::shim::Manager::new(root.path()).unwrap();
        let m = manifest_from(
            r#"{"version":"1","url":"https://e/x.zip","hash":"h",
                "bin":"app.exe","env_add_path":"sub"}"#,
        );
        let created = create_package_shims(
            &manager,
            manager.shims_meta_dir(),
            "demo",
            &install_dir,
            None,
            &m,
            "",
        )
        .expect("shims");

        assert!(created.contains(&"app".to_string()), "{created:?}");
        assert!(created.contains(&"helper".to_string()), "{created:?}");
        // Non-executables are never shimmed.
        assert!(!created.contains(&"skip".to_string()), "{created:?}");

        // The config targets the install dir (the fallback shim dir).
        let cfg: crate::shim::Config =
            serde_json::from_slice(&fs::read(manager.shims_meta_dir().join("app.json")).unwrap())
                .unwrap();
        assert!(cfg.path.ends_with("app.exe"));
    }

    /// a conflicting owner is skipped, not replaced.
    #[test]
    fn create_package_shims_skips_conflicting_name() {
        let root = temp_dir("shim-conflict-pipeline");
        let store = Store::new(root.path().join("store")).unwrap();
        let _ = test_services(root.path(), store);
        let install_dir = root.path().join("app");
        fs::create_dir_all(&install_dir).unwrap();
        fs::write(install_dir.join("node.exe"), b"bin").unwrap();

        // Another package already owns the `node` shim name. The config path
        // must live under `apps/<pkg>/...` for the owner to be resolvable
        // (semantics).
        let other = root.path().join("apps").join("otherpkg").join("current");
        fs::create_dir_all(&other).unwrap();
        fs::write(other.join("node.exe"), b"other").unwrap();

        let manager = crate::shim::Manager::new(root.path()).unwrap();
        manager
            .create(
                "node",
                &other.join("node.exe").to_string_lossy(),
                &crate::shim::CreateOpts::default(),
            )
            .unwrap();

        let m =
            manifest_from(r#"{"version":"1","url":"https://e/x.zip","hash":"h","bin":"node.exe"}"#);
        let created = create_package_shims(
            &manager,
            manager.shims_meta_dir(),
            "demopkg",
            &install_dir,
            None,
            &m,
            "",
        )
        .expect("shims");

        assert!(created.is_empty(), "{created:?}");
        let cfg: crate::shim::Config = serde_json::from_slice(
            &fs::read(root.path().join("shims-meta").join("node.json")).unwrap(),
        )
        .unwrap();
        assert!(cfg.path.contains("otherpkg"), "{}", cfg.path);
    }

    #[test]
    fn scoop_move_flatten_detection() {
        // Matches the literal `mv` command only.
        assert!(is_scoop_move_flatten_pre_install(&["mv */* .".to_string()]));
        assert!(is_scoop_move_flatten_pre_install(&[
            "mv *\\* $dir".to_string()
        ]));
        // Multi-line hook with runs of whitespace still matches
        assert!(is_scoop_move_flatten_pre_install(&[
            "cd \"$dir\"\nmv   */*   .".to_string()
        ]));
        // Regression: `Remove-Item` CONTAINS `move-item`. A cmdlet-substring
        // heuristic used to return true here, and both call sites then cleared
        // the hook list — the cleanup never ran (apngasm's $PLUGINSDIR).
        assert!(!is_scoop_move_flatten_pre_install(&[
            "'$PLUGINSDIR', 'Uninstall.exe' | ForEach-Object { Remove-Item \"$dir\\$_\" -Recurse -Force }"
                .to_string()
        ]));
        // PowerShell's Move-Item is NOT the pattern: flattening for that
        // shape comes from `install_needs_move_flatten` (layout), not here.
        assert!(!is_scoop_move_flatten_pre_install(&[
            "Move-Item \"$dir\\nested\\*\" \"$dir\"".to_string()
        ]));
        // A hook doing actual work (expanding) is not a move-flatten.
        assert!(!is_scoop_move_flatten_pre_install(&[
            "Expand-7ZipArchive -Path \"$fname\" -DestinationPath \"$dir\"".to_string()
        ]));
        assert!(!is_scoop_move_flatten_pre_install(&[]));
    }

    /// one nested dir, no loose files.
    #[test]
    fn install_needs_move_flatten_layout() {
        let dir = temp_dir("move-flatten");
        // Exactly one subdirectory, no loose files → needs flatten.
        fs::create_dir_all(dir.path().join("nested")).unwrap();
        assert!(install_needs_move_flatten(dir.path()));

        // A loose file breaks the layout.
        fs::write(dir.path().join("app.exe"), b"x").unwrap();
        assert!(!install_needs_move_flatten(dir.path()));
    }

    /// hoists children, removes container.
    #[test]
    fn ensure_scoop_move_flatten_hoists_children() {
        let dir = temp_dir("hoist");
        let nested = dir.path().join("nested");
        fs::create_dir_all(&nested).unwrap();
        fs::write(nested.join("app.exe"), b"x").unwrap();
        fs::write(nested.join("lib.dll"), b"y").unwrap();

        let m =
            manifest_from(r#"{"version":"1","url":"https://e/x.zip","hash":"h","bin":"app.exe"}"#);
        ensure_scoop_move_flatten_install_dir(dir.path(), &m).unwrap();

        assert!(dir.path().join("app.exe").exists());
        assert!(dir.path().join("lib.dll").exists());
        assert!(!nested.exists(), "the empty container should be gone");
    }

    #[test]
    fn manifest_bin_exists_at_root_checks_each_bin() {
        let dir = temp_dir("bins-root");
        fs::write(dir.path().join("a.exe"), b"a").unwrap();
        let m = manifest_from(
            r#"{"version":"1","url":"https://e/x.zip","hash":"h","bin":["a.exe","b.exe"]}"#,
        );
        assert!(!manifest_bin_exists_at_root(dir.path(), &m));
        fs::write(dir.path().join("b.exe"), b"b").unwrap();
        assert!(manifest_bin_exists_at_root(dir.path(), &m));
    }

    #[test]
    fn archive_hash_for_download_finds_by_base_name() {
        let mut files: BTreeMap<String, String> = BTreeMap::new();
        files.insert("aaa".into(), "bin/tool.exe".into());
        files.insert("bbb".into(), "other/lonely.txt".into());
        // The download name may be a bare base name while the index stores a rel path.
        assert_eq!(archive_hash_for_download(&files, "tool.exe"), "aaa");
        assert_eq!(archive_hash_for_download(&files, "missing.exe"), "");
    }

    /// underscore names replaced in place.
    #[test]
    fn normalize_sevenzip_arm64_names_renames() {
        let dir = temp_dir("7z-normalize");
        fs::write(dir.path().join("_7z.exe"), b"z").unwrap();
        fs::write(dir.path().join("_7zip.dll"), b"d").unwrap();
        fs::write(dir.path().join("unrelated.txt"), b"t").unwrap();

        normalize_sevenzip_arm64_names(dir.path()).unwrap();

        assert!(dir.path().join("7z.exe").exists());
        assert!(dir.path().join("7-zip.dll").exists());
        assert!(!dir.path().join("_7z.exe").exists());
        assert!(dir.path().join("unrelated.txt").exists());
    }

    /// SFX leftovers removed, keep the rest.
    #[test]
    fn cleanup_sevenzip_sfx_artifacts_removes_leftovers() {
        let dir = temp_dir("7z-cleanup");
        fs::write(dir.path().join("Uninstall.exe"), b"u").unwrap();
        fs::write(dir.path().join("setup.exe"), b"s").unwrap();
        fs::write(dir.path().join("helper-arm64.exe"), b"h").unwrap();
        fs::write(dir.path().join("7zFM.exe"), b"f").unwrap();

        cleanup_sevenzip_sfx_artifacts(dir.path(), "setup.exe");

        assert!(!dir.path().join("Uninstall.exe").exists());
        assert!(!dir.path().join("setup.exe").exists());
        assert!(!dir.path().join("helper-arm64.exe").exists());
        assert!(dir.path().join("7zFM.exe").exists());
    }

    /// a no-op when nothing to repair.
    #[test]
    fn repair_sevenzip_arm64_layout_noop_without_marker() {
        let dir = temp_dir("7z-repair");
        assert!(repair_sevenzip_arm64_layout(dir.path()).is_ok());
        fs::write(dir.path().join("_7zip.dll"), b"d").unwrap();
        fs::write(dir.path().join("_7z.exe"), b"z").unwrap();
        repair_sevenzip_arm64_layout(dir.path()).unwrap();
        assert!(dir.path().join("7-zip.dll").exists());
        assert!(dir.path().join("7z.exe").exists());
    }

    /// Persist entries parse from a root string and an array of pairs.
    #[test]
    fn persist_entries_forms() {
        let m =
            manifest_from(r#"{"version":"1","url":"https://e/x.zip","hash":"h","persist":"data"}"#);
        let entries = m.persist_entries();
        assert_eq!(entries.len(), 1);
        assert_eq!(entries[0].install, "data");
        assert_eq!(entries[0].data, "data");
        assert_eq!(entries[0].install_name(), "data");
        assert_eq!(entries[0].data_name(), "data");

        let m = manifest_from(
            r#"{"version":"1","url":"https://e/x.zip","hash":"h",
                "persist":["config.ini",["install","data"]]}"#,
        );
        let entries = m.persist_entries();
        assert_eq!(entries.len(), 2);
        assert_eq!(entries[1].install, "install");
        assert_eq!(entries[1].data, "data");
        // A bare path doubles as the data name.
        assert_eq!(entries[0].data_name(), "config.ini");
    }

    /// An architecture block's `persist` wins only when the block is selectable
    /// and the value parses. parity: a block needs a URL or arch metadata
    /// (`extract_dir` etc.) to be selectable at all.
    #[test]
    fn persist_entries_arch_block_precedence() {
        let m = manifest_from(
            r#"{"version":"1","url":"https://e/x.zip","hash":"h","persist":"root",
                "architecture":{"64bit":{"url":"https://e/x64.zip","persist":["block"]}}}"#,
        );
        assert_eq!(m.persist_entries_for_install("64bit")[0].install, "block");
        // Empty override = host pick, which on this x64 host is the 64bit
        // block (`archBlockForInstall("")` →).
        assert_eq!(m.persist_entries_for_install("")[0].install, "block");
        // Unknown override → block nil → root.
        assert_eq!(m.persist_entries_for_install("bogus")[0].install, "root");

        // A block that carries only `persist` is NOT selectable → root wins.
        let m = manifest_from(
            r#"{"version":"1","url":"https://e/x.zip","hash":"h","persist":"root",
                "architecture":{"64bit":{"persist":["ignored"]}}}"#,
        );
        assert_eq!(m.persist_entries_for_install("64bit")[0].install, "root");
        // Nothing selectable for the host either → root.
        assert_eq!(m.persist_entries_for_install("")[0].install, "root");
    }

    /// `LooksLikeFile` distinguishes known file extensions from dotted dirs.
    #[test]
    fn persist_entry_looks_like_file() {
        let m = manifest_from(
            r#"{"version":"1","url":"https://e/x.zip","hash":"h",
                "persist":["config.ini","profile.default","data"]}"#,
        );
        let entries = m.persist_entries();
        assert!(entries[0].looks_like_file(), "config.ini is a file");
        // Tor Browser persists profile.default as a directory.
        assert!(!entries[1].looks_like_file(), "profile.default is a dir");
        assert!(!entries[2].looks_like_file(), "data is a dir");
    }

    /// Fixture: `buckets/<bucket>/bucket/<pkg>.json`, the on-disk bucket the
    /// registry reads. Returns the manifest path.
    fn seed_bucket_manifest(root: &Path, bucket: &str, pkg: &str, json: &str) -> PathBuf {
        let path = root
            .join("buckets")
            .join(bucket)
            .join("bucket")
            .join(format!("{pkg}.json"));
        fs::create_dir_all(path.parent().unwrap()).unwrap();
        fs::write(&path, json).unwrap();
        path
    }

    /// Seeds a set of `(bucket, package, manifest)` triples and returns the
    /// services pointing at that data root.
    fn services_with_manifests(root: &Path, manifests: &[(&str, &str, &str)]) -> Services {
        for (bucket, pkg, json) in manifests {
            seed_bucket_manifest(root, bucket, pkg, json);
        }
        let store = Store::new(root.join("store")).unwrap();
        test_services(root, store)
    }

    /// `depends` are expanded depth-first, dependencies first, and the
    /// requested package is not part of the install list (installing it here
    /// would make the real install short-circuit as already-installed).
    #[test]
    fn missing_dependencies_are_ordered_before_their_dependents() {
        let root = temp_dir("depends-order");
        let services = services_with_manifests(
            root.path(),
            &[
                (
                    "main",
                    "app",
                    r#"{"version":"1","url":"https://e/a.zip","hash":"h","depends":"lib"}"#,
                ),
                (
                    "main",
                    "lib",
                    r#"{"version":"1","url":"https://e/l.zip","hash":"h","depends":["base"]}"#,
                ),
                (
                    "main",
                    "base",
                    r#"{"version":"1","url":"https://e/b.zip","hash":"h"}"#,
                ),
            ],
        );

        assert_eq!(
            missing_dependencies(&services, "app").expect("deps"),
            vec!["base".to_string(), "lib".to_string()]
        );
    }

    /// A bare dependency name resolves across buckets exactly like a typed
    /// install ref, so `"depends": "tool"` finds `games/tool`.
    #[test]
    fn missing_dependencies_resolve_bare_names_across_buckets() {
        let root = temp_dir("depends-cross-bucket");
        let services = services_with_manifests(
            root.path(),
            &[
                (
                    "extras",
                    "roms",
                    r#"{"version":"1","url":"https://e/r.zip","hash":"h","depends":"tool"}"#,
                ),
                (
                    "games",
                    "tool",
                    r#"{"version":"1","url":"https://e/t.zip","hash":"h"}"#,
                ),
            ],
        );

        assert_eq!(
            missing_dependencies(&services, "extras/roms").expect("deps"),
            vec!["games/tool".to_string()]
        );
    }

    /// An installed dependency is skipped; Scoop never upgrades a dependency as
    /// a side effect of installing something else.
    #[test]
    fn missing_dependencies_skip_installed_packages() {
        let root = temp_dir("depends-installed");
        let services = services_with_manifests(
            root.path(),
            &[
                (
                    "main",
                    "app",
                    r#"{"version":"1","url":"https://e/a.zip","hash":"h","depends":"lib"}"#,
                ),
                (
                    "main",
                    "lib",
                    r#"{"version":"1","url":"https://e/l.zip","hash":"h"}"#,
                ),
            ],
        );
        services
            .cache()
            .add("lib", "1", &std::collections::BTreeMap::new(), 1)
            .expect("cache entry");
        services
            .cache()
            .add_installed("lib", "1", "ignored", 0, &serde_json::Map::new())
            .expect("record install");

        assert!(missing_dependencies(&services, "app")
            .expect("deps")
            .is_empty());
    }

    /// A cycle terminates with a readable error instead of recursing forever.
    #[test]
    fn dependency_cycle_is_reported() {
        let root = temp_dir("depends-cycle");
        let services = services_with_manifests(
            root.path(),
            &[
                (
                    "main",
                    "a",
                    r#"{"version":"1","url":"https://e/a.zip","hash":"h","depends":"b"}"#,
                ),
                (
                    "main",
                    "b",
                    r#"{"version":"1","url":"https://e/b.zip","hash":"h","depends":"a"}"#,
                ),
            ],
        );

        let err = missing_dependencies(&services, "a")
            .expect_err("cycle must fail")
            .to_string();
        assert!(err.contains("dependency cycle"), "{err}");
        assert!(err.contains("a -> b -> a"), "{err}");
    }

    /// `--independent` skips the expansion entirely: even an unresolvable
    /// dependency never gets looked up.
    #[test]
    fn independent_install_skips_dependency_expansion() {
        let root = temp_dir("depends-independent");
        let services = services_with_manifests(
            root.path(),
            &[(
                "main",
                "app",
                r#"{"version":"1","url":"https://e/a.zip","hash":"h","depends":"missing-dep"}"#,
            )],
        );
        let emit = |_: ProgressEvent| {};

        let independent = options_for("app");
        assert!(install_missing_dependencies(&services, &independent, &emit, None).is_ok());

        let expanding = InstallOptions {
            independent: false,
            ..options_for("app")
        };
        assert!(install_missing_dependencies(&services, &expanding, &emit, None).is_err());
    }

    /// Installing something already installed is a no-op, so it must not pull
    /// in dependencies as a side effect.
    #[test]
    fn a_noop_install_does_not_pull_dependencies() {
        let root = temp_dir("depends-noop");
        let services = services_with_manifests(
            root.path(),
            &[(
                "main",
                "app",
                r#"{"version":"1","url":"https://e/a.zip","hash":"h","depends":"missing-dep"}"#,
            )],
        );
        services
            .cache()
            .add("app", "1", &std::collections::BTreeMap::new(), 1)
            .expect("cache entry");
        services
            .cache()
            .add_installed("app", "1", "ignored", 0, &serde_json::Map::new())
            .expect("record install");

        let options = InstallOptions {
            independent: false,
            ..options_for("app")
        };
        assert!(
            install_missing_dependencies(&services, &options, &|_: ProgressEvent| {}, None).is_ok()
        );
    }

    /// A pinned version the bucket never published is built from the
    /// manifest's `autoupdate` block (`Manifest.ForVersion`): the URL is
    /// substituted and its hash is taken from the bytes the URL serves, because
    /// the manifest only carries instructions for extracting the hash
    /// (`autoupdate.hash`) and not the hash value itself.
    ///
    /// The seeded bucket has no `.git`, which is also the case that used to
    /// report "not a git checkout" instead of trying `autoupdate`.
    #[test]
    fn autoupdate_pin_generates_the_url_and_hashes_the_served_bytes() {
        let root = temp_dir("autoupdate-pin");
        let body = b"generated pin payload".to_vec();
        let (base, _server) = serve_any_body_limited(&body, 4);
        let store = Store::new(root.path().join("store")).unwrap();
        let expected = store.write(&body[..]).unwrap();

        seed_bucket_manifest(
            root.path(),
            "main",
            "demo",
            &format!(
                r#"{{"version":"1.0.0","url":"{base}/app-1.0.0.zip","hash":"aa",
                    "autoupdate":{{"url":"{base}/app-$version.zip"}}}}"#
            ),
        );

        let mut services = test_services(root.path(), store);
        // A system proxy would swallow a loopback request.
        services.set_proxy_route(Default::default(), Vec::new());
        let mut st = InstallState::new("demo");
        st.pin_version = "2.0.0".to_string();
        resolve_install_phase(
            &services,
            &mut st,
            &options_for("demo"),
            &|_: ProgressEvent| {},
        )
        .expect("resolve");

        let m = st.manifest.as_ref().expect("manifest");
        assert_eq!(m.version, "2.0.0");
        assert_eq!(m.get_url(), format!("{base}/app-2.0.0.zip"));
        assert_eq!(
            m.get_hashes(),
            vec![format!("sha256:{expected}")],
            "the hash must describe the bytes the generated URL served"
        );
    }

    /// A hook the generated manifest cannot satisfy keeps the history hint:
    /// a shallow bucket may still hold the published manifest, and deepening is
    /// the fix that may make the pin work.
    #[test]
    fn autoupdate_pin_with_a_checkver_variable_keeps_the_history_hint() {
        let root = temp_dir("autoupdate-pin-unresolved");
        seed_bucket_manifest(
            root.path(),
            "main",
            "demo",
            r#"{"version":"1.0.0","url":"https://e/app.zip","hash":"aa",
                "autoupdate":{"url":"https://e/app-$matchBuild.zip"}}"#,
        );

        let store = Store::new(root.path().join("store")).unwrap();
        let services = test_services(root.path(), store);
        let mut st = InstallState::new("demo");
        st.pin_version = "2.0.0".to_string();
        let err = resolve_install_phase(
            &services,
            &mut st,
            &options_for("demo"),
            &|_: ProgressEvent| {},
        )
        .expect_err("cannot build");
        let text = err.to_string();
        assert!(text.contains("not a git checkout"), "{text}");
    }

    /// The full finalize phase writes install.json, links current, shims and
    /// indexes the cache — the parts that no longer need an external 7z.
    #[test]
    fn finalize_writes_record_current_shims_and_cache() {
        let root = temp_dir("finalize-full");
        let store = Store::new(root.path().join("store")).unwrap();
        let services = test_services(root.path(), store.clone());
        let install_dir = root.path().join("apps").join("demo").join("1.0.0");
        fs::create_dir_all(install_dir.join("bin")).unwrap();

        let real_file = install_dir.join("bin/app.exe");
        fs::write(&real_file, b"payload").unwrap();
        let hash = store.write(&b"payload"[..]).unwrap();

        let manifest_path = root.path().join("manifest.json");
        fs::write(
            &manifest_path,
            br#"{"version":"1.0.0","url":"https://e/x.zip","hash":"h","bin":"bin/app.exe"}"#,
        )
        .unwrap();

        let mut st = InstallState {
            pkg_ref: "main/demo".to_string(),
            pkg_name: "demo".to_string(),
            manifest: Some(manifest_from(
                r#"{"version":"1.0.0","url":"https://e/x.zip","hash":"h","bin":"bin/app.exe"}"#,
            )),
            manifest_path: Some(manifest_path),
            target_version: "1.0.0".to_string(),
            install_arch: "64bit".into(),
            install_dir: install_dir.clone(),
            installed_files: [(hash, "bin/app.exe".to_string())].into_iter().collect(),
            total_size: 7,
            ..InstallState::default_for_test()
        };

        finalize_package_install(&services, &mut st, &|_: ProgressEvent| {}).expect("finalize");

        assert!(st.install_succeeded);
        // 1. `apps/demo/current` points at the version dir.
        assert_eq!(
            apps::read_current(&root.path().join("apps").join("demo")),
            Some("1.0.0".into())
        );
        // 2. install.json landed in the version dir with the bucket and version.
        let record = apps::load_install_record(&install_dir).expect("install record");
        assert_eq!(record.version, "1.0.0");
        assert_eq!(record.bucket, "main");
        // `glue reset` rebuilds the version's shims from this record, and the
        // arch it was installed with is part of the state it has to reproduce.
        assert_eq!(record.arch, "64bit");
        assert!(record.manifest.is_some());
        // 3. The bin entry produced a shim pointing into the install dir.
        let shim_cfg: crate::shim::Config = serde_json::from_slice(
            &fs::read(root.path().join("shims-meta").join("app.json")).unwrap(),
        )
        .expect("shim config");
        assert!(shim_cfg.path.contains("bin") && shim_cfg.path.ends_with("app.exe"));
        // 4. The cache index records the package as installed.
        assert!(services.cache().get_installed("demo").is_some());
        assert_eq!(services.cache().get("demo").unwrap().version, "1.0.0");
    }

    #[test]
    fn release_downloaded_archive_drops_blob_and_member_index() {
        let root = temp_dir("release-archive");
        let store = Store::new(root.path().join("store")).unwrap();
        let services = test_services(root.path(), store.clone());
        let install_dir = root.path().join("apps").join("demo").join("1.0.0");
        fs::create_dir_all(&install_dir).unwrap();
        fs::write(install_dir.join("app.exe"), b"member").unwrap();

        let member_hash = store.write(&b"member"[..]).unwrap();
        let archive_bytes = b"archive-bytes";
        let archive_hash = store.write(&archive_bytes[..]).unwrap();
        let mut members = BTreeMap::new();
        members.insert("app.exe".to_string(), member_hash.clone());
        crate::zip::save(store.path(), &archive_hash, &members, 6).unwrap();

        let mut st = InstallState {
            pkg_ref: "main/demo".to_string(),
            pkg_name: "demo".to_string(),
            install_dir,
            download_name: "demo.zip".to_string(),
            archive_hash: archive_hash.clone(),
            installed_files: [
                (member_hash.clone(), "app.exe".to_string()),
                (archive_hash.clone(), "demo.zip".to_string()),
            ]
            .into_iter()
            .collect(),
            total_size: 6 + archive_bytes.len() as i64,
            ..InstallState::default_for_test()
        };

        release_downloaded_archives(&services, &mut st);

        assert!(!store.has(&archive_hash));
        assert!(store.has(&member_hash));
        assert!(crate::zip::load(store.path(), &archive_hash).is_none());
        assert!(!st.installed_files.contains_key(&archive_hash));
        assert_eq!(st.total_size, 6);
    }

    #[test]
    fn release_keeps_archive_when_it_is_an_installed_file() {
        let root = temp_dir("release-archive-linked");
        let store = Store::new(root.path().join("store")).unwrap();
        let services = test_services(root.path(), store.clone());
        let install_dir = root.path().join("apps").join("demo").join("1.0.0");
        fs::create_dir_all(&install_dir).unwrap();
        fs::write(install_dir.join("demo.exe"), b"portable").unwrap();

        let hash = store.write(&b"portable"[..]).unwrap();
        let mut st = InstallState {
            pkg_ref: "main/demo".to_string(),
            pkg_name: "demo".to_string(),
            install_dir,
            download_name: "demo.exe".to_string(),
            archive_hash: hash.clone(),
            installed_files: [(hash.clone(), "demo.exe".to_string())]
                .into_iter()
                .collect(),
            total_size: 8,
            ..InstallState::default_for_test()
        };

        release_downloaded_archives(&services, &mut st);

        assert!(store.has(&hash));
        assert!(st.installed_files.contains_key(&hash));
    }

    #[test]
    fn release_keeps_archive_shared_with_another_package() {
        let root = temp_dir("release-archive-shared");
        let store = Store::new(root.path().join("store")).unwrap();
        let services = test_services(root.path(), store.clone());
        let install_dir = root.path().join("apps").join("demo").join("1.0.0");
        fs::create_dir_all(&install_dir).unwrap();

        let archive_hash = store.write(&b"shared-archive"[..]).unwrap();
        let mut other_files = BTreeMap::new();
        other_files.insert(archive_hash.clone(), "other.zip".to_string());
        services
            .cache()
            .add("other", "1.0.0", &other_files, 1)
            .unwrap();

        let mut st = InstallState {
            pkg_ref: "main/demo".to_string(),
            pkg_name: "demo".to_string(),
            install_dir,
            download_name: "demo.zip".to_string(),
            archive_hash: archive_hash.clone(),
            installed_files: [(archive_hash.clone(), "demo.zip".to_string())]
                .into_iter()
                .collect(),
            total_size: 14,
            ..InstallState::default_for_test()
        };

        release_downloaded_archives(&services, &mut st);

        assert!(store.has(&archive_hash));
        assert!(st.installed_files.contains_key(&archive_hash));
    }

    /// Releasing the archive must not disable cache reuse: the entry now lists
    /// only members, and linking them is exactly what a reinstall needs.
    #[test]
    fn released_archive_still_allows_cache_reuse_from_members() {
        let root = temp_dir("cache-reuse-members");
        let store = Store::new(root.path().join("store")).unwrap();
        let member_hash = store.write(&b"member"[..]).unwrap();
        let mut files = BTreeMap::new();
        files.insert(member_hash, "app.exe".to_string());
        let entry = PackageEntry {
            version: "1.0.0".to_string(),
            installed: String::new(),
            files,
            size: 6,
        };
        assert!(cache_reusable_for_install(
            &store, &entry, "1.0.0", "demo.zip", ""
        ));
    }

    // ---- multi-artifact concurrent download (deliberate deviation) ----

    use std::io::{BufRead as TestBufRead, BufReader as TestBufReader, Write as TestWrite};
    use std::net::TcpListener;

    /// Serves `/ok/<i>.bin` with `bodies[i]` (200 + Content-Length) and
    /// answers 404 for anything else. Accepts exactly `bodies.len()`
    /// connections so the thread exits with the test.
    /// Serves `body` for every GET, for `connections` connections, then stops
    /// accepting. The thread is never joined: it blocks on `incoming()` when
    /// the downloader makes fewer requests than that (a joined listener is how
    /// a test hung for minutes).
    fn serve_any_body_limited(
        body: &[u8],
        connections: usize,
    ) -> (String, std::thread::JoinHandle<()>) {
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let addr = listener.local_addr().unwrap();
        let base = format!("http://{addr}");
        let body = body.to_vec();
        let handle = std::thread::spawn(move || {
            for stream in listener.incoming().take(connections) {
                let Ok(mut stream) = stream else { continue };
                let mut reader = TestBufReader::new(stream.try_clone().unwrap());
                let mut line = String::new();
                if reader.read_line(&mut line).unwrap_or(0) == 0 {
                    continue;
                }
                loop {
                    let mut l = String::new();
                    if reader.read_line(&mut l).unwrap_or(0) == 0 || l.trim().is_empty() {
                        break;
                    }
                }
                let head = format!(
                    "HTTP/1.1 200 OK\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
                    body.len()
                );
                stream.write_all(head.as_bytes()).unwrap();
                stream.write_all(&body).unwrap();
            }
        });
        (base, handle)
    }

    fn serve_multi_artifacts(bodies: &[Vec<u8>]) -> (String, std::thread::JoinHandle<()>) {
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let addr = listener.local_addr().unwrap();
        let base = format!("http://{addr}");
        let bodies = bodies.to_vec();
        let handle = std::thread::spawn(move || {
            for stream in listener.incoming().take(bodies.len()) {
                let Ok(mut stream) = stream else { continue };
                let mut reader = TestBufReader::new(stream.try_clone().unwrap());
                let mut line = String::new();
                if reader.read_line(&mut line).unwrap_or(0) == 0 {
                    continue;
                }
                let path = line.split_whitespace().nth(1).unwrap_or("/").to_string();
                loop {
                    let mut l = String::new();
                    if reader.read_line(&mut l).unwrap_or(0) == 0 || l.trim().is_empty() {
                        break;
                    }
                }
                let (status, body) = match path
                    .strip_prefix("/ok/")
                    .and_then(|s| s.trim_end_matches(".bin").parse::<usize>().ok())
                    .and_then(|i| bodies.get(i))
                {
                    Some(b) => (200u16, b.clone()),
                    None => (404u16, Vec::new()),
                };
                let reason = if status == 200 { "OK" } else { "Not Found" };
                let head = format!(
                    "HTTP/1.1 {status} {reason}\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
                    body.len()
                );
                stream.write_all(head.as_bytes()).unwrap();
                stream.write_all(&body).unwrap();
            }
        });
        (base, handle)
    }

    fn multi_artifact_state(base: &str, bodies: &[Vec<u8>], missing_last: bool) -> InstallState {
        let mut st = InstallState::new("multi/test");
        st.multi_artifact = true;
        for (i, b) in bodies.iter().enumerate() {
            let last_missing = missing_last && i + 1 == bodies.len();
            let url = if last_missing {
                format!("{base}/nope.bin")
            } else {
                format!("{base}/ok/{i}.bin")
            };
            st.url_hash_pairs.push(UrlHashPair {
                url,
                hash_algo: "sha256".to_string(),
                hash_value: crate::hash::hash_bytes(b),
            });
        }
        st
    }

    /// More pairs than workers (6 vs `MULTI_ARTIFACT_WORKERS` = 4) so the
    /// cursor-stealing path runs: every pair must be fetched exactly once and
    /// results must come back in input order (record_file + deploy depend on
    /// the sequential loop's ordering).
    #[test]
    fn download_multi_artifacts_concurrent_keeps_order_and_completeness() {
        let bodies: Vec<Vec<u8>> = (0..6u8)
            .map(|i| vec![b'a' + i; 512 * (i as usize + 1)])
            .collect();
        let (base, server) = serve_multi_artifacts(&bodies);
        let td = temp_dir("multi-art");
        let store = Store::new(td.path().join("store")).unwrap();
        let services = test_services(td.path(), store);
        let mut st = multi_artifact_state(&base, &bodies, false);
        let emit = |_: ProgressEvent| {};
        download_multi_artifacts(&services, &mut st, &emit).expect("multi-artifact download");
        server.join().unwrap();
        assert_eq!(st.download_results.len(), bodies.len());
        for (i, r) in st.download_results.iter().enumerate() {
            assert_eq!(
                r.task.filename,
                format!("{i}.bin"),
                "result {i} out of order"
            );
            assert_eq!(r.hash, crate::hash::hash_bytes(&bodies[i]));
            assert_eq!(r.size, bodies[i].len() as u64);
        }
        // installed_files was recorded once per pair, keyed by content hash.
        assert_eq!(st.installed_files.len(), bodies.len());
    }

    /// One failing pair must fail the whole phase with phase error
    /// (the concurrent fan-out collects the first error identically).
    #[test]
    fn download_multi_artifacts_first_error_fails_phase() {
        let bodies: Vec<Vec<u8>> = (0..4u8).map(|i| vec![b'x' + i; 1024]).collect();
        let (base, server) = serve_multi_artifacts(&bodies);
        let td = temp_dir("multi-art-err");
        let store = Store::new(td.path().join("store")).unwrap();
        let services = test_services(td.path(), store);
        let mut st = multi_artifact_state(&base, &bodies, true);
        let emit = |_: ProgressEvent| {};
        let err = download_multi_artifacts(&services, &mut st, &emit)
            .expect_err("one 404 pair must fail the phase");
        let msg = err.to_string();
        assert!(
            msg.contains("404") || msg.contains("download"),
            "unexpected error text: {msg}"
        );
        server.join().unwrap();
    }

    /// `Resolved` is emitted right after manifest resolution and carries the
    /// metadata the CLI prints (including for already-installed packages).
    #[test]
    fn resolved_event_carries_manifest_metadata() {
        let mut st = InstallState::new("main/demo");
        st.target_version = "1.2.3".to_string();
        st.description = "Demo package".to_string();
        let events = std::sync::Mutex::new(Vec::new());
        emit_resolved(&st, &|event| events.lock().unwrap().push(event));
        let events = events.into_inner().unwrap();
        assert_eq!(events.len(), 1);
        match &events[0] {
            ProgressEvent::Resolved {
                package,
                version,
                bucket,
                description,
                arch: _,
            } => {
                assert_eq!(package, "demo");
                assert_eq!(version, "1.2.3");
                assert_eq!(bucket, "main");
                assert_eq!(description, "Demo package");
            }
            other => panic!("unexpected event: {other:?}"),
        }
    }

    /// A bare name that resolved into another bucket reports that bucket: the
    /// typed ref alone reads as `main` (`package_bucket_name`'s default), which
    /// would mislabel `glue install 86box-roms` as a main-bucket package.
    #[test]
    fn resolved_event_prefers_the_resolved_bucket() {
        let mut st = InstallState::new("86box-roms");
        st.bucket_name = "extras".to_string();
        let events = std::sync::Mutex::new(Vec::new());
        emit_resolved(&st, &|event| events.lock().unwrap().push(event));
        let events = events.into_inner().unwrap();
        match &events[0] {
            ProgressEvent::Resolved { bucket, .. } => assert_eq!(bucket, "extras"),
            other => panic!("unexpected event: {other:?}"),
        }
    }
}
