//! Engine top-level operations and the `Engine` struct.
//!
//! The `Engine` is the CLI-facing facade over the shared [`Services`]
//! aggregate: package operations (install/update/uninstall/reset), catalog
//! queries (list/search/info/doctor), bucket and cache management, and the
//! cross-process mutation lock that guards everything that writes the data
//! root.
//!
//! but reuses [`Services`] for the collaborator wiring rather than duplicating
//! the store/cache/downloader/registry fields.

use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::Mutex;

use crate::apps;
// Keeps two identical copies of this check (
// and); the implementation has one, aliased to the name the
// listing code uses.
use crate::cache::rebuild::package_version_installed as version_dir_installed;
use crate::engine::install::{
    install, parse_bin_pattern_full, InstallError, InstallOptions, InstallOutcome,
};
use crate::engine::progress::{ProgressEvent, ProgressFn};
use crate::engine::services::{EngineConfig, Services, ServicesError};

pub use crate::engine::install::{
    InstallOptions as InstallPlanOptions, InstallOutcome as InstallResult,
};

/// Re-export of the install outcome, kept under the historical `InstallResult`
/// name used by the CLI.
pub type EngineInstallResult = InstallOutcome;

/// Progress callback for [`Engine::rebuild_cache_index`]: invoked once per
/// indexed package with `(name, version, file_count)`.
pub type CacheRebuildProgress = dyn Fn(&str, &str, usize) + Send + Sync;

/// Outcome of an uninstall operation.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct UninstallOutcome {
    pub package: String,
    pub version: String,
    /// Install directory that was removed.
    pub install_dir: PathBuf,
    /// Number of files in the removed version directory.
    pub file_count: usize,
    /// Total bytes of the removed version directory.
    pub size: i64,
    /// Shim names removed for this package.
    pub removed_shims: Vec<String>,
}

/// Outcome of a list operation: one installed package row.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct InstalledPackageSummary {
    pub name: String,
    pub version: String,
    pub install_dir: PathBuf,
    pub install_time: String,
    pub size: i64,
}

/// What [`Engine::launch`] started.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct LaunchTarget {
    /// Executable that was started (a shim or the program itself).
    pub program: PathBuf,
    /// Name shown to the user (the shim or executable name).
    pub name: String,
}

/// Outcome of a search operation: one available package.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SearchResult {
    pub bucket: String,
    pub name: String,
    pub version: String,
    pub description: String,
}

/// Upstream update status for one bucket (see `engine.check_bucket_updates`).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct BucketUpdateStatus {
    pub name: String,
    pub ok: bool,
    pub has_updates: bool,
    pub local_commit: String,
    pub remote_commit: String,
    pub error: String,
}

/// An installed package whose bucket manifest has a newer version
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct PackageUpdate {
    pub name: String,
    pub installed_version: String,
    pub latest_version: String,
    /// Bucket the package was installed from ("" when unknown).
    pub bucket: String,
}

impl PackageUpdate {
    /// The install ref to pass to `Engine::update`:
    /// `bucket/name` for non-main buckets, bare name otherwise.
    pub fn install_ref(&self) -> String {
        manifest_lookup_ref(&self.name, &self.bucket)
    }
}

/// `bucket/name` unless bucket is empty/main.
pub(crate) fn manifest_lookup_ref(pkg_name: &str, bucket: &str) -> String {
    if !bucket.is_empty() && bucket != "main" {
        format!("{bucket}/{pkg_name}")
    } else {
        pkg_name.to_string()
    }
}

/// One cached package (cache index `packages` row).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct CachePackageInfo {
    pub name: String,
    pub version: String,
    pub installed: String,
    pub size: i64,
    pub file_count: usize,
}

/// One file of a cached package, resolved against the content store.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct CachedFile {
    /// Path inside the installed package.
    pub name: String,
    /// Content hash, i.e. the store object's file name.
    pub hash: String,
    /// Size of the store object (0 when it is empty or missing).
    pub size: i64,
    /// Whether the store still holds the object. A `false` here means the index
    /// outlived the blob, which `size` alone cannot express (`0` is a valid
    /// size for an empty file).
    pub present: bool,
}

/// One cached package plus its file list (the storage details dialog).
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct CachePackageDetail {
    pub name: String,
    pub version: String,
    pub installed: String,
    /// Total size in bytes, as recorded by the index.
    pub size: i64,
    /// Every indexed file, ordered by path.
    pub files: Vec<CachedFile>,
}

/// Aggregate cache index stats.
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct CacheSummary {
    pub package_count: usize,
    pub total_files: i64,
    pub total_size: i64,
}

/// One package whose cache-index rows `cache clear` removed.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct CacheClearEntry {
    pub name: String,
    pub version: String,
    pub file_count: i64,
    pub size: i64,
}

/// Outcome of `cache clear`, bucketed per requested name.
///
/// **Intentional divergence from.** `cache clear` only drops rows for
/// packages that are *not* installed. `Index.Remove` deletes the
/// `packages`, `package_files` **and** `installed_packages` rows of whatever
/// name it is handed, so clearing a live install silently forgets its version
/// lock and install metadata; `--all` additionally cascades `install_history`
/// away. Nothing is reclaimed by doing so: the store blobs stay hardlinked
/// into `apps/`, the index is only ~0.2 % of the store size, and `gc` cannot
/// free a blob that is still linked. The only defensible use of the command is
/// therefore "forget the reinstall cache of packages I removed", which is
/// exactly what this implementation does.
#[derive(Debug, Clone, Default)]
pub struct CacheClearReport {
    /// Packages whose index rows were removed.
    pub cleared: Vec<CacheClearEntry>,
    /// Packages that are still installed and were left alone.
    pub skipped_installed: Vec<String>,
    /// Requested names that had no cache-index row to begin with.
    pub not_indexed: Vec<String>,
}

impl CacheClearReport {
    /// Totals over the removed rows (what the index held, not disk freed).
    pub fn summary(&self) -> CacheSummary {
        CacheSummary {
            package_count: self.cleared.len(),
            total_files: self.cleared.iter().map(|e| e.file_count).sum(),
            total_size: self.cleared.iter().map(|e| e.size).sum(),
        }
    }
}

/// One version directory removed by [`Engine::cleanup_old_versions`].
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct OldVersionRemoval {
    pub package: String,
    pub version: String,
    pub size: u64,
}

/// What [`Engine::cleanup_old_versions`] removed.
#[derive(Debug, Default, Clone, PartialEq, Eq)]
pub struct OldVersionCleanup {
    pub removed: Vec<OldVersionRemoval>,
    /// Total bytes freed by the removals.
    pub freed: u64,
}

/// Recursive on-disk size of a directory (0 when it cannot be read).
///
/// Links are not followed: a junction pointing back into the store would
/// otherwise make the total meaningless.
fn dir_bytes(path: &Path) -> u64 {
    let Ok(entries) = std::fs::read_dir(path) else {
        return 0;
    };
    let mut total = 0;
    for entry in entries.flatten() {
        let Ok(file_type) = entry.file_type() else {
            continue;
        };
        if file_type.is_dir() {
            total += dir_bytes(&entry.path());
        } else if file_type.is_file() {
            total += entry.metadata().map(|meta| meta.len()).unwrap_or(0);
        }
    }
    total
}

/// A dependency of an install plan.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct PlanRef {
    pub ref_: String,
    pub installed: bool,
}

/// An optional suggestion of an install plan.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct PlanSuggestion {
    pub ref_: String,
    pub label: String,
    pub installed: bool,
}

/// Install plan produced by `plan_install` (depends + suggestions).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct InstallPlan {
    pub package: String,
    pub depends: Vec<PlanRef>,
    pub suggestions: Vec<PlanSuggestion>,
}

/// Detailed metadata for an installed package (`glue info`).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct InstalledPackageDetail {
    pub name: String,
    pub version: String,
    pub install_path: String,
    pub current_path: String,
    pub installed_at: String,
    pub size: i64,
    pub file_count: usize,
    pub shims: Vec<String>,
    pub bucket: String,
    pub description: String,
    pub homepage: String,
    pub license: String,
    pub depends: Vec<String>,
    pub notes: Vec<String>,
    pub update_available: bool,
    /// Latest version available in the bucket manifests (empty when unknown).
    pub latest_version: String,
}

/// Installed versions of one package (`glue list --all`).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct InstalledPackageVersions {
    pub name: String,
    pub versions: Vec<String>,
    pub current: String,
}

