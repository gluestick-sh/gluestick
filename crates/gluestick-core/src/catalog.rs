//! Package-reference resolution and catalog browse/search queries.
//!
//! These functions take exactly the collaborators they need — a `&Registry`
//! and/or an `&Index`, plus the root dir when bucket-on-disk discovery is
//! required — rather than a shared aggregate. That keeps each function
//! testable in isolation and avoids threading a god-object through the
//! resolver.

use std::collections::BTreeSet;
use std::path::{Path, PathBuf};

use crate::apperr::{AppError, AppErrorKind, ManifestSuggest};
use crate::bucket::Registry;
use crate::config;
use crate::manifest::{self, Match};
use crate::search_index::{self, Index};

// ---------------------------------------------------------------------------
// Package reference helpers
// ---------------------------------------------------------------------------

/// Splits `pkg@version` or `bucket/pkg@version` into name and version.
///
/// The returned name is the *base* name (path
/// components stripped).
pub fn parse_pkg_ref(pkg_ref: &str) -> (String, String) {
    let mut ref_ = pkg_ref.trim();
    let mut version = String::new();
    if let Some(at) = ref_.rfind('@') {
        version = ref_[at + 1..].to_string();
        ref_ = &ref_[..at];
    }
    (package_base_name(ref_), version)
}

/// Returns the `bucket/package` path without the `@version` suffix.
pub fn manifest_lookup_ref(pkg_ref: &str) -> String {
    let mut ref_ = pkg_ref.trim();
    if let Some(at) = ref_.rfind('@') {
        ref_ = &ref_[..at];
    }
    if ref_.contains(['/', '\\']) {
        return ref_.to_string();
    }
    package_base_name(ref_)
}

/// Returns the app directory name from a package reference.
pub fn package_base_name(pkg_ref: &str) -> String {
    let mut ref_ = pkg_ref;
    if let Some(at) = ref_.rfind('@') {
        ref_ = &ref_[..at];
    }
    match ref_.rfind(['/', '\\']) {
        Some(i) => ref_[i + 1..].to_string(),
        None => ref_.to_string(),
    }
}

/// Returns the bucket from a package reference (default `"main"`).
pub fn package_bucket_name(pkg_ref: &str) -> String {
    let mut ref_ = pkg_ref;
    if let Some(at) = ref_.rfind('@') {
        ref_ = &ref_[..at];
    }
    match ref_.find(['/', '\\']) {
        Some(i) => ref_[..i].to_string(),
        None => "main".to_string(),
    }
}

// ---------------------------------------------------------------------------
// Bucket presence
// ---------------------------------------------------------------------------

/// Whether `bucket_dir` contains at least one manifest JSON file.
///
/// Checks the standard Scoop layout dirs only and never walks `.git` (full
/// bucket walks are slow).
pub fn bucket_dir_has_manifests(bucket_dir: &Path) -> bool {
    if bucket_dir.as_os_str().is_empty() {
        return false;
    }
    let is_dir = std::fs::metadata(bucket_dir)
        .map(|m| m.is_dir())
        .unwrap_or(false);
    if !is_dir {
        return false;
    }
    if dir_has_any_json_file(bucket_dir) {
        return true;
    }
    let bucket_name = bucket_dir
        .file_name()
        .map(|n| n.to_string_lossy().into_owned())
        .unwrap_or_default();
    for dir in [
        bucket_dir.join("bucket"),
        bucket_dir.join(&bucket_name),
        bucket_dir.join("deprecated"),
    ] {
        if manifest_tree_has_json(&dir) {
            return true;
        }
    }
    false
}

/// Whether a directory contains any `.json` files (top level only).
fn dir_has_any_json_file(dir: &Path) -> bool {
    let entries = match std::fs::read_dir(dir) {
        Ok(e) => e,
        Err(_) => return false,
    };
    for entry in entries.flatten() {
        let is_dir = std::fs::metadata(entry.path())
            .map(|m| m.is_dir())
            .unwrap_or(false);
        if is_dir {
            continue;
        }
        if entry
            .file_name()
            .to_string_lossy()
            .to_lowercase()
            .ends_with(".json")
        {
            return true;
        }
    }
    false
}

/// Walks a directory tree to find `.json` files, skipping `.git` directories.
fn manifest_tree_has_json(dir: &Path) -> bool {
    let is_dir = std::fs::metadata(dir).map(|m| m.is_dir()).unwrap_or(false);
    if !is_dir {
        return false;
    }
    let entries = match std::fs::read_dir(dir) {
        Ok(e) => e,
        Err(_) => return false,
    };
    // Scope: a manifest tree is at most a couple of levels deep; recurse fully
    // but bail as soon as any .json is found.
    for entry in entries.flatten() {
        let path = entry.path();
        let meta = match std::fs::metadata(&path) {
            Ok(m) => m,
            Err(_) => continue,
        };
        let name = entry.file_name().to_string_lossy().into_owned();
        if meta.is_dir() {
            if name == ".git" {
                continue;
            }
            if manifest_tree_has_json(&path) {
                return true;
            }
            continue;
        }
        if name.to_lowercase().ends_with(".json") {
            return true;
        }
    }
    false
}

/// Rejects a bucket that is absent from a non-empty allow list.
pub fn ensure_bucket_allowed(bucket_name: &str, buckets: &[String]) -> Result<(), String> {
    if buckets.is_empty() {
        return Ok(());
    }
    if buckets.iter().any(|b| b == bucket_name) {
        return Ok(());
    }
    Err(format!(
        "bucket \"{bucket_name}\" is not in allowed buckets list: {:?}",
        buckets
    ))
}

// ---------------------------------------------------------------------------
// Catalog match helpers
// ---------------------------------------------------------------------------

/// Builds the install ref for a catalog match.
pub fn install_ref_from_match(m: &Match) -> String {
    if m.bucket.is_empty() || m.bucket == "main" {
        m.name.clone()
    } else {
        format!("{}/{}", m.bucket, m.name)
    }
}

/// Chooses a single catalog match when multiple buckets contain the name.
///
/// Returns `None` when the choice is ambiguous (more than one non-main match).
pub fn pick_install_match(matches: &[Match]) -> Option<&Match> {
    if matches.len() == 1 {
        return Some(&matches[0]);
    }
    let non_main: Vec<&Match> = matches
        .iter()
        .filter(|m| !m.bucket.is_empty() && m.bucket != "main")
        .collect();
    if non_main.len() == 1 {
        return Some(non_main[0]);
    }
    None
}

