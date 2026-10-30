//! Catalog facade over [`Services`].
//!
//! Thin wrappers that pull the collaborators out of the engine state and hand
//! them to the free functions in [`crate::catalog`] — the same delegation
//! `engine/internal/catalog` performs against `*runtime.Services`.

use crate::apperr::AppError;
use crate::catalog as core_catalog;
use crate::engine::services::{Services, ServicesError};

// Re-export the catalog data types so callers only need `engine::catalog`.
pub use crate::catalog::{
    CatalogBucketSummary, CatalogBucketsQuery, CatalogPackagePage, CatalogPackageQuery,
    CatalogResolveRequest, Package,
};

/// Installed buckets with indexed package counts.
pub fn catalog_buckets(
    services: &Services,
    query: &CatalogBucketsQuery,
) -> Vec<CatalogBucketSummary> {
    let index = services.search_index();
    let registry = services.registry();
    core_catalog::catalog_buckets(Some(&index), &registry, query, services.root_dir())
}

/// Paginated packages from the search index.
pub fn list_catalog_packages(
    services: &Services,
    query: &CatalogPackageQuery,
) -> CatalogPackagePage {
    let index = services.search_index();
    core_catalog::list_catalog_packages(Some(&index), query, services.root_dir())
}

/// Resolves package names for template cards.
pub fn resolve_catalog_packages(
    services: &Services,
    requests: &[CatalogResolveRequest],
) -> Vec<Package> {
    let index = services.search_index();
    core_catalog::resolve_catalog_packages(Some(&index), requests)
}

/// Searches the catalog index, optionally limited to `limit` results.
pub fn search_packages(
    services: &Services,
    query: &str,
    buckets: &[String],
    limit: usize,
) -> Vec<Package> {
    let index = services.search_index();
    core_catalog::search_packages(Some(&index), query, buckets, limit, services.root_dir())
}

/// Hides a package from catalog browse/search results.
pub fn hide_catalog_package(services: &Services, pkg_ref: &str) -> Result<(), ServicesError> {
    core_catalog::hide_catalog_package(services.root_dir(), pkg_ref).map_err(ServicesError::Config)
}

/// Total indexed packages across all buckets.
pub fn count_available_packages(services: &Services, hide_deprecated: bool) -> usize {
    let index = services.search_index();
    core_catalog::count_available_packages(Some(&index), hide_deprecated, services.root_dir())
}

/// Indexed package counts per installed bucket.
pub fn package_counts_by_bucket(services: &Services) -> std::collections::BTreeMap<String, usize> {
    let index = services.search_index();
    core_catalog::package_counts_by_bucket(Some(&index), services.root_dir())
}

/// Ensures the bucket referenced by `pkg_ref` exists on disk, cloning a known
/// bucket on demand.
///
/// Ports the remote half of `EnsureBucketForInstall`: on a fresh install
/// `glue install vim` adds the `main` bucket (`install extras/foo` adds
/// `extras`) before resolving instead of failing with "bucket not installed".
/// Unknown bucket names are left to the resolver's typed error.
fn ensure_bucket_checkout(services: &Services, pkg_ref: &str) -> Result<(), ServicesError> {
    let bucket_name = core_catalog::package_bucket_name(pkg_ref);
    let buckets_dir = services.buckets_dir();
    let bucket_dir = buckets_dir.join(&bucket_name);
    if core_catalog::bucket_dir_has_manifests(&bucket_dir) {
        return Ok(());
    }
    let Some(url) = crate::bucket::known_bucket_url(&bucket_name) else {
        return Ok(());
    };
    // One checkout at a time per engine: two concurrent resolves must not
    // clone the same bucket into the same directory twice.
    let _setup = services.bucket_setup();
    // Another resolver may have won the race while we waited for the setup
    // lock.
    if core_catalog::bucket_dir_has_manifests(&bucket_dir) {
        return Ok(());
    }
    if !bucket_dir.exists() {
        crate::bootstrap::ensure_git(services, None).map_err(|err| {
            ServicesError::Resolve(Box::new(std::io::Error::other(format!(
                "git unavailable, cannot add bucket '{bucket_name}': {err}"
            ))))
        })?;
        crate::git::clone(services.root_dir(), url, &bucket_dir).map_err(|err| {
            ServicesError::Resolve(Box::new(std::io::Error::other(format!(
                "clone bucket '{bucket_name}': {err}"
            ))))
        })?;
    }
    let (mut registry, mut index) = services.registry_and_index_mut();
    let _ = registry.reload_from_disk();
    let buckets: Vec<crate::bucket::Bucket> = registry.list().into_iter().cloned().collect();
    index.sync(&buckets, false);
    Ok(())
}