/// All engine errors.
#[derive(Debug, thiserror::Error)]
pub enum EngineError {
    #[error("{0}")]
    Install(#[from] InstallError),
    #[error("{0}")]
    Services(#[from] ServicesError),
    #[error("cache index: {0}")]
    Index(#[from] crate::cache::IndexError),
    #[error("cache refs: {0}")]
    Refs(#[from] crate::cache::refs::RefsError),
    #[error("git {0}: {1}")]
    Git(String, String),
    #[error("bucket {0}: {1}")]
    Bucket(String, String),
    #[error("package {0} is not installed")]
    NotInstalled(String),
    /// One or more executables from the package are still running; the caller
    /// must stop them (or pass `kill_running`) before the files can be removed.
    #[error("{0}")]
    ProcessesRunning(String),
    #[error("package {0} is already up to date at version {1}")]
    UpToDate(String, String),
    #[error("no newer version found for {0}")]
    NoNewerVersion(String),
    #[error("package {0} has no version {1}")]
    VersionNotFound(String, String),
    #[error("version {1} of {0} is the active version; switch to another one first")]
    VersionInUse(String, String),
    /// [`Engine::launch`] could not find or start the program.
    #[error("{0}")]
    Launch(String),
    #[error(transparent)]
    Lock(#[from] crate::lock::LockError),
}

impl EngineError {
    /// Whether the operation stopped because its [`crate::cancel::CancelToken`]
    /// was flipped, rather than because it failed. A UI marks such work as
    /// paused and keeps it queued.
    pub fn is_cancelled(&self) -> bool {
        matches!(self, EngineError::Install(err) if err.is_cancelled())
    }
}

/// The engine: central handle for all package operations.
pub struct Engine {
    services: Services,
}

/// Outcome of an automatic bucket refresh (see
/// [`Engine::sync_buckets_if_due`]).
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct BucketSync {
    /// Buckets that were pulled successfully.
    pub pulled: usize,
    /// `(bucket, error)` for every pull that failed; the refresh continues with
    /// the remaining buckets.
    pub failed: Vec<(String, String)>,
}

/// Upstream update probe over concrete bucket checkouts (name + local root).
///
/// Split out of [`Engine::check_bucket_updates`] so a caller can release its
/// engine lock before the fetches: each one is a `git` process plus a network
/// round trip, which can take many seconds.
///
/// `glue_root` is what resolves the git executable (a bootstrapped MinGit
/// lives under it), so it is passed even though the buckets carry their own
/// paths.
pub fn check_bucket_updates_for(
    glue_root: &Path,
    buckets: &[(String, std::path::PathBuf)],
) -> Vec<BucketUpdateStatus> {
    // Cap the fan-out: a fetch is a git process plus network wait, and a
    // large bucket set should not spawn dozens of processes at once.
    const MAX_FETCH_WORKERS: usize = 8;
    let workers = buckets.len().min(MAX_FETCH_WORKERS);
    let results: Mutex<Vec<BucketUpdateStatus>> = Mutex::new(Vec::new());
    let cursor = AtomicUsize::new(0);

    if workers > 0 {
        std::thread::scope(|scope| {
            for _ in 0..workers {
                let results = &results;
                let cursor = &cursor;
                std::thread::Builder::new()
                    .stack_size(crate::cache::workers::WORKER_STACK_SIZE)
                    .spawn_scoped(scope, move || loop {
                        let index = cursor.fetch_add(1, Ordering::Relaxed);
                        let Some((name, root)) = buckets.get(index) else {
                            break;
                        };
                        let mut status = BucketUpdateStatus {
                            name: name.clone(),
                            ok: false,
                            has_updates: false,
                            local_commit: String::new(),
                            remote_commit: String::new(),
                            error: String::new(),
                        };
                        match crate::git::fetch(glue_root, root) {
                            Ok(()) => {
                                let local = crate::git::rev_parse_head(glue_root, root);
                                let remote = crate::git::rev_parse(glue_root, root, "@{u}");
                                status.ok = true;
                                status.local_commit = local;
                                status.remote_commit = remote.clone();
                                status.has_updates =
                                    !remote.is_empty() && remote != status.local_commit;
                            }
                            Err(err) => status.error = err.to_string(),
                        }
                        results
                            .lock()
                            .unwrap_or_else(|poisoned| poisoned.into_inner())
                            .push(status);
                    })
                    .expect("spawn bucket fetch worker");
            }
        });
    }

    let mut out = results
        .into_inner()
        .unwrap_or_else(|poisoned| poisoned.into_inner());
    out.sort_by(|a, b| a.name.cmp(&b.name));
    out
}

/// Bucket checkouts under `<glue_root>/buckets`, sorted by name.
///
/// Only git checkouts are returned: a pull needs `.git`, and a stray directory
/// must not become a per-bucket failure on every refresh. Reads the disk rather
/// than the engine registry so the caller can run outside the engine lock —
/// the desktop pulls and checks from a worker thread.
pub fn bucket_checkouts_from_disk(glue_root: &Path) -> Vec<(String, PathBuf)> {
    let dir = glue_root.join("buckets");
    let entries = match std::fs::read_dir(&dir) {
        Ok(entries) => entries,
        Err(_) => return Vec::new(),
    };
    let mut out: Vec<(String, PathBuf)> = entries
        .flatten()
        .filter(|entry| entry.file_type().map(|kind| kind.is_dir()).unwrap_or(false))
        .filter(|entry| entry.path().join(".git").is_dir())
        .map(|entry| {
            (
                entry.file_name().to_string_lossy().into_owned(),
                entry.path(),
            )
        })
        .collect();
    out.sort_by(|a, b| a.0.cmp(&b.0));
    out
}

/// Concurrent bucket pulls for one refresh (each is a `git` process plus a
/// network round trip).
const MAX_PULL_WORKERS: usize = 8;

/// Pull every checkout concurrently.
///
/// Never fails as a whole: a bucket that cannot reach its remote is reported in
/// [`BucketSync::failed`] while the others still run. Does not touch the engine
/// registry — the caller reloads it once after the batch, so a refresh costs one
/// in-memory rebuild instead of one per bucket.
pub fn pull_bucket_checkouts(glue_root: &Path, buckets: &[(String, PathBuf)]) -> BucketSync {
    let workers = buckets.len().min(MAX_PULL_WORKERS);
    if workers == 0 {
        return BucketSync::default();
    }
    let cursor = AtomicUsize::new(0);
    // (name, error) — an empty error means the pull succeeded.
    let results: Mutex<Vec<(String, String)>> = Mutex::new(Vec::new());
    std::thread::scope(|scope| {
        for _ in 0..workers {
            let cursor = &cursor;
            let results = &results;
            scope.spawn(move || loop {
                let index = cursor.fetch_add(1, Ordering::Relaxed);
                let Some((name, root)) = buckets.get(index) else {
                    break;
                };
                let error = match crate::git::pull(glue_root, root) {
                    Ok(_) => String::new(),
                    Err(e) => e.to_string(),
                };
                results
                    .lock()
                    .unwrap_or_else(|poisoned| poisoned.into_inner())
                    .push((name.clone(), error));
            });
        }
    });

    let mut sync = BucketSync::default();
    for (name, error) in results
        .into_inner()
        .unwrap_or_else(|poisoned| poisoned.into_inner())
    {
        if error.is_empty() {
            sync.pulled += 1;
        } else {
            sync.failed.push((name, error));
        }
    }
    sync.failed.sort_by(|a, b| a.0.cmp(&b.0));
    sync
}

/// Whether `pkg_name` is pinned to its installed version.
///
/// One implementation for the engine's update check, the install pipeline and
/// the desktop's installed list.
pub(crate) fn version_locked(services: &Services, pkg_name: &str) -> bool {
    let Some(installed) = services.cache().get_installed(pkg_name) else {
        return false;
    };
    matches!(
        installed.metadata.get("versionLocked"),
        Some(serde_json::Value::Bool(true))
    )
}

/// What an update must do for one package once its metadata is known — the
/// cheap pass of [`Engine::update`], shared with [`Engine::update_many`].
enum UpdateStep {
    /// Nothing to fetch: a version already on disk was activated, or the
    /// package is current (which is reported as an error instead).
    Done(InstallOutcome),
    /// Fetch + deploy this resolved install.
    Fetch(InstallOptions),
}

impl Engine {
    /// Open or create the engine at `glue_root`. Opens the store, cache index,
    /// extractor and bucket registry through [`Services::new`].
    pub fn new(glue_root: impl AsRef<Path>) -> Result<Engine, EngineError> {
        Engine::with_config(EngineConfig::new(glue_root))
    }

    /// Open an engine with an explicit configuration.
    ///
    /// `EngineConfig::sync_installed = false` skips the startup heal of
    /// `installed_packages`; only for commands that never read installed
    /// state.
    pub fn with_config(config: EngineConfig) -> Result<Engine, EngineError> {
        let services = Services::new(config)?;
        Ok(Engine { services })
    }

    /// The shared engine state (store, cache, downloader, extractor, …).
    pub fn services(&self) -> &Services {
        &self.services
    }

    /// Mutable access to the shared engine state (for refresh/lock paths).
    pub fn services_mut(&mut self) -> &mut Services {
        &mut self.services
    }

    /// The data root this engine operates on.
    pub fn glue_root(&self) -> &Path {
        self.services.root_dir()
    }

    /// Make sure a usable `git` exists for this data root, bootstrapping MinGit
    /// under `<root>/bin/git` on a machine that has none.
    ///
    /// `git` is a runtime dependency rather than a crate one, so a fresh
    /// machine simply does not have it; every bucket operation calls this
    /// first. It costs a couple of stats on a machine that already has git, so
    /// calling it per bucket hangs no measurable weight on a bucket update.
    pub fn ensure_git(&self) -> Result<(), EngineError> {
        crate::bootstrap::ensure_git(&self.services, None)
            .map(|_| ())
            .map_err(|e| EngineError::Git("bootstrap git".to_string(), e))
    }

    /// Exclusive cross-process guard for operations that mutate the data root.
    /// Read-only commands never take it, so they can run alongside an install.
    fn mutation_lock(&self) -> Result<crate::lock::MutationLock, EngineError> {
        Ok(crate::lock::MutationLock::acquire(self.glue_root())?)
    }

    /// Install a package. `options.package_ref` selects the package; the
    /// optional `progress` callback receives coarse pipeline events.
    ///
    /// Takes `&self`: the pipeline resolves through `&Services` (the registry
    /// and search index are internally synchronized), so a caller holding only
    /// a shared borrow — the desktop worker's read lock — can run it, and two
    /// installs can overlap, one downloading while the other deploys.
    pub fn install(
        &self,
        options: InstallOptions,
        progress: Option<ProgressFn>,
    ) -> Result<InstallOutcome, EngineError> {
        let lock = self.mutation_lock()?;
        self.install_with_lock(options, progress, &lock)
    }

    /// [`Self::install`] with a caller-supplied data-root hold.
    ///
    /// The desktop shares one [`crate::lock::MutationLock`] between concurrent
    /// installs: a second handle in the same process cannot take the OS lock,
    /// so every install in flight must reuse the same guard (nested fetch
    /// windows keep the hold given up until the last download ends).
    pub fn install_with_lock(
        &self,
        mut options: InstallOptions,
        progress: Option<ProgressFn>,
        lock: &crate::lock::MutationLock,
    ) -> Result<InstallOutcome, EngineError> {
        if options.progress.is_none() {
            options.progress = progress;
        }
        install(&self.services, &options, lock).map_err(EngineError::Install)
    }

    /// Install several packages, running their fetch + deploy concurrently
    /// (resolve stays sequential). Non-fail-fast: returns one result per input
    /// option, in input order. Deliberate divergence from, which installs
    /// multiple packages sequentially — see
    /// [`crate::engine::install::install_many`] for the model and the shared
    /// `shims/`-namespace caveat.
    pub fn install_many(
        &self,
        options_list: Vec<InstallOptions>,
        progress: Option<ProgressFn>,
    ) -> Vec<Result<InstallOutcome, EngineError>> {
        let _lock = match crate::lock::MutationLock::acquire(self.glue_root()) {
            Ok(lock) => lock,
            // One result per input, matching the non-fail-fast contract.
            Err(err) => {
                return options_list
                    .iter()
                    .map(|_| Err(EngineError::Lock(err.clone())))
                    .collect()
            }
        };
        crate::engine::install::install_many(&self.services, options_list, progress)
            .into_iter()
            .map(|r| r.map_err(EngineError::Install))
            .collect()
    }

    /// Resolve a package's active install directory, if any.
    pub fn installed_dir(&self, package: &str) -> Option<PathBuf> {
        let pkg_root = apps::pkg_root(self.glue_root(), package);
        apps::active_install_dir(&pkg_root).map(|(dir, _)| dir)
    }

    /// Start an installed package's program.
    ///
    /// `program` picks one of [`launch_candidates`]; `None` starts the default
    /// (the first openable program, GUI before console). A GUI program is
    /// started through the shell; a console program (`.exe`/`.bat`/`.cmd`/
    /// `.jar`) gets its own Command Prompt window that stays open, so its
    /// output is readable instead of flashing past.
    pub fn launch(
        &self,
        package: &str,
        program: Option<&Path>,
    ) -> Result<LaunchTarget, EngineError> {
        let name = crate::catalog::package_base_name(package);
        let pkg_root = apps::pkg_root(self.glue_root(), &name);
        let candidates = launch_candidates(self.glue_root(), &name);
        let (chosen_program, chosen_name, chosen_kind) = match program {
            Some(requested) => match candidates
                .iter()
                .find(|candidate| same_path(&candidate.program, requested))
            {
                Some(candidate) => (
                    candidate.program.clone(),
                    candidate.name.clone(),
                    candidate.kind,
                ),
                None if requested.is_file() => (
                    requested.to_path_buf(),
                    requested
                        .file_stem()
                        .and_then(|stem| stem.to_str())
                        .unwrap_or(&name)
                        .to_string(),
                    launch_kind_for(requested),
                ),
                None => {
                    return Err(EngineError::Launch(format!(
                        "{} is not a program of {name}",
                        requested.display()
                    )))
                }
            },
            None => match candidates.iter().find(|candidate| candidate.openable()) {
                Some(candidate) => (
                    candidate.program.clone(),
                    candidate.name.clone(),
                    candidate.kind,
                ),
                None => {
                    return match apps::active_install_dir(&pkg_root) {
                        Some(_) => Err(EngineError::Launch(format!(
                            "{name} has no program to start (every candidate is hidden, see the launch settings)"
                        ))),
                        None => Err(EngineError::NotInstalled(package.to_string())),
                    };
                }
            },
        };
        if chosen_kind == LaunchKind::Skip {
            return Err(EngineError::Launch(format!(
                "{chosen_name} is hidden from the launch menu"
            )));
        }

        let working_dir = apps::active_install_dir(&pkg_root)
            .map(|(dir, _)| dir)
            .or_else(|| chosen_program.parent().map(Path::to_path_buf))
            .unwrap_or_else(|| self.glue_root().to_path_buf());
        open_launch_file(&chosen_program, chosen_kind, &working_dir)
            .map_err(|e| EngineError::Launch(format!("start {}: {e}", chosen_program.display())))?;
        Ok(LaunchTarget {
            program: chosen_program,
            name: chosen_name,
        })
    }

    /// Number of packages with an active install under `apps/`.
    pub fn installed_count(&self) -> usize {
        self.services.installed_package_count()
    }

    /// Uninstall a package: remove shims, shortcuts, env_set, the version
    /// directory, and the cache-index installed row.
    pub fn uninstall(
        &self,
        package: &str,
        progress: Option<ProgressFn>,
    ) -> Result<UninstallOutcome, EngineError> {
        let lock = self.mutation_lock()?;
        self.uninstall_with_lock(package, progress, &lock)
    }

    /// [`Self::uninstall`] with a caller-supplied data-root hold, so a frontend
    /// that already shares one guard between its concurrent operations (the
    /// desktop) does not open a second OS handle and time out waiting for its
    /// own lock. Kept for the CLI and existing callers; does not kill running
    /// processes.
    pub fn uninstall_with_lock(
        &self,
        package: &str,
        progress: Option<ProgressFn>,
        lock: &crate::lock::MutationLock,
    ) -> Result<UninstallOutcome, EngineError> {
        self.uninstall_with_lock_kill(package, progress, lock, false)
    }

    /// [`Self::uninstall_with_lock`] plus process handling. When
    /// `kill_running` is `true`, executables from the package that are still
    /// running are terminated before removal; otherwise their presence is
    /// reported as [`EngineError::ProcessesRunning`] so the caller can ask the
    /// user first.
    pub fn uninstall_with_lock_kill(
        &self,
        package: &str,
        progress: Option<ProgressFn>,
        _lock: &crate::lock::MutationLock,
        kill_running: bool,
    ) -> Result<UninstallOutcome, EngineError> {
        let emit = |event: ProgressEvent| {
            if let Some(cb) = &progress {
                cb(event);
            }
        };

        let pkg_root = apps::pkg_root(self.glue_root(), package);
        let Some((install_dir, version)) = apps::active_install_dir(&pkg_root) else {
            return Err(EngineError::NotInstalled(package.to_string()));
        };
        // Measured before anything is deleted, so the CLI can report what was
        // actually removed (prints `Removing <dir> (N file(s), X MiB)`).
        let (file_count, size) = dir_stats(&install_dir);

        emit(ProgressEvent::Removing {
            package: package.to_string(),
            version: version.clone(),
        });

        let manifest: Option<crate::manifest::Manifest> = apps::load_install_record(&install_dir)
            .ok()
            .and_then(|r| r.manifest)
            .and_then(|v| serde_json::from_value(v).ok());

        // falls back to the bucket manifest
        // when the version directory has no install record (installs made by an
        // older gluestick, or a partially extracted directory).
        let install_arch_from_record = apps::load_install_record(&install_dir)
            .ok()
            .map(|r| r.bucket)
            .unwrap_or_default();
        let (manifest, bucket_name) = match manifest {
            Some(m) => (Some(m), install_arch_from_record),
            None => {
                let resolved =
                    crate::engine::catalog::resolve_install_ref(self.services(), package).ok();
                let loaded = resolved.and_then(|r| {
                    self.services
                        .registry()
                        .find_manifest(&r)
                        .ok()
                        .map(|(bucket, _path, m)| (m, bucket))
                });
                match loaded {
                    Some((m, bucket)) => (Some(m), bucket),
                    None => (None, String::new()),
                }
            }
        };
        let bucket_name = if bucket_name.is_empty() {
            "main".to_string()
        } else {
            bucket_name
        };

        let install_arch = manifest
            .as_ref()
            .map(|_m| crate::manifest::host_arch())
            .unwrap_or_default();
        let pkg_ref = format!("{bucket_name}/{package}");

        // --- pre_uninstall + uninstaller.script ---
        // These run before anything is deleted: service teardown hooks must
        // precede process checks and file removal (Scoop order).
        if let Some(m) = &manifest {
            crate::engine::install::run_manifest_uninstall_hooks(
                &self.services,
                m,
                &install_arch,
                &install_dir,
                package,
                &pkg_ref,
                &version,
            )
            .map_err(EngineError::Install)?;
        }

        // --- remove shims for this package's binaries ---
        // ownership scan over shims-meta,
        // not manifest bin-name derivation (install-time shim names drop
        // executable extensions and `env_add_path` shims have no bin entry).
        let mut removed_shims = Vec::new();
        if let Ok(mgr) = crate::shim::Manager::new(self.glue_root()) {
            removed_shims = mgr.remove_shims_for_package(package);
        }

        // --- remove start-menu shortcuts ---
        if let Some(m) = &manifest {
            let _ = crate::winshell::remove_package_shortcuts(m, &install_arch);
        }

        // --- remove user env_set values ---
        if let Some(m) = &manifest {
            let env_names: Vec<String> = m.env_set_for_install(&install_arch).into_keys().collect();
            if !env_names.is_empty() {
                let _ = crate::winshell::remove_user_env(&env_names);
            }
        }

        // --- save persist data out of the version directory ---
        if let Some(m) = &manifest {
            let persist_dir = self.glue_root().join("persist").join(package);
            crate::engine::persist::save_persist_on_uninstall(
                &install_dir,
                &persist_dir,
                &m.persist_entries_for_install(&install_arch),
            )
            .map_err(|err| {
                EngineError::Install(crate::engine::install::InstallError::App(Box::new(err)))
            })?;
        }
        // --- stop still-running programs from this package ---
        // Windows cannot delete a mapped executable, so a running app (e.g. an
        // Electron program with no manifest `uninstaller.script`) would make
        // `remove_dir_all` fail and leave the directory behind. Detect the
        // package's own executables and either kill them (when the user already
        // agreed) or refuse so the caller can prompt.
        let running = running_exes_in(&install_dir);
        if !running.is_empty() {
            if kill_running {
                kill_exes(&running);
                // Give the OS a beat to flush the process off the files before
                // the removal attempts (the manifest hook settle does the same
                // for script-driven kills).
                std::thread::sleep(std::time::Duration::from_millis(1500));
            } else {
                return Err(EngineError::ProcessesRunning(running.join(", ")));
            }
        }

        // --- delete the version directory ---
        // A failure here was previously swallowed; a vanished directory is the
        // one outcome the UI cannot recover from otherwise.
        std::fs::remove_dir_all(&install_dir).map_err(|e| {
            EngineError::Install(crate::engine::install::InstallError::App(Box::new(e)))
        })?;

        // --- post_uninstall (runs it after removal; failures are warnings) ---
        if let Some(m) = &manifest {
            let _ = crate::engine::install::run_manifest_post_uninstall_hooks(
                &self.services,
                m,
                &install_arch,
                &install_dir,
                package,
                &pkg_ref,
                &version,
            );
        }

        // --- if no versions remain, drop the current link + pkg root ---
        let remaining = apps::list_versions(&pkg_root);
        if remaining.is_empty() {
            let current = pkg_root.join(apps::CURRENT_LINK_NAME);
            let _ = apps::remove_current_link(&current);
            let _ = std::fs::remove_dir(&pkg_root);
        } else if let Some(newest) = remaining.last() {
            // Older versions survive an uninstall of the active one. Re-point
            // `current` at the newest survivor so the package stays usable and
            // `update` compares against what is actually on disk instead of a
            // dangling link to the removed version.
            let _ = apps::link_current(&pkg_root, newest);
        }

        // --- update cache index (keep content cache for fast reinstall) ---
        self.services
            .cache()
            .remove_installed(package)
            .map_err(|e| EngineError::Install(crate::engine::install::InstallError::Index(e)))?;

        emit(ProgressEvent::Uninstalled {
            package: package.to_string(),
        });

        Ok(UninstallOutcome {
            package: package.to_string(),
            version,
            install_dir,
            file_count,
            size,
            removed_shims,
        })
    }

    /// Update a package to the latest bucket version. If already current,
    /// returns [`EngineError::UpToDate`].
    pub fn update(
        &self,
        package: &str,
        progress: Option<ProgressFn>,
    ) -> Result<InstallOutcome, EngineError> {
        let lock = self.mutation_lock()?;
        self.update_with_lock(package, progress, None, &lock)
    }

    /// [`Self::update`] with a stop signal and a caller-supplied data-root
    /// hold (see [`Self::install_with_lock`]).
    pub fn update_with_lock(
        &self,
        package: &str,
        progress: Option<ProgressFn>,
        cancel: Option<crate::cancel::CancelToken>,
        lock: &crate::lock::MutationLock,
    ) -> Result<InstallOutcome, EngineError> {
        match self.update_preflight(package, progress.as_ref())? {
            UpdateStep::Done(outcome) => Ok(outcome),
            UpdateStep::Fetch(mut options) => {
                options.progress = progress;
                options.cancel = cancel;
                install(&self.services, &options, lock).map_err(EngineError::Install)
            }
        }
    }

    /// Update several packages with their downloads and deploys overlapped.
    ///
    /// Each package's metadata pass ([`Self::update_preflight`]) runs first,
    /// in input order, because it needs `&mut Services`; everything that moves
    /// bytes then goes through [`crate::engine::install::install_many`], which
    /// runs the per-package fetch + deploy under one `thread::scope`. This is
    /// the batch counterpart of [`Self::update`]: a package that is current or
    /// not installed fails on its own and the rest proceed (non-fail-fast, one
    /// result per input, in input order).
    ///
    /// The shared-`shims/` caveat documented on `install_many` applies here
    /// too: two packages in the same batch providing the same shim name race.
    pub fn update_many(
        &self,
        packages: &[String],
        progress: Option<ProgressFn>,
    ) -> Vec<Result<InstallOutcome, EngineError>> {
        // One hold for the whole batch: `install_many` never releases the
        // data-root hold mid-run, and its workers must not try to take it.
        let _lock = match crate::lock::MutationLock::acquire(self.glue_root()) {
            Ok(lock) => lock,
            Err(err) => {
                return packages
                    .iter()
                    .map(|_| Err(EngineError::Lock(err.clone())))
                    .collect()
            }
        };

        let mut slots: Vec<Option<Result<InstallOutcome, EngineError>>> =
            (0..packages.len()).map(|_| None).collect();
        // Packages whose metadata pass asked for a real install, with the
        // input index each result belongs to.
        let mut pending: Vec<(usize, InstallOptions)> = Vec::new();
        for (i, package) in packages.iter().enumerate() {
            match self.update_preflight(package, progress.as_ref()) {
                Ok(UpdateStep::Done(outcome)) => slots[i] = Some(Ok(outcome)),
                Ok(UpdateStep::Fetch(options)) => pending.push((i, options)),
                Err(err) => slots[i] = Some(Err(err)),
            }
        }

        if !pending.is_empty() {
            let indices: Vec<usize> = pending.iter().map(|(i, _)| *i).collect();
            let options: Vec<InstallOptions> =
                pending.into_iter().map(|(_, options)| options).collect();
            let results = crate::engine::install::install_many(&self.services, options, progress);
            for (index, result) in indices.into_iter().zip(results) {
                slots[index] = Some(result.map_err(EngineError::Install));
            }
        }

        slots
            .into_iter()
            .map(|slot| slot.expect("every input got a result"))
            .collect()
    }

    /// The metadata pass shared by [`Self::update`] and [`Self::update_many`].
    fn update_preflight(
        &self,
        package: &str,
        progress: Option<&ProgressFn>,
    ) -> Result<UpdateStep, EngineError> {
        // `package` may be a bucket-prefixed install ref (`extras/magpie-ai`);
        // the on-disk layout is always `apps/<baseName>`, so strip the bucket
        // (and any `@version`) before resolving the install dir. The ref is
        // still forwarded to `resolve_install_ref` below, which needs the
        // bucket to locate the manifest — parity: PkgRoot is always
        // `apps/<name>` regardless of which bucket a package came from.
        let pkg_root = apps::pkg_root(
            self.glue_root(),
            &crate::catalog::package_base_name(package),
        );
        let Some((_install_dir, current_version)) = apps::active_install_dir(&pkg_root) else {
            return Err(EngineError::NotInstalled(package.to_string()));
        };

        let resolved = crate::engine::catalog::resolve_install_ref(&self.services, package)
            .map_err(|e| {
                EngineError::Install(crate::engine::install::InstallError::App(Box::new(e)))
            })?;

        let (_bucket, _path, manifest) = self
            .services
            .registry()
            .find_manifest(&resolved)
            .map_err(|e| EngineError::Install(crate::engine::install::InstallError::Bucket(e)))?;

        let latest = manifest.version.clone();
        if latest == current_version {
            return Err(EngineError::UpToDate(package.to_string(), current_version));
        }

        // A downgrade keeps the higher version's directory on disk, so an
        // "update" back to it is a switch, not a reinstall: the bytes are
        // already there, and switching reuses that version's own links,
        // shims and persist data.
        let name = crate::catalog::package_base_name(package);
        let latest_dir = pkg_root.join(&latest);
        if apps::load_install_record(&latest_dir).is_ok() {
            let emit = |event: ProgressEvent| {
                if let Some(cb) = progress {
                    cb(event);
                }
            };
            emit(ProgressEvent::Started {
                package: package.to_string(),
            });
            self.activate_version(&name, &latest)?;
            emit(ProgressEvent::Completed {
                package: package.to_string(),
                version: latest.clone(),
            });
            return Ok(UpdateStep::Done(InstallOutcome {
                bucket: crate::catalog::package_bucket_name(package),
                package: name,
                version: latest,
                install_dir: latest_dir,
                size: 0,
                files: 0,
                from_cache: true,
                already_installed: true,
                description: String::new(),
                shims: Vec::new(),
                linked_files: 0,
                suggestions: manifest.suggestions(),
            }));
        }

        Ok(UpdateStep::Fetch(InstallOptions {
            package_ref: resolved.clone(),
            version: String::new(),
            arch: String::new(),
            // Update path always installs with Force: true so the version
            // switch really relinks `current`. `reuse_cache` keeps the artifact
            // blob from the store instead of re-downloading identical bytes:
            // the manifest hash it is verified against is unchanged.
            force: true,
            reuse_cache: true,
            // `update` has no --timings flag (parity: only `install` does).
            timings: false,
            // Updates never run interactively: a wizard on an unseen window
            // would block a bulk upgrade.
            interactive: false,
            // Filled in by the caller: `update` forwards its own token here.
            cancel: None,
            progress: None,
            // An upgrade installs only what the user asked for: pulling in new
            // `depends` behind their back is not `update`'s job (Scoop's
            // `update` does not install dependencies either).
            independent: true,
        }))
    }

    /// Compare installed packages against bucket manifests
    /// Version-locked packages are skipped; the result
    /// is ordered by package name.
    pub fn check_package_updates(&self) -> Result<Vec<PackageUpdate>, EngineError> {
        let versions = self.installed_packages_for_update_check()?;
        let mut updates = Vec::new();
        // A BTreeMap iterates in key order, which.
        for (name, indexed) in versions {
            if indexed.is_empty() {
                continue;
            }
            if version_locked(&self.services, &name) {
                continue;
            }
            let installed = self.resolve_installed_version(&name, &indexed);
            let bucket = self.installed_bucket_for_update(&name, &installed);
            let Some(latest) = self.latest_manifest_version(&name, &bucket) else {
                continue;
            };
            if !crate::engine::version::update_available(&installed, &latest) {
                continue;
            }
            updates.push(PackageUpdate {
                name,
                installed_version: installed,
                latest_version: latest,
                bucket,
            });
        }
        Ok(updates)
    }

    /// the apps/ dir on disk is
    /// authoritative; cache index versions are hints only (a cleared cache
    /// must not hide available updates).
    fn installed_packages_for_update_check(
        &self,
    ) -> Result<std::collections::BTreeMap<String, String>, EngineError> {
        let apps_dir = self.glue_root().join("apps");
        let entries = match std::fs::read_dir(&apps_dir) {
            Ok(e) => e,
            // No apps dir means nothing is installed; the content cache may
            // still hold rows for uninstalled packages and must not turn them
            // into "installed, outdated".
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => {
                return Ok(std::collections::BTreeMap::new())
            }
            Err(e) => {
                return Err(EngineError::Bucket(
                    apps_dir.display().to_string(),
                    format!("read dir: {e}"),
                ))
            }
        };
        let mut out = std::collections::BTreeMap::new();
        for entry in entries.flatten() {
            let name = entry.file_name().to_string_lossy().into_owned();
            let is_dir = std::fs::metadata(entry.path())
                .map(|m| m.is_dir())
                .unwrap_or(false);
            if !is_dir {
                continue;
            }
            let Some((_dir, disk_version)) =
                active_listed_install(self.glue_root(), &name, self.services.cache())
            else {
                continue;
            };
            // The disk is authoritative. The content-cache row still names the
            // version it last indexed, which after `uninstall` of the active
            // version is exactly the version that is gone.
            out.insert(name, disk_version);
        }
        Ok(out)
    }

    /// the on-disk active version wins; a stale
    /// index row is healed in place.
    fn resolve_installed_version(&self, pkg_name: &str, indexed_version: &str) -> String {
        let pkg_root = apps::pkg_root(self.glue_root(), pkg_name);
        let Some(current) = apps::read_current(&pkg_root) else {
            return indexed_version.to_string();
        };
        if current.is_empty() {
            return indexed_version.to_string();
        }
        // Uninstalling the active version can leave `current` pointing at a
        // directory that no longer exists. Trusting it would report the removed
        // version as installed and hide the upgrade that is now available.
        if !version_dir_installed(self.glue_root(), pkg_name, &current) {
            return indexed_version.to_string();
        }
        if current != indexed_version {
            let install_dir = pkg_root.join(&current);
            let _ = self.services.cache().set_active_version_install_dir(
                pkg_name,
                &current,
                &install_dir.to_string_lossy(),
            );
        }
        current
    }

    /// (bucket half only — the manifest-info
    /// half serves the detailed list path, which lives in `list`).
    fn installed_bucket_for_update(&self, name: &str, installed: &str) -> String {
        let pkg_root = apps::pkg_root(self.glue_root(), name);
        let ver = if !installed.is_empty() {
            installed.to_string()
        } else {
            apps::read_current(&pkg_root).unwrap_or_default()
        };
        if !ver.is_empty() {
            if let Ok(rec) = apps::load_install_record(&pkg_root.join(&ver)) {
                if !rec.bucket.is_empty() {
                    return rec.bucket;
                }
            }
        }
        // No bucket from the install record: fall back to the bucket repos.
        self.find_bucket_manifest(name).unwrap_or_default()
    }

    /// locate `name` in the bucket repos under root.
    fn find_bucket_manifest(&self, name: &str) -> Option<String> {
        let registry = self.services.registry();
        for b in registry.list() {
            let manifest_path = b.root.join("bucket").join(format!("{name}.json"));
            if manifest_path.is_file() && crate::manifest::parse_file(&manifest_path).is_ok() {
                return Some(b.name.clone());
            }
        }
        None
    }

    /// the bucket manifest version for a package,
    /// or `None` when the manifest is missing or unversioned.
    fn latest_manifest_version(&self, pkg_name: &str, bucket: &str) -> Option<String> {
        let ref_ = manifest_lookup_ref(pkg_name, bucket);
        let (_, _, manifest) = self.services.registry().find_manifest(&ref_).ok()?;
        if manifest.version.is_empty() {
            return None;
        }
        Some(manifest.version)
    }

    /// List installed packages, sorted by name.
    ///
    /// `apps/` is authoritative; the cache index is only a hint
    /// A package counts as installed when its active
    /// version directory exists, is non-empty and carries a readable
    /// `install.json`. Reading the index alone makes a wiped index look like
    /// "nothing installed" even though every package is still on disk — the
    /// state `update` never gets into, because it already scans `apps/`.
    pub fn list_installed(&self) -> Result<Vec<InstalledPackageSummary>, EngineError> {
        let apps_dir = self.glue_root().join("apps");
        let entries = match std::fs::read_dir(&apps_dir) {
            Ok(entries) => entries,
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(Vec::new()),
            Err(e) => {
                return Err(EngineError::Bucket(
                    apps_dir.display().to_string(),
                    format!("read dir: {e}"),
                ))
            }
        };

        let mut names: Vec<String> = Vec::new();
        for entry in entries.flatten() {
            // Skips non-directories directly under apps/.
            if entry.file_type().map(|t| t.is_dir()).unwrap_or(false) {
                names.push(entry.file_name().to_string_lossy().into_owned());
            }
        }
        // Sorts the result by package name.
        names.sort();

        // Metadata only: `get()` also loads every `package_files` row per
        // package (8k+ rows for large packages) and `list` never reads the
        // file map. One bulk query replaces N per-package two-query fetches.
        let cache_meta = self.services.cache().list_packages();
        let mut out = Vec::with_capacity(names.len());
        for name in names {
            let Some((install_dir, disk_version)) = self.active_listed_install(&name) else {
                continue;
            };
            // Size / install time come from the content-cache row
            // without one the package is still listed,
            // just without those numbers. The version always comes from disk:
            // the cache row keeps naming the version it last indexed, which
            // after an uninstall of the active version is the removed one.
            let (version, size, install_time) = match cache_meta.get(&name) {
                Some(entry) => {
                    // The row's own version lags behind a version switch, and
                    // it is what `cache_reusable_for_install` compares against
                    // the manifest: heal it here as well
                    // otherwise a switched package re-downloads on its next
                    // update.
                    self.resolve_listed_version(&name, &entry.version);
                    (
                        self.resolve_listed_version(&name, &disk_version),
                        entry.size,
                        entry.installed.clone(),
                    )
                }
                None => (disk_version, 0, String::new()),
            };
            out.push(InstalledPackageSummary {
                name,
                version,
                install_dir,
                install_time,
                size,
            });
        }
        Ok(out)
    }

    /// install dir and version of a *completed*
    /// install, or `None` when the package is not (fully) installed.
    ///
    /// Tries the cache hint first, then the `current` junction, then every
    /// version dir newest-first. The latter two require a readable
    /// `install.json`, which is what separates a finished install from a
    /// half-deployed directory.
    fn active_listed_install(&self, pkg_name: &str) -> Option<(PathBuf, String)> {
        active_listed_install(self.glue_root(), pkg_name, self.services.cache())
    }

    /// the `current` junction wins over the
    /// indexed version, and a disagreement is written back so the index heals
    /// itself (calls `Cache.SetActiveVersionInstallDir`).
    fn resolve_listed_version(&self, pkg_name: &str, indexed_version: &str) -> String {
        let pkg_root = apps::pkg_root(self.glue_root(), pkg_name);
        let Some(current) = apps::read_current(&pkg_root) else {
            return indexed_version.to_string();
        };
        if current.is_empty() {
            return indexed_version.to_string();
        }
        // Uninstalling the active version can leave `current` pointing at a
        // directory that no longer exists. Trusting it would report the removed
        // version as installed and hide the upgrade that is now available.
        if !version_dir_installed(self.glue_root(), pkg_name, &current) {
            return indexed_version.to_string();
        }
        if current != indexed_version {
            let install_dir = pkg_root.join(&current);
            let _ = self.services.cache().set_active_version_install_dir(
                pkg_name,
                &current,
                &install_dir.to_string_lossy(),
            );
        }
        current
    }

    /// Search available packages across buckets.
    pub fn search(
        &self,
        query: &str,
        buckets: &[String],
        limit: usize,
    ) -> Result<Vec<SearchResult>, EngineError> {
        let results =
            crate::engine::catalog::search_packages(&self.services, query, buckets, limit);
        Ok(results
            .into_iter()
            .map(|p| SearchResult {
                bucket: p.bucket,
                name: p.name,
                version: p.version,
                description: p.description,
            })
            .collect())
    }

    /// Doctor: environment and dependency checks (.Doctor`).
    pub fn doctor(&self) -> Result<DoctorReport, EngineError> {
        let root = self.glue_root();

        let root_exists = root.exists();
        let buckets_dir = root.join("buckets");
        let mut buckets: Vec<String> = Vec::new();
        if let Ok(entries) = std::fs::read_dir(&buckets_dir) {
            for entry in entries.flatten() {
                let path = entry.path();
                if std::fs::metadata(&path)
                    .map(|m| m.is_dir())
                    .unwrap_or(false)
                {
                    buckets.push(entry.file_name().to_string_lossy().into_owned());
                }
            }
        }
        buckets.sort();

        let host = crate::manifest::HostInfo::detect();
        // Both installers ship the stub beside `glue.exe`, so an installation has
        // one before any shim is created; otherwise it is cached in the
        // workspace on the first shim operation.
        let install_root = crate::paths::installed_data_root();
        let exe = std::env::current_exe().ok();
        let shim_present = shim_stub_present(root, exe.as_deref(), install_root.as_deref());
        // Configuration is program-scoped, so this may live beside the
        // installation rather than in the workspace.
        let config = crate::config::config_path(root);

        let installed = self.installed_count();

        // Installed packages whose `depends` are not installed. The check has
        // to predict what `install` would do, so it uses the installer's own
        // predicate (`cache.get_installed(base_name).is_some()`) instead of
        // probing `apps/`: a package on disk without a cache row is one the
        // installer would fetch again. Root-level `depends` only — the
        // installer never expands `depends_pre`/`depends_post`.
        let mut missing_dependencies = Vec::new();
        for package in self.list_installed().unwrap_or_default() {
            // An unreadable record is a broken install, not evidence that a
            // dependency is missing: skip it rather than guess.
            let Some(manifest) =
                self.installed_package_manifest(&package.name, &package.install_dir)
            else {
                continue;
            };
            for dependency in manifest.depends() {
                // An unresolvable ref still counts as missing: `install` would
                // fail trying to fetch it, which is exactly what the user
                // needs to know before running the install.
                let name = crate::catalog::package_base_name(dependency);
                if self.services.cache().get_installed(&name).is_none() {
                    missing_dependencies.push(MissingDependency {
                        package: package.name.clone(),
                        dependency: dependency.clone(),
                    });
                }
            }
        }
        // Deterministic report order: `list_installed` is sorted by name, but
        // the manifests' own `depends` order is not something the CLI should
        // leak into lint output.
        missing_dependencies.sort_by(|a, b| {
            a.package
                .cmp(&b.package)
                .then_with(|| a.dependency.cmp(&b.dependency))
        });

        // Every helper this build bootstraps, ready or not: a data directory
        // carried over from the version can contain extra helpers that
        // this build never uses, so the check walks the fixed list rather
        // than whatever happens to sit under `bin/`.
        let helpers = crate::bootstrap::helper_status(root);

        Ok(DoctorReport {
            data_root: root.to_path_buf(),
            data_root_exists: root_exists,
            buckets,
            host_arch: host.arch.to_string(),
            win_build: host.win_build,
            shim_present,
            config_present: config.exists(),
            installed_count: installed,
            missing_dependencies,
            helpers,
            long_paths_enabled: crate::winpath::long_paths_enabled(),
            // Cache only: `doctor` diagnoses the machine and must not become a
            // place that talks to the network.
            self_update: crate::self_update::cached_notice(root),
        })
    }

    /// The manifest an installed package was installed from: the copy in
    /// `install.json` first, then the bucket manifest.
    ///
    /// The record copy is what the install actually used, but records written
    /// before the field existed (or by an interrupted install) carry none, so
    /// the bucket lookup keeps those packages checkable. That lookup names the
    /// recorded bucket: a bare name sends `resolve_install_ref` down its
    /// unqualified path, which clones a missing `main` bucket — a network side
    /// effect a read-only report must not have.
    fn installed_package_manifest(
        &self,
        package: &str,
        install_dir: &Path,
    ) -> Option<crate::manifest::Manifest> {
        let record = apps::load_install_record(install_dir).ok();
        if let Some(manifest) = record
            .as_ref()
            .and_then(|r| r.manifest.clone())
            .and_then(|v| serde_json::from_value(v).ok())
        {
            return Some(manifest);
        }
        let bucket = record.map(|r| r.bucket).unwrap_or_default();
        let pkg_ref = if bucket.is_empty() {
            package.to_string()
        } else {
            format!("{bucket}/{package}")
        };
        let resolved =
            crate::engine::catalog::resolve_install_ref(self.services(), &pkg_ref).ok()?;
        self.services
            .registry()
            .find_manifest(&resolved)
            .ok()
            .map(|(_bucket, _path, manifest)| manifest)
    }

    /// Add (clone) a bucket repository into `<root>/buckets/<name>`.
    ///
    /// When `url` is `None` or empty, a known-bucket URL is looked up by name
    /// (.GetKnownBucketURL`). Fails with [`EngineError::Bucket`]
    /// if the name is not a known bucket and no URL was supplied.
    pub fn add_bucket(
        &mut self,
        name: &str,
        url: Option<&str>,
        progress: Option<ProgressFn>,
    ) -> Result<(), EngineError> {
        let _lock = self.mutation_lock()?;
        let resolved = match url {
            Some(u) if !u.trim().is_empty() => u.trim().to_string(),
            _ => match crate::bucket::known_bucket_url(name) {
                Some(known) => known.to_string(),
                None => {
                    return Err(EngineError::Bucket(
                        name.to_string(),
                        format!("unknown bucket '{name}', please provide a repository URL",),
                    ));
                }
            },
        };

        let buckets_dir = self.services.buckets_dir();
        let dest = buckets_dir.join(name);

        if dest.exists() {
            return Err(EngineError::Bucket(
                name.to_string(),
                "bucket already exists on disk".to_string(),
            ));
        }

        let emit = |event: ProgressEvent| {
            if let Some(cb) = &progress {
                cb(event);
            }
        };

        emit(ProgressEvent::Started {
            package: format!("bucket/{name}"),
        });

        self.ensure_git()?;

        crate::git::clone(self.glue_root(), &resolved, &dest)
            .map_err(|e| EngineError::Git(format!("clone {name}"), e.to_string()))?;

        // Refresh the in-memory registry so the new bucket is discoverable.
        self.services
            .registry_mut()
            .reload_from_disk()
            .map_err(|e| EngineError::Bucket(name.to_string(), e.to_string()))?;
        self.reload_search_index();

        emit(ProgressEvent::Completed {
            package: format!("bucket/{name}"),
            version: String::new(),
        });

        Ok(())
    }

    /// Remove a bucket by deleting its directory under `<root>/buckets/<name>`.
    /// Refreshes the registry and search index afterwards.
    pub fn remove_bucket(
        &mut self,
        name: &str,
        progress: Option<ProgressFn>,
    ) -> Result<(), EngineError> {
        let _lock = self.mutation_lock()?;
        let dest = self.services.buckets_dir().join(name);
        if !dest.exists() {
            return Err(EngineError::Bucket(
                name.to_string(),
                "bucket not found on disk".to_string(),
            ));
        }

        let emit = |event: ProgressEvent| {
            if let Some(cb) = &progress {
                cb(event);
            }
        };
        emit(ProgressEvent::Started {
            package: format!("bucket/{name}"),
        });

        // Windows keeps directory handles open briefly; retry a few times.
        let mut last_err = None;
        for _ in 0..3 {
            match std::fs::remove_dir_all(&dest) {
                Ok(()) => {
                    last_err = None;
                    break;
                }
                Err(e) => {
                    last_err = Some(e);
                    std::thread::sleep(std::time::Duration::from_millis(50));
                }
            }
        }
        if let Some(e) = last_err {
            return Err(EngineError::Bucket(
                name.to_string(),
                format!("remove bucket dir: {e}"),
            ));
        }

        self.services
            .registry_mut()
            .reload_from_disk()
            .map_err(|e| EngineError::Bucket(name.to_string(), e.to_string()))?;
        self.reload_search_index();

        emit(ProgressEvent::Completed {
            package: format!("bucket/{name}"),
            version: String::new(),
        });
        Ok(())
    }

    /// Names of all locally installed buckets (sorted by name).
    pub fn list_buckets(&self) -> Vec<String> {
        let mut names: Vec<String> = self
            .services
            .registry()
            .list()
            .iter()
            .map(|b| b.name.clone())
            .collect();
        names.sort();
        names
    }

    /// Check upstream for updates to one or more buckets (or all if `names` is
    /// empty). Fetches every bucket concurrently: each fetch starts a `git`
    /// process and waits on the network, so a sequential loop pays one process
    /// start + round trip per bucket.
    pub fn check_bucket_updates(&self, names: &[String]) -> Vec<BucketUpdateStatus> {
        // Bootstrapping git here rather than leaving it to the workers keeps a
        // machine without one from failing all eight fetches with the same
        // error; if the bootstrap itself fails, that is what they then report.
        let _ = self.ensure_git();
        let registry = self.services.registry();
        let all = registry.list();
        let wanted: Vec<(String, std::path::PathBuf)> = all
            .into_iter()
            .filter(|b| names.is_empty() || names.iter().any(|n| n == &b.name))
            .map(|b| (b.name.clone(), b.root.clone()))
            .collect();
        drop(registry);
        check_bucket_updates_for(self.glue_root(), &wanted)
    }

    /// Reload buckets from disk and rebuild the search index in one shot.
    fn reload_search_index(&mut self) {
        let _ = self.services.reload();
    }

    /// List all cached packages (cache index `packages` rows) with file counts.
    pub fn list_cache_packages(&self) -> Vec<CachePackageInfo> {
        let packages = self.services.cache().list_packages();
        let mut out: Vec<CachePackageInfo> = packages
            .into_iter()
            .map(|(name, entry)| {
                let file_count = self
                    .services
                    .cache()
                    .get(&name)
                    .map(|e| e.files.len())
                    .unwrap_or(0);
                CachePackageInfo {
                    name,
                    version: entry.version,
                    installed: entry.installed,
                    size: entry.size,
                    file_count,
                }
            })
            .collect();
        out.sort_by(|a, b| a.name.cmp(&b.name));
        out
    }

    /// One cached package with every file it indexed, for the storage tab's
    /// details dialog.
    ///
    /// Files are sorted by path and their sizes come from the store object each
    /// one is hardlinked to, so the dialog can show what the package actually
    /// occupies. A missing object reports `present: false` instead of failing:
    /// the index outliving a deleted blob is exactly the state the row would
    /// show as a size mismatch.
    pub fn cache_package_detail(&self, name: &str) -> Option<CachePackageDetail> {
        let entry = self.services.cache().get(name)?;
        let store = self.services.store();
        let mut files: Vec<CachedFile> = entry
            .files
            .iter()
            .map(|(hash, filename)| {
                // Objects are sharded (`store\ab\cdef…`), so asking the store
                // for the path is the only correct way to find one.
                let meta = std::fs::metadata(store.object_path(hash)).ok();
                CachedFile {
                    name: filename.clone(),
                    hash: hash.clone(),
                    size: meta.as_ref().map(|meta| meta.len() as i64).unwrap_or(0),
                    present: meta.is_some(),
                }
            })
            .collect();
        files.sort_by(|a, b| {
            a.name
                .to_lowercase()
                .cmp(&b.name.to_lowercase())
                .then_with(|| a.name.cmp(&b.name))
        });
        Some(CachePackageDetail {
            name: name.to_string(),
            version: entry.version,
            installed: entry.installed,
            size: entry.size,
            files,
        })
    }

    /// Aggregate cache index stats (package/file/size totals).
    pub fn cache_summary(&self) -> CacheSummary {
        let packages = self.services.cache().list_packages();
        let mut total_files: i64 = 0;
        let mut total_size: i64 = 0;
        for (name, entry) in &packages {
            let fc = self
                .services
                .cache()
                .get(name)
                .map(|e| e.files.len() as i64)
                .unwrap_or(0);
            total_files += fc;
            total_size += entry.size;
        }
        CacheSummary {
            package_count: packages.len(),
            total_files,
            total_size,
        }
    }

    /// Clear the cache-index rows for the named packages (store blobs kept).
    ///
    /// Packages that are still installed are skipped rather than cleared; see
    /// [`CacheClearReport`] for why this diverges from.
    pub fn clear_cache_index(&self, names: &[String]) -> Result<CacheClearReport, EngineError> {
        let _lock = self.mutation_lock()?;
        let mut report = CacheClearReport::default();
        for name in names {
            let Some(entry) = self.services.cache().get(name) else {
                report.not_indexed.push(name.clone());
                continue;
            };
            if self.active_listed_install(name).is_some() {
                report.skipped_installed.push(name.clone());
                continue;
            }
            self.services.cache().remove(name)?;
            report.cleared.push(CacheClearEntry {
                name: name.clone(),
                version: entry.version,
                file_count: entry.files.len() as i64,
                size: entry.size,
            });
        }
        Ok(report)
    }

    /// Clear every cache-index row whose package is no longer installed
    /// (store blobs kept on disk).
    pub fn clear_all_cache_index(&self) -> Result<CacheClearReport, EngineError> {
        let names: Vec<String> = self.services.cache().list_packages().into_keys().collect();
        self.clear_cache_index(&names)
    }

    /// Rescan installed apps and rebuild the cache index from on-disk hardlinks.
    pub fn rebuild_cache_index(
        &self,
        on_package: Option<&CacheRebuildProgress>,
    ) -> Result<usize, EngineError> {
        let _lock = self.mutation_lock()?;
        let apps_dir = self.glue_root().join("apps");
        let store = self.services.store();
        Ok(self
            .services
            .cache()
            .rebuild(&apps_dir, store, on_package)?)
    }

    /// Delete store blobs not referenced by the index or installed apps.
    /// Returns (removed count, freed bytes).
    pub fn run_cache_gc(&self) -> Result<(usize, i64), EngineError> {
        self.run_cache_gc_with_progress(None)
    }

    /// Orphan-blob GC with a phase callback so the CLI can show that a
    /// multi-minute scan is alive
    ///
    /// The scan runs **without** the mutation lock: it takes minutes, and
    /// holding the lock for it made every other maintenance op (per-package
    /// cache clear, install) wait out the 60s lock timeout and fail. The lock
    /// is taken only for the deletion, where the index references are re-read
    /// so a package cleared or installed during the scan cannot lose blobs.
    pub fn run_cache_gc_with_progress(
        &self,
        on_phase: Option<crate::cache::refs::GcPhaseFn<'_>>,
    ) -> Result<(usize, i64), EngineError> {
        let apps_dir = self.glue_root().join("apps");
        let store = self.services.store();
        // Blobs a running install has just ingested are not referenced yet;
        // its marker keeps them out of this sweep.
        let keep_since = crate::cache::inflight::keep_since(self.glue_root());
        let orphans = crate::cache::refs::plan_orphan_blobs(
            self.services.cache(),
            store,
            &apps_dir,
            keep_since,
            on_phase,
        )?;
        if orphans.is_empty() {
            return Ok((0, 0));
        }
        let _lock = self.mutation_lock()?;
        let referenced = self.services.cache().index_referenced_hashes()?;
        let survivors: Vec<_> = orphans
            .into_iter()
            .filter(|orphan| !referenced.contains(&orphan.hash))
            .collect();
        Ok(crate::cache::refs::delete_orphans(&survivors, on_phase)?)
    }

    /// Re-hash every store object and report the ones whose content no longer
    /// matches the hash they are stored under.
    ///
    /// Store hits are trusted by name, so corruption is otherwise invisible.
    /// With `repair`, the corrupt objects are deleted and the next install of
    /// an affected package re-downloads them.
    ///
    /// The scan itself is read-only and does not take `mutation_lock` — the
    /// scan of a large store takes a minute, and holding the guard that long
    /// made unrelated operations (version switch, install) wait behind it. The
    /// lock is taken only for the repair, and each candidate is re-hashed under
    /// it so a blob that was replaced in the meantime is not deleted.
    pub fn verify_cache(
        &self,
        repair: bool,
        on_phase: Option<crate::cache::refs::GcPhaseFn<'_>>,
    ) -> Result<crate::cache::verify::StoreVerifyReport, EngineError> {
        let mut report = crate::cache::verify::verify_store(self.services.store(), on_phase)?;
        // A blob and the files it was linked into are one inode: corruption
        // introduced through an install path corrupts both, and deleting the
        // blob alone leaves the installed package broken. Name the affected
        // packages (repair or not) so the user knows what to reinstall.
        let hashes: Vec<String> = report.corrupt.iter().map(|b| b.hash.clone()).collect();
        report.affected_packages = self.services.cache().packages_sharing_hashes(&hashes, "");
        if repair {
            let _lock = self.mutation_lock()?;
            let mut still_corrupt = Vec::new();
            for blob in &report.corrupt {
                // Re-check under the lock: a reinstall may have restored the
                // object while the scan was running.
                let still_bad = crate::hash::hash_file(&blob.path)
                    .map(|hash| hash != blob.hash)
                    .unwrap_or(true);
                if !still_bad {
                    continue;
                }
                // Missing objects are fine: delete only needs to shrink the
                // set of trusted blobs.
                let _ = self.services.store().delete(&blob.hash);
                still_corrupt.push(blob.clone());
            }
            report.corrupt = still_corrupt;
        }
        report.corrupt.sort_by(|a, b| a.hash.cmp(&b.hash));
        Ok(report)
    }

    /// Hold (`locked = true`) or unhold (`locked = false`) a package, storing
    /// the version-lock in the installed_packages metadata.
    ///
    /// Deliberately skips `mutation_lock`: this only rewrites one metadata
    /// column (SQLite serialises it) and changes no file references, so it is
    /// safe to run while GC/verify hold the mutation guard — otherwise the
    /// GUI could wait a minute to toggle a lock. Trade-off: a concurrent
    /// install of the same package in *another process* could overwrite the
    /// flag.
    pub fn set_package_version_lock(&self, name: &str, locked: bool) -> Result<(), EngineError> {
        if self.services.cache().get_installed(name).is_none() {
            return Err(EngineError::NotInstalled(name.to_string()));
        }
        self.services.cache().set_version_locked(name, locked)?;
        Ok(())
    }

    /// Whether the installed package is pinned to its current version.
    ///
    /// The installed list and the detail dialog read this instead of decoding
    /// the cache index's metadata JSON themselves.
    pub fn is_version_locked(&self, name: &str) -> bool {
        version_locked(&self.services, name)
    }

    /// Raw manifest JSON for a package ref, for a read-only viewer.
    ///
    /// Resolves unqualified refs the same way an install does, so `7zip` and
    /// `main/7zip` open the same file.
    pub fn manifest_source(&self, pkg_ref: &str) -> Result<String, EngineError> {
        let resolved = crate::engine::catalog::resolve_install_ref(&self.services, pkg_ref)
            .map_err(|e| EngineError::Bucket(pkg_ref.to_string(), e.to_string()))?;
        let (_bucket, path, _manifest) = self
            .services
            .registry()
            .find_manifest(&resolved)
            .map_err(|e| EngineError::Bucket(resolved.clone(), e.to_string()))?;
        std::fs::read_to_string(&path)
            .map_err(|e| EngineError::Bucket(resolved, format!("read manifest: {e}")))
    }

    /// Build an install plan for `pkg_ref`: missing dependencies and optional
    /// suggestions with their installed status. No packages are modified.
    pub fn plan_install(&self, pkg_ref: &str) -> Result<InstallPlan, EngineError> {
        let resolved = crate::engine::catalog::resolve_install_ref(&self.services, pkg_ref)
            .map_err(|e| EngineError::Bucket(pkg_ref.to_string(), e.to_string()))?;
        let (_bucket, _path, manifest) = self
            .services
            .registry()
            .find_manifest(&resolved)
            .map_err(|e| EngineError::Bucket(resolved.clone(), e.to_string()))?;

        let mut depends = Vec::new();
        for dep in manifest.depends() {
            let base = crate::catalog::package_base_name(dep);
            let installed = self.services.cache().get_installed(&base).is_some();
            depends.push(PlanRef {
                ref_: dep.clone(),
                installed,
            });
        }
        let mut suggestions = Vec::new();
        for s in manifest.suggestions() {
            let base = crate::catalog::package_base_name(&s.ref_);
            let installed = self.services.cache().get_installed(&base).is_some();
            suggestions.push(PlanSuggestion {
                ref_: s.ref_,
                label: s.label,
                installed,
            });
        }
        Ok(InstallPlan {
            package: resolved,
            depends,
            suggestions,
        })
    }

    /// Detailed metadata for an installed package (info command).
    pub fn get_installed_package_detail(
        &self,
        name: &str,
    ) -> Result<InstalledPackageDetail, EngineError> {
        let pkg_root = apps::pkg_root(self.glue_root(), name);
        let Some((install_dir, version)) = apps::active_install_dir(&pkg_root) else {
            return Err(EngineError::NotInstalled(name.to_string()));
        };
        let raw_record = apps::load_install_record(&install_dir).ok();
        let bucket = raw_record
            .as_ref()
            .map(|r| r.bucket.clone())
            .unwrap_or_default();
        let record = raw_record
            .and_then(|r| r.manifest)
            .and_then(|v| serde_json::from_value::<crate::manifest::Manifest>(v).ok());

        let mut shims = Vec::new();
        let mut description = String::new();
        let mut homepage = String::new();
        let mut license = String::new();
        let mut depends = Vec::new();
        let mut notes = Vec::new();
        if let Some(m) = &record {
            for bin in m.binaries() {
                let (exe, alias, _extra) = parse_bin_pattern_full(&bin);
                shims.push(if alias.is_empty() { exe } else { alias });
            }
            description = m.description.clone();
            homepage = m.homepage.clone();
            license = m.get_license();
            depends = m.depends().to_vec();
            notes = m.get_notes();
        }
        shims.sort();
        shims.dedup();

        let cache_installed = self.services.cache().get_installed(name);
        let install_time = cache_installed
            .as_ref()
            .map(|i| i.install_time.clone())
            .unwrap_or_default();
        // The CLI labels this value "indexed files" when an install row exists
        // (and "files" otherwise). Use the index count in that case: it is a
        // single SQL aggregate instead of walking a potentially huge install
        // tree (git: ~19k files), and it matches the label's semantics.
        let file_count = match &cache_installed {
            Some(_) => self
                .services
                .cache()
                .package_file_count(name)
                .map(|count| count.max(0) as usize)
                .unwrap_or_else(|| count_files(&install_dir)),
            None => count_files(&install_dir),
        };

        // Update availability: resolved exactly like `check_package_updates`
        // (the install record's bucket first, then a semantic version compare).
        // A bare-name lookup could land on a different bucket's manifest — or
        // miss the package entirely — which silently hid the upgrade link for
        // packages installed from a non-main bucket.
        let bucket_for_update = if bucket.is_empty() {
            self.installed_bucket_for_update(name, &version)
        } else {
            bucket.clone()
        };
        let latest_version = self
            .latest_manifest_version(name, &bucket_for_update)
            .unwrap_or_default();
        let update_available = crate::engine::version::update_available(&version, &latest_version);

        Ok(InstalledPackageDetail {
            name: name.to_string(),
            version,
            install_path: install_dir.to_string_lossy().into_owned(),
            current_path: pkg_root
                .join(apps::CURRENT_LINK_NAME)
                .to_string_lossy()
                .into_owned(),
            installed_at: install_time,
            size: cache_installed.as_ref().map(|i| i.size).unwrap_or(0),
            file_count,
            shims,
            bucket,
            description,
            homepage,
            license,
            depends,
            notes,
            update_available,
            latest_version,
        })
    }

    /// Switch the active version symlink for a package to the version in
    /// `pkg_ref` (after `@`), rebuild that version's shims, `env_set` values and
    /// Start-menu shortcuts, then update the cache index version.
    ///
    /// Takes `&self`: the work is one symlink swap plus the derived state of one
    /// package, serialised by `mutation_lock`, so an exclusive engine borrow is
    /// not needed and GUI callers can run it without waiting for a write lock.
    pub fn reset_package(&self, pkg_ref: &str) -> Result<(), EngineError> {
        let lock = self.mutation_lock()?;
        self.reset_package_with_lock(pkg_ref, &lock)
    }

    /// [`Self::reset_package`] with a caller-supplied data-root hold, so a
    /// frontend that already shares one guard between its concurrent operations
    /// (the desktop) does not deadlock against its own lock.
    ///
    /// This is the "switch, do not reinstall" path: a restore or a version pick
    /// that lands on a version whose directory already exists must relink, not
    /// clear the directory and download the artifact again.
    pub fn reset_package_with_lock(
        &self,
        pkg_ref: &str,
        _lock: &crate::lock::MutationLock,
    ) -> Result<(), EngineError> {
        let (name, version) = match pkg_ref.split_once('@') {
            Some((n, v)) if !v.is_empty() => (n.to_string(), v.to_string()),
            _ => {
                return Err(EngineError::VersionNotFound(
                    pkg_ref.to_string(),
                    "no version specified (use name@version)".to_string(),
                ));
            }
        };
        // `brave` and `extras/brave` name the same package directory.
        let name = crate::catalog::package_base_name(&name);
        let pkg_root = apps::pkg_root(self.glue_root(), &name);
        let versions = apps::list_versions(&pkg_root);
        if !versions.iter().any(|v| v == &version) {
            return Err(EngineError::VersionNotFound(name, version));
        }
        self.activate_version(&name, &version)
    }

    /// Remove one installed version directory that is not the active one.
    ///
    /// This is disk cleanup, not an uninstall: the package stays installed and
    /// everything that belongs to the active version — the `current` link,
    /// shims, shortcuts, env vars and persist data — is left alone, so no
    /// uninstall hooks run. The content cache is kept for the same reason
    /// [`Self::uninstall`] keeps it: a later reinstall stays cheap.
    ///
    /// The active version is rejected with [`EngineError::VersionInUse`] —
    /// callers switch away first, which keeps the shim/env/`current` teardown
    /// on the one path that knows how to do it.
    pub fn remove_installed_version(
        &self,
        package: &str,
        version: &str,
    ) -> Result<(), EngineError> {
        let _lock = self.mutation_lock()?;
        let name = crate::catalog::package_base_name(package);
        let pkg_root = apps::pkg_root(self.glue_root(), &name);
        let versions = apps::list_versions(&pkg_root);
        if !versions.iter().any(|v| v == version) {
            return Err(EngineError::VersionNotFound(name, version.to_string()));
        }
        let current = apps::read_current(&pkg_root).unwrap_or_default();
        if current == version {
            return Err(EngineError::VersionInUse(name, version.to_string()));
        }
        let install_dir = pkg_root.join(version);
        std::fs::remove_dir_all(&install_dir).map_err(|e| {
            EngineError::Bucket(name, format!("remove {}: {e}", install_dir.display()))
        })?;
        // The index's packages/installed_packages rows describe the active
        // version and its files, which this removal never touches.
        Ok(())
    }

    /// Remove every installed version directory that is not the active one,
    /// for all packages (or only the named ones).
    ///
    /// This is the bulk form of [`Self::remove_installed_version`]: each
    /// package keeps its active version, `current` link, shims, shortcuts, env
    /// vars and persist data, no uninstall hooks run, and the content cache is
    /// kept so a later reinstall stays cheap. Packages whose `current` link is
    /// missing are skipped — without it there is no safe way to tell which
    /// version directory is the live one.
    pub fn cleanup_old_versions(
        &self,
        filter: &[String],
    ) -> Result<OldVersionCleanup, EngineError> {
        let _lock = self.mutation_lock()?;
        let apps_dir = self.glue_root().join("apps");
        let mut cleanup = OldVersionCleanup::default();
        let Ok(entries) = std::fs::read_dir(&apps_dir) else {
            return Ok(cleanup);
        };
        let mut names: Vec<String> = entries
            .flatten()
            .filter(|entry| entry.path().is_dir())
            .map(|entry| entry.file_name().to_string_lossy().into_owned())
            .collect();
        names.sort();
        for name in names {
            if !filter.is_empty() && !filter.iter().any(|f| f == &name) {
                continue;
            }
            let pkg_root = apps::pkg_root(self.glue_root(), &name);
            let Some(current) = apps::read_current(&pkg_root) else {
                continue;
            };
            for version in apps::list_versions(&pkg_root) {
                if version == current {
                    continue;
                }
                let install_dir = pkg_root.join(&version);
                let size = dir_bytes(&install_dir);
                std::fs::remove_dir_all(&install_dir).map_err(|e| {
                    EngineError::Bucket(
                        name.clone(),
                        format!("remove {}: {e}", install_dir.display()),
                    )
                })?;
                cleanup.freed += size;
                cleanup.removed.push(OldVersionRemoval {
                    package: name.clone(),
                    version,
                    size,
                });
            }
        }
        Ok(cleanup)
    }

    /// Point `current` at an installed version and update the index rows.
    ///
    /// The caller holds the data-root mutation lock (this does no locking of
    /// its own, so callers that already hold it — `update` — can reuse it).
    ///
    /// Moving the junction is not the whole switch: a package's shims expand
    /// `$dir` to a concrete version directory in `shims-meta/<pkg>.json`, the
    /// `env_set` values in the user environment do the same, and Start-menu
    /// shortcuts store an absolute `TargetPath`. They are rebuilt here through
    /// [`rebuild_version_links`](crate::engine::install::rebuild_version_links),
    /// so `glue reset` (Scoop's `reset`) leaves nothing pointing at the version
    /// it switched away from. `current` has already moved when the rebuild
    /// runs, so a failure leaves the new version active with the previous
    /// version's shims and re-running `reset` retries it.
    fn activate_version(&self, name: &str, version: &str) -> Result<(), EngineError> {
        let pkg_root = apps::pkg_root(self.glue_root(), name);
        apps::link_current(&pkg_root, version)
            .map_err(|e| EngineError::Bucket(name.to_string(), format!("link current: {e}")))?;
        let install_dir = pkg_root.join(version);
        self.services.cache().set_active_version_install_dir(
            name,
            version,
            install_dir.to_string_lossy().as_ref(),
        )?;
        crate::engine::install::rebuild_version_links(&self.services, name, &install_dir)
            .map_err(EngineError::Install)?;
        Ok(())
    }

    /// Walk `apps/` and report each package's installed versions, marking the
    /// active one (used by `list --all`).
    pub fn list_installed_all_versions(&self, filter: &[String]) -> Vec<InstalledPackageVersions> {
        let apps_dir = self.glue_root().join("apps");
        let mut out = Vec::new();
        if let Ok(entries) = std::fs::read_dir(&apps_dir) {
            for entry in entries.flatten() {
                let pkg_root = entry.path();
                if !std::fs::metadata(&pkg_root)
                    .map(|m| m.is_dir())
                    .unwrap_or(false)
                {
                    continue;
                }
                let name = entry.file_name().to_string_lossy().into_owned();
                if !filter.is_empty() && !filter.iter().any(|f| f == &name) {
                    continue;
                }
                let mut versions = apps::list_versions(&pkg_root);
                versions.sort();
                versions.reverse();
                let current = apps::read_current(&pkg_root).unwrap_or_default();
                out.push(InstalledPackageVersions {
                    name,
                    versions,
                    current,
                });
            }
        }
        out.sort_by(|a, b| a.name.cmp(&b.name));
        out
    }

    /// Pull (fast-forward) an existing bucket repository.
    pub fn pull_bucket(
        &mut self,
        name: &str,
        progress: Option<ProgressFn>,
    ) -> Result<(), EngineError> {
        let _lock = self.mutation_lock()?;
        let buckets_dir = self.services.buckets_dir();
        let dest = buckets_dir.join(name);

        if !dest.exists() {
            return Err(EngineError::Bucket(
                name.to_string(),
                "bucket not found on disk".to_string(),
            ));
        }

        let emit = |event: ProgressEvent| {
            if let Some(cb) = &progress {
                cb(event);
            }
        };

        emit(ProgressEvent::Started {
            package: format!("bucket/{name}"),
        });

        self.ensure_git()?;

        let _head = crate::git::pull(self.glue_root(), &dest)
            .map_err(|e| EngineError::Git(format!("pull {name}"), e.to_string()))?;

        // Refresh the in-memory registry.
        self.services
            .registry_mut()
            .reload_from_disk()
            .map_err(|e| EngineError::Bucket(name.to_string(), e.to_string()))?;

        emit(ProgressEvent::Completed {
            package: format!("bucket/{name}"),
            version: String::new(),
        });

        Ok(())
    }

    /// Fetch a shallow bucket's full history so older `pkg@version` manifests
    /// can be resolved ([`crate::bucket_history`]).
    ///
    /// Returns `true` when history was fetched and `false` when the clone was
    /// already complete — the caller reports the difference because the
    /// download is the whole point of the command and can be large.
    ///
    /// Never called from an install: `glue install pkg@old` searches the
    /// history that is already on disk and names this command when it is not
    /// there, so an install never turns into an unexpected bulk download.
    pub fn deepen_bucket(&mut self, name: &str) -> Result<bool, EngineError> {
        let _lock = self.mutation_lock()?;
        let dest = self.services.buckets_dir().join(name);
        if !dest.exists() {
            return Err(EngineError::Bucket(
                name.to_string(),
                "bucket not found on disk".to_string(),
            ));
        }
        self.ensure_git()?;
        let root = self.glue_root().to_path_buf();
        if !crate::git::is_shallow(&root, &dest) {
            return Ok(false);
        }
        crate::git::unshallow(&root, &dest)
            .map_err(|e| EngineError::Git(format!("unshallow {name}"), e.to_string()))?;
        Ok(true)
    }

    /// Update all known buckets by pulling each one that exists on disk.
    pub fn update_buckets(
        &mut self,
        _progress: Option<ProgressFn>,
    ) -> Result<BucketSync, EngineError> {
        let _lock = self.mutation_lock()?;
        let _ = self.ensure_git();
        let buckets = bucket_checkouts_from_disk(self.glue_root());
        if buckets.is_empty() {
            return Ok(BucketSync::default());
        }
        let sync = pull_bucket_checkouts(self.glue_root(), &buckets);
        // One rebuild after the batch, instead of one per bucket.
        self.services
            .registry_mut()
            .reload_from_disk()
            .map_err(|e| EngineError::Bucket("buckets".to_string(), e.to_string()))?;
        Ok(sync)
    }

    /// Refresh bucket checkouts when `bucket_sync_mode` is `auto` and the
    /// configured interval has elapsed
    ///
    /// Returns `None` when the sync is not due (manual mode or still fresh).
    /// Failures are reported per bucket rather than aborting the command — a
    /// bucket that cannot reach its remote must not hide updates found in the
    /// others. The attempt timestamp is recorded even then, so a down network
    /// does not make every later command retry and stall.
    pub fn sync_buckets_if_due(&mut self) -> Result<Option<BucketSync>, EngineError> {
        if !crate::config::bucket_sync_due(self.glue_root()) {
            return Ok(None);
        }
        let _lock = self.mutation_lock()?;
        // A machine with no git fails every pull the same way; bootstrapping
        // once here keeps that from being eight identical spawn errors.
        let _ = self.ensure_git();
        let buckets = bucket_checkouts_from_disk(self.glue_root());
        let sync = pull_bucket_checkouts(self.glue_root(), &buckets);
        // Stamped even when a pull failed: the interval bounds how often a
        // broken remote is retried by later commands.
        crate::config::stamp_bucket_check(self.glue_root());
        if !buckets.is_empty() {
            self.services
                .registry_mut()
                .reload_from_disk()
                .map_err(|e| EngineError::Bucket("buckets".to_string(), e.to_string()))?;
        }
        Ok(Some(sync))
    }
}

/// Running executables for an installed package, given its data root and
/// package name. Resolves the active install directory first; returns nothing
/// when the package (or its install record) is missing.
pub fn running_processes_for(root: &std::path::Path, package: &str) -> Vec<String> {
    let name = crate::catalog::package_base_name(package);
    let pkg_root = apps::pkg_root(root, &name);
    match apps::active_install_dir(&pkg_root) {
        Some((dir, _version)) => running_exes_in(&dir),
        None => Vec::new(),
    }
}

/// Executable file names under `dir` (recursively) that currently have a
/// running process, de-duplicated and case-insensitive. Used by the desktop to
/// warn before uninstalling a running app: Windows cannot delete a mapped
/// executable, so a silent `remove_dir_all` would leave the directory behind.
pub fn running_exes_in(dir: &std::path::Path) -> Vec<String> {
    let mut names: Vec<String> = Vec::new();
    collect_exe_names(dir, 0, &mut names);
    names.sort();
    names.dedup();
    names
        .into_iter()
        .filter(|name| exe_is_running(name))
        .collect()
}

/// Terminate every process whose image name is in `names` (used after the user
/// confirms killing a running app so uninstall can proceed). Failures are
/// intentionally ignored: the subsequent removal reports its own error.
pub fn kill_exes(names: &[String]) {
    for name in names {
        let output = std::process::Command::new("taskkill")
            .args(["/F", "/T", "/IM", name])
            .output();
        // A best-effort kill: the caller re-checks and reports removal errors.
        drop(output);
    }
}

/// Recursively collect `.exe` file names (without directory) under `dir`,
/// bounded by `depth` to stay cheap on packages that ship a deep tree.
fn collect_exe_names(dir: &std::path::Path, depth: usize, out: &mut Vec<String>) {
    if depth > 6 {
        return;
    }
    let entries = match std::fs::read_dir(dir) {
        Ok(entries) => entries,
        Err(_) => return,
    };
    for entry in entries.flatten() {
        let path = entry.path();
        let file_type = match entry.file_type() {
            Ok(ft) => ft,
            Err(_) => continue,
        };
        if file_type.is_dir() {
            collect_exe_names(&path, depth + 1, out);
        } else if path
            .extension()
            .is_some_and(|e| e.eq_ignore_ascii_case("exe"))
        {
            if let Some(name) = path.file_name().and_then(|n| n.to_str()) {
                out.push(name.to_ascii_lowercase());
            }
        }
    }
}

/// Whether a process with the given image name is currently running, via
/// `tasklist`. The name is matched case-insensitively by `tasklist`.
fn exe_is_running(name: &str) -> bool {
    let output = match std::process::Command::new("tasklist")
        .args(["/FI", &format!("IMAGENAME eq {name}"), "/NH"])
        .output()
    {
        Ok(o) => o,
        Err(_) => return false,
    };
    let text = String::from_utf8_lossy(&output.stdout).to_lowercase();
    text.contains(&name.to_lowercase())
}

impl std::fmt::Debug for Engine {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Engine")
            .field("glue_root", &self.glue_root())
            .finish_non_exhaustive()
    }
}

/// Whether a `shim.exe` stub is available to create shims with.
///
/// Two locations count: the workspace cache (`<root>\shim.exe`, written on the
/// first shim operation) and the copy shipped beside the program — both
/// installers put `shim.exe` next to `glue.exe`, so an installation has one
/// before any shim exists.
///
/// The copy beside the program only counts for a product entry point or for a
/// marked installation: a test harness or helper that happens to sit next to a
/// `shim.exe` build artifact — for example under `target\<profile>\deps\` —
/// must not report a stub it merely lives near. The desktop's executable is
/// `gluestick-desktop.exe`, not `glue.exe`, which is why the installation
/// marker has to vouch for it as well.
fn shim_stub_present(root: &Path, exe: Option<&Path>, install_root: Option<&Path>) -> bool {
    if root.join("shim.exe").is_file() {
        return true;
    }
    let Some(dir) = exe.and_then(Path::parent) else {
        return false;
    };
    if !dir.join("shim.exe").is_file() {
        return false;
    }
    let entry_point = exe.is_some_and(|exe| {
        exe.file_stem().is_some_and(|stem| {
            stem.eq_ignore_ascii_case("glue") || stem.eq_ignore_ascii_case("gluestick-desktop")
        })
    });
    entry_point || install_root == Some(dir)
}

/// Doctor report.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct DoctorReport {
    pub data_root: PathBuf,
    pub data_root_exists: bool,
    pub buckets: Vec<String>,
    pub host_arch: String,
    pub win_build: i32,
    pub shim_present: bool,
    pub config_present: bool,
    pub installed_count: usize,
    /// Installed packages whose manifest declares a `depends` that is not
    /// installed, sorted by package then dependency.
    ///
    /// Uses the installer's own "already installed" predicate
    /// (`cache.get_installed(base_name)`), so `glue doctor` and `glue install`
    /// cannot disagree about which dependencies are missing. Root-level
    /// `depends` only: the installer does not expand `depends_pre` or
    /// `depends_post` either.
    pub missing_dependencies: Vec<MissingDependency>,
    /// Bootstrap helper tools (7-Zip, innounp, WiX, git) with readiness.
    pub helpers: Vec<crate::bootstrap::HelperStatus>,
    /// Windows `LongPathsEnabled` policy; `None` off Windows. External tools
    /// (PowerShell hooks, `msiexec`, `cmd`) need it for paths past MAX_PATH.
    pub long_paths_enabled: Option<bool>,
    /// A newer published version, read from the self-update cache only.
    ///
    /// `doctor` must not add a network call, so this is populated from
    /// [`crate::self_update::cached_notice`]: `None` when no check has run yet
    /// and when the cached version is not newer than the running one.
    pub self_update: Option<crate::self_update::UpdateNotice>,
}

/// An installed package that declares a dependency the data root does not
/// have.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct MissingDependency {
    /// Installed package that declares it (base name).
    pub package: String,
    /// The dependency exactly as the manifest declares it (`bucket/name`).
    pub dependency: String,
}

// NOTE: `package_base_name` lives in `crate::catalog` — the canonical
// parser (shared with `engine::install`). An ops-local copy was removed:
// it split only on '/', so backslash-separated refs (Windows style)
// resolved to the wrong base name in depends/suggestions checks.

/// Recursively count regular files under `dir` (used by `info` when no
/// installed row exists, where the CLI labels the value "files").
///
/// `DirEntry::file_type` is served from the Windows directory-enumeration
/// buffer; the previous `fs::metadata` call opened every entry (measured
/// ~150 ms vs ~10 ms for an 8k-file install). Links/junctions still get a
/// follow-up stat so their targets are counted as before.
fn count_files(dir: &Path) -> usize {
    let mut count = 0;
    if let Ok(entries) = std::fs::read_dir(dir) {
        for entry in entries.flatten() {
            let Ok(file_type) = entry.file_type() else {
                continue;
            };
            if file_type.is_dir() {
                count += count_files(&entry.path());
            } else if file_type.is_symlink() {
                match std::fs::metadata(entry.path()) {
                    Ok(meta) if meta.is_dir() => count += count_files(&entry.path()),
                    Ok(_) => count += 1,
                    Err(_) => {}
                }
            } else {
                count += 1;
            }
        }
    }
    count
}

/// Recursively total regular files and their bytes under `dir` (used by
/// `uninstall` to report what it removed). Hardlinks count once per directory
/// entry; `file_type()` is served from the directory-enumeration buffer, so a
/// large tree is walked without a per-file `stat`.
fn dir_stats(dir: &Path) -> (usize, i64) {
    let mut files = 0usize;
    let mut bytes = 0i64;
    if let Ok(entries) = std::fs::read_dir(dir) {
        for entry in entries.flatten() {
            let Ok(file_type) = entry.file_type() else {
                continue;
            };
            if file_type.is_dir() {
                let (sub_files, sub_bytes) = dir_stats(&entry.path());
                files += sub_files;
                bytes += sub_bytes;
            } else if file_type.is_symlink() {
                // A junction/symlink may point at a directory; follow it.
                if let Ok(meta) = std::fs::metadata(entry.path()) {
                    if meta.is_dir() {
                        let (sub_files, sub_bytes) = dir_stats(&entry.path());
                        files += sub_files;
                        bytes += sub_bytes;
                    } else {
                        files += 1;
                        bytes += meta.len() as i64;
                    }
                }
            } else {
                files += 1;
                bytes += entry.metadata().map(|m| m.len() as i64).unwrap_or(0);
            }
        }
    }
    (files, bytes)
}

// NOTE: `parse_bin_pattern_full` lives in `engine::install` — the canonical
// -parity parser (see its doc comment). A divergent ops-local copy was
// removed: it applied a `-`/`/` heuristic does not have, so
// `"tool.exe,--flag"` resolved to alias="" and uninstall deleted the wrong
// shim name (or orphaning the real one).

/// the completed install to compare against — the
/// cache-indexed version when its directory is non-empty, else the `current`
/// link, else the newest version carrying an install record.
fn active_listed_install(
    root: &Path,
    pkg_name: &str,
    cache: &crate::cache::Index,
) -> Option<(PathBuf, String)> {
    let pkg_root = apps::pkg_root(root, pkg_name);
    if let Some(inst) = cache.get_installed(pkg_name) {
        if crate::engine::install::version_dir_installed(root, pkg_name, &inst.version) {
            let dir = if inst.install_dir.is_empty() {
                pkg_root.join(&inst.version)
            } else {
                PathBuf::from(&inst.install_dir)
            };
            return Some((dir, inst.version));
        }
    }
    if let Some(ver) = apps::read_current(&pkg_root) {
        if crate::engine::install::version_dir_installed(root, pkg_name, &ver) {
            let dir = pkg_root.join(&ver);
            if apps::load_install_record(&dir).is_ok() {
                return Some((dir, ver));
            }
        }
    }
    for ver in apps::list_versions(&pkg_root).into_iter().rev() {
        let dir = pkg_root.join(&ver);
        if apps::load_install_record(&dir).is_ok() {
            return Some((dir, ver));
        }
    }
    None
}

/// One program a package can start.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct LaunchCandidate {
    /// Executable (or shim) that would be started.
    pub program: PathBuf,
    /// Display name (shim or file stem).
    pub name: String,
    /// Install-dir-relative path of the program the entry starts (for a shim:
    /// the program behind it). This is the key user preferences are stored
    /// under.
    pub rel_path: String,
    /// How it is started, after applying any user preference.
    pub kind: LaunchKind,
    /// The kind came from the user's preference rather than detection.
    pub user_set: bool,
}

impl LaunchCandidate {
    /// Whether the entry may be started (a user can hide one).
    pub fn openable(&self) -> bool {
        self.kind != LaunchKind::Skip
    }
}

/// How a launch candidate is started.
///
/// Same model as the Go implementation (`engine/internal/launch`): a windowed
/// program is opened through the shell, a console program in a Command Prompt
/// that stays open, and `Skip` hides an entry from the open-program menu.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum LaunchKind {
    /// A windowed program (`PE` subsystem `WINDOWS_GUI`).
    Gui,
    /// A console program, or a `.bat`/`.cmd`/`.jar` script.
    Console,
    /// Hidden from the menu by the user.
    Skip,
}