/// Whether a package reference is unqualified (no bucket prefix).
///
/// Returns true when the reference contains no path separators and resolves to
/// the `main` bucket.
pub fn is_unqualified_ref(pkg_ref: &str) -> bool {
    let ref_ = pkg_ref.trim();
    if ref_.is_empty() {
        return false;
    }
    if ref_.contains(['/', '\\']) {
        return false;
    }
    package_bucket_name(ref_) == "main"
}

// ---------------------------------------------------------------------------
// Catalog queries (api.go / hidden.go)
// ---------------------------------------------------------------------------

/// Hidden catalog package keys from config (lowercased `bucket/name`).
fn hidden_catalog_packages(root_dir: &Path) -> BTreeSet<String> {
    if root_dir.as_os_str().is_empty() {
        return BTreeSet::new();
    }
    match config::read_config_hidden_catalog_packages(root_dir) {
        Ok(refs) if !refs.is_empty() => refs
            .into_iter()
            .map(|r| {
                crate::search_index::catalog_package_key(
                    &package_bucket_name(&r),
                    &package_base_name(&r),
                )
            })
            .collect(),
        _ => BTreeSet::new(),
    }
}

/// Hides a package from catalog browse/search results.
pub fn hide_catalog_package(root_dir: &Path, pkg_ref: &str) -> Result<(), config::ConfigError> {
    config::add_config_hidden_catalog_package(root_dir, pkg_ref)
}

/// One package row as presented by catalog queries
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct Package {
    pub name: String,
    pub version: String,
    pub description: String,
    pub bucket: String,
    pub homepage: String,
    pub deprecated: bool,
}

impl Package {
    fn from_entry(entry: &search_index::Entry) -> Package {
        Package {
            name: entry.name.clone(),
            version: entry.version.clone(),
            description: entry.description.clone(),
            bucket: entry.bucket.clone(),
            homepage: entry.homepage.clone(),
            deprecated: entry.browse_deprecated(),
        }
    }
}

/// A bucket with its indexed package count.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct CatalogBucketSummary {
    pub name: String,
    pub description: String,
    pub package_count: usize,
}

/// Filters bucket summaries for the software catalog.
#[derive(Debug, Clone, Default)]
pub struct CatalogBucketsQuery {
    pub hide_deprecated: bool,
}

/// Sort column for [`list_catalog_packages`].
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub enum CatalogSort {
    /// Package name (default), case-insensitive.
    #[default]
    Name,
    /// Manifest version, using the same comparison as update checks.
    Version,
    Bucket,
}

/// Lists packages from the search index with optional bucket filter.
#[derive(Debug, Clone, Default)]
pub struct CatalogPackageQuery {
    pub bucket: String,
    pub query: String,
    pub page: i64,
    pub page_size: i64,
    pub hide_deprecated: bool,
    /// Sort column applied before pagination (default name ascending).
    pub sort: CatalogSort,
    /// Sort descending when true.
    pub sort_desc: bool,
}

/// A paginated catalog listing result.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct CatalogPackagePage {
    pub items: Vec<Package>,
    pub total: usize,
    pub page: i64,
    pub page_size: i64,
}

/// The `page`/`page_size` normalization used by `list_catalog_packages`.
fn normalize_page(page: i64, page_size: i64) -> (i64, i64) {
    let page = if page < 1 { 1 } else { page };
    let page_size = if !(1..=200).contains(&page_size) {
        30
    } else {
        page_size
    };
    (page, page_size)
}

/// Installed buckets with indexed package counts.
///
/// Iterates the *registered* buckets (so a bucket
/// directory that is not yet registered is not listed) and reports each one's
/// visible package count. Without a search index the result is empty.
pub fn catalog_buckets(
    index: Option<&Index>,
    registry: &Registry,
    query: &CatalogBucketsQuery,
    root_dir: &Path,
) -> Vec<CatalogBucketSummary> {
    let Some(index) = index else {
        return Vec::new();
    };
    let hidden = hidden_catalog_packages(root_dir);
    let counts = index.count_by_bucket(query.hide_deprecated, &hidden);

    registry
        .list()
        .into_iter()
        .map(|b| CatalogBucketSummary {
            name: b.name.clone(),
            description: crate::bucket::known_bucket_description(&b.name).to_string(),
            package_count: counts.get(&b.name).copied().unwrap_or(0),
        })
        .collect()
}

/// Paginated packages from the search index.
///
/// Without an index the result is an empty
/// page with the normalized page fields.
pub fn list_catalog_packages(
    index: Option<&Index>,
    query: &CatalogPackageQuery,
    root_dir: &Path,
) -> CatalogPackagePage {
    let (page, page_size) = normalize_page(query.page, query.page_size);
    let Some(index) = index else {
        return CatalogPackagePage {
            items: Vec::new(),
            total: 0,
            page,
            page_size,
        };
    };
    let hidden = hidden_catalog_packages(root_dir);
    let mut matches =
        index.list_entries(&query.bucket, &query.query, query.hide_deprecated, &hidden);
    // Sort the *whole* match set before pagination so page order is correct.
    matches.sort_by(|a, b| {
        let ord = match query.sort {
            CatalogSort::Name => a.name.to_lowercase().cmp(&b.name.to_lowercase()),
            CatalogSort::Version => crate::engine::version::version_compare(&a.version, &b.version),
            CatalogSort::Bucket => a.bucket.cmp(&b.bucket),
        };
        if query.sort_desc {
            ord.reverse()
        } else {
            ord
        }
    });
    let total = matches.len();

    let page_size_usize = page_size as usize;
    let offset = ((page - 1) as usize * page_size_usize).min(total);
    let end = (offset + page_size_usize).min(total);
    let items = matches[offset..end]
        .iter()
        .map(Package::from_entry)
        .collect();

    CatalogPackagePage {
        items,
        total,
        page,
        page_size,
    }
}

/// Every catalog entry, matching nothing in particular: `bucket`, `name`,
/// `version`, but no sorting, paging or text matching.
///
/// [`list_catalog_packages`] cannot answer "the whole catalog": its page size
/// caps at 200. Callers that do their own matching — shell completion filters
/// by prefix and by `bucket/name` — need one flat list, so they use this.
/// Hidden catalog packages are still excluded, exactly as in search.
pub fn all_catalog_entries(index: Option<&Index>, root_dir: &Path) -> Vec<search_index::Entry> {
    let Some(index) = index else {
        return Vec::new();
    };
    let hidden = hidden_catalog_packages(root_dir);
    index.list_entries("", "", false, &hidden)
}

