//! Scoop-style bucket management: well-known bucket registry and local bucket
//! discovery under `<root>/buckets`.
//!
//! Covers the well-known bucket registry plus local bucket discovery. Git
//! clone/pull bootstrap lives in [`crate::git`].

use std::collections::BTreeMap;
use std::path::{Path, PathBuf};

use thiserror::Error;

use crate::manifest::{self, Manifest};

#[derive(Debug, Error)]
pub enum BucketError {
    #[error("create buckets directory: {0}")]
    CreateDir(#[source] std::io::Error),
    #[error("bucket not found: {0}")]
    NotFound(String),
    #[error("parse manifest: {0}")]
    Parse(#[from] manifest::ManifestError),
    #[error("manifest not found: {0} (searched in: {1})")]
    ManifestNotFound(String, String),
    #[error("read buckets dir: {0}")]
    ReadDir(#[source] std::io::Error),
}

pub type Result<T> = std::result::Result<T, BucketError>;

/// A Scoop bucket (git repository of manifests).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Bucket {
    pub name: String,
    pub repo_url: String,
    /// Local directory path.
    pub root: PathBuf,
    /// Whether updates are available.
    pub updated: bool,
}

/// Well-known Scoop buckets (name → repo URL).
pub fn known_buckets() -> BTreeMap<&'static str, &'static str> {
    BTreeMap::from([
        ("main", "https://github.com/ScoopInstaller/Main"),
        ("extras", "https://github.com/ScoopInstaller/Extras"),
        ("versions", "https://github.com/ScoopInstaller/Versions"),
        ("nightlies", "https://github.com/ScoopInstaller/Nightlies"),
        ("nirsoft", "https://github.com/kodybrown/scoop-nirsoft"),
        (
            "sysinternals",
            "https://github.com/asheroto/scoop-sysinternals",
        ),
        ("java", "https://github.com/ScoopInstaller/Java"),
        ("php", "https://github.com/ScoopInstaller/PHP"),
        ("games", "https://github.com/Calinou/scoop-games"),
    ])
}

/// Whether `name` is a known bucket.
pub fn is_known_bucket(name: &str) -> bool {
    known_buckets().contains_key(name)
}

/// Repository URL for a known bucket.
pub fn known_bucket_url(name: &str) -> Option<&'static str> {
    known_buckets().get(name).copied()
}

/// Short descriptions for well-known buckets.
pub fn known_bucket_descriptions() -> BTreeMap<&'static str, &'static str> {
    BTreeMap::from([
        ("main", "Scoop official packages"),
        ("extras", "Popular third-party apps not in main"),
        ("versions", "Multiple versions / non-latest stable releases"),
        ("nightlies", "Nightly builds and preview releases"),
        ("nirsoft", "NirSoft utilities collection"),
        ("sysinternals", "Microsoft Sysinternals tools"),
        ("java", "Java runtimes and development tools"),
        ("php", "PHP and related tools"),
        ("games", "Games"),
    ])
}

/// Description for a known bucket name ("" when unknown).
pub fn known_bucket_description(name: &str) -> &'static str {
    known_bucket_descriptions().get(name).copied().unwrap_or("")
}

/// Manifest file paths for a package, in search priority order
pub fn bucket_manifest_candidate_paths(
    bucket_root: &Path,
    bucket_name: &str,
    pkg_name: &str,
) -> Vec<PathBuf> {
    let file = format!("{pkg_name}.json");
    vec![
        bucket_root.join(&file),                    // flat
        bucket_root.join("bucket").join(&file),     // Scoop standard
        bucket_root.join(bucket_name).join(&file),  // alternative
        bucket_root.join("deprecated").join(&file), // archived (e.g. lemon bucket)
    ]
}

/// Local bucket registry: disk-scanning lookup without git operations.
pub struct Registry {
    buckets_dir: PathBuf,
    buckets: BTreeMap<String, Bucket>,
}

impl Registry {
    /// Create a registry over `<root_dir>/buckets`, creating the directory.
    pub fn new(root_dir: &Path) -> Result<Registry> {
        let buckets_dir = root_dir.join("buckets");
        std::fs::create_dir_all(&buckets_dir).map_err(BucketError::CreateDir)?;
        Ok(Registry {
            buckets_dir,
            buckets: BTreeMap::new(),
        })
    }

    /// The `buckets` directory this registry manages.
    pub fn buckets_dir(&self) -> &Path {
        &self.buckets_dir
    }