/// Programs a package can start, best default first.
///
/// Sources, in the same order as the Go implementation:
///
/// 1. the manifest's `shortcuts` targets — the programs it declares as "to be
///    opened" (a package often points its `bin` at a console build and its
///    shortcuts at the windowed one: Godot's `godot.console.exe` vs
///    `godot.exe`),
/// 2. the manifest's `bin` entries — here, the package's shims, which carry
///    the manifest's default arguments and env vars,
/// 3. a scan of the install folder, **only** when 1–2 found nothing.
///
/// That last rule is what keeps the menu short: an app can ship a hundred
/// helper executables, and listing them all buries the one the user wants.
///
/// Windowed programs come first (`PE` subsystem), console programs after;
/// within a group the executable named after the package wins. The first
/// entry is the default.
pub fn launch_candidates(root: &Path, package: &str) -> Vec<LaunchCandidate> {
    let name = crate::catalog::package_base_name(package);
    let pkg_root = apps::pkg_root(root, &name);
    let Some((install_dir, _version)) = apps::active_install_dir(&pkg_root) else {
        return Vec::new();
    };
    let index = load_launch_index(root);
    let mut out: Vec<LaunchCandidate> = Vec::new();
    let push = |candidate: LaunchCandidate, out: &mut Vec<LaunchCandidate>| {
        if !out
            .iter()
            .any(|existing| same_path(&existing.program, &candidate.program))
        {
            out.push(candidate);
        }
    };

    // 1. Manifest shortcuts.
    let mut declared: Vec<PathBuf> = Vec::new();
    let manifest = installed_manifest(&install_dir);
    if let Some(manifest) = &manifest {
        for entry in manifest.shortcut_entries() {
            let target = install_dir.join(entry.target.replace('\\', "/"));
            if !target.is_file() || !is_runnable_file(&target) {
                continue;
            }
            declared.push(target.clone());
            let label = if entry.label.trim().is_empty() {
                target
                    .file_stem()
                    .and_then(|stem| stem.to_str())
                    .unwrap_or(&name)
                    .to_string()
            } else {
                entry.label.clone()
            };
            push(
                launch_entry(&install_dir, &target, label, target.clone(), &index, &name),
                &mut out,
            );
        }
    }

    // 2. Manifest bins, through their shims.
    //
    // When the manifest declares bins, only those count as "the program to
    // open": the shims created for an `env_add_path` directory (`python`'s
    // `Scripts\pip.exe`, `f2py.exe`, …) exist for the command line and would
    // otherwise bury the entry a user actually wants.
    let declared_bins: Vec<PathBuf> = manifest
        .as_ref()
        .map(|manifest| crate::engine::install::manifest_bin_targets(&install_dir, manifest))
        .unwrap_or_default();
    if let Ok(manager) = crate::shim::Manager::new(root) {
        if let Ok(configs) = manager.list() {
            for config in configs {
                if !crate::shim::belongs_to_package(&config.path, &name) {
                    continue;
                }
                let shim = manager.bin_dir().join(format!("{}.exe", config.name));
                if !shim.is_file() {
                    continue;
                }
                let target = Path::new(&config.path);
                if !declared_bins.is_empty()
                    && !declared_bins.iter().any(|bin| same_path(bin, target))
                {
                    continue;
                }
                // Already offered as a shortcut target: one program, one entry.
                if declared.iter().any(|known| same_path(known, target)) {
                    continue;
                }
                // The shim stub is a console program itself; what matters is
                // the program behind it. Label by the *target* when the shim
                // name would hide which build it starts.
                let target_stem = target
                    .file_stem()
                    .and_then(|stem| stem.to_str())
                    .unwrap_or(&config.name);
                let label = if target_stem.eq_ignore_ascii_case(&config.name) {
                    config.name.clone()
                } else {
                    target_stem.to_string()
                };
                push(
                    launch_entry(&install_dir, target, label, shim, &index, &name),
                    &mut out,
                );
            }
        }
    }

    // 3. Nothing declared by the manifest: scan, so an installer-based package
    //    (no `bin`, no `shortcuts`) can still be started.
    if out.is_empty() {
        for file in runnable_files(&install_dir) {
            let Some(stem) = file.file_stem().and_then(|stem| stem.to_str()) else {
                continue;
            };
            push(
                launch_entry(
                    &install_dir,
                    &file,
                    stem.to_string(),
                    file.clone(),
                    &index,
                    &name,
                ),
                &mut out,
            );
        }
    }

    // Removed entries stay out of the menu until the user restores them.
    out.retain(|candidate| !launch_is_removed(&index, &name, &candidate.rel_path));

    out.sort_by(|a, b| {
        (
            a.kind == LaunchKind::Skip,
            a.kind != LaunchKind::Gui,
            !a.name.eq_ignore_ascii_case(&name),
            a.name.to_lowercase(),
        )
            .cmp(&(
                b.kind == LaunchKind::Skip,
                b.kind != LaunchKind::Gui,
                !b.name.eq_ignore_ascii_case(&name),
                b.name.to_lowercase(),
            ))
    });
    out
}

