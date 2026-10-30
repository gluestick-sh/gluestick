//! Resolve `pkg@version` against the bucket's own git history.
//!
//! Scoop's `install app@version` accepts any version the bucket has ever
//! published, not only the one in the checked-out manifest. Bucket manifests
//! are ordinary files in a git checkout, so the older ones are still there:
//! `git log -- bucket/app.json` lists the commits that touched the file, and
//! the blob at each commit is the manifest exactly as it was published.
//!
//! This route is deliberately **local and offline**: no network, no
//! `autoupdate` templating. Generating a manifest from `autoupdate`
//! (`Manifest.ForVersion`) is the other route — see [`crate::autoupdate`], which
//! the install pipeline falls back to when this search comes up empty or the
//! clone cannot be searched.
//!
//! # Why a shallow clone is called out separately
//!
//! [`crate::git::clone`] deepens nothing — it passes `--depth 1` so that adding
//! a bucket stays small and fast — which means a fresh bucket normally has *no*
//! older revisions. Reporting that as "version not found" would send the user
//! looking for a typo, so the lookup distinguishes "the history was never
//! fetched" ([`NoHistory::Shallow`]) from "the history is here and the version
//! is not in it" ([`LookupOutcome::NotFound`]). The install error then names
//! `glue bucket deepen` as the one-time fix.
//!
//! # Cost
//!
//! Two git processes, and the expensive half is the path-limited log walk: it
//! has to visit the commit ancestry to know which commits touched the file
//! (measured ~1.3 s on a 5 900-commit window on Windows, warm). Blob reads go
//! through one `cat-file --batch` at ~15 ms per manifest. A `--depth 1` bucket
//! answers in ~20 ms because there is nothing to walk — which is also why the
//! install pipeline checks the already-installed fast path *before* asking.

use std::path::Path;

use crate::git::{self, GitError};
use crate::manifest::Manifest;

/// Cap on the number of *path-touching* commits inspected.
///
/// The cap bounds the `cat-file --batch` work, not the log walk (git still
/// visits the ancestry to find those commits). A manifest that was revised
/// more than 200 times is pathological; stopping there keeps a pathological
/// case from turning into a minute-long command.
const MAX_REVISIONS: usize = 200;

/// Result of a history lookup.
#[derive(Debug)]
pub enum LookupOutcome {
    /// The manifest exactly as it was committed with that version.
    Found(Box<Manifest>),
    /// There is no local history to search.
    NoHistory(NoHistory),
    /// History was searched and the version is not in it.
    NotFound,
}

/// Why a bucket cannot answer a history lookup.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum NoHistory {
    /// No `.git` under the bucket directory (hand-copied or exported bundle).
    NotACheckout,
    /// The clone is shallow: `glue bucket add` clones with `--depth 1`.
    Shallow,
    /// No usable `git` executable.
    GitUnavailable,
}

/// Find `version` in the history of `manifest_path`.
///
/// `manifest_path` is the manifest the bucket currently resolves to
/// (`<root>/buckets/<bucket>/bucket/<pkg>.json`). The git pathspec is derived
/// from its path relative to the bucket root, so the flat, `bucket/` and
/// `deprecated/` layouts all work without special cases — whatever path the
/// resolver picked is the path whose history we ask for.
pub fn find_version(root: &Path, manifest_path: &Path, version: &str) -> LookupOutcome {
    if version.is_empty() {
        return LookupOutcome::NotFound;
    }
    let Some(bucket_dir) = manifest_path.parent().and_then(Path::parent) else {
        return LookupOutcome::NotFound;
    };
    let Ok(relative) = manifest_path.strip_prefix(bucket_dir) else {
        return LookupOutcome::NotFound;
    };
    if !bucket_dir.join(".git").exists() {
        return LookupOutcome::NoHistory(NoHistory::NotACheckout);
    }
    let relative = relative.to_string_lossy().replace('\\', "/");

    let commits = match git::log_path(root, bucket_dir, &relative, MAX_REVISIONS) {
        Ok(commits) => commits,
        Err(GitError::NotFound) => return LookupOutcome::NoHistory(NoHistory::GitUnavailable),
        // A broken or unreadable repository is reported as "not in history":
        // the install error below says the bucket cannot produce the version,
        // which is what the user needs to know; the git failure behind it is
        // not actionable on its own.
        Err(_) => return LookupOutcome::NotFound,
    };

    let specs: Vec<String> = commits
        .iter()
        .map(|commit| format!("{commit}:{relative}"))
        .collect();
    let blobs = match git::cat_file_batch(root, bucket_dir, &specs) {
        Ok(blobs) => blobs,
        Err(GitError::NotFound) => return LookupOutcome::NoHistory(NoHistory::GitUnavailable),
        Err(_) => return LookupOutcome::NotFound,
    };

    for blob in blobs.into_iter().flatten() {
        // Compare the raw `version` field first: a full parse rejects old
        // schema variants, and a manifest that fails to parse should not cost
        // us the chance to find a newer revision that does parse.
        if manifest_version(&blob).as_deref() != Some(version) {
            continue;
        }
        let text = String::from_utf8_lossy(&blob);
        if let Ok(manifest) = crate::manifest::parse_str(&text) {
            return LookupOutcome::Found(Box::new(manifest));
        }
    }

    if git::is_shallow(root, bucket_dir) {
        // One extra git process, only on the miss path: it is what turns a
        // confusing "not found" into the right instruction.
        return LookupOutcome::NoHistory(NoHistory::Shallow);
    }
    LookupOutcome::NotFound
}