/// A package-name resolution request.
#[derive(Debug, Clone, Default)]
pub struct CatalogResolveRequest {
    pub name: String,
    pub bucket: String,
}

/// Resolves package names for template cards.
pub fn resolve_catalog_packages(
    index: Option<&Index>,
    requests: &[CatalogResolveRequest],
) -> Vec<Package> {
    let Some(index) = index else {
        return Vec::new();
    };
    if requests.is_empty() {
        return Vec::new();
    }
    let mut out = Vec::with_capacity(requests.len());
    for req in requests {
        if req.name.is_empty() {
            continue;
        }
        if let Some(entry) = index.resolve_package(&req.name, &req.bucket) {
            out.push(Package::from_entry(&entry));
        }
    }
    out
}

/// Searches the catalog index, optionally limited to `limit` results.
pub fn search_packages(
    index: Option<&Index>,
    query: &str,
    buckets: &[String],
    limit: usize,
    root_dir: &Path,
) -> Vec<Package> {
    let Some(index) = index else {
        return Vec::new();
    };
    let hidden = hidden_catalog_packages(root_dir);
    let matches = index.search(query, buckets, &hidden);
    let mut packages = Vec::with_capacity(matches.len());
    for entry in &matches {
        packages.push(Package::from_entry(entry));
        if limit > 0 && packages.len() >= limit {
            return packages;
        }
    }
    packages
}

/// Indexed package counts per installed bucket.
pub fn package_counts_by_bucket(
    index: Option<&Index>,
    root_dir: &Path,
) -> std::collections::BTreeMap<String, usize> {
    let Some(index) = index else {
        return std::collections::BTreeMap::new();
    };
    let hidden = hidden_catalog_packages(root_dir);
    index.count_by_bucket(false, &hidden)
}

/// Total indexed packages across all buckets.
pub fn count_available_packages(
    index: Option<&Index>,
    hide_deprecated: bool,
    root_dir: &Path,
) -> usize {
    let Some(index) = index else {
        return 0;
    };
    let hidden = hidden_catalog_packages(root_dir);
    index.count_all(hide_deprecated, &hidden)
}

// ---------------------------------------------------------------------------
// innounp detection
// ---------------------------------------------------------------------------

/// Whether installer script hooks reference innounp.
pub fn installer_script_needs_innounp(hooks: &[String]) -> bool {
    let body = hooks.join("\n").to_lowercase();
    body.contains("expand-innoarchive") || body.contains("innounp")
}

/// Whether a manifest needs innounp.
pub fn manifest_may_need_innounp(m: &manifest::Manifest) -> bool {
    if m.innosetup {
        return true;
    }
    if installer_script_needs_innounp(&m.installer_script_hooks_for_install("")) {
        return true;
    }
    if installer_script_needs_innounp(&m.pre_install_hooks_for_install("")) {
        return true;
    }
    false
}

/// Whether any manifest under the registered buckets needs innounp.
pub fn catalog_needs_innounp(registry: &Registry) -> bool {
    catalog_has_matching_manifest(registry, manifest_may_need_innounp)
}

/// Whether any manifest in the catalog satisfies the predicate.
pub fn catalog_has_matching_manifest(
    registry: &Registry,
    predicate: fn(&manifest::Manifest) -> bool,
) -> bool {
    for b in registry.list() {
        for path in search_index::bucket_manifest_paths(&b.root, &b.name) {
            if let Ok(m) = manifest::parse_file(&path) {
                if predicate(&m) {
                    return true;
                }
            }
        }
    }
    false
}

// ---------------------------------------------------------------------------
// Install target resolution
// ---------------------------------------------------------------------------

/// Resolves an unqualified package name to `bucket/pkg` when needed.
///
/// `buckets_dir` is `<root>/buckets`; `index` may be `None` when no search
/// index is available (only qualified refs can then be resolved).
pub fn resolve_install_ref(
    registry: &mut Registry,
    mut index: Option<&mut Index>,
    buckets_dir: &Path,
    pkg_ref: &str,
) -> Result<String, Box<dyn std::error::Error + Send + Sync>> {
    let ref_ = pkg_ref.trim().to_string();
    if ref_.is_empty() {
        return Err("empty package name".into());
    }

    let lookup_ref = manifest_lookup_ref(&ref_);
    ensure_bucket_for_install(
        registry,
        index.as_deref_mut(),
        buckets_dir,
        &lookup_ref,
        &[],
    )?;
    if registry.find_manifest(&lookup_ref).is_ok() {
        return Ok(ref_);
    }

    if !is_unqualified_ref(&ref_) {
        let err = registry.find_manifest(&lookup_ref).unwrap_err();
        // The bucket named in the ref does not hold it; if another bucket
        // does (`games/godot` vs `extras/godot`), say so.
        return Err(wrap_manifest_not_found(
            registry,
            index.as_deref(),
            &ref_,
            Box::new(err),
        ));
    }

    let pkg_name = package_base_name(&ref_);
    // Fast path: probe the bucket manifest files directly. Avoids the full
    // search-index build (~3s on a full catalog) for the common bare-name
    // install; see `find_exact_matches_on_disk`.
    let matches = resolve_unqualified_matches(registry, index.as_deref(), &pkg_name);
    if matches.is_empty() {
        return Err(Box::new(AppError::manifest_not_found(pkg_name)));
    }

    match pick_install_match(&matches) {
        None => {
            let refs: Vec<String> = matches.iter().map(install_ref_from_match).collect();
            Err(Box::new(AppError::ManifestAmbiguous {
                name: pkg_name,
                matches: refs,
            }))
        }
        Some(chosen) => {
            let resolved = install_ref_from_match(chosen);
            ensure_bucket_for_install(registry, index, buckets_dir, &resolved, &[])?;
            Ok(resolved)
        }
    }
}

/// Finds packages by exact name match in the search index.
pub fn find_exact_packages(index: Option<&Index>, pkg_name: &str) -> Vec<Match> {
    let Some(index) = index else {
        return Vec::new();
    };
    index
        .find_exact_name(pkg_name)
        .into_iter()
        .map(|entry| Match {
            name: entry.name,
            bucket: entry.bucket,
            description: entry.description,
            version: entry.version,
        })
        .collect()
}