/// The manifest recorded with the installed version, when there is one.
fn installed_manifest(install_dir: &Path) -> Option<crate::manifest::Manifest> {
    apps::load_install_record(install_dir)
        .ok()
        .and_then(|record| record.manifest)
        .and_then(|value| serde_json::from_value(value).ok())
}

/// One candidate row: detection result, user override and the preference key.
fn launch_entry(
    install_dir: &Path,
    target: &Path,
    name: String,
    program: PathBuf,
    index: &LaunchIndex,
    package: &str,
) -> LaunchCandidate {
    let rel_path = launch_rel_path(install_dir, target);
    let auto = launch_kind_for(target);
    let (kind, user_set) = match launch_index_kind(index, package, &rel_path) {
        Some(kind) => (kind, true),
        None => (auto, false),
    };
    LaunchCandidate {
        program,
        name,
        rel_path,
        kind,
        user_set,
    }
}

/// Install-dir-relative, slash-separated key for a program.
fn launch_rel_path(install_dir: &Path, program: &Path) -> String {
    program
        .strip_prefix(install_dir)
        .map(|rel| rel.to_string_lossy().replace('\\', "/"))
        .unwrap_or_else(|_| program.to_string_lossy().replace('\\', "/"))
}

// ---------------------------------------------------------------------------
// Launch preferences (`launch-index.json`)
// ---------------------------------------------------------------------------

