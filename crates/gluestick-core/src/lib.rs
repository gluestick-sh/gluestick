//! Gluestick embeddable engine — Windows package management core library.
//!
//! Scoop-compatible manifest parsing, path-safety validation, hashing, config,
//! CAS store, buckets, downloader, and the cache index live here.
//!
//! The `engine` layer is wired up: the catalog half (bucket discovery, the
//! in-memory search index, browse/search/resolve queries), the install
//! pipeline (resolve → fetch → deploy → finalize, via [`engine::Engine`]),
//! and the top-level operations (uninstall/update/search/doctor/list, bucket
//! and cache management). Remaining gaps are listed in the repository README.

pub mod apperr;
pub mod apps;
pub mod archmember;
pub(crate) mod atomic;
pub mod autoupdate;
pub mod bootstrap;
pub mod bucket;
pub mod bucket_history;
pub mod cache;
pub mod cancel;
pub mod catalog;
pub mod config;
pub mod disk;
pub mod downloader;
pub mod engine;
#[cfg(feature = "extract-profile")]
pub(crate) mod extract_profile;
#[cfg(not(feature = "extract-profile"))]
#[path = "extract_profile_stub.rs"]
pub(crate) mod extract_profile;
pub mod extractor;
pub mod ftp;
pub mod git;
pub mod hash;
pub mod lock;
pub mod manifest;
pub mod pathenv;
pub mod paths;
pub mod persist;
pub mod procutil;
pub mod proxy;
pub mod safepath;
mod schannel;
pub mod search_index;
pub mod self_update;
pub mod shim;
pub mod store;
pub mod tararchive;
pub mod templates;
#[cfg(test)]
pub(crate) mod testutil;
pub mod uninstall;
pub mod winpath;
pub mod winshell;
pub mod zip;
pub mod ziparchive;

pub use manifest::{Manifest, ManifestError};