/// Resolves an unqualified package name to `bucket/pkg`, realigning the search
/// index when a bucket has to be discovered on disk.
///
/// Takes `&Services`: the registry and index guards are taken internally, so
/// resolve can run concurrently with other installs (each may lazily reload
/// the registry when it discovers a new bucket on disk).
pub fn resolve_install_ref(services: &Services, pkg_ref: &str) -> Result<String, ServicesError> {
    ensure_bucket_checkout(services, pkg_ref)?;
    let buckets_dir = services.buckets_dir();
    let (mut registry, mut index) = services.registry_and_index_mut();
    core_catalog::resolve_install_ref(&mut registry, Some(&mut index), &buckets_dir, pkg_ref)
        .map_err(ServicesError::Resolve)
}

/// Resolves `pkg_ref` and ensures its manifest exists before depends/install.
///
/// Returns the resolved ref (`extras/zotero` for a bare `zotero`), which the
/// install pipeline needs: `Registry::find_manifest` defaults a bare name to
/// `main`, so looking the manifest up again with the typed ref would miss every
/// package that lives in another bucket.
pub fn validate_install_target(
    services: &Services,
    name: &str,
    buckets: &[String],
) -> Result<String, ServicesError> {
    ensure_bucket_checkout(services, name)?;
    let buckets_dir = services.buckets_dir();
    let (mut registry, mut index) = services.registry_and_index_mut();
    core_catalog::validate_install_target(
        &mut registry,
        Some(&mut index),
        &buckets_dir,
        name,
        buckets,
    )
    .map_err(ServicesError::Resolve)
}

/// Whether any indexed manifest needs innounp.
pub fn catalog_needs_innounp(services: &Services) -> bool {
    let registry = services.registry();
    core_catalog::catalog_needs_innounp(&registry)
}

/// Formats a resolve/manifest error as a CLI Note block.
pub fn format_install_resolve_notice(err: &(dyn std::error::Error + 'static)) -> String {
    core_catalog::format_install_resolve_notice(err)
}

/// The typed app error inside `err`, when present.
pub fn app_error<'a>(err: &'a (dyn std::error::Error + 'static)) -> Option<&'a AppError> {
    crate::apperr::is_app_error(err)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::engine::services::EngineConfig;
    use crate::testutil::TempDir;
    use std::path::Path;

    fn write_manifest(dir: &Path, name: &str) {
        std::fs::create_dir_all(dir).unwrap();
        let body = r#"{"version":"1.0.0","description":"test","url":"https://example.com/x.zip","hash":"abc"}"#;
        std::fs::write(dir.join(format!("{name}.json")), body).unwrap();
    }

    fn services_with(root: &Path, buckets: &[(&str, &str)]) -> Services {
        for (bucket, pkg) in buckets {
            write_manifest(&root.join("buckets").join(bucket).join("bucket"), pkg);
        }
        Services::new(EngineConfig::new(root)).unwrap()
    }

    #[test]
    fn facade_lists_and_searches() {
        let root = TempDir::new("glue-engcat-list");
        let services = services_with(&root, &[("main", "git"), ("extras", "zotero")]);

        let page = list_catalog_packages(
            &services,
            &CatalogPackageQuery {
                page: 1,
                page_size: 30,
                ..Default::default()
            },
        );
        assert_eq!(page.total, 2);

        assert_eq!(search_packages(&services, "test", &[], 0).len(), 2);
        assert_eq!(count_available_packages(&services, false), 2);

        let summary = catalog_buckets(&services, &CatalogBucketsQuery::default());
        assert_eq!(summary.len(), 2);
    }

    #[test]
    fn facade_resolves_and_respects_allow_list() {
        let root = TempDir::new("glue-engcat-resolve");
        let services = services_with(&root, &[("main", "git"), ("extras", "zotero")]);

        assert_eq!(
            resolve_install_ref(&services, "zotero").unwrap(),
            "extras/zotero"
        );
        assert_eq!(resolve_install_ref(&services, "git").unwrap(), "git");

        assert!(validate_install_target(&services, "zotero", &["main".to_string()]).is_err());
        assert!(validate_install_target(&services, "git", &["main".to_string()]).is_ok());
        // The resolved ref is the whole point: install feeds it back into the
        // manifest lookup, where a bare name would only probe `main`.
        assert_eq!(
            validate_install_target(&services, "zotero", &[]).unwrap(),
            "extras/zotero"
        );
    }

    #[test]
    fn facade_hides_package_and_formats_notice() {
        let root = TempDir::new("glue-engcat-hide");
        let services = services_with(&root, &[("main", "git")]);

        hide_catalog_package(&services, "git").unwrap();
        let page = list_catalog_packages(&services, &CatalogPackageQuery::default());
        assert_eq!(page.total, 0);

        let err = AppError::manifest_not_found("x");
        let notice = format_install_resolve_notice(&err);
        assert!(notice.starts_with("Note: manifest not found: x"));
        assert!(app_error(&err).is_some());
    }
}