/// Per-package launch preferences, stored in `<root>/launch-index.json`.
///
/// Same model as the Go implementation (`launch-index.json` there): `packages`
/// maps a package to executable-relative-path → `gui` / `console` / `skip`,
/// and `removed` lists the entries the user deleted from the menu (they can be
/// restored, so the row is not lost for good).
#[derive(Debug, Clone, Default, serde::Serialize, serde::Deserialize)]
struct LaunchIndex {
    #[serde(default)]
    packages: std::collections::BTreeMap<String, std::collections::BTreeMap<String, String>>,
    #[serde(default)]
    removed: std::collections::BTreeMap<String, Vec<String>>,
}

fn launch_index_path(root: &Path) -> PathBuf {
    root.join("launch-index.json")
}

fn load_launch_index(root: &Path) -> LaunchIndex {
    std::fs::read(launch_index_path(root))
        .ok()
        .and_then(|bytes| serde_json::from_slice(&bytes).ok())
        .unwrap_or_default()
}

fn save_launch_index(root: &Path, index: &LaunchIndex) -> Result<(), String> {
    let bytes = serde_json::to_vec_pretty(index).map_err(|e| e.to_string())?;
    crate::atomic::write_atomic(&launch_index_path(root), &bytes).map_err(|e| e.to_string())
}

fn launch_kind_str(kind: LaunchKind) -> &'static str {
    match kind {
        LaunchKind::Gui => "gui",
        LaunchKind::Console => "console",
        LaunchKind::Skip => "skip",
    }
}

/// Parse a stored kind; `auto`/empty means "no override".
pub fn launch_kind_from_str(value: &str) -> Option<LaunchKind> {
    match value.trim().to_ascii_lowercase().as_str() {
        "gui" => Some(LaunchKind::Gui),
        "console" => Some(LaunchKind::Console),
        "skip" => Some(LaunchKind::Skip),
        _ => None,
    }
}

fn launch_index_kind(index: &LaunchIndex, package: &str, rel_path: &str) -> Option<LaunchKind> {
    index
        .packages
        .get(package)?
        .iter()
        .find(|(key, _)| key.eq_ignore_ascii_case(rel_path))
        .and_then(|(_, value)| launch_kind_from_str(value))
}

fn launch_is_removed(index: &LaunchIndex, package: &str, rel_path: &str) -> bool {
    index.removed.get(package).is_some_and(|list| {
        list.iter()
            .any(|entry| entry.eq_ignore_ascii_case(rel_path))
    })
}

fn normalize_launch_rel(rel_path: &str) -> String {
    rel_path.trim().replace('\\', "/")
}

/// The launch menu for a package: every discovered entry (hidden ones included,
/// marked [`LaunchKind::Skip`]) plus the entries the user removed.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct LaunchMenu {
    pub candidates: Vec<LaunchCandidate>,
    /// Install-relative paths removed from the menu; restorable.
    pub removed: Vec<String>,
}

/// Read the launch menu for `package`.
pub fn launch_menu(root: &Path, package: &str) -> LaunchMenu {
    let name = crate::catalog::package_base_name(package);
    let index = load_launch_index(root);
    LaunchMenu {
        candidates: launch_candidates(root, &name),
        removed: index.removed.get(&name).cloned().unwrap_or_default(),
    }
}

/// Set (or clear, with `None`) the user's launch kind for one entry.
pub fn set_launch_kind(
    root: &Path,
    package: &str,
    rel_path: &str,
    kind: Option<LaunchKind>,
) -> Result<(), String> {
    let name = crate::catalog::package_base_name(package);
    let rel = normalize_launch_rel(rel_path);
    if rel.is_empty() {
        return Err("empty launch entry path".to_string());
    }
    let mut index = load_launch_index(root);
    match kind {
        Some(kind) => {
            index
                .packages
                .entry(name)
                .or_default()
                .insert(rel, launch_kind_str(kind).to_string());
        }
        None => {
            if let Some(map) = index.packages.get_mut(&name) {
                map.retain(|key, _| !key.eq_ignore_ascii_case(&rel));
                if map.is_empty() {
                    index.packages.remove(&name);
                }
            }
        }
    }
    save_launch_index(root, &index)
}

/// Hide every other entry of the package (the row's "hide others" action).
pub fn hide_other_launchers(root: &Path, package: &str, keep_rel_path: &str) -> Result<(), String> {
    let name = crate::catalog::package_base_name(package);
    let keep = normalize_launch_rel(keep_rel_path);
    let others: Vec<String> = launch_candidates(root, &name)
        .into_iter()
        .map(|candidate| candidate.rel_path)
        .filter(|rel| !rel.eq_ignore_ascii_case(&keep))
        .collect();
    for rel in others {
        set_launch_kind(root, &name, &rel, Some(LaunchKind::Skip))?;
    }
    Ok(())
}

/// Drop an entry from the menu (restorable via [`restore_launcher`]).
pub fn remove_launcher(root: &Path, package: &str, rel_path: &str) -> Result<(), String> {
    let name = crate::catalog::package_base_name(package);
    let rel = normalize_launch_rel(rel_path);
    if rel.is_empty() {
        return Err("empty launch entry path".to_string());
    }
    let mut index = load_launch_index(root);
    {
        let list = index.removed.entry(name.clone()).or_default();
        if !list.iter().any(|entry| entry.eq_ignore_ascii_case(&rel)) {
            list.push(rel.clone());
        }
    }
    if let Some(map) = index.packages.get_mut(&name) {
        map.retain(|key, _| !key.eq_ignore_ascii_case(&rel));
        if map.is_empty() {
            index.packages.remove(&name);
        }
    }
    save_launch_index(root, &index)
}

/// Bring a removed entry back into the menu.
pub fn restore_launcher(root: &Path, package: &str, rel_path: &str) -> Result<(), String> {
    let name = crate::catalog::package_base_name(package);
    let rel = normalize_launch_rel(rel_path);
    let mut index = load_launch_index(root);
    if let Some(list) = index.removed.get_mut(&name) {
        list.retain(|entry| !entry.eq_ignore_ascii_case(&rel));
        if list.is_empty() {
            index.removed.remove(&name);
        }
    }
    save_launch_index(root, &index)
}

/// What [`purge_partials`] removed.
#[derive(Debug, Default, Clone, Copy, PartialEq, Eq)]
pub struct PartialPurge {
    /// Fragment files deleted (`<name>.part` plus its `.meta` sidecar).
    pub files: usize,
    /// Bytes the deleted files occupied.
    pub bytes: u64,
    /// Fragments that could not be deleted (a running download holds them).
    pub failed: usize,
}

/// Delete the fragments of interrupted downloads under `<root>\store\.partial`.
///
/// A `.part` file only exists for an unfinished transfer, and a resumed install
/// looks it up again by URL id — so this reclaims disk space rather than fixing
/// anything. Fragments a live download still holds open are counted in
/// [`PartialPurge::failed`] instead of aborting the sweep, and a missing
/// directory (nothing was ever interrupted) is not an error.
pub fn purge_partials(root: &Path) -> PartialPurge {
    let dir = root.join("store").join(".partial");
    let Ok(entries) = std::fs::read_dir(&dir) else {
        return PartialPurge::default();
    };
    let mut purge = PartialPurge::default();
    for entry in entries.flatten() {
        let Ok(meta) = entry.metadata() else {
            continue;
        };
        if !meta.is_file() {
            continue;
        }
        match std::fs::remove_file(entry.path()) {
            Ok(()) => {
                purge.files += 1;
                purge.bytes += meta.len();
            }
            Err(_) => purge.failed += 1,
        }
    }
    purge
}

