//! SQLite-backed cache index at `~/.glue/cache/index.db` plus install-path
//! helpers.

pub mod catalog_cache;
pub mod fileid;
pub mod index;
pub mod inflight;
pub mod paths;
pub mod rebuild;
pub mod refs;
pub mod scan;
pub mod verify;
pub mod workers;

pub use catalog_cache::CatalogCache;
pub use index::{Index, IndexError, InstalledPackage, PackageEntry};
pub use paths::is_hidden_install_path;
pub use rebuild::RebuildPackageFunc;
pub use refs::{
    app_referenced_hashes, collect_referenced_hashes, purge_orphan_blobs,
    remove_unreferenced_blobs, RefsError,
};
pub use verify::{verify_store, CorruptBlob, StoreVerifyReport};