    /// Reload buckets from the filesystem
    ///
    /// Repo URLs are not resolved here (needs the git runner port); directories
    /// are registered for manifest lookup either way.
    pub fn reload_from_disk(&mut self) -> Result<()> {
        let entries = match std::fs::read_dir(&self.buckets_dir) {
            Ok(e) => e,
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => {
                self.buckets.clear();
                return Ok(());
            }
            Err(e) => return Err(BucketError::ReadDir(e)),
        };
        let mut buckets = BTreeMap::new();
        for entry in entries.flatten() {
            let name = entry.file_name().to_string_lossy().into_owned();
            let bucket_dir = entry.path();
            let is_dir = std::fs::metadata(&bucket_dir)
                .map(|m| m.is_dir())
                .unwrap_or(false);
            if !is_dir {
                continue;
            }
            buckets.insert(
                name.clone(),
                Bucket {
                    name,
                    repo_url: String::new(),
                    root: bucket_dir,
                    updated: false,
                },
            );
        }
        self.buckets = buckets;
        Ok(())
    }

    /// Look up a bucket by name.
    pub fn get(&self, name: &str) -> Result<&Bucket> {
        self.buckets
            .get(name)
            .ok_or_else(|| BucketError::NotFound(name.to_string()))
    }

    /// All buckets, ordered by name.
    pub fn list(&self) -> Vec<&Bucket> {
        self.buckets.values().collect()
    }

    /// Locate a package manifest across buckets. `pkg_ref` may be
    /// `bucket/name[@version]`, `name[@version]`, defaulting the bucket to
    /// `main`. Returns `(bucket_name, manifest_path, manifest)`.
    pub fn find_manifest(&self, pkg_ref: &str) -> Result<(String, PathBuf, Manifest)> {
        let mut pkg_ref = pkg_ref;
        if let Some(at) = pkg_ref.rfind('@') {
            pkg_ref = &pkg_ref[..at];
        }
        let (bucket_name, pkg_name) = match pkg_ref.split_once('/') {
            Some((b, p)) => (b.to_string(), p.to_string()),
            None => ("main".to_string(), pkg_ref.to_string()),
        };
        let bucket = self.get(&bucket_name)?;
        for path in bucket_manifest_candidate_paths(&bucket.root, &bucket_name, &pkg_name) {
            if path.is_file() {
                let m = manifest::parse_file(&path)?;
                return Ok((bucket_name, path, m));
            }
        }
        Err(BucketError::ManifestNotFound(
            pkg_ref.to_string(),
            bucket.root.display().to_string(),
        ))
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::testutil::TempDir;

    #[test]
    fn known_bucket_registry() {
        assert!(is_known_bucket("main"));
        assert!(is_known_bucket("extras"));
        assert!(!is_known_bucket("bogus"));
        assert_eq!(
            known_bucket_url("main"),
            Some("https://github.com/ScoopInstaller/Main")
        );
        assert_eq!(known_bucket_url("bogus"), None);
        assert_eq!(known_bucket_description("games"), "Games");
        assert_eq!(known_bucket_description("bogus"), "");
        assert_eq!(known_buckets().len(), 9);
        assert_eq!(known_bucket_descriptions().len(), 9);
    }

    #[test]
    fn candidate_paths_priority() {
        let root = Path::new("C:/buckets/main");
        let paths = bucket_manifest_candidate_paths(root, "main", "git");
        assert_eq!(paths[0], root.join("git.json"));
        assert_eq!(paths[1], root.join("bucket").join("git.json"));
        assert_eq!(paths[2], root.join("main").join("git.json"));
        assert_eq!(paths[3], root.join("deprecated").join("git.json"));
    }

    #[test]
    fn registry_reload_and_find_manifest() {
        let root = TempDir::new("glue-bucket-test");
        let scoop_std = root.join("buckets").join("main").join("bucket");
        std::fs::create_dir_all(&scoop_std).unwrap();
        std::fs::write(
            scoop_std.join("tiny.json"),
            r#"{"version":"1.0","url":"https://example.com/t.zip","hash":"abcd"}"#,
        )
        .unwrap();
        // A stray file that is not a directory must be skipped.
        std::fs::write(root.join("buckets").join("README.txt"), "x").unwrap();

        let mut reg = Registry::new(&root).unwrap();
        reg.reload_from_disk().unwrap();
        assert_eq!(reg.list().len(), 1);
        assert_eq!(reg.list()[0].name, "main");

        let (bucket_name, path, m) = reg.find_manifest("tiny").unwrap();
        assert_eq!(bucket_name, "main");
        assert!(path.ends_with("tiny.json"));
        let _ = m;

        // @version suffix is stripped before lookup.
        assert!(reg.find_manifest("tiny@1.0").is_ok());
        // Unknown bucket and unknown package errors.
        assert!(matches!(
            reg.find_manifest("extras/x"),
            Err(BucketError::NotFound(_))
        ));
        assert!(matches!(
            reg.find_manifest("nope"),
            Err(BucketError::ManifestNotFound(_, _))
        ));
    }

    #[test]
    fn reload_missing_dir_is_empty() {
        let root = TempDir::new("glue-bucket-test-missing");
        let mut reg = Registry::new(&root).unwrap();
        reg.reload_from_disk().unwrap();
        assert!(reg.list().is_empty());
    }
}