/// Drop launch-index entries that no longer describe a program to open.
///
/// The candidate rules tightened over time (an install folder used to be
/// scanned wholesale, so `python`'s `Scripts\*.exe` helpers and FreeCAD's
/// bundled tools all became entries). Entries that do not match a current
/// candidate are removed, and a package left without entries disappears from
/// the file. Returns how many entries were dropped.
pub fn prune_launch_index(root: &Path) -> Result<usize, String> {
    let mut index = load_launch_index(root);
    let mut dropped = 0usize;

    let packages: Vec<String> = index.packages.keys().cloned().collect();
    for package in packages {
        let candidates: Vec<String> = launch_candidates(root, &package)
            .into_iter()
            .map(|candidate| candidate.rel_path.to_lowercase())
            .collect();
        if let Some(map) = index.packages.get_mut(&package) {
            let before = map.len();
            map.retain(|key, _| candidates.contains(&key.replace('\\', "/").to_lowercase()));
            dropped += before - map.len();
            if map.is_empty() {
                index.packages.remove(&package);
            }
        }
    }

    // A removed entry is worth keeping while its file still exists (the user
    // may want to restore it); once the version it belonged to is gone it is
    // noise.
    let removed_packages: Vec<String> = index.removed.keys().cloned().collect();
    for package in removed_packages {
        let pkg_root = apps::pkg_root(root, &crate::catalog::package_base_name(&package));
        let install_dir = apps::active_install_dir(&pkg_root).map(|(dir, _)| dir);
        if let Some(list) = index.removed.get_mut(&package) {
            let before = list.len();
            list.retain(|rel| {
                install_dir
                    .as_ref()
                    .is_some_and(|dir| dir.join(rel.replace('\\', "/")).is_file())
            });
            dropped += before - list.len();
            if list.is_empty() {
                index.removed.remove(&package);
            }
        }
    }

    if dropped > 0 {
        save_launch_index(root, &index)?;
    }
    Ok(dropped)
}

/// Files a launcher can start under `dir`: `.exe`, `.bat`, `.cmd`, `.jar`.
///
/// Depth-limited (four levels, like the Go implementation) and skipping the
/// usual junk directories, so a large app tree does not turn the menu into
/// thousands of entries.
fn runnable_files(dir: &Path) -> Vec<PathBuf> {
    fn skip_dir(name: &str) -> bool {
        let lower = name.to_ascii_lowercase();
        matches!(lower.as_str(), "node_modules" | ".git" | "__pycache__") || name.starts_with('.')
    }
    fn walk(dir: &Path, depth: usize, out: &mut Vec<PathBuf>) {
        if depth > 4 {
            return;
        }
        let Ok(entries) = std::fs::read_dir(dir) else {
            return;
        };
        for entry in entries.flatten() {
            let path = entry.path();
            if path.is_dir() {
                let name = entry.file_name().to_string_lossy().into_owned();
                if !skip_dir(&name) {
                    walk(&path, depth + 1, out);
                }
                continue;
            }
            if is_runnable_file(&path) {
                out.push(path);
            }
        }
    }

    let mut out = Vec::new();
    walk(dir, 0, &mut out);
    out.sort();
    out
}

/// Whether the launcher knows how to run this file type.
fn is_runnable_file(path: &Path) -> bool {
    path.extension().is_some_and(|ext| {
        let ext = ext.to_string_lossy();
        ["exe", "bat", "cmd", "jar"]
            .iter()
            .any(|known| ext.eq_ignore_ascii_case(known))
    })
}

/// How a file is started: a GUI executable, everything else a console run.
fn launch_kind_for(path: &Path) -> LaunchKind {
    if is_gui_executable(path) {
        LaunchKind::Gui
    } else {
        LaunchKind::Console
    }
}

/// Whether a Windows executable is a GUI program (`PE` optional-header
/// subsystem 2) rather than a console one (3).
///
/// Unreadable or non-PE files report `false`.
fn is_gui_executable(path: &Path) -> bool {
    use std::io::{Read, Seek, SeekFrom};

    const IMAGE_SUBSYSTEM_WINDOWS_GUI: u16 = 2;
    // Offset of `Subsystem` inside the optional header, same for PE32/PE32+.
    const SUBSYSTEM_OFFSET: u64 = 68;

    let Ok(mut file) = std::fs::File::open(path) else {
        return false;
    };
    let mut dos = [0u8; 0x40];
    if file.read_exact(&mut dos).is_err() || &dos[0..2] != b"MZ" {
        return false;
    }
    let pe_offset = u32::from_le_bytes([dos[0x3c], dos[0x3d], dos[0x3e], dos[0x3f]]) as u64;
    if file.seek(SeekFrom::Start(pe_offset)).is_err() {
        return false;
    }
    // PE signature (4) + COFF header (20) precede the optional header.
    let mut head = [0u8; 24];
    if file.read_exact(&mut head).is_err() || &head[0..4] != b"PE\0\0" {
        return false;
    }
    if file
        .seek(SeekFrom::Start(pe_offset + 24 + SUBSYSTEM_OFFSET))
        .is_err()
    {
        return false;
    }
    let mut subsystem = [0u8; 2];
    if file.read_exact(&mut subsystem).is_err() {
        return false;
    }
    u16::from_le_bytes(subsystem) == IMAGE_SUBSYSTEM_WINDOWS_GUI
}

/// Whether two paths name the same file (case-insensitively on Windows).
fn same_path(a: &Path, b: &Path) -> bool {
    let canonical =
        |path: &Path| std::fs::canonicalize(path).unwrap_or_else(|_| path.to_path_buf());
    canonical(a)
        .to_string_lossy()
        .eq_ignore_ascii_case(&canonical(b).to_string_lossy())
}

/// Start `program` the way the Go implementation does.
///
/// A GUI program is opened through the shell (`cmd /c start ""`), so apps that
/// rely on shell activation work. A console program gets a Command Prompt
/// window that stays open (`cmd /k`): its output is readable instead of
/// flashing past. Batches run through `call`, jars through `java -jar`.
fn open_launch_file(program: &Path, kind: LaunchKind, working_dir: &Path) -> std::io::Result<()> {
    let program = program.to_path_buf();
    let dir = if working_dir.as_os_str().is_empty() {
        program.parent().map(Path::to_path_buf).unwrap_or_default()
    } else {
        working_dir.to_path_buf()
    };
    let lower = program.to_string_lossy().to_ascii_lowercase();

    if kind == LaunchKind::Skip {
        return Err(std::io::Error::new(
            std::io::ErrorKind::InvalidInput,
            "hidden from the launch menu",
        ));
    }

    if kind == LaunchKind::Gui {
        // Started directly, never through `cmd /c start`: `start` gives a
        // console program its own new console window, and a GUI candidate is
        // often the package's *shim* (a console stub that then starts the real
        // app) — which is exactly the stray Command Prompt users saw. With
        // `CREATE_NO_WINDOW` the stub stays invisible and the app opens its
        // own window.
        let is_jar = lower.ends_with(".jar");
        // A program launched directly cannot live past MAX_PATH
        // (`CreateProcessW`), and the raw OS error is misleading. A `.jar`
        // only goes to `javaw` as an argument, so it is not affected.
        if !is_jar && crate::winpath::exceeds_max_path(&program) {
            return Err(std::io::Error::new(
                std::io::ErrorKind::InvalidInput,
                format!(
                    "program path exceeds MAX_PATH and cannot be started: {}",
                    program.display()
                ),
            ));
        }
        let mut command = if is_jar {
            let mut command = std::process::Command::new("javaw");
            command.args(["-jar"]).arg(&program);
            command
        } else {
            std::process::Command::new(&program)
        };
        command.current_dir(&dir);
        #[cfg(windows)]
        {
            use std::os::windows::process::CommandExt;
            const CREATE_NO_WINDOW: u32 = 0x0800_0000;
            command.creation_flags(CREATE_NO_WINDOW);
        }
        return command.spawn().map(|_child| ());
    }

    let token = quote_cmd_token(&program);
    let run = if lower.ends_with(".jar") {
        format!("java -jar {token}")
    } else if lower.ends_with(".bat") || lower.ends_with(".cmd") {
        format!("call {token}")
    } else {
        token
    };
    // `start "" /D <dir> cmd /k <run>`: a real console window, kept open after
    // the program exits so its output stays readable. The helper `cmd` itself
    // is hidden; the inner one is the window the user wants.
    //
    // The whole tail is handed to cmd.exe as one *raw* command line. Passing
    // the `/k` script as a separate argument makes Rust escape its quotes as
    // `\"`, which cmd.exe does not unescape — it then tries to run
    // `\"C:\path with spaces\x.exe\"` as the command name and reports it as not
    // recognized. Caret-escaping the quotes and `&&` lets the outer cmd strip
    // the escapes and hand the inner cmd a normally quoted command.
    let script = format!("title Command Prompt ^&^& {run}");
    let mut command = std::process::Command::new("cmd");
    command.current_dir(&dir);
    #[cfg(windows)]
    {
        use std::os::windows::process::CommandExt;
        const CREATE_NO_WINDOW: u32 = 0x0800_0000;
        command.raw_arg(format!(
            "/c start \"\" /D \"{}\" cmd /k {}",
            dir.display(),
            script
        ));
        command.creation_flags(CREATE_NO_WINDOW);
    }
    #[cfg(not(windows))]
    {
        command
            .args(["/c", "start", "", "/D"])
            .arg(&dir)
            .arg("cmd")
            .arg("/k")
            .arg(script);
    }
    command.spawn().map(|_child| ())
}