/// The `version` field of a manifest blob, without a full parse.
fn manifest_version(blob: &[u8]) -> Option<String> {
    let value: serde_json::Value = serde_json::from_slice(blob).ok()?;
    value.get("version")?.as_str().map(str::to_string)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::testutil::TempDir;
    use std::fs;
    use std::process::Command;

    /// `git` is a runtime dependency, so a machine without it cannot exercise
    /// these tests. Returning quietly keeps the suite green there — the code
    /// under test degrades to [`NoHistory::GitUnavailable`], covered separately.
    fn git_available() -> bool {
        Command::new("git")
            .arg("--version")
            .output()
            .map(|out| out.status.success())
            .unwrap_or(false)
    }

    /// Run git with an identity, so the test never depends on the machine's
    /// global git config (and never touches it).
    fn run_git(dir: &Path, args: &[&str]) {
        let out = Command::new("git")
            .args([
                "-c",
                "user.name=glue-test",
                "-c",
                "user.email=glue-test@example.com",
                "-c",
                "commit.gpgsign=false",
            ])
            .args(args)
            .current_dir(dir)
            .output()
            .expect("run git");
        assert!(
            out.status.success(),
            "git {args:?} failed: {}",
            String::from_utf8_lossy(&out.stderr)
        );
    }

    fn write_manifest(bucket_dir: &Path, version: &str) {
        let dir = bucket_dir.join("bucket");
        fs::create_dir_all(&dir).expect("bucket dir");
        let body = format!(
            r#"{{"version":"{version}","url":"https://example.com/demo-{version}.zip","hash":"sha256:{version}"}}"#
        );
        fs::write(dir.join("demo.json"), body).expect("manifest");
    }

    fn commit(bucket_dir: &Path, version: &str, message: &str) {
        write_manifest(bucket_dir, version);
        run_git(bucket_dir, &["add", "."]);
        run_git(bucket_dir, &["commit", "-qm", message]);
    }

    #[test]
    fn a_plain_directory_has_no_history() {
        let dir = TempDir::new("glue-history-plain");
        let bucket_dir = dir.path().join("buckets").join("main");
        fs::create_dir_all(bucket_dir.join("bucket")).unwrap();
        let manifest_path = bucket_dir.join("bucket").join("demo.json");
        fs::write(&manifest_path, "{}").unwrap();

        assert!(matches!(
            find_version(dir.path(), &manifest_path, "1.0.0"),
            LookupOutcome::NoHistory(NoHistory::NotACheckout)
        ));
    }

    #[test]
    fn finds_the_manifest_as_it_was_committed() {
        if !git_available() {
            return;
        }
        let dir = TempDir::new("glue-history-found");
        let bucket_dir = dir.path().join("buckets").join("main");
        fs::create_dir_all(&bucket_dir).unwrap();
        run_git(&bucket_dir, &["init", "-q"]);
        commit(&bucket_dir, "1.0.0", "1.0.0");
        commit(&bucket_dir, "1.5.0", "1.5.0");
        commit(&bucket_dir, "2.0.0", "2.0.0");

        let manifest_path = bucket_dir.join("bucket").join("demo.json");
        match find_version(dir.path(), &manifest_path, "1.0.0") {
            LookupOutcome::Found(manifest) => {
                assert_eq!(manifest.version, "1.0.0");
                // The URL is the one published back then, not today's.
                let urls = manifest.get_urls_for_install("64bit");
                assert!(
                    urls.iter().any(|url| url.contains("demo-1.0.0")),
                    "urls: {urls:?}"
                );
            }
            other => panic!("expected Found, got {other:?}"),
        }
    }

    #[test]
    fn a_version_that_was_never_published_is_not_found() {
        if !git_available() {
            return;
        }
        let dir = TempDir::new("glue-history-missing");
        let bucket_dir = dir.path().join("buckets").join("main");
        fs::create_dir_all(&bucket_dir).unwrap();
        run_git(&bucket_dir, &["init", "-q"]);
        commit(&bucket_dir, "1.0.0", "1.0.0");

        let manifest_path = bucket_dir.join("bucket").join("demo.json");
        assert!(matches!(
            find_version(dir.path(), &manifest_path, "0.9.0"),
            LookupOutcome::NotFound
        ));
    }
}