/// Finds exact-name matches by probing bucket manifest files directly.
///
/// The search index holds the same information (file stem == package name,
/// parseable manifest), but building it parses every manifest in every bucket
/// (~3s on a full catalog). Probing the four candidate paths per registered
/// bucket costs a handful of `stat` calls, so the common `install <name>` path
/// resolves without paying for the full build.
///
/// Mirrors the index's selection rules exactly: a candidate that exists but
/// fails to parse is skipped (the index does the same), and matches come back
/// in the order the index would produce them (bucket asc, then candidate path
/// asc). Callers should fall back to [`find_exact_packages`] when this returns
/// empty, so stale-index scenarios keep their existing behavior.
///
/// Intentional divergence: bare names are resolved through the search index.
/// When that index is cold the bucket files are probed directly instead, which
/// preserves install latency and determinism (no partially built index) while
/// yielding the same matches.
fn find_exact_matches_on_disk(registry: &Registry, pkg_name: &str) -> Vec<Match> {
    let mut matches = Vec::new();
    for b in registry.list() {
        let mut hits: Vec<PathBuf> =
            crate::bucket::bucket_manifest_candidate_paths(&b.root, &b.name, pkg_name)
                .into_iter()
                .filter(|p| p.is_file())
                .collect();
        // Same ordering the index uses for same-name entries (sorted paths).
        hits.sort();
        for path in hits {
            // Parse-skip parity: a file the index could not read must not
            // become a phantom match here either.
            let Ok(m) = manifest::parse_file(&path) else {
                continue;
            };
            matches.push(Match {
                name: pkg_name.to_string(),
                bucket: b.name.clone(),
                description: m.description.clone(),
                version: m.version.clone(),
            });
        }
    }
    matches
}

/// Resolves an unqualified name against the buckets on disk, falling back to
/// the search index when the direct probe finds nothing.
///
/// The disk probe is the fast path (no index build); the index fallback keeps
/// parity for scenarios where the index knows a manifest the probe cannot see
/// (e.g. a stale index root), at the cost of the full build in the true-miss
/// case — which pays the index build it would have paid anyway.
fn resolve_unqualified_matches(
    registry: &Registry,
    index: Option<&Index>,
    pkg_name: &str,
) -> Vec<Match> {
    let disk = find_exact_matches_on_disk(registry, pkg_name);
    if disk.is_empty() {
        find_exact_packages(index, pkg_name)
    } else {
        disk
    }
}

/// Augments manifest-not-found errors with bucket suggestions.
///
/// Suggestions come from the same
/// disk-first probe as [`resolve_install_ref`], so error hints never pay for
/// the full search-index build either.
///
/// A bucket-qualified ref is looked up in that bucket only, so a package that
/// lives in a *different* bucket (`games/godot`, while `godot` is in `extras`)
/// would read as "not found anywhere". The base name is checked across the
/// other buckets too, so the error says where the package actually is.
pub fn wrap_manifest_not_found(
    registry: &Registry,
    index: Option<&Index>,
    pkg_ref: &str,
    err: Box<dyn std::error::Error + Send + Sync>,
) -> Box<dyn std::error::Error + Send + Sync> {
    let pkg_name = package_base_name(pkg_ref);
    let matches = resolve_unqualified_matches(registry, index, &pkg_name);
    let bucket = if is_unqualified_ref(pkg_ref) {
        None
    } else {
        Some(package_bucket_name(pkg_ref))
    };
    let hints: Vec<String> = matches
        .iter()
        .filter(|m| bucket.as_deref() != Some(m.bucket.as_str()))
        .map(|m| format!("glue install {}", install_ref_from_match(m)))
        .collect();
    if hints.is_empty() {
        return format!("find manifest: {err}").into();
    }
    Box::new(ManifestSuggest::new(err, hints))
}

/// Ensures the bucket referenced by `pkg_ref` is available.
///
/// Ports the local-disk half of `EnsureBucketForInstall`: when the bucket
/// directory already holds manifests, the registry is refreshed (and the search
/// index realigned) and lookup proceeds. The remote git-clone fallback needs the
/// git runner port and is deferred — a bucket that is known but absent from disk
/// reports `BucketNotInstalled`, which is the same user-visible condition.
///
/// `index` is realigned in place when supplied
/// `SyncSearchIndex(e, false)` call inside.
pub fn ensure_bucket_for_install(
    registry: &mut Registry,
    index: Option<&mut Index>,
    buckets_dir: &Path,
    pkg_ref: &str,
    buckets: &[String],
) -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
    let mut ref_ = pkg_ref;
    if let Some(at) = ref_.rfind('@') {
        ref_ = &ref_[..at];
    }
    let bucket_name = package_bucket_name(ref_);

    if let Err(msg) = ensure_bucket_allowed(&bucket_name, buckets) {
        return Err(msg.into());
    }

    let bucket_dir = buckets_dir.join(&bucket_name);
    if bucket_dir_has_manifests(&bucket_dir) {
        refresh_bucket_index(registry, index, &bucket_name);
        return Ok(());
    }

    if crate::bucket::known_bucket_url(&bucket_name).is_none() {
        return Err(Box::new(AppError::BucketNotInstalled { name: bucket_name }));
    }
    // A known bucket that is not on disk needs a git clone; that arrives with
    // the git runner port. Report the same error would eventually produce.
    Err(Box::new(AppError::BucketNotInstalled { name: bucket_name }))
}

/// Reloads the registry when a bucket dir is missing from it and realigns the
/// search index so the newly discovered bucket becomes searchable.
///
/// A bucket that is already loaded in the index
/// is left untouched; otherwise the registry is reloaded from disk and the index
/// is synced.
pub fn refresh_bucket_index(registry: &mut Registry, index: Option<&mut Index>, bucket_name: &str) {
    let already_indexed = registry.get(bucket_name).is_ok()
        && index
            .as_ref()
            .map(|i| i.has_loaded_bucket(bucket_name))
            .unwrap_or(false);
    if already_indexed {
        return;
    }
    if registry.get(bucket_name).is_err() {
        let _ = registry.reload_from_disk();
    }
    if let Some(index) = index {
        let buckets: Vec<crate::bucket::Bucket> = registry.list().into_iter().cloned().collect();
        index.sync(&buckets, false);
    }
}

