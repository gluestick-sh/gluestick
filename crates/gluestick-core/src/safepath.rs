//! Path validation for manifest-relative and archive member paths.
//!

use std::path::{Component, Path, PathBuf};
use thiserror::Error;

/// Errors produced when a path escapes its intended base.
///
/// Callers match on the variant instead
/// of.
#[derive(Debug, Clone, PartialEq, Eq, Error)]
pub enum SafepathError {
    #[error("unsafe path: absolute path {0:?}")]
    Absolute(String),
    #[error("unsafe path: path traversal in {0:?}")]
    Traversal(String),
    #[error("unsafe path: empty segment in {0:?}")]
    EmptySegment(String),
    #[error("unsafe path: {0:?} escapes {1:?}")]
    Escapes(String, String),
}

pub type Result<T> = std::result::Result<T, SafepathError>;

/// Normalizes a Scoop manifest-relative path and rejects traversal.
///
/// Empty or `"."` paths are allowed and normalize to `""`.
/// Backslashes are converted to `/`.
pub fn validate_manifest_rel_path(p: &str) -> Result<String> {
    let p = p.trim().replace('\\', "/");
    if p.is_empty() || p == "." {
        return Ok(String::new());
    }
    // Absolute: rooted ("/..."), UNC ("//..."), or drive-qualified ("C:...").
    if p.starts_with('/') || (p.len() >= 2 && p.as_bytes()[1] == b':') {
        return Err(SafepathError::Absolute(p));
    }
    let trimmed = p.trim_matches('/');
    if trimmed.is_empty() {
        return Ok(String::new());
    }
    for seg in trimmed.split('/') {
        if seg == ".." {
            return Err(SafepathError::Traversal(trimmed.to_string()));
        }
        if seg.is_empty() {
            return Err(SafepathError::EmptySegment(trimmed.to_string()));
        }
    }
    Ok(trimmed.to_string())
}

/// Joins `rel` under `base` and ensures the result stays within `base`.
pub fn join_under_base(base: &Path, rel: &str) -> Result<PathBuf> {
    let safe = validate_manifest_rel_path(rel)?;
    if safe.is_empty() {
        return Ok(clean_path(base));
    }
    let base_clean = clean_path(base);
    let mut target = base_clean.clone();
    for seg in safe.split('/') {
        target.push(seg);
    }
    let target = clean_path(&target);
    if !is_under(&target, &base_clean) {
        return Err(SafepathError::Escapes(
            rel.to_string(),
            base_clean.display().to_string(),
        ));
    }
    Ok(target)
}

/// Lexical path cleaning (semantics, without touching disk).
fn clean_path(p: &Path) -> PathBuf {
    use Component::*;
    let mut comps: Vec<Component> = Vec::new();
    for c in p.components() {
        match c {
            CurDir => {}
            ParentDir => match comps.last() {
                Some(Normal(_)) => {
                    comps.pop();
                }
                // Clean: ".." at an absolute root is a no-op.
                Some(Prefix(_)) | Some(RootDir) => {}
                // Relative paths and leading ".." keep the element.
                _ => comps.push(ParentDir),
            },
            other => comps.push(other),
        }
    }
    if comps.is_empty() {
        return PathBuf::from(".");
    }
    let mut out = PathBuf::new();
    for c in &comps {
        match c {
            Prefix(prefix) => out.push(prefix.as_os_str()),
            RootDir => out.push("/"),
            Normal(seg) => out.push(seg),
            ParentDir => out.push(".."),
            CurDir => unreachable!("CurDir is filtered above"),
        }
    }
    out
}

/// Component-wise prefix check: is `target` inside (or equal to) `base`?
fn is_under(target: &Path, base: &Path) -> bool {
    target.starts_with(base)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::testutil::TempDir;

    #[test]
    fn validate_manifest_rel_path_cases() {
        let cases: &[(&str, &str)] = &[
            ("", ""),
            (".", ""),
            (r"IDE\bin", "IDE/bin"),
            ("a/", "a"),
            ("a/./b", "a/./b"),
        ];
        for (input, want) in cases {
            let got = validate_manifest_rel_path(input)
                .unwrap_or_else(|e| panic!("validate({input:?}) err = {e}"));
            assert_eq!(got, *want, "validate({input:?})");
        }

        for input in [
            r"../outside",
            r"foo/../../etc",
            "/abs",
            "C:/x",
            "a//b",
            r"\\server\s",
            "/",
        ] {
            let err = validate_manifest_rel_path(input)
                .expect_err(&format!("expected error for {input:?}"));
            assert!(
                matches!(
                    err,
                    SafepathError::Absolute(_)
                        | SafepathError::Traversal(_)
                        | SafepathError::EmptySegment(_)
                ),
                "unexpected variant for {input:?}: {err}"
            );
        }
    }

    #[test]
    fn join_under_base_cases() {
        let base = TempDir::new("glue-safepath-test");

        let target = join_under_base(&base, "app/bin/tool.exe").unwrap();
        assert_eq!(
            target,
            clean_path(&base).join("app").join("bin").join("tool.exe")
        );

        assert!(matches!(
            join_under_base(&base, "../escape"),
            Err(SafepathError::Traversal(_))
        ));

        // Empty rel resolves to the cleaned base itself.
        assert_eq!(join_under_base(&base, ".").unwrap(), clean_path(&base));
    }
}