/// Quote a path for the raw command line handed to `cmd.exe`.
///
/// The opening quote is caret-escaped (`^"…"`) so the outer
/// `cmd /c start …` parse keeps it as a literal quote; the closing quote is
/// left plain so the inner `cmd /k` sees a normally quoted token. Escaping
/// both would leave a stray `^` in the command. See `open_launch_file`.
fn quote_cmd_token(path: &Path) -> String {
    let token = path.display().to_string();
    let needs_quotes = token
        .chars()
        .any(|c| matches!(c, ' ' | '\t' | '&' | '|' | '<' | '>' | '^' | '"'));
    if needs_quotes {
        format!("^\"{}\"", token.replace('"', "\"\""))
    } else {
        token
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::testutil::TempDir;

    #[test]
    fn engine_new_opens_services() {
        let root = TempDir::new("glue-engine-test");
        let engine = Engine::new(root.path()).expect("engine");
        assert_eq!(engine.glue_root(), root.path());
        assert_eq!(engine.installed_count(), 0);
        assert!(engine.installed_dir("nope").is_none());
    }

    #[test]
    fn list_installed_empty_returns_empty() {
        let root = TempDir::new("glue-list-test");
        let engine = Engine::new(root.path()).expect("engine");
        let list = engine.list_installed().expect("list");
        assert!(list.is_empty());
    }

    /// Fabricate a completed install under `apps/<pkg>/<ver>`: one file plus
    /// an `install.json`. `link` also points `current` at it when asked.
    fn fake_install(root: &Path, pkg: &str, version: &str, link: bool) {
        let dir = apps::pkg_root(root, pkg).join(version);
        std::fs::create_dir_all(&dir).unwrap();
        std::fs::write(dir.join("tool.exe"), b"x").unwrap();
        apps::save_install_record(&dir, "main", version, None).unwrap();
        if link {
            apps::link_current(&apps::pkg_root(root, pkg), version).unwrap();
        }
    }

    /// Writes `buckets/<bucket>/bucket/<pkg>.json` — the on-disk shape the
    /// registry reads when `Engine::new` builds its index.
    fn write_bucket_manifest(root: &Path, bucket: &str, pkg: &str, body: &str) {
        let dir = root.join("buckets").join(bucket).join("bucket");
        std::fs::create_dir_all(&dir).unwrap();
        std::fs::write(dir.join(format!("{pkg}.json")), body).unwrap();
    }

    /// [`fake_install`] with the bucket a real install records, plus an
    /// optional copy of the manifest the install used.
    fn fake_install_from_bucket(
        root: &Path,
        bucket: &str,
        pkg: &str,
        version: &str,
        manifest: Option<&str>,
    ) {
        let dir = apps::pkg_root(root, pkg).join(version);
        std::fs::create_dir_all(&dir).unwrap();
        std::fs::write(dir.join("tool.exe"), b"x").unwrap();
        let manifest = manifest.map(|body| serde_json::from_str(body).unwrap());
        apps::save_install_record(&dir, bucket, version, manifest).unwrap();
        apps::link_current(&apps::pkg_root(root, pkg), version).unwrap();
    }

    /// The regression this guards: a wiped/stale cache index must not hide
    /// packages that are installed on disk. `update` always scanned `apps/`
    /// and kept working, while `list` used to answer from `installed_packages`
    /// alone and printed "No packages installed" for a full root.
    #[test]
    fn list_installed_ignores_empty_cache_index() {
        let root = TempDir::new("glue-list-noindex");
        fake_install(root.path(), "alpha", "1.0.0", true);
        fake_install(root.path(), "beta", "2.3.4", false);

        let engine = Engine::new(root.path()).expect("engine");
        // Precondition: the index really is empty.
        assert!(engine.services.cache().list_installed().unwrap().is_empty());

        let list = engine.list_installed().expect("list");
        let names: Vec<&str> = list.iter().map(|p| p.name.as_str()).collect();
        assert_eq!(names, vec!["alpha", "beta"], "sorted by name");
        assert_eq!(list[0].version, "1.0.0");
        assert_eq!(list[1].version, "2.3.4");
        assert!(list[0].install_dir.ends_with("1.0.0"));
        // No content-cache row → no size/install time, but still listed.
        assert_eq!(list[0].size, 0);
        assert!(list[0].install_time.is_empty());
    }

    /// A version dir without `install.json` is a half-deployed directory, not
    /// an install (requires the record).
    #[test]
    fn list_installed_skips_dir_without_install_record() {
        let root = TempDir::new("glue-list-norecord");
        fake_install(root.path(), "good", "1.0.0", true);
        let half = apps::pkg_root(root.path(), "half").join("0.9.0");
        std::fs::create_dir_all(&half).unwrap();
        std::fs::write(half.join("partial.exe"), b"x").unwrap();
        apps::link_current(&apps::pkg_root(root.path(), "half"), "0.9.0").unwrap();

        let engine = Engine::new(root.path()).expect("engine");
        let list = engine.list_installed().expect("list");
        let names: Vec<&str> = list.iter().map(|p| p.name.as_str()).collect();
        assert_eq!(names, vec!["good"]);
    }

    /// With several versions on disk and no `current` junction, the newest
    /// (lexicographically greatest, `ListVersions` order) wins.
    #[test]
    fn list_installed_picks_newest_version_without_current_link() {
        let root = TempDir::new("glue-list-newest");
        fake_install(root.path(), "multi", "1.0.0", false);
        fake_install(root.path(), "multi", "2.0.0", false);
        // Newest version exists but is empty → skipped, so 2.0.0 is picked.
        std::fs::create_dir_all(apps::pkg_root(root.path(), "multi").join("3.0.0")).unwrap();

        let engine = Engine::new(root.path()).expect("engine");
        let list = engine.list_installed().expect("list");
        assert_eq!(list.len(), 1);
        assert_eq!(list[0].version, "2.0.0");
    }

    /// Size and install time come from the content-cache row, and the
    /// `current` junction wins over the indexed version
    #[test]
    fn list_installed_uses_cache_row_and_current_version() {
        let root = TempDir::new("glue-list-cacherow");
        fake_install(root.path(), "pkg", "1.0.0", false);
        fake_install(root.path(), "pkg", "2.0.0", true);

        let engine = Engine::new(root.path()).expect("engine");
        let mut files = std::collections::BTreeMap::new();
        files.insert("h1".to_string(), "tool.exe".to_string());
        engine
            .services
            .cache()
            .add("pkg", "1.0.0", &files, 4096)
            .unwrap();

        let list = engine.list_installed().expect("list");
        assert_eq!(list.len(), 1);
        // Indexed version is 1.0.0 but `current` points at 2.0.0.
        assert_eq!(list[0].version, "2.0.0");
        assert_eq!(list[0].size, 4096);
        assert!(!list[0].install_time.is_empty());
        // The disagreement is written back so the index heals itself
        // (→ `SetActiveVersionInstallDir`).
        assert_eq!(engine.services.cache().get("pkg").unwrap().version, "2.0.0");
    }

    /// `glue list` reads only the `packages` summary row; the `package_files`
    /// detail must not influence it. Guards the metadata-only read from being
    /// turned back into a file-row fetch (or into a file-derived size).
    #[test]
    fn list_installed_ignores_package_file_rows() {
        let root = TempDir::new("glue-list-no-file-rows");
        fake_install(root.path(), "pkg", "1.0.0", true);

        let engine = Engine::new(root.path()).expect("engine");
        let mut files = std::collections::BTreeMap::new();
        for i in 0..64 {
            files.insert(format!("h{i}"), format!("bin/f{i}.dll"));
        }
        engine
            .services
            .cache()
            .add("pkg", "1.0.0", &files, 8192)
            .unwrap();

        let listed = engine.list_installed().expect("list with file rows");
        assert_eq!(listed.len(), 1);
        assert_eq!(listed[0].version, "1.0.0");
        assert_eq!(listed[0].size, 8192);
        assert!(listed[0].install_dir.ends_with("1.0.0"));

        // Wipe the detail rows but keep the summary: the listed package must
        // be unchanged — the read has no dependency on the detail table.
        engine
            .services
            .cache()
            .add("pkg", "1.0.0", &std::collections::BTreeMap::new(), 8192)
            .unwrap();
        assert!(engine.services.cache().get("pkg").unwrap().files.is_empty());

        let listed = engine.list_installed().expect("list without file rows");
        assert_eq!(listed.len(), 1);
        assert_eq!(listed[0].version, "1.0.0");
        assert_eq!(listed[0].size, 8192);
        assert!(listed[0].install_dir.ends_with("1.0.0"));
    }

    /// `cache clear` must not touch a package that is still installed.
    /// `Index.Remove` deletes `packages`, `package_files` **and**
    /// `installed_packages` (dropping the version lock and install metadata)
    /// for whatever name it is handed, and frees no disk while doing it — the
    /// blobs stay hardlinked into `apps/`. Only uninstalled packages are
    /// cleared here.
    #[test]
    fn clear_cache_index_skips_installed_packages() {
        let root = TempDir::new("glue-clear-skip");
        fake_install(root.path(), "live", "1.0.0", true);
        let engine = Engine::new(root.path()).expect("engine");
        let files = std::collections::BTreeMap::from([("h1".to_string(), "tool.exe".to_string())]);
        engine
            .services
            .cache()
            .add("live", "1.0.0", &files, 100)
            .unwrap();
        engine
            .services
            .cache()
            .add("gone", "2.0.0", &files, 200)
            .unwrap();

        let report = engine
            .clear_cache_index(&["live".to_string(), "gone".to_string(), "never".to_string()])
            .expect("clear");

        assert_eq!(report.skipped_installed, vec!["live".to_string()]);
        assert_eq!(report.not_indexed, vec!["never".to_string()]);
        assert_eq!(report.cleared.len(), 1);
        assert_eq!(report.cleared[0].name, "gone");
        assert_eq!(report.cleared[0].file_count, 1);
        assert_eq!(report.summary().total_size, 200);

        // The live package keeps its content row; the uninstalled one is gone.
        assert!(engine.services.cache().get("live").is_some());
        assert!(engine.services.cache().get("gone").is_none());
    }

    /// The details dialog reads per-file sizes from the store objects, so an
    /// index row whose blob was deleted reports `present: false` instead of
    /// failing or looking like an empty file.
    #[test]
    fn cache_package_detail_reports_files_with_store_sizes() {
        let root = TempDir::new("glue-cache-detail");
        let engine = Engine::new(root.path()).expect("engine");
        // Go through the store itself: objects are sharded, and hand-writing
        // `store\<hash>` (as an earlier version of this test did) hid exactly
        // that mistake.
        let store = engine.services.store();
        let h1 = store
            .write(std::io::Cursor::new(b"12345".to_vec()))
            .expect("blob 1");
        let h2 = store
            .write(std::io::Cursor::new(b"123".to_vec()))
            .expect("blob 2");
        let missing = "0".repeat(64);
        let files = std::collections::BTreeMap::from([
            (h1, "zeta/app.exe".to_string()),
            (h2, "Alpha.txt".to_string()),
            (missing, "missing.bin".to_string()),
        ]);
        engine
            .services
            .cache()
            .add("pkg", "1.2.3", &files, 999)
            .unwrap();

        let detail = engine.cache_package_detail("pkg").expect("detail");
        assert_eq!(detail.name, "pkg");
        assert_eq!(detail.version, "1.2.3");
        assert_eq!(detail.size, 999);
        let names: Vec<&str> = detail.files.iter().map(|file| file.name.as_str()).collect();
        assert_eq!(names, vec!["Alpha.txt", "missing.bin", "zeta/app.exe"]);
        let sizes: Vec<i64> = detail.files.iter().map(|file| file.size).collect();
        assert_eq!(sizes, vec![3, 0, 5]);
        let present: Vec<bool> = detail.files.iter().map(|file| file.present).collect();
        assert_eq!(present, vec![true, false, true]);
        assert!(engine.cache_package_detail("absent").is_none());
    }

    /// `--all` follows the same rule and reports real file counts (the old
    /// implementation summed `list_packages()` entries, whose `files` map is
    /// always empty, so it printed `0 file(s)`).
    #[test]
    fn clear_all_cache_index_keeps_installed_packages() {
        let root = TempDir::new("glue-clear-all");
        fake_install(root.path(), "live", "1.0.0", true);
        let engine = Engine::new(root.path()).expect("engine");
        let files = std::collections::BTreeMap::from([("h1".to_string(), "tool.exe".to_string())]);
        for name in ["live", "gone1", "gone2"] {
            engine
                .services
                .cache()
                .add(name, "1.0.0", &files, 10)
                .unwrap();
        }

        let report = engine.clear_all_cache_index().expect("clear all");
        assert_eq!(report.skipped_installed, vec!["live".to_string()]);
        let cleared: Vec<&str> = report.cleared.iter().map(|e| e.name.as_str()).collect();
        assert_eq!(cleared, vec!["gone1", "gone2"]);
        assert_eq!(report.summary().total_files, 2);

        let remaining: Vec<String> = engine
            .services
            .cache()
            .list_packages()
            .into_keys()
            .collect();
        assert_eq!(remaining, vec!["live".to_string()]);
    }

    /// The scan hashes install files the store index does not know about
    /// (fallback), so a fabricated install is
    /// still indexed. That is also why "0 packages scanned" is so rare: the
    /// empty-scan guard can only trip on a wholesale scan failure.
    #[test]
    fn rebuild_indexes_files_missing_from_the_store() {
        let root = TempDir::new("glue-rebuild-guard");
        fake_install(root.path(), "live", "1.0.0", true);
        let engine = Engine::new(root.path()).expect("engine");

        assert_eq!(engine.rebuild_cache_index(None).expect("rebuild"), 1);
        let entry = engine.services.cache().get("live").expect("indexed");
        assert_eq!(entry.version, "1.0.0");
        // `tool.exe` plus `install.json` (not hidden, so it is indexed too).
        assert_eq!(entry.files.len(), 2);
    }

    /// An empty root is a legitimate no-op rebuild, not an error.
    #[test]
    fn rebuild_allows_empty_scan_without_installs() {
        let root = TempDir::new("glue-rebuild-empty");
        let engine = Engine::new(root.path()).expect("engine");
        assert_eq!(engine.rebuild_cache_index(None).expect("rebuild"), 0);
    }

    #[test]
    fn search_empty_index_returns_no_results() {
        let root = TempDir::new("glue-search-test");
        let engine = Engine::new(root.path()).expect("engine");
        let results = engine
            .search("nonexistent-package-xyz", &[], 10)
            .expect("search");
        assert!(results.is_empty());
    }

    /// `search` must answer from `catalog_entries` on the next run instead of
    /// re-parsing every manifest. Proof: tamper with the stored row and watch a
    /// fresh engine return the tampered description.
    #[test]
    fn search_uses_persisted_catalog_rows() {
        let root = TempDir::new("glue-search-persist");
        let dir = root.path().join("buckets/main/bucket");
        std::fs::create_dir_all(&dir).unwrap();
        std::fs::write(
            dir.join("git.json"),
            br#"{"version":"1.0.0","description":"Distributed VCS","url":"https://e/g.zip","hash":"h"}"#,
        )
        .unwrap();

        let first = Engine::new(root.path()).expect("engine");
        let hits = first.search("git", &[], 10).expect("search");
        assert_eq!(hits.len(), 1);
        assert_eq!(hits[0].description, "Distributed VCS");

        // Rewrite the persisted row (the fingerprint only covers the manifests,
        // so the cache still considers itself valid).
        {
            let db = first.services.cache().db.lock().unwrap();
            let n = db
                .execute(
                    "UPDATE catalog_entries SET description = 'FROM CACHE'
                     WHERE bucket = 'main' AND name = 'git'",
                    [],
                )
                .unwrap();
            assert_eq!(n, 1, "the catalog row was persisted");
        }
        drop(first);

        let second = Engine::new(root.path()).expect("engine");
        let hits = second.search("FROM CACHE", &[], 10).expect("search");
        assert_eq!(hits.len(), 1, "description came from catalog_entries");
        assert_eq!(hits[0].name, "git");
    }

    #[test]
    fn doctor_returns_report() {
        let root = TempDir::new("glue-doctor-test");
        let engine = Engine::new(root.path()).expect("engine");
        let report = engine.doctor().expect("doctor");
        assert_eq!(report.data_root, root.path());
        assert!(report.data_root_exists);
        assert!(report.buckets.is_empty());
        assert!(!report.shim_present);
        assert!(!report.config_present);
        assert_eq!(report.installed_count, 0);
        assert!(!report.host_arch.is_empty());
        assert!(report.win_build > 0);
        // The doctor walks the fixed Rust helper list, not whatever the data
        // directory happens to contain.
        assert_eq!(
            report.helpers.iter().map(|s| s.tool).collect::<Vec<_>>(),
            crate::bootstrap::ALL_HELPERS.to_vec()
        );
        // An empty root has no archive helpers; `git` is excluded because a
        // host git on PATH legitimately counts as ready.
        for status in &report.helpers {
            if status.tool != crate::bootstrap::HelperTool::Git {
                assert!(!status.ready, "{:?} should be missing", status.tool);
            }
        }
        assert!(report.missing_dependencies.is_empty());
    }

    /// The stub ships beside the program, which is neither the workspace nor
    /// necessarily named `glue.exe`: the desktop installer puts
    /// `gluestick-desktop.exe` next to `shim.exe` and the marker.
    #[test]
    fn shim_stub_is_found_beside_an_installed_program() {
        let workspace = TempDir::new("glue-doctor-shim-workspace");
        let install = TempDir::new("glue-doctor-shim-install");
        let desktop = install.path().join("gluestick-desktop.exe");

        // Nothing anywhere yet.
        assert!(!shim_stub_present(
            workspace.path(),
            Some(&desktop),
            Some(install.path())
        ));

        // The installed stub is enough on its own, before any shim is created.
        std::fs::write(install.path().join("shim.exe"), b"MZ").expect("write stub");
        assert!(shim_stub_present(
            workspace.path(),
            Some(&desktop),
            Some(install.path())
        ));

        // A harness beside the same artifact is not an installation.
        let harness = install
            .path()
            .join("gluestick_desktop-0123456789abcdef.exe");
        assert!(!shim_stub_present(workspace.path(), Some(&harness), None));

        // A portable `glue.exe` beside its stub needs no marker.
        assert!(shim_stub_present(
            workspace.path(),
            Some(&install.path().join("glue.exe")),
            None
        ));

        // The cached copy in the workspace counts on its own.
        std::fs::write(workspace.path().join("shim.exe"), b"MZ").expect("write cached stub");
        assert!(shim_stub_present(workspace.path(), None, None));
    }

    /// An install whose record carries no manifest still gets checked: the
    /// bucket manifest is the fallback source of `depends`.
    #[test]
    fn doctor_reports_missing_dependency() {
        let root = TempDir::new("glue-doctor-missing-dep");
        write_bucket_manifest(
            root.path(),
            "extras",
            "jhead",
            r#"{"version":"1.0","url":"https://e/j.zip","hash":"h","depends":"extras/mozjpeg"}"#,
        );
        fake_install_from_bucket(root.path(), "extras", "jhead", "1.0", None);

        let engine = Engine::new(root.path()).expect("engine");
        let report = engine.doctor().expect("doctor");
        assert_eq!(
            report.missing_dependencies,
            vec![MissingDependency {
                package: "jhead".to_string(),
                dependency: "extras/mozjpeg".to_string(),
            }]
        );
    }

    /// The record's manifest copy is the primary source, and a dependency that
    /// resolves to no bucket at all is still missing: `install` would fail on
    /// it, which is exactly what the report should surface.
    #[test]
    fn doctor_reports_unresolvable_dependency_from_record_manifest() {
        let root = TempDir::new("glue-doctor-record-dep");
        fake_install_from_bucket(
            root.path(),
            "extras",
            "jhead",
            "1.0",
            Some(r#"{"version":"1.0","url":"https://e/j.zip","hash":"h","depends":"extras/gone"}"#),
        );

        let engine = Engine::new(root.path()).expect("engine");
        let report = engine.doctor().expect("doctor");
        assert_eq!(
            report.missing_dependencies,
            vec![MissingDependency {
                package: "jhead".to_string(),
                dependency: "extras/gone".to_string(),
            }]
        );
    }

    /// The predicate is the installer's own cache lookup, not a directory
    /// probe: once the dependency has an installed row the pair disappears.
    #[test]
    fn doctor_missing_dependency_empty_once_dependency_installed() {
        let root = TempDir::new("glue-doctor-dep-installed");
        write_bucket_manifest(
            root.path(),
            "extras",
            "jhead",
            r#"{"version":"1.0","url":"https://e/j.zip","hash":"h","depends":"extras/mozjpeg"}"#,
        );
        fake_install_from_bucket(root.path(), "extras", "jhead", "1.0", None);
        fake_install(root.path(), "mozjpeg", "2.0", true);

        let engine = Engine::new(root.path()).expect("engine");
        // `installed_packages` has a foreign key to `packages`, so the row has
        // to be seeded before the install can be recorded.
        engine
            .services
            .cache()
            .add("mozjpeg", "2.0", &std::collections::BTreeMap::new(), 1)
            .unwrap();
        engine
            .services
            .cache()
            .add_installed("mozjpeg", "2.0", "ignored", 0, &serde_json::Map::new())
            .unwrap();

        let report = engine.doctor().expect("doctor");
        assert!(
            report.missing_dependencies.is_empty(),
            "{:?}",
            report.missing_dependencies
        );
    }

    #[test]
    fn uninstall_not_installed_returns_error() {
        let root = TempDir::new("glue-uninst-test");
        let engine = Engine::new(root.path()).expect("engine");
        let result = engine.uninstall("nope", None);
        assert!(matches!(result, Err(EngineError::NotInstalled(_))));
    }

    /// Launching an uninstalled package is an error, not a panic.
    #[test]
    fn launch_of_a_missing_package_errors() {
        let root = TempDir::new("glue-launch-missing");
        let engine = Engine::new(root.path()).expect("engine");
        let err = engine.launch("nope", None).expect_err("nothing to launch");
        assert!(matches!(err, EngineError::NotInstalled(_)), "{err:?}");
    }

    /// The executable named after the package wins over other executables in
    /// the install folder.
    #[test]
    fn launch_prefers_the_package_executable() {
        let root = TempDir::new("glue-launch-pick");
        fake_install(root.path(), "demo", "1.0", true);
        let dir = apps::pkg_root(root.path(), "demo").join("1.0");
        // A plain-MZ stub is not a PE, so both count as console and the name
        // match decides the order.
        std::fs::write(dir.join("helper.exe"), b"MZ").unwrap();
        std::fs::write(dir.join("demo.exe"), b"MZ").unwrap();
        let candidates = launch_candidates(root.path(), "demo");
        assert!(!candidates.is_empty(), "candidates: {candidates:?}");
        assert_eq!(
            candidates[0].program.file_name().unwrap(),
            "demo.exe",
            "the package-named executable is the default"
        );
    }

    /// The launch index round-trips: a preference hides or re-modes an entry,
    /// and removed entries are listed separately until restored.
    #[test]
    fn launch_preferences_hide_and_restore_entries() {
        let root = TempDir::new("glue-launch-index");
        fake_install(root.path(), "demo", "1.0", true);
        let dir = apps::pkg_root(root.path(), "demo").join("1.0");
        std::fs::write(dir.join("demo.exe"), b"MZ").unwrap();
        std::fs::write(dir.join("demo-helper.exe"), b"MZ").unwrap();

        let menu = launch_menu(root.path(), "demo");
        let before = menu.candidates.len();
        assert!(before >= 2, "{menu:?}");
        let keep = menu
            .candidates
            .iter()
            .find(|c| c.name == "demo-helper")
            .expect("helper candidate")
            .rel_path
            .clone();

        // Hide every other entry; the helper keeps its detected kind.
        hide_other_launchers(root.path(), "demo", &keep).unwrap();
        let menu = launch_menu(root.path(), "demo");
        let demo = menu
            .candidates
            .iter()
            .find(|c| c.name == "demo")
            .expect("demo candidate")
            .clone();
        assert_eq!(demo.kind, LaunchKind::Skip);
        assert!(demo.user_set, "the skip came from the preference file");
        assert!(menu
            .candidates
            .iter()
            .find(|c| c.name == "demo-helper")
            .is_some_and(|c| c.kind != LaunchKind::Skip));

        // Re-mode it, then clear the override.
        set_launch_kind(
            root.path(),
            "demo",
            &demo.rel_path,
            Some(LaunchKind::Console),
        )
        .unwrap();
        let menu = launch_menu(root.path(), "demo");
        assert_eq!(
            menu.candidates
                .iter()
                .find(|c| c.name == "demo")
                .unwrap()
                .kind,
            LaunchKind::Console
        );
        set_launch_kind(root.path(), "demo", &demo.rel_path, None).unwrap();
        assert!(
            !launch_menu(root.path(), "demo")
                .candidates
                .iter()
                .find(|c| c.name == "demo")
                .unwrap()
                .user_set
        );

        // Remove, then restore.
        remove_launcher(root.path(), "demo", &demo.rel_path).unwrap();
        let menu = launch_menu(root.path(), "demo");
        assert_eq!(menu.candidates.len(), before - 1);
        assert_eq!(menu.removed, vec![demo.rel_path.clone()]);
        restore_launcher(root.path(), "demo", &demo.rel_path).unwrap();
        assert_eq!(launch_menu(root.path(), "demo").candidates.len(), before);
    }

    /// GUI programs are recognised from their PE subsystem: packages that ship
    /// both (`godot.exe` / `godot_console.exe`) must default to the windowed
    /// one instead of flashing a console.
    #[test]
    fn pe_subsystem_distinguishes_gui_from_console() {
        let sys = std::env::var("SystemRoot").unwrap_or_else(|_| "C:\\Windows".to_string());
        let gui = Path::new(&sys).join("System32").join("notepad.exe");
        let console = Path::new(&sys).join("System32").join("cmd.exe");
        if gui.is_file() && console.is_file() {
            assert!(is_gui_executable(&gui), "notepad is a GUI program");
            assert!(!is_gui_executable(&console), "cmd is a console program");
            assert_eq!(launch_kind_for(&gui), LaunchKind::Gui);
            assert_eq!(launch_kind_for(&console), LaunchKind::Console);
        }
    }

    /// The launcher knows `.exe`, `.bat`, `.cmd` and `.jar`; scripts have no
    /// PE header, so they run in a console window.
    #[test]
    fn runnable_types_default_to_console_for_scripts() {
        assert!(is_runnable_file(Path::new("a.exe")));
        assert!(is_runnable_file(Path::new("a.BAT")));
        assert!(is_runnable_file(Path::new("a.cmd")));
        assert!(is_runnable_file(Path::new("a.jar")));
        assert!(!is_runnable_file(Path::new("readme.txt")));
        assert_eq!(launch_kind_for(Path::new("run.bat")), LaunchKind::Console);
    }

    /// A path with spaces must reach the inner `cmd /k` as a literal quoted
    /// token. Plain quotes are eaten by the outer `cmd /c start` parse (and
    /// Rust's argument escaping turns them into `\"`), so they are caret-
    /// escaped instead.
    #[test]
    fn cmd_launch_escapes_quotes_for_the_outer_cmd() {
        assert_eq!(
            quote_cmd_token(Path::new(r"C:\glue\bin\git.exe")),
            r"C:\glue\bin\git.exe"
        );
        assert_eq!(
            quote_cmd_token(Path::new(r"C:\Program Files\app\x.exe")),
            r#"^"C:\Program Files\app\x.exe""#
        );
    }

    /// A manifest may point its shim at the console build
    /// (`godot.console.exe`) while the windowed `godot.exe` sits next to it.
    /// Both must be offered exactly once — the shim labelled by the program it
    /// starts — instead of the GUI build being dropped as a "duplicate name".
    #[test]
    fn shim_and_windowed_build_are_both_offered() {
        let root = TempDir::new("glue-launch-shim-gui");
        fake_install(root.path(), "godot", "1.0", true);
        let dir = apps::pkg_root(root.path(), "godot").join("1.0");
        std::fs::write(dir.join("godot.exe"), b"MZ").unwrap();
        std::fs::write(dir.join("godot.console.exe"), b"MZ").unwrap();
        // A helper the manifest does not declare: it must not show up.
        std::fs::write(dir.join("godot-helper.exe"), b"MZ").unwrap();
        // The real manifest: `bin` is the console build, the windowed one is
        // declared as a shortcut. The record carries it for an installed app.
        apps::save_install_record(
            &dir,
            "main",
            "1.0",
            Some(serde_json::json!({
                "version": "1.0",
                "bin": [["godot.console.exe", "godot"]],
                "shortcuts": [["godot.exe", "Godot Engine"]]
            })),
        )
        .unwrap();

        let meta = root.path().join("shims-meta");
        std::fs::create_dir_all(&meta).unwrap();
        let target = dir.join("godot.console.exe").display().to_string();
        let quoted = serde_json::to_string(&target).unwrap();
        std::fs::write(
            meta.join("godot.json"),
            format!(r#"{{"name":"godot","command":{quoted},"path":{quoted}}}"#),
        )
        .unwrap();
        std::fs::create_dir_all(root.path().join("shims")).unwrap();
        std::fs::write(root.path().join("shims").join("godot.exe"), b"MZ").unwrap();

        let candidates = launch_candidates(root.path(), "godot");
        let names: Vec<&str> = candidates.iter().map(|c| c.name.as_str()).collect();
        assert!(
            names.contains(&"godot.console"),
            "the shim is labelled by its target: {names:?}"
        );
        assert!(
            names.contains(&"Godot Engine"),
            "the manifest's shortcut target is offered (the windowed build): {names:?}"
        );
        assert_eq!(
            names.iter().filter(|n| **n == "godot.console").count(),
            1,
            "the shim's target must not appear twice: {names:?}"
        );
        assert!(
            !names.contains(&"godot-helper"),
            "the install tree is not scanned when the manifest declares programs: {names:?}"
        );
    }

    /// `env_add_path` shims (`python`'s `Scripts\pip.exe`, `f2py.exe`, …) are
    /// there for the command line, not the launch menu: only the manifest's
    /// declared bins are offered.
    #[test]
    fn env_add_path_shims_stay_out_of_the_launch_menu() {
        let root = TempDir::new("glue-launch-envpath");
        fake_install(root.path(), "python", "1.0", true);
        let dir = apps::pkg_root(root.path(), "python").join("1.0");
        std::fs::create_dir_all(dir.join("Scripts")).unwrap();
        std::fs::write(dir.join("python.exe"), b"MZ").unwrap();
        std::fs::write(dir.join("Scripts").join("pip.exe"), b"MZ").unwrap();
        apps::save_install_record(
            &dir,
            "main",
            "1.0",
            Some(serde_json::json!({
                "version": "1.0",
                "bin": ["python.exe"],
                "env_add_path": ["Scripts"]
            })),
        )
        .unwrap();

        // Shims for both, as an install creates them.
        let meta = root.path().join("shims-meta");
        std::fs::create_dir_all(&meta).unwrap();
        std::fs::create_dir_all(root.path().join("shims")).unwrap();
        for (shim_name, target) in [
            ("python", dir.join("python.exe")),
            ("pip", dir.join("Scripts").join("pip.exe")),
        ] {
            let target = target.display().to_string();
            let quoted = serde_json::to_string(&target).unwrap();
            std::fs::write(
                meta.join(format!("{shim_name}.json")),
                format!(r#"{{"name":"{shim_name}","command":{quoted},"path":{quoted}}}"#),
            )
            .unwrap();
            std::fs::write(
                root.path().join("shims").join(format!("{shim_name}.exe")),
                b"MZ",
            )
            .unwrap();
        }

        let names: Vec<String> = launch_candidates(root.path(), "python")
            .into_iter()
            .map(|c| c.name)
            .collect();
        assert!(names.iter().any(|name| name == "python"), "{names:?}");
        assert!(
            !names.iter().any(|name| name == "pip"),
            "an env_add_path shim is not a program to open: {names:?}"
        );
    }

    /// A package whose manifest declares nothing keeps working: the install
    /// folder is scanned as a fallback.
    #[test]
    fn undeclared_package_falls_back_to_scanning() {
        let root = TempDir::new("glue-launch-scan");
        fake_install(root.path(), "tool", "1.0", true);
        let dir = apps::pkg_root(root.path(), "tool").join("1.0");
        std::fs::write(dir.join("tool-gui.exe"), b"MZ").unwrap();
        let names: Vec<String> = launch_candidates(root.path(), "tool")
            .into_iter()
            .map(|c| c.name)
            .collect();
        assert!(names.contains(&"tool".to_string()), "{names:?}");
        assert!(names.contains(&"tool-gui".to_string()), "{names:?}");
    }

    /// Removing the active version of a multi-version package leaves the older
    /// directories in place; `current` must follow them instead of dangling.
    #[test]
    fn uninstall_active_version_repoints_current_to_survivor() {
        let root = TempDir::new("glue-uninst-repoint");
        fake_install(root.path(), "pkg", "1.0.0", false);
        fake_install(root.path(), "pkg", "2.0.0", true);
        let engine = Engine::new(root.path()).expect("engine");

        let outcome = engine.uninstall("pkg", None).expect("uninstall");
        assert_eq!(outcome.version, "2.0.0");

        let pkg_root = apps::pkg_root(root.path(), "pkg");
        assert!(!pkg_root.join("2.0.0").exists());
        assert!(pkg_root.join("1.0.0").exists());
        assert_eq!(apps::read_current(&pkg_root).as_deref(), Some("1.0.0"));
    }

    /// A `current` link left over from an uninstalled version must not make
    /// `update` believe that (removed) version is still what is installed.
    #[test]
    fn update_reports_survivor_when_current_link_dangles() {
        let root = TempDir::new("glue-update-dangling");
        seed_update_fixture(root.path(), "demo", "main", "1.0.0", "2.0.0");
        let pkg_root = apps::pkg_root(root.path(), "demo");
        let newer = pkg_root.join("2.0.0");
        std::fs::create_dir_all(&newer).unwrap();
        std::fs::write(newer.join("marker.txt"), b"x").unwrap();
        apps::save_install_record(&newer, "main", "2.0.0", None).unwrap();
        apps::link_current(&pkg_root, "2.0.0").unwrap();
        // Simulate the old uninstall behavior: active version removed, link left.
        std::fs::remove_dir_all(&newer).unwrap();

        let engine = Engine::new(root.path()).expect("engine");
        let updates = engine.check_package_updates().expect("check updates");
        assert_eq!(updates.len(), 1);
        assert_eq!(updates[0].name, "demo");
        assert_eq!(updates[0].installed_version, "1.0.0");
        assert_eq!(updates[0].latest_version, "2.0.0");
    }

    #[test]
    fn update_not_installed_returns_error() {
        let root = TempDir::new("glue-update-test");
        let engine = Engine::new(root.path()).expect("engine");
        let result = engine.update("nope", None);
        assert!(matches!(result, Err(EngineError::NotInstalled(_))));
    }

    /// A batch is non-fail-fast: one bad input must not swallow the others'
    /// results, and the caller gets one slot per input, in order.
    #[test]
    fn update_many_reports_each_missing_package() {
        let root = TempDir::new("glue-update-many-missing");
        let engine = Engine::new(root.path()).expect("engine");
        let refs = vec!["missing-a".to_string(), "missing-b".to_string()];
        let results = engine.update_many(&refs, None);
        assert_eq!(results.len(), 2);
        assert!(matches!(&results[0], Err(EngineError::NotInstalled(_))));
        assert!(matches!(&results[1], Err(EngineError::NotInstalled(_))));
    }

    /// The batch's metadata pass keeps the "already on disk → switch" path:
    /// that package never reaches the concurrent fetch half.
    #[test]
    fn update_many_switches_versions_already_on_disk() {
        let root = TempDir::new("glue-update-many-switch");
        seed_update_fixture(root.path(), "demo", "main", "1.0.0", "2.0.0");
        apps::link_current(&apps::pkg_root(root.path(), "demo"), "1.0.0").unwrap();
        fake_install(root.path(), "demo", "2.0.0", false);

        let engine = Engine::new(root.path()).expect("engine");
        let mut files = std::collections::BTreeMap::new();
        files.insert("h1".to_string(), "tool.exe".to_string());
        engine
            .services
            .cache()
            .add("demo", "1.0.0", &files, 4096)
            .unwrap();

        let results = engine.update_many(&["demo".to_string()], None);
        assert_eq!(results.len(), 1);
        let outcome = results.into_iter().next().unwrap().expect("update");
        assert_eq!(outcome.version, "2.0.0");
        assert!(outcome.already_installed);
        assert_eq!(
            apps::read_current(&apps::pkg_root(root.path(), "demo")).as_deref(),
            Some("2.0.0")
        );
    }

    /// Removing an old version is cleanup: the active version, its `current`
    /// link and the package's install state stay exactly as they were.
    #[test]
    fn remove_installed_version_deletes_only_an_inactive_version() {
        let root = TempDir::new("glue-remove-version");
        fake_install(root.path(), "demo", "1.0.0", false);
        fake_install(root.path(), "demo", "2.0.0", false);
        apps::link_current(&apps::pkg_root(root.path(), "demo"), "2.0.0").unwrap();

        let engine = Engine::new(root.path()).expect("engine");
        engine
            .remove_installed_version("demo", "1.0.0")
            .expect("remove old version");

        let pkg_root = apps::pkg_root(root.path(), "demo");
        assert!(!pkg_root.join("1.0.0").exists());
        assert!(pkg_root.join("2.0.0").is_dir());
        assert_eq!(
            apps::read_current(&pkg_root).as_deref(),
            Some("2.0.0"),
            "the active version must not move"
        );

        let err = engine
            .remove_installed_version("demo", "2.0.0")
            .expect_err("the active version cannot be removed");
        assert!(matches!(err, EngineError::VersionInUse(_, _)), "{err:?}");
        assert!(pkg_root.join("2.0.0").is_dir());

        let err = engine
            .remove_installed_version("demo", "9.9.9")
            .expect_err("a version that is not installed");
        assert!(matches!(err, EngineError::VersionNotFound(_, _)), "{err:?}");
    }

    /// Bulk old-version cleanup sweeps every installed package at once and
    /// leaves the active version of each alone.
    #[test]
    fn cleanup_old_versions_removes_only_inactive_versions() {
        let root = TempDir::new("glue-cleanup-old-versions");
        fake_install(root.path(), "alpha", "1.0.0", false);
        fake_install(root.path(), "alpha", "2.0.0", true);
        fake_install(root.path(), "beta", "0.9.0", false);
        fake_install(root.path(), "beta", "1.1.0", true);
        fake_install(root.path(), "solo", "1.0.0", true);

        let engine = Engine::new(root.path()).expect("engine");
        let cleanup = engine.cleanup_old_versions(&[]).expect("cleanup");

        let mut removed: Vec<(String, String)> = cleanup
            .removed
            .iter()
            .map(|entry| (entry.package.clone(), entry.version.clone()))
            .collect();
        removed.sort();
        assert_eq!(
            removed,
            vec![
                ("alpha".to_string(), "1.0.0".to_string()),
                ("beta".to_string(), "0.9.0".to_string()),
            ]
        );
        assert!(cleanup.freed > 0, "the removed payload counts as freed");
        for (pkg, version) in [("alpha", "2.0.0"), ("beta", "1.1.0"), ("solo", "1.0.0")] {
            let dir = apps::pkg_root(root.path(), pkg).join(version);
            assert!(dir.is_dir(), "{} must survive", dir.display());
            assert_eq!(
                apps::read_current(&apps::pkg_root(root.path(), pkg)).as_deref(),
                Some(version),
                "the active version must not move"
            );
        }

        // Nothing left to sweep.
        let again = engine.cleanup_old_versions(&[]).expect("second cleanup");
        assert!(again.removed.is_empty(), "{:?}", again.removed);

        // The filter keeps the sweep to the named packages.
        fake_install(root.path(), "beta", "0.5.0", false);
        let filtered = engine
            .cleanup_old_versions(&["alpha".to_string()])
            .expect("filtered cleanup");
        assert!(filtered.removed.is_empty());
        assert!(apps::pkg_root(root.path(), "beta").join("0.5.0").is_dir());
    }

    #[test]
    fn purge_partials_clears_fragments_and_ignores_missing_dir() {
        let root = TempDir::new("glue-purge-partials");
        // Nothing downloaded yet: a missing store\.partial is not an error.
        assert_eq!(purge_partials(root.path()), PartialPurge::default());

        let dir = root.path().join("store").join(".partial");
        std::fs::create_dir_all(&dir).unwrap();
        std::fs::write(dir.join("abc.part"), b"12345").unwrap();
        std::fs::write(dir.join("abc.meta"), b"12").unwrap();
        // A directory inside the fragment dir is not a fragment.
        std::fs::create_dir_all(dir.join("sub")).unwrap();

        let purge = purge_partials(root.path());
        assert_eq!(purge.files, 2, "{purge:?}");
        assert_eq!(purge.bytes, 7, "{purge:?}");
        assert_eq!(purge.failed, 0);
        assert!(dir.is_dir(), "the fragment directory itself survives");
        assert!(dir.join("sub").is_dir(), "directories are left alone");
        let left: Vec<_> = std::fs::read_dir(&dir).unwrap().flatten().collect();
        assert_eq!(left.len(), 1, "{left:?}");
    }

    /// Fixture: `apps/<pkg>/<version>` with an install record + one payload
    /// file, plus `<bucket>/bucket/<pkg>.json` at `manifest_version`.
    fn seed_update_fixture(
        root: &Path,
        pkg: &str,
        bucket: &str,
        installed: &str,
        manifest_version: &str,
    ) {
        let ver_dir = apps::pkg_root(root, pkg).join(installed);
        std::fs::create_dir_all(&ver_dir).unwrap();
        std::fs::write(ver_dir.join("marker.txt"), b"x").unwrap();
        let manifest_json: serde_json::Value = serde_json::from_str(&format!(
            r#"{{"version":"{manifest_version}","url":"https://example.com/app.zip","hash":"deadbeef"}}"#
        ))
        .unwrap();
        apps::save_install_record(&ver_dir, bucket, installed, Some(manifest_json)).unwrap();
        let manifest_path = root
            .join("buckets")
            .join(bucket)
            .join("bucket")
            .join(format!("{pkg}.json"));
        std::fs::create_dir_all(manifest_path.parent().unwrap()).unwrap();
        std::fs::write(
            &manifest_path,
            format!(
                r#"{{"version":"{manifest_version}","url":"https://example.com/app.zip","hash":"deadbeef"}}"#
            ),
        )
        .unwrap();
    }

    #[test]
    fn update_switches_when_the_target_version_is_already_installed() {
        // A downgrade keeps the higher version's directory. Updating back to it
        // must switch `current`, not re-download the same bytes.
        let root = TempDir::new("glue-update-switch");
        seed_update_fixture(root.path(), "demo", "main", "1.0.0", "2.0.0");
        apps::link_current(&apps::pkg_root(root.path(), "demo"), "1.0.0").unwrap();
        fake_install(root.path(), "demo", "2.0.0", false);

        let engine = Engine::new(root.path()).expect("engine");
        // A real install always leaves a content-cache row; the switch updates
        // its version.
        let mut files = std::collections::BTreeMap::new();
        files.insert("h1".to_string(), "tool.exe".to_string());
        engine
            .services
            .cache()
            .add("demo", "1.0.0", &files, 4096)
            .unwrap();

        let outcome = engine.update("demo", None).expect("update");
        assert_eq!(outcome.version, "2.0.0");
        assert!(
            outcome.already_installed,
            "update must switch, not reinstall"
        );
        assert_eq!(
            apps::read_current(&apps::pkg_root(root.path(), "demo")).as_deref(),
            Some("2.0.0"),
            "current must point at the latest version"
        );
    }

    /// `glue reset` is not just a junction swap: shim `env` values expand `$dir`
    /// at install time (and so do `env_set` values and shortcut targets), so
    /// without a rebuild they keep launching the version that was active before
    /// the switch.
    #[test]
    fn reset_rebuilds_shims_for_the_switched_version() {
        let root = TempDir::new("glue-reset-shims");
        let pkg_root = apps::pkg_root(root.path(), "demo");
        for version in ["1.0.0", "2.0.0"] {
            let dir = pkg_root.join(version);
            std::fs::create_dir_all(&dir).unwrap();
            std::fs::write(dir.join("tool.exe"), b"MZ").unwrap();
            apps::save_install_record(
                &dir,
                "main",
                version,
                Some(serde_json::json!({
                    "version": version,
                    "bin": ["tool.exe"],
                    "env": {"DEMO_HOME": "$dir"}
                })),
            )
            .unwrap();
        }
        apps::link_current(&pkg_root, "2.0.0").unwrap();

        let engine = Engine::new(root.path()).expect("engine");
        // A real install leaves a content-cache row; `reset` updates its
        // version, and the switch fails without one.
        let mut files = std::collections::BTreeMap::new();
        files.insert("h1".to_string(), "tool.exe".to_string());
        engine
            .services
            .cache()
            .add("demo", "2.0.0", &files, 4096)
            .unwrap();

        // Returns the shim config after asserting `DEMO_HOME` follows `version`.
        let shim_config = |version: &str| -> serde_json::Value {
            let path = root.path().join("shims-meta").join("tool.json");
            let config: serde_json::Value =
                serde_json::from_slice(&std::fs::read(&path).unwrap()).unwrap();
            let home = config["env"]["DEMO_HOME"].as_str().expect("env is kept");
            assert_eq!(
                Path::new(home),
                pkg_root.join(version).as_path(),
                "the shim's $dir must follow the active version"
            );
            config
        };

        // Shims as an install of the active version leaves them.
        engine.reset_package("demo@2.0.0").expect("reset to active");
        shim_config("2.0.0");

        engine.reset_package("demo@1.0.0").expect("reset down");
        assert_eq!(apps::read_current(&pkg_root).as_deref(), Some("1.0.0"));
        let config = shim_config("1.0.0");
        // The runner itself stays version-independent: it goes through `current`.
        let command = config["command"].as_str().unwrap();
        let expected_tail = format!(
            "demo{sep}current{sep}tool.exe",
            sep = std::path::MAIN_SEPARATOR
        );
        assert!(command.ends_with(&expected_tail), "{command}");
        assert!(
            root.path().join("shims").join("tool.exe").is_file()
                || root.path().join("shims").join("tool.bat").is_file(),
            "the runner is recreated for the switched version"
        );

        engine.reset_package("demo@2.0.0").expect("reset back up");
        shim_config("2.0.0");
    }

    /// The desktop restores under its own data-root hold, so the switch must
    /// accept that hold instead of taking the lock again (which would wait for
    /// itself until the timeout), and it must accept a bucket-scoped ref.
    #[test]
    fn reset_package_with_lock_switches_and_accepts_a_bucket_ref() {
        let root = TempDir::new("glue-reset-locked");
        let pkg_root = apps::pkg_root(root.path(), "demo");
        for version in ["1.0.0", "2.0.0"] {
            let dir = pkg_root.join(version);
            std::fs::create_dir_all(&dir).unwrap();
            std::fs::write(
                dir.join(apps::INSTALL_RECORD_FILE),
                format!(r#"{{"version":"{version}","bucket":"main"}}"#),
            )
            .unwrap();
        }
        apps::link_current(&pkg_root, "2.0.0").unwrap();

        let engine = Engine::new(root.path()).expect("engine");
        let mut files = std::collections::BTreeMap::new();
        files.insert("h1".to_string(), "tool.exe".to_string());
        engine
            .services
            .cache()
            .add("demo", "2.0.0", &files, 4096)
            .unwrap();

        let hold = crate::lock::MutationLock::acquire(root.path()).expect("hold");
        engine
            .reset_package_with_lock("extras/demo@1.0.0", &hold)
            .expect("switch under the caller's hold");
        assert_eq!(apps::read_current(&pkg_root).as_deref(), Some("1.0.0"));
    }

    #[test]
    fn check_package_updates_reports_newer_manifest() {
        let root = TempDir::new("glue-updcheck-test");
        seed_update_fixture(root.path(), "demo", "main", "1.0.0", "2.0.0");
        let engine = Engine::new(root.path()).expect("engine");
        let updates = engine.check_package_updates().expect("check updates");
        assert_eq!(updates.len(), 1);
        let u = &updates[0];
        assert_eq!(u.name, "demo");
        assert_eq!(u.installed_version, "1.0.0");
        assert_eq!(u.latest_version, "2.0.0");
        assert_eq!(u.bucket, "main");
        assert_eq!(u.install_ref(), "demo");
    }

    #[test]
    fn check_package_updates_install_ref_prefixed_for_non_main_bucket() {
        let root = TempDir::new("glue-updcheck-ref-test");
        seed_update_fixture(root.path(), "demo", "extras", "1.0.0", "1.1.0");
        let engine = Engine::new(root.path()).expect("engine");
        let updates = engine.check_package_updates().expect("check updates");
        assert_eq!(updates.len(), 1);
        assert_eq!(updates[0].install_ref(), "extras/demo");
    }

    #[test]
    fn update_with_bucket_prefixed_ref_resolves_installed_base_name() {
        // Regression for `glue update extras/demo` reporting "not installed"
        // even though `glue update` lists it: the install lives at `apps/demo`,
        // so the bucket prefix must be stripped before computing PkgRoot.
        let root = TempDir::new("glue-update-ref-resolve-test");
        seed_update_fixture(root.path(), "demo", "extras", "1.0.0", "1.0.0");
        let engine = Engine::new(root.path()).expect("engine");
        // Installed == manifest version, so update short-circuits to UpToDate
        // (it must find the install instead of returning NotInstalled).
        let result = engine.update("extras/demo", None);
        assert!(
            !matches!(result, Err(EngineError::NotInstalled(_))),
            "bucket-prefixed ref must resolve the base-name install dir"
        );
        assert!(matches!(result, Err(EngineError::UpToDate(_, v)) if v == "1.0.0"));
    }

    #[test]
    fn check_package_updates_empty_when_current() {
        let root = TempDir::new("glue-updcheck-clear-test");
        seed_update_fixture(root.path(), "demo", "main", "1.0.0", "1.0.0");
        let engine = Engine::new(root.path()).expect("engine");
        assert!(engine
            .check_package_updates()
            .expect("check updates")
            .is_empty());
    }

    #[test]
    fn check_package_updates_skips_locked_package() {
        let root = TempDir::new("glue-updcheck-lock-test");
        seed_update_fixture(root.path(), "demo", "main", "1.0.0", "2.0.0");
        let engine = Engine::new(root.path()).expect("engine");
        let install_dir = apps::pkg_root(root.path(), "demo").join("1.0.0");
        engine
            .services()
            .cache()
            .add("demo", "1.0.0", &std::collections::BTreeMap::new(), 1)
            .unwrap();
        engine
            .services()
            .cache()
            .add_installed(
                "demo",
                "1.0.0",
                &install_dir.to_string_lossy(),
                1,
                &serde_json::Map::new(),
            )
            .unwrap();
        engine.set_package_version_lock("demo", true).unwrap();
        assert!(engine
            .check_package_updates()
            .expect("check updates")
            .is_empty());
    }

    #[test]
    fn add_bucket_already_exists_returns_error() {
        let root = TempDir::new("glue-bucket-add-test");
        std::fs::create_dir_all(root.path().join("buckets").join("main")).unwrap();
        let mut engine = Engine::new(root.path()).expect("engine");
        let result = engine.add_bucket("main", Some("https://example.com/repo.git"), None);
        assert!(matches!(
            result,
            Err(EngineError::Bucket(ref n, _)) if n == "main"
        ));
    }

    #[test]
    fn pull_bucket_not_found_returns_error() {
        let root = TempDir::new("glue-bucket-pull-test");
        let mut engine = Engine::new(root.path()).expect("engine");
        let result = engine.pull_bucket("main", None);
        assert!(matches!(
            result,
            Err(EngineError::Bucket(ref n, _)) if n == "main"
        ));
    }

    #[test]
    fn update_buckets_empty_is_noop() {
        let root = TempDir::new("glue-bucket-update-test");
        let mut engine = Engine::new(root.path()).expect("engine");
        let result = engine.update_buckets(None);
        assert!(result.is_ok());
    }

    #[test]
    fn gc_progress_events_are_emitted() {
        let root = TempDir::new("glue-gc-test");
        let _engine = Engine::new(root.path()).expect("engine");
        use std::sync::{Arc, Mutex};
        let events = Arc::new(Mutex::new(Vec::<ProgressEvent>::new()));
        let events_clone = Arc::clone(&events);
        let progress: ProgressFn = Box::new(move |e| {
            events_clone.lock().unwrap().push(e.clone());
        });

        // Simulate GC events manually (engine doesn't run GC yet, but we verify
        // the event types are constructible and the callback receives them).
        progress(ProgressEvent::GcStarted {
            bucket: "main".to_string(),
        });
        progress(ProgressEvent::GcProgress {
            bucket: "main".to_string(),
            removed: 3,
            kept: 10,
        });
        progress(ProgressEvent::GcCompleted {
            bucket: "main".to_string(),
            removed: 3,
        });

        let events = events.lock().unwrap().clone();
        assert_eq!(events.len(), 3);
        assert!(matches!(
            events[0],
            ProgressEvent::GcStarted { ref bucket } if bucket == "main"
        ));
        assert!(matches!(
            events[1],
            ProgressEvent::GcProgress { removed, kept, .. }
                if removed == 3 && kept == 10
        ));
        assert!(matches!(
            events[2],
            ProgressEvent::GcCompleted { removed, .. } if removed == 3
        ));
    }

    /// Every requested bucket appears in the result, sorted by name, even
    /// when the fetch fails; the checks fan out but the output stays ordered.
    #[test]
    fn check_bucket_updates_reports_each_bucket() {
        let root = TempDir::new("glue-bucket-updates");
        for name in ["extras", "main"] {
            std::fs::create_dir_all(root.path().join("buckets").join(name).join("bucket")).unwrap();
        }
        let engine = Engine::new(root.path()).expect("engine");
        let statuses = engine.check_bucket_updates(&[]);
        let names: Vec<&str> = statuses.iter().map(|s| s.name.as_str()).collect();
        assert_eq!(names, vec!["extras", "main"]);
        // Neither directory is a git repository, so each fetch fails — but
        // both rows must be reported, not dropped on the first error.
        assert!(statuses.iter().all(|s| !s.ok));
        assert!(statuses.iter().all(|s| !s.error.is_empty()));
    }

    /// With an installed row, `info` reports the indexed file count (the CLI
    /// labels it "indexed files") instead of walking the install tree.
    #[test]
    fn info_uses_indexed_file_count_when_installed_row_exists() {
        let root = TempDir::new("glue-info-index-count");
        fake_install(root.path(), "pkg", "1.0.0", true);
        let dir = root.path().join("apps").join("pkg").join("1.0.0");
        for i in 0..5 {
            std::fs::write(dir.join(format!("extra{i}.bin")), b"x").unwrap();
        }
        let engine = Engine::new(root.path()).expect("engine");
        let mut files = std::collections::BTreeMap::new();
        files.insert("h1".to_string(), "tool.exe".to_string());
        files.insert("h2".to_string(), "extra0.bin".to_string());
        engine
            .services
            .cache()
            .add("pkg", "1.0.0", &files, 4096)
            .unwrap();
        engine
            .services
            .cache()
            .add_installed(
                "pkg",
                "1.0.0",
                dir.to_string_lossy().as_ref(),
                4096,
                &serde_json::Map::new(),
            )
            .unwrap();

        let detail = engine.get_installed_package_detail("pkg").expect("detail");
        assert_eq!(detail.file_count, 2, "index count, not the 7 files on disk");
        assert!(!detail.installed_at.is_empty());
    }

    /// Without an installed row the CLI labels the value "files", so it stays
    /// a disk walk.
    #[test]
    fn info_falls_back_to_disk_file_count_without_installed_row() {
        let root = TempDir::new("glue-info-disk-count");
        fake_install(root.path(), "pkg", "1.0.0", true);
        let engine = Engine::new(root.path()).expect("engine");
        let detail = engine.get_installed_package_detail("pkg").expect("detail");
        assert_eq!(detail.file_count, 2); // tool.exe + install.json
        assert!(detail.installed_at.is_empty());
    }
}