/// Resolves `pkg_ref` and ensures its manifest exists.
///
/// Returns the resolved ref (`bucket/name` once a bare name has been matched
/// across buckets), because [`Registry::find_manifest`] defaults an unqualified
/// name to `main` — a caller that looks the manifest up again with the *typed*
/// ref would probe the wrong bucket.
pub fn validate_install_target(
    registry: &mut Registry,
    mut index: Option<&mut Index>,
    buckets_dir: &Path,
    name: &str,
    buckets: &[String],
) -> Result<String, Box<dyn std::error::Error + Send + Sync>> {
    let resolved = match resolve_install_ref(registry, index.as_deref_mut(), buckets_dir, name) {
        Ok(r) => r,
        Err(err) => {
            if is_unqualified_ref(name) {
                return Err(wrap_manifest_not_found(
                    registry,
                    index.as_deref(),
                    name,
                    err,
                ));
            }
            // A bucket-qualified miss already went through the wrapper above,
            // hints included.
            return Err(err);
        }
    };
    let lookup_ref = manifest_lookup_ref(&resolved);
    ensure_bucket_for_install(
        registry,
        index.as_deref_mut(),
        buckets_dir,
        &lookup_ref,
        buckets,
    )?;
    if let Err(err) = registry.find_manifest(&lookup_ref) {
        return Err(wrap_manifest_not_found(
            registry,
            index.as_deref(),
            name,
            Box::new(err),
        ));
    }
    Ok(resolved)
}

