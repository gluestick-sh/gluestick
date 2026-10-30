//! Shared engine state.
//!
//! `Services` is a god-object holding the store, cache index, downloader,
//! extractor, bootstrap, bucket registry, shim manager and search index.
//!
//! The search index is built *lazily* on first access (see `search_index`), so
//! commands that never search or list the catalog skip the manifest scan
//! entirely — a measurable startup win on large bucket sets.

use std::path::{Path, PathBuf};
use std::sync::{Mutex, MutexGuard, RwLock, RwLockReadGuard, RwLockWriteGuard};

use thiserror::Error;

use crate::bucket::{BucketError, Registry};
use crate::cache::Index as CacheIndex;
use crate::config;
use crate::downloader::Downloader;
use crate::extractor::Extractor;
use crate::search_index::Index as SearchIndex;
use crate::store::{Store, StoreError};

/// Engine construction and operation errors.
#[derive(Debug, Error)]
pub enum ServicesError {
    #[error("initialize cache store: {0}")]
    Store(#[from] StoreError),
    #[error("initialize cache index: {0}")]
    Cache(#[from] crate::cache::IndexError),
    #[error("initialize bucket registry: {0}")]
    Bucket(#[from] BucketError),
    #[error("read config: {0}")]
    Config(#[from] crate::config::ConfigError),
    #[error("resolve package: {0}")]
    Resolve(Box<dyn std::error::Error + Send + Sync>),
}

/// Configuration for [`Services::new`].
#[derive(Debug, Clone)]
pub struct EngineConfig {
    /// Data root (`~/.glue` by default).
    pub root_dir: PathBuf,
    /// Download worker count; `0` (or negative) leaves the value in
    /// config.json — then the downloader default — in charge.
    pub workers: i64,
    /// Whether to use parallel Range downloads; `None` reads
    /// `parallel_download` from config.json.
    pub parallel: Option<bool>,
    /// Heal `installed_packages` from the on-disk `apps/` layout at open.
    ///
    /// It costs a stat per installed package plus a small table scan, so
    /// commands that never read installed state (`search`, `bucket`,
    /// `config`, `doctor`, `cache gc` …) turn it off. Commands that do read
    /// it must leave it on: their output would otherwise miss packages
    /// removed outside glue.
    pub sync_installed: bool,
}

impl EngineConfig {
    /// A config rooted at `root_dir` with default download settings.
    pub fn new(root_dir: impl AsRef<Path>) -> EngineConfig {
        EngineConfig {
            root_dir: root_dir.as_ref().to_path_buf(),
            workers: 0,
            parallel: None,
            sync_installed: true,
        }
    }
}

/// Shared runtime state for package operations.
///
/// The aggregate of the subsystems the engine operates on.
///
/// The bucket registry and the search index sit behind locks even though the
/// rest of the state does not: find/resolve lazily reloads them when a bucket
/// appears on disk that the in-memory view has not seen yet (see
/// [`crate::catalog::refresh_bucket_index`]). Making that interior-mutable is
/// what lets resolve run from `&Services`, so several installs can overlap:
/// one package's download no longer has to finish before the next one can even
/// be resolved.
pub struct Services {
    config: EngineConfig,
    store: Store,
    cache: CacheIndex,
    downloader: Downloader,
    /// In-process ZIP + external 7z extractor.
    extractor: Extractor,
    registry: RwLock<Registry>,
    search_index: RwLock<SearchIndex>,
    /// Serializes on-demand bucket checkout. Resolving two packages at once
    /// can want the same missing bucket cloned; one clone must be enough, and
    /// two writers into `buckets/<name>` would corrupt it.
    bucket_setup: Mutex<()>,
}

impl Services {
    /// Build the engine over `config.root_dir`, opening the store, cache index
    /// and bucket registry and performing the initial search-index rebuild.
    pub fn new(config: EngineConfig) -> Result<Services, ServicesError> {
        let root = config.root_dir.clone();

        let store = Store::new(root.join("store"))?;
        // Deliberately NOT `store.prereqs()`: it eagerly creates all 256 shard
        // directories on *every* command (measured 167 ms here — 256
        // create_dir_all calls). `Store::write` creates the shard lazily before
        // the rename and memoizes it in `Store::shard_dirs`, so nothing depends
        // on the eager layout and repeated writes to one shard cost nothing.
        // calls Prereqs once at install time; here even that is unnecessary.

        let cache = CacheIndex::new(&root)?;
        // Healing the installed table is a per-package stat plus a scan of the
        // package rows. Read-only commands that never look at installed state
        // switch it off (`EngineConfig::sync_installed`); the ones that do
        // keep it, so external deletions cannot leave stale rows behind.
        if config.sync_installed {
            cache.sync_installed_from_packages(&root)?;
            cache.prune_uninstalled_packages(&root)?;
        }

        let mut registry = Registry::new(&root)?;
        registry.reload_from_disk()?;

        let proxies = config::load_proxies(&root);
        let mut downloader = Downloader::new(store.clone());
        // Download settings come from config.json unless the caller pinned
        // them: `glue config set parallel_download|download_workers` then
        // applies to the CLI and the desktop alike, instead of each front-end
        // patching the downloader after construction.
        let workers = if config.workers > 0 {
            Some(config.workers)
        } else {
            config::read_config_download_workers(&root).ok().flatten()
        };
        if let Some(workers) = workers.filter(|n| *n > 0) {
            downloader.set_workers(workers as u32);
        }
        let parallel = config
            .parallel
            .unwrap_or_else(|| config::read_config_parallel_download(&root).unwrap_or(true));
        downloader.set_github_proxies(&proxies);
        downloader.set_parallel_download(parallel);
        if let Some(mb) = config::read_config_cancel_direct_max_mb(&root)
            .ok()
            .flatten()
        {
            let mb = mb.clamp(0, config::MAX_CANCEL_DIRECT_MAX_MB as i64) as u64;
            downloader.set_cancel_direct_max_bytes(mb * 1024 * 1024);
        }
        // Environment, the Windows system proxy, or the `proxy` config key;
        // `config.json` wins over both (see `proxy::resolve`). With
        // `proxy_packages` set, only those packages' hosts use it.
        downloader.set_proxy(crate::proxy::resolve(&root));
        downloader
            .set_proxy_packages(config::read_config_proxy_packages(&root).unwrap_or_default());

        // The extractor probes for 7z.exe at construction.
        let mut extractor = Extractor::new(Some(store.clone()));
        // `-mmt` for the external 7-Zip and the parallel in-process ZIP
        // decompression both default to 4 threads; use the machine's cores.
        extractor.set_workers(
            std::thread::available_parallelism()
                .map(std::num::NonZeroUsize::get)
                .unwrap_or(4),
        );
        // `SetFullSevenZipFromLocal`: when PATH has no
        // 7z, register the full-codec 7-Zip already installed under the glue root
        // (apps/7zip, then bin) so 7z-family installs (#/dl.7z, .7z, SFX, MSI) work
        // offline instead of failing with `7z.exe not found in PATH`. The bootstrap
        // download layer for brand-new machines is still a documented gap.
        if let Some(p) = crate::extractor::resolve_full_local_7z(&config.root_dir) {
            extractor.set_7z_path(p.to_string_lossy().into_owned());
        }

        // Record the bucket roots but defer the (expensive) manifest scan until
        // the first query that needs the catalog (list/search/resolve). Commands
        // that never search — install, uninstall, info, doctor — pay no parsing
        // cost. rebuilds eagerly on a background goroutine; here we build
        // lazily on first access instead.
        //
        // The scan is additionally backed by `catalog_entries` in the cache
        // index: a bucket whose manifest fingerprint is unchanged is read back
        // from SQLite instead of re-parsing thousands of JSON files, so the
        // cost is paid once per bucket update rather than once per process.
        let mut search_index = SearchIndex::new();
        search_index.set_entry_cache(std::sync::Arc::new(cache.catalog_cache()));
        let roots: Vec<(PathBuf, String)> = registry
            .list()
            .iter()
            .map(|b| (b.root.clone(), b.name.clone()))
            .collect();
        search_index.set_roots(roots);

        Ok(Services {
            config,
            store,
            cache,
            downloader,
            extractor,
            registry: RwLock::new(registry),
            search_index: RwLock::new(search_index),
            bucket_setup: Mutex::new(()),
        })
    }

    /// The configured data root.
    pub fn root_dir(&self) -> &Path {
        &self.config.root_dir
    }

    /// The `<root>/buckets` directory.
    pub fn buckets_dir(&self) -> PathBuf {
        self.config.root_dir.join("buckets")
    }

    /// The content-addressed store.
    pub fn store(&self) -> &Store {
        &self.store
    }

    /// The SQLite cache index.
    pub fn cache(&self) -> &CacheIndex {
        &self.cache
    }

    /// The downloader.
    pub fn downloader(&self) -> &Downloader {
        &self.downloader
    }

    /// The archive extractor (in-process ZIP + external 7z).
    pub fn extractor(&self) -> &Extractor {
        &self.extractor
    }

    /// The engine configuration.
    pub fn config(&self) -> &EngineConfig {
        &self.config
    }

    /// The bucket registry.
    ///
    /// Returns a read guard, so the caller must not hold it across a call that
    /// lazily reloads the registry (resolve paths) — take the guard per query.
    pub fn registry(&self) -> RwLockReadGuard<'_, Registry> {
        self.registry
            .read()
            .unwrap_or_else(|poisoned| poisoned.into_inner())
    }

    /// Mutable access to the bucket registry (for refresh/reload paths).
    pub fn registry_mut(&self) -> RwLockWriteGuard<'_, Registry> {
        self.registry
            .write()
            .unwrap_or_else(|poisoned| poisoned.into_inner())
    }

    /// The in-memory search index.
    pub fn search_index(&self) -> RwLockReadGuard<'_, SearchIndex> {
        self.search_index
            .read()
            .unwrap_or_else(|poisoned| poisoned.into_inner())
    }

    /// Mutable access to the search index (for realignment after bucket changes).
    pub fn search_index_mut(&self) -> RwLockWriteGuard<'_, SearchIndex> {
        self.search_index
            .write()
            .unwrap_or_else(|poisoned| poisoned.into_inner())
    }

