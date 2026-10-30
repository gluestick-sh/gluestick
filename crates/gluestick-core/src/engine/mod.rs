//! Engine layer: catalog browse/search/resolve + the install pipeline.
//!
//! The aggregate state lives in [`Services`], which also serves the catalog
//! queries and the install pipeline ([`Engine::install`]).
//!
//! # Scope
//!
//! Covers catalog browse/search/resolve and the install pipeline, including
//! uninstall, update and listing. Gaps inside the pipeline itself are
//! documented in [`install`] and surface as dedicated error variants.
//!
//! Sub-modules:
//!
//! - `progress` — `ProgressEvent` enum + `ProgressFn` callback type
//! - `version` — version comparison helpers
//! - `services` — [`Services`]: the shared engine state
//! - `catalog` — catalog browse/search/resolve facade
//! - `install` — the install pipeline
//! - `ops` — [`Engine`], the CLI-facing facade (`engine` package)

pub mod catalog;
pub mod install;
pub mod ops;
pub mod persist;
pub mod progress;
pub mod services;
pub mod timings;
pub mod version;

pub use catalog::{
    CatalogBucketSummary, CatalogBucketsQuery, CatalogPackagePage, CatalogPackageQuery,
    CatalogResolveRequest, Package,
};
pub use install::{install, InstallOptions, InstallOutcome};
pub use ops::{BucketSync, Engine, EngineError, LaunchTarget, PackageUpdate};
pub use progress::{noop, ProgressEvent, ProgressFn};
pub use services::{Services, ServicesError};
pub use timings::{parse_line as parse_timings_line, Bucket, PhaseTimings, Timings};