/// Reports whether an error is a user-facing resolve/manifest notice.
pub fn is_install_resolve_notice(err: &(dyn std::error::Error + 'static)) -> bool {
    crate::apperr::is_resolve_notice(err)
}

/// Formats a resolve/manifest error as a CLI Note block.
pub fn format_install_resolve_notice(err: &(dyn std::error::Error + 'static)) -> String {
    if err.to_string().is_empty() {
        return String::new();
    }
    let mut msg = err.to_string().trim().to_string();
    loop {
        let mut trimmed = msg.clone();
        if let Some(rest) = trimmed.strip_prefix("find manifest: ") {
            trimmed = rest.trim().to_string();
        }
        if trimmed.starts_with("resolve \"") {
            if let Some(idx) = trimmed.find("\": ") {
                trimmed = trimmed[idx + 3..].trim().to_string();
            }
        }
        if trimmed == msg {
            break;
        }
        msg = trimmed;
    }
    let mut lines: Vec<String> = msg.split('\n').map(String::from).collect();
    if let Some(first) = lines.first_mut() {
        *first = format!("Note: {first}");
    }
    lines.join("\n")
}

/// Whether `root_dir` is a usable catalog root (helper for callers).
pub fn is_valid_root(root_dir: &Path) -> bool {
    !root_dir.as_os_str().is_empty()
}

/// Error kind accessor re-exported for callers that only need the sentinel.
pub fn error_kind(err: &(dyn std::error::Error + 'static)) -> Option<AppErrorKind> {
    crate::apperr::is_app_error(err).map(AppError::kind)
}

/// Buckets directory under a root (`<root>/buckets`).
pub fn buckets_dir(root_dir: &Path) -> PathBuf {
    root_dir.join("buckets")
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::testutil::TempDir;

    /// Create `<dir>/<name>.json` with a minimal valid manifest.
    fn write_manifest(dir: &Path, name: &str) {
        std::fs::create_dir_all(dir).unwrap();
        let body = r#"{"version":"1.0.0","description":"test","url":"https://example.com/x.zip","hash":"abc"}"#;
        std::fs::write(dir.join(format!("{name}.json")), body).unwrap();
    }

    /// A bucket root carrying a manifest plus a minimal `.git` tree, so the
    /// registry treats it as a checkout.
    fn write_manifest_with_git(dir: &Path, name: &str) {
        let bucket_dir = dir.parent().unwrap();
        std::fs::create_dir_all(bucket_dir.join(".git").join("objects")).unwrap();
        std::fs::create_dir_all(bucket_dir.join(".git").join("refs")).unwrap();
        std::fs::write(
            bucket_dir.join(".git").join("HEAD"),
            "ref: refs/heads/main\n",
        )
        .unwrap();
        write_manifest(dir, name);
    }

    fn registry_with(root: &Path, buckets: &[(&str, &str)]) -> Registry {
        for (bucket, pkg) in buckets {
            let dir = root.join("buckets").join(bucket).join("bucket");
            write_manifest_with_git(&dir, pkg);
        }
        let mut reg = Registry::new(root).unwrap();
        reg.reload_from_disk().unwrap();
        reg
    }

    fn index_for(reg: &Registry) -> Index {
        let mut idx = Index::new();
        idx.rebuild(&reg.list().into_iter().cloned().collect::<Vec<_>>());
        idx
    }

    // --- reference helpers (runtime/refs.go) ---

    #[test]
    fn pkg_ref_helpers() {
        assert_eq!(parse_pkg_ref("git"), ("git".to_string(), String::new()));
        assert_eq!(
            parse_pkg_ref("extras/zotero@6.0"),
            ("zotero".to_string(), "6.0".to_string())
        );
        assert_eq!(
            parse_pkg_ref("  main/git@2.40  "),
            ("git".to_string(), "2.40".to_string())
        );

        assert_eq!(manifest_lookup_ref("git@1.0"), "git");
        assert_eq!(manifest_lookup_ref("extras/zotero@1"), "extras/zotero");

        assert_eq!(package_base_name("extras/zotero@1"), "zotero");
        assert_eq!(package_base_name("git"), "git");
        assert_eq!(package_bucket_name("extras/zotero"), "extras");
        assert_eq!(package_bucket_name("git"), "main");
        assert_eq!(package_bucket_name("git@1"), "main");
    }

    // --- match helpers (resolve.go) ---

    #[test]
    fn install_ref_from_match_parity() {
        assert_eq!(
            install_ref_from_match(&Match {
                name: "git".to_string(),
                bucket: "main".to_string(),
                ..Default::default()
            }),
            "git"
        );
        assert_eq!(
            install_ref_from_match(&Match {
                name: "zotero".to_string(),
                bucket: "extras".to_string(),
                ..Default::default()
            }),
            "extras/zotero"
        );
    }

    #[test]
    fn pick_install_match_parity() {
        let m = |name: &str, bucket: &str| Match {
            name: name.to_string(),
            bucket: bucket.to_string(),
            ..Default::default()
        };
        // main + extras -> extras wins.
        let matches = vec![m("foo", "main"), m("foo", "extras")];
        assert_eq!(pick_install_match(&matches).unwrap().bucket, "extras");
        // Two non-main buckets -> ambiguous.
        let ambiguous = vec![m("foo", "extras"), m("foo", "versions")];
        assert!(pick_install_match(&ambiguous).is_none());
        // Single match -> itself.
        assert_eq!(pick_install_match(&[m("bar", "main")]).unwrap().name, "bar");
        // Only main-bucket duplicates -> ambiguous (no non-main match).
        let mains = vec![m("foo", "main"), m("foo", "main")];
        assert!(pick_install_match(&mains).is_none());
    }

    #[test]
    fn is_unqualified_ref_parity() {
        assert!(is_unqualified_ref("git"));
        assert!(is_unqualified_ref("git@1.0"));
        assert!(!is_unqualified_ref("extras/zotero"));
        assert!(!is_unqualified_ref(""));
        assert!(!is_unqualified_ref("a\\b"));
    }

    // --- bucket presence (resolve.go) ---

    #[test]
    fn bucket_dir_has_manifests_skips_git_tree() {
        let root = TempDir::new("glue-cat-bdir");
        let bucket_dir = root.join("main");
        write_manifest(&bucket_dir.join("bucket"), "git");
        // 200 json files inside .git/objects must not influence the result.
        let git_objects = bucket_dir.join(".git").join("objects");
        std::fs::create_dir_all(&git_objects).unwrap();
        for i in 0..200 {
            std::fs::write(git_objects.join(format!("obj-{i:04}")), "x").unwrap();
        }
        assert!(bucket_dir_has_manifests(&bucket_dir));

        // A directory with no json anywhere is not a bucket.
        let empty = root.join("empty");
        std::fs::create_dir_all(&empty).unwrap();
        assert!(!bucket_dir_has_manifests(&empty));
    }

    #[test]
    fn ensure_bucket_allowed_parity() {
        assert!(ensure_bucket_allowed("main", &[]).is_ok());
        assert!(ensure_bucket_allowed("main", &["main".to_string()]).is_ok());
        let err = ensure_bucket_allowed("extras", &["main".to_string()]).unwrap_err();
        assert!(err.contains("extras"));
        assert!(err.contains("allowed buckets"));
    }

    // --- resolve (resolve.go) ---

    #[test]
    fn resolve_install_ref_missing_package_is_typed_not_found() {
        let root = TempDir::new("glue-cat-resolve-missing");
        // A registered `main` bucket with one unrelated package: the bucket is
        // present, so resolution must fail with ManifestNotFound (not bucket).
        let reg = registry_with(&root, &[("main", "git")]);
        let mut idx = index_for(&reg);
        let mut reg = reg;

        let err = resolve_install_ref(
            &mut reg,
            Some(&mut idx),
            &root.join("buckets"),
            "missing-package",
        )
        .unwrap_err();
        let app = crate::apperr::is_app_error(&*err).expect("typed app error");
        assert_eq!(app.kind(), AppErrorKind::ManifestNotFound);
    }

    #[test]
    fn resolve_install_ref_absent_bucket_is_bucket_not_installed() {
        let root = TempDir::new("glue-cat-resolve-absent");
        let mut reg = Registry::new(&root).unwrap();
        reg.reload_from_disk().unwrap();
        let mut idx = Index::new();
        // No buckets on disk at all: the missing `main` checkout is reported as
        // BucketNotInstalled.
        let err = resolve_install_ref(&mut reg, Some(&mut idx), &root.join("buckets"), "anything")
            .unwrap_err();
        let app = crate::apperr::is_app_error(&*err).expect("typed app error");
        assert_eq!(app.kind(), AppErrorKind::BucketNotInstalled);
    }

    #[test]
    fn resolve_install_ref_prefers_non_main_bucket() {
        let root = TempDir::new("glue-cat-resolve-nonmain");
        let reg = registry_with(&root, &[("main", "git"), ("extras", "zotero")]);
        let mut idx = index_for(&reg);
        let mut reg = reg;
        let dir = root.join("buckets");

        let got = resolve_install_ref(&mut reg, Some(&mut idx), &dir, "zotero").unwrap();
        assert_eq!(got, "extras/zotero");
        let got = resolve_install_ref(&mut reg, Some(&mut idx), &dir, "git").unwrap();
        assert_eq!(got, "git");
    }

    #[test]
    fn resolve_install_ref_disk_probe_resolves_without_index() {
        // The bucket-file fast path answers bare-name resolution on its own:
        // no search index is built (and none is required) to resolve `zotero`.
        // would need its (warm) search index here; see
        // `find_exact_matches_on_disk` for the divergence rationale.
        let root = TempDir::new("glue-cat-resolve-noidx");
        let reg = registry_with(&root, &[("main", "git"), ("extras", "zotero")]);
        let mut reg = reg;
        let dir = root.join("buckets");

        let got = resolve_install_ref(&mut reg, None, &dir, "zotero").unwrap();
        assert_eq!(got, "extras/zotero");
        let got = resolve_install_ref(&mut reg, None, &dir, "git").unwrap();
        assert_eq!(got, "git");
    }

    #[test]
    fn resolve_install_ref_disk_probe_skips_unparseable_manifest() {
        // A bucket file that exists but does not parse must not become a
        // phantom match: the search index skips it, and so must the fast path
        // (otherwise the non-main `extras` copy would win over the valid
        // `main` one).
        let root = TempDir::new("glue-cat-resolve-badjson");
        let reg = registry_with(&root, &[("main", "foo"), ("extras", "foo")]);
        std::fs::write(
            root.join("buckets")
                .join("extras")
                .join("bucket")
                .join("foo.json"),
            "{ not json",
        )
        .unwrap();
        let mut idx = index_for(&reg);
        let mut reg = reg;
        let dir = root.join("buckets");

        let got = resolve_install_ref(&mut reg, Some(&mut idx), &dir, "foo").unwrap();
        assert_eq!(got, "foo");
    }

    #[test]
    fn wrap_manifest_not_found_adds_hint() {
        let root = TempDir::new("glue-cat-wrap");
        let reg = registry_with(&root, &[("main", "git"), ("extras", "zotero")]);
        let idx = index_for(&reg);
        let err = wrap_manifest_not_found(
            &reg,
            Some(&idx),
            "zotero",
            Box::new(AppError::manifest_not_found("zotero")),
        );
        let msg = err.to_string();
        assert!(msg.contains("glue install extras/zotero"), "got: {msg}");
    }

    /// `games/godot` while `godot` lives in `extras`: a bucket-qualified miss
    /// must point at the bucket that actually holds the package.
    #[test]
    fn wrap_manifest_not_found_points_a_qualified_ref_at_the_right_bucket() {
        let root = TempDir::new("glue-cat-wrap-qualified");
        let reg = registry_with(
            &root,
            &[("main", "git"), ("extras", "godot"), ("games", "0ad")],
        );
        let idx = index_for(&reg);
        let err = wrap_manifest_not_found(
            &reg,
            Some(&idx),
            "games/godot",
            Box::new(AppError::manifest_not_found("games/godot")),
        );
        let msg = err.to_string();
        assert!(msg.contains("glue install extras/godot"), "got: {msg}");
    }

    #[test]
    fn wrap_manifest_not_found_hint_without_index() {
        // Hints come from the same disk probe, so they are produced without
        // building the search index.
        let root = TempDir::new("glue-cat-wrap-noidx");
        let reg = registry_with(&root, &[("main", "git"), ("extras", "zotero")]);
        let err = wrap_manifest_not_found(
            &reg,
            None,
            "zotero",
            Box::new(AppError::manifest_not_found("zotero")),
        );
        let msg = err.to_string();
        assert!(msg.contains("glue install extras/zotero"), "got: {msg}");
    }

    #[test]
    fn format_install_resolve_notice_strips_prefix() {
        let err = AppError::ManifestNotFound {
            name: "lemon/notepad2".to_string(),
            searched: r"C:/Users/test/.glue/buckets/lemon".to_string(),
        };
        let wrapped: Box<dyn std::error::Error> = format!("find manifest: {err}").into();
        let got = format_install_resolve_notice(&*wrapped);
        assert!(
            got.starts_with("Note: manifest not found: lemon/notepad2"),
            "got: {got}"
        );
    }

    #[test]
    fn is_install_resolve_notice_parity() {
        assert!(is_install_resolve_notice(&AppError::manifest_not_found(
            "foo"
        )));
        assert!(is_install_resolve_notice(&AppError::ManifestAmbiguous {
            name: "foo".to_string(),
            matches: vec!["main/foo".to_string(), "extras/foo".to_string()],
        }));
        let dl = std::io::Error::other("dependency git: download failed");
        assert!(!is_install_resolve_notice(&dl));
    }

    // --- ensure bucket (resolve.go, local-disk path) ---

    #[test]
    fn ensure_bucket_for_install_registers_disk_bucket() {
        let root = TempDir::new("glue-cat-ensure-disk");
        // Manifest present, registry NOT yet reloaded.
        write_manifest(&root.join("buckets").join("main").join("bucket"), "git");

        let mut reg = Registry::new(&root).unwrap();
        let mut idx = Index::new();
        let res =
            ensure_bucket_for_install(&mut reg, Some(&mut idx), &root.join("buckets"), "git", &[]);
        assert!(res.is_ok(), "{res:?}");
        assert!(reg.get("main").is_ok());
        // The index is realigned so the bucket becomes searchable
        assert!(idx.has_loaded_bucket("main"));
        assert_eq!(idx.find_exact_name("git").len(), 1);
    }

    #[test]
    fn ensure_bucket_for_install_applies_allow_list() {
        let root = TempDir::new("glue-cat-ensure-filter");
        let mut reg = registry_with(&root, &[("main", "git"), ("extras", "zotero")]);
        let dir = root.join("buckets");

        // Unrestricted passes for both.
        assert!(ensure_bucket_for_install(&mut reg, None, &dir, "git", &[]).is_ok());
        assert!(ensure_bucket_for_install(&mut reg, None, &dir, "zotero", &[]).is_ok());

        // Only main allowed -> extras refused.
        let only_main = vec!["main".to_string()];
        assert!(ensure_bucket_for_install(&mut reg, None, &dir, "git", &only_main).is_ok());
        assert!(
            ensure_bucket_for_install(&mut reg, None, &dir, "extras/zotero", &only_main).is_err()
        );

        // Only extras allowed -> main refused.
        let only_extras = vec!["extras".to_string()];
        assert!(
            ensure_bucket_for_install(&mut reg, None, &dir, "extras/zotero", &only_extras).is_ok()
        );
        assert!(ensure_bucket_for_install(&mut reg, None, &dir, "git", &only_extras).is_err());

        // Empty list allows all.
        assert!(ensure_bucket_for_install(&mut reg, None, &dir, "git", &[]).is_ok());
        assert!(ensure_bucket_for_install(&mut reg, None, &dir, "extras/zotero", &[]).is_ok());
    }

    #[test]
    fn ensure_bucket_for_install_missing_known_bucket_is_bucket_not_installed() {
        let root = TempDir::new("glue-cat-ensure-missing");
        let mut reg = Registry::new(&root).unwrap();
        let err =
            ensure_bucket_for_install(&mut reg, None, &root.join("buckets"), "extras/zotero", &[])
                .unwrap_err();
        let app = crate::apperr::is_app_error(&*err).expect("typed app error");
        assert_eq!(app.kind(), AppErrorKind::BucketNotInstalled);
    }

    #[test]
    fn ensure_bucket_for_install_unknown_bucket_is_bucket_not_installed() {
        let root = TempDir::new("glue-cat-ensure-unknown");
        let mut reg = Registry::new(&root).unwrap();
        let err =
            ensure_bucket_for_install(&mut reg, None, &root.join("buckets"), "bogus/pkg", &[])
                .unwrap_err();
        let app = crate::apperr::is_app_error(&*err).expect("typed app error");
        assert_eq!(app.kind(), AppErrorKind::BucketNotInstalled);
        assert!(err.to_string().contains("bogus"));
    }

    // --- catalog queries (api.go / hidden.go) ---

    #[test]
    fn list_catalog_packages_hides_main_bucket_package() {
        let root = TempDir::new("glue-cat-hidden");
        write_manifest(
            &root.join("buckets").join("main").join("deprecated"),
            "acorn",
        );
        config::add_config_hidden_catalog_package(&root, "acorn").unwrap();

        let mut reg = Registry::new(&root).unwrap();
        reg.reload_from_disk().unwrap();
        let idx = index_for(&reg);

        let page = list_catalog_packages(
            Some(&idx),
            &CatalogPackageQuery {
                bucket: "main".to_string(),
                query: "acorn".to_string(),
                page: 1,
                page_size: 30,
                ..Default::default()
            },
            &root,
        );
        assert!(
            page.items.is_empty(),
            "expected acorn hidden: {:?}",
            page.items
        );
    }

    #[test]
    fn all_catalog_entries_returns_every_package_and_still_hides() {
        let root = TempDir::new("glue-cat-all");
        let dir = root.join("buckets").join("main").join("bucket");
        for name in ["alpha", "beta"] {
            write_manifest(&dir, name);
        }
        config::add_config_hidden_catalog_package(&root, "beta").unwrap();

        let mut reg = Registry::new(&root).unwrap();
        reg.reload_from_disk().unwrap();
        let idx = index_for(&reg);

        // One flat pass, no paging cap and no text filter: shell completion
        // matches by prefix itself.
        let mut names: Vec<String> = all_catalog_entries(Some(&idx), &root)
            .into_iter()
            .map(|entry| format!("{}/{}", entry.bucket, entry.name))
            .collect();
        names.sort();
        assert_eq!(names, vec!["main/alpha".to_string()]);
    }

    #[test]
    fn list_catalog_packages_paginates_and_normalizes() {
        let root = TempDir::new("glue-cat-page");
        let dir = root.join("buckets").join("main").join("bucket");
        for name in ["a1", "a2", "a3", "a4", "a5"] {
            write_manifest(&dir, name);
        }
        let mut reg = Registry::new(&root).unwrap();
        reg.reload_from_disk().unwrap();
        let idx = index_for(&reg);

        // page_size out of range -> default 30; page < 1 -> 1.
        let page = list_catalog_packages(
            Some(&idx),
            &CatalogPackageQuery {
                page: 0,
                page_size: 0,
                ..Default::default()
            },
            &root,
        );
        assert_eq!(page.page, 1);
        assert_eq!(page.page_size, 30);
        assert_eq!(page.total, 5);
        assert_eq!(page.items.len(), 5);

        // A real page slice.
        let page = list_catalog_packages(
            Some(&idx),
            &CatalogPackageQuery {
                page: 2,
                page_size: 2,
                ..Default::default()
            },
            &root,
        );
        assert_eq!(page.page, 2);
        assert_eq!(page.page_size, 2);
        let names: Vec<&str> = page.items.iter().map(|p| p.name.as_str()).collect();
        assert_eq!(names, vec!["a3", "a4"]);

        // Page past the end -> empty slice, totals intact.
        let page = list_catalog_packages(
            Some(&idx),
            &CatalogPackageQuery {
                page: 99,
                page_size: 2,
                ..Default::default()
            },
            &root,
        );
        assert!(page.items.is_empty());
        assert_eq!(page.total, 5);
    }

    #[test]
    fn catalog_queries_without_index_are_empty() {
        let root = TempDir::new("glue-cat-noindex");
        let reg = Registry::new(&root).unwrap();
        assert!(catalog_buckets(None, &reg, &CatalogBucketsQuery::default(), &root).is_empty());
        let page = list_catalog_packages(None, &CatalogPackageQuery::default(), &root);
        assert!(page.items.is_empty());
        assert_eq!(page.page, 1);
        assert_eq!(page.page_size, 30);
        assert!(search_packages(None, "x", &[], 0, &root).is_empty());
        assert_eq!(count_available_packages(None, false, &root), 0);
    }

    #[test]
    fn catalog_buckets_counts_per_registered_bucket() {
        let root = TempDir::new("glue-cat-buckets");
        let reg = registry_with(&root, &[("main", "git"), ("extras", "zotero")]);
        let idx = index_for(&reg);

        let summary = catalog_buckets(Some(&idx), &reg, &CatalogBucketsQuery::default(), &root);
        let by_name: std::collections::BTreeMap<&str, usize> = summary
            .iter()
            .map(|s| (s.name.as_str(), s.package_count))
            .collect();
        assert_eq!(by_name.get("main"), Some(&1));
        assert_eq!(by_name.get("extras"), Some(&1));
        assert_eq!(
            summary
                .iter()
                .find(|s| s.name == "main")
                .unwrap()
                .description,
            "Scoop official packages"
        );
    }

    #[test]
    fn search_packages_respects_limit_and_buckets() {
        let root = TempDir::new("glue-cat-search");
        let reg = registry_with(&root, &[("main", "git"), ("extras", "zotero")]);
        let idx = index_for(&reg);

        // Both manifests carry description "test".
        assert_eq!(search_packages(Some(&idx), "test", &[], 0, &root).len(), 2);
        assert_eq!(search_packages(Some(&idx), "test", &[], 1, &root).len(), 1);
        let only_extras = vec!["extras".to_string()];
        let got = search_packages(Some(&idx), "test", &only_extras, 0, &root);
        assert_eq!(got.len(), 1);
        assert_eq!(got[0].bucket, "extras");
    }

    #[test]
    fn resolve_catalog_packages_picks_preferred_bucket() {
        let root = TempDir::new("glue-cat-resolvepkg");
        let reg = registry_with(&root, &[("main", "git"), ("extras", "zotero")]);
        let idx = index_for(&reg);

        let got = resolve_catalog_packages(
            Some(&idx),
            &[
                CatalogResolveRequest {
                    name: "git".to_string(),
                    bucket: String::new(),
                },
                CatalogResolveRequest {
                    name: String::new(),
                    bucket: String::new(),
                },
                CatalogResolveRequest {
                    name: "nope".to_string(),
                    bucket: String::new(),
                },
            ],
        );
        assert_eq!(got.len(), 1);
        assert_eq!(got[0].name, "git");
    }

    #[test]
    fn package_counts_and_totals() {
        let root = TempDir::new("glue-cat-counts");
        let reg = registry_with(&root, &[("main", "git"), ("extras", "zotero")]);
        let idx = index_for(&reg);
        let counts = package_counts_by_bucket(Some(&idx), &root);
        assert_eq!(counts.get("main"), Some(&1));
        assert_eq!(counts.get("extras"), Some(&1));
        assert_eq!(count_available_packages(Some(&idx), false, &root), 2);
    }

    // --- innounp detection (resolve.go) ---

    #[test]
    fn installer_script_needs_innounp_parity() {
        assert!(installer_script_needs_innounp(&[
            "Expand-InnoArchive -Path x".to_string()
        ]));
        assert!(installer_script_needs_innounp(&["innounp -x".to_string()]));
        assert!(!installer_script_needs_innounp(&[
            "Expand-Archive x".to_string()
        ]));
        assert!(!installer_script_needs_innounp(&[]));
    }

    #[test]
    fn catalog_needs_innounp_detects_manifest() {
        let root = TempDir::new("glue-cat-inno");
        let dir = root.join("buckets").join("main").join("bucket");
        std::fs::create_dir_all(&dir).unwrap();
        std::fs::write(
            dir.join("setup.json"),
            r#"{"version":"1","url":"https://e/s.exe","hash":"h","innosetup":true}"#,
        )
        .unwrap();
        let mut reg = Registry::new(&root).unwrap();
        reg.reload_from_disk().unwrap();
        assert!(catalog_needs_innounp(&reg));

        // A bucket with no inno manifests -> false.
        let root2 = TempDir::new("glue-cat-inno-none");
        let reg2 = registry_with(&root2, &[("main", "git")]);
        assert!(!catalog_needs_innounp(&reg2));
    }
}