    /// Mutable access to the registry and search index at once.
    ///
    /// Needed by the resolve paths, which reload the registry and realign the
    /// index in the same call. Always acquires registry first, then the index:
    /// no other path takes the two together, so the order cannot deadlock.
    pub fn registry_and_index_mut(
        &self,
    ) -> (
        RwLockWriteGuard<'_, Registry>,
        RwLockWriteGuard<'_, SearchIndex>,
    ) {
        let registry = self.registry_mut();
        let index = self.search_index_mut();
        (registry, index)
    }

    /// Serializes on-demand bucket checkout for this engine.
    pub fn bucket_setup(&self) -> MutexGuard<'_, ()> {
        self.bucket_setup
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner())
    }

    /// Reload buckets from disk and rebuild the search index from scratch.
    ///
    /// Takes `&self` (both views are locked): callers that only need the
    /// refresh no longer need a mutable engine, which is what lets one
    /// install's resolve run while another's fetch is in flight.
    pub fn reload(&self) -> Result<(), ServicesError> {
        let buckets: Vec<crate::bucket::Bucket> = {
            let mut registry = self.registry_mut();
            registry.reload_from_disk()?;
            registry.list().into_iter().cloned().collect()
        };
        self.search_index_mut().rebuild(&buckets);
        Ok(())
    }

    /// Reload GitHub mirror settings from config and push them to the downloader.
    pub fn reload_github_proxies(&mut self) -> Result<(), ServicesError> {
        let proxies = config::load_proxies(&self.config.root_dir);
        self.downloader.set_github_proxies(&proxies);
        Ok(())
    }

    /// Re-resolve the HTTP proxy (config → environment → system) and apply it.
    ///
    /// Used by the desktop settings dialog so a change takes effect without a
    /// restart.
    pub fn reload_proxy(&mut self) {
        self.downloader
            .set_proxy(crate::proxy::resolve(&self.config.root_dir));
        self.downloader.set_proxy_packages(
            config::read_config_proxy_packages(&self.config.root_dir).unwrap_or_default(),
        );
    }

    /// Apply a route chosen for this run (direct or proxy) plus the package
    /// allowlist. Used by the install-time speed probe.
    pub fn set_proxy_route(&mut self, proxy: crate::proxy::ProxySettings, packages: Vec<String>) {
        self.downloader.set_proxy(proxy);
        self.downloader.set_proxy_packages(packages);
    }

    /// Apply a download-worker count at runtime (config key
    /// `download_workers`, normalized to 2–16 in steps of 2 by the
    /// downloader). Lets the desktop settings dialog apply a change without a
    /// restart.
    pub fn set_download_workers(&mut self, workers: u32) {
        self.downloader.set_workers(workers);
    }

    /// Apply the cancellable direct-stream ceiling at runtime (config key
    /// `cancel_direct_max_mb`; `None` restores the built-in default). Lets the
    /// desktop settings dialog apply a change without a restart.
    pub fn set_cancel_direct_max_mb(&mut self, mb: Option<i64>) {
        let bytes = match mb {
            Some(mb) => mb.clamp(0, config::MAX_CANCEL_DIRECT_MAX_MB as i64) as u64 * 1024 * 1024,
            None => crate::downloader::CANCEL_DIRECT_MAX_BYTES,
        };
        self.downloader.set_cancel_direct_max_bytes(bytes);
    }

    /// The downloader's current effective worker count.
    pub fn download_workers(&self) -> u32 {
        self.downloader.workers()
    }

    /// Number of packages with an active install under `apps/`.
    pub fn installed_package_count(&self) -> usize {
        let apps_dir = self.config.root_dir.join("apps");
        let entries = match std::fs::read_dir(&apps_dir) {
            Ok(e) => e,
            Err(_) => return 0,
        };
        entries
            .flatten()
            .filter(|entry| {
                let pkg_root = entry.path();
                std::fs::metadata(&pkg_root)
                    .map(|m| m.is_dir())
                    .unwrap_or(false)
                    && crate::apps::active_install_dir(&pkg_root).is_some()
            })
            .count()
    }

    /// Build a `Services` rooted at `root` around an already-opened `store`.
    ///
    /// Test-only: skips the cache/registry reload that [`Services::new`] does,
    /// so unit tests can wire the install pipeline without a full data root.
    #[cfg(test)]
    pub(crate) fn for_test(root_dir: PathBuf, store: Store) -> Services {
        let config = EngineConfig::new(&root_dir);
        let cache = CacheIndex::new(&root_dir).expect("test cache index");
        let downloader = Downloader::new(store.clone());
        let extractor = Extractor::new(Some(store.clone()));
        let registry = Registry::new(&root_dir).expect("test bucket registry");
        Services {
            config,
            store,
            cache,
            downloader,
            extractor,
            registry: RwLock::new(registry),
            search_index: RwLock::new(SearchIndex::new()),
            bucket_setup: Mutex::new(()),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::testutil::TempDir;

    fn write_manifest(dir: &Path, name: &str) {
        std::fs::create_dir_all(dir).unwrap();
        let body = r#"{"version":"1.0.0","description":"test","url":"https://example.com/x.zip","hash":"abc"}"#;
        std::fs::write(dir.join(format!("{name}.json")), body).unwrap();
    }

    #[test]
    fn services_bootstraps_and_indexes_buckets() {
        let root = TempDir::new("glue-engine-services");
        write_manifest(&root.join("buckets").join("main").join("bucket"), "git");
        write_manifest(
            &root.join("buckets").join("extras").join("bucket"),
            "zotero",
        );

        let services = Services::new(EngineConfig::new(root.path())).unwrap();
        assert!(services.search_index().has_loaded_bucket("main"));
        assert!(services.search_index().has_loaded_bucket("extras"));
        assert_eq!(services.search_index().find_exact_name("git").len(), 1);
        assert_eq!(services.installed_package_count(), 0);
    }

    #[test]
    fn services_reload_picks_up_new_buckets() {
        let root = TempDir::new("glue-engine-reload");
        write_manifest(&root.join("buckets").join("main").join("bucket"), "git");

        let services = Services::new(EngineConfig::new(root.path())).unwrap();
        assert!(!services.search_index().has_loaded_bucket("extras"));

        write_manifest(
            &root.join("buckets").join("extras").join("bucket"),
            "zotero",
        );
        services.reload().unwrap();
        assert!(services.search_index().has_loaded_bucket("extras"));
        assert_eq!(services.search_index().find_exact_name("zotero").len(), 1);
    }
}
