//! Stable, inspectable errors for core operations.
//!
//! Callers test a category (a sentinel kind) instead of matching variants
//! directly:
//!
//! - one `AppError` enum whose variants carry the error's data,
//! - a `kind()` accessor returning the `AppErrorKind` sentinel,
//! - error strings kept stable so CLI output does not change between builds.

use std::fmt;

/// Sentinel categories for [`AppError`].
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum AppErrorKind {
    ManifestNotFound,
    ManifestAmbiguous,
    BucketNotInstalled,
    PackageNotInstalled,
}

/// Errors that callers may need to inspect rather than merely display.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum AppError {
    /// No manifest matches a package reference.
    ManifestNotFound { name: String, searched: String },
    /// Multiple buckets provide the same package name.
    ManifestAmbiguous { name: String, matches: Vec<String> },
    /// A required bucket is missing locally.
    BucketNotInstalled { name: String },
    /// An operation targets an uninstalled package.
    PackageNotInstalled { name: String, version: String },
}

impl AppError {
    /// The category this error belongs to, for category-level tests.
    pub fn kind(&self) -> AppErrorKind {
        match self {
            AppError::ManifestNotFound { .. } => AppErrorKind::ManifestNotFound,
            AppError::ManifestAmbiguous { .. } => AppErrorKind::ManifestAmbiguous,
            AppError::BucketNotInstalled { .. } => AppErrorKind::BucketNotInstalled,
            AppError::PackageNotInstalled { .. } => AppErrorKind::PackageNotInstalled,
        }
    }

    /// Constructor for the common `ManifestNotFound` (no `searched` suffix).
    pub fn manifest_not_found(name: impl Into<String>) -> AppError {
        AppError::ManifestNotFound {
            name: name.into(),
            searched: String::new(),
        }
    }
}

impl fmt::Display for AppError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            AppError::ManifestNotFound { name, searched } => {
                if searched.is_empty() {
                    write!(f, "manifest not found: {name}")
                } else {
                    write!(f, "manifest not found: {name} (searched in: {searched})")
                }
            }
            AppError::ManifestAmbiguous { name, matches } => write!(
                f,
                "manifest not found: \"{name}\" is ambiguous (found in: {})",
                matches.join(", ")
            ),
            AppError::BucketNotInstalled { name } => write!(
                f,
                "bucket \"{name}\" is not installed; run: glue bucket add {name} <repo-url>"
            ),
            AppError::PackageNotInstalled { name, version } => {
                if version.is_empty() {
                    write!(
                        f,
                        "package \"{name}\" is not installed (check with glue list)"
                    )
                } else {
                    write!(f, "{name}@{version} is not installed")
                }
            }
        }
    }
}

impl std::error::Error for AppError {}

/// A manifest-not-found error augmented with install hints.
///
/// Wraps the cause plus hints and formats them with a `find manifest:` prefix
/// and a `Did you mean:` block.
#[derive(Debug)]
pub struct ManifestSuggest {
    pub cause: Box<dyn std::error::Error + Send + Sync>,
    pub hints: Vec<String>,
}

impl ManifestSuggest {
    pub fn new(cause: Box<dyn std::error::Error + Send + Sync>, hints: Vec<String>) -> Self {
        ManifestSuggest { cause, hints }
    }
}

impl fmt::Display for ManifestSuggest {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "find manifest: {}", self.cause)?;
        if self.hints.is_empty() {
            return Ok(());
        }
        write!(f, "\n\nDid you mean:")?;
        // Each hint is written as "\n  %s\n"; trailing newlines are trimmed.
        let mut body = String::new();
        for hint in &self.hints {
            body.push_str("\n  ");
            body.push_str(hint);
            body.push('\n');
        }
        f.write_str(body.trim_end_matches('\n'))
    }
}

impl std::error::Error for ManifestSuggest {
    fn source(&self) -> Option<&(dyn std::error::Error + 'static)> {
        Some(&*self.cause)
    }
}

/// Reports whether an error is a user-facing resolve/manifest notice.
///
/// True for a `ManifestSuggest` wrapper, or for an error whose chain contains
/// `ManifestNotFound` / `ManifestAmbiguous`.
/// Downcast an error chain to an `AppError`.
pub fn is_app_error<'a>(err: &'a (dyn std::error::Error + 'static)) -> Option<&'a AppError> {
    // Walk the source chain looking for an AppError.
    let mut current: Option<&'a (dyn std::error::Error + 'static)> = Some(err);
    while let Some(e) = current {
        if let Some(app) = e.downcast_ref::<AppError>() {
            return Some(app);
        }
        current = e.source();
    }
    None
}

/// Reports whether an error is a resolve notice (see `is_app_error`).
///
/// True when the chain contains a `ManifestSuggest` wrapper, or a
/// `ManifestNotFound` / `ManifestAmbiguous`.
pub fn is_resolve_notice(err: &(dyn std::error::Error + 'static)) -> bool {
    let mut current: Option<&(dyn std::error::Error + 'static)> = Some(err);
    while let Some(e) = current {
        if e.downcast_ref::<ManifestSuggest>().is_some() {
            return true;
        }
        if let Some(app) = e.downcast_ref::<AppError>() {
            if matches!(
                app.kind(),
                AppErrorKind::ManifestNotFound | AppErrorKind::ManifestAmbiguous
            ) {
                return true;
            }
        }
        current = e.source();
    }
    false
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn manifest_not_found_display() {
        let e = AppError::manifest_not_found("foo");
        assert_eq!(e.to_string(), "manifest not found: foo");
        let e = AppError::ManifestNotFound {
            name: "lemon/notepad2".to_string(),
            searched: r"C:\Users\test\.glue\buckets\lemon".to_string(),
        };
        assert_eq!(
            e.to_string(),
            r"manifest not found: lemon/notepad2 (searched in: C:\Users\test\.glue\buckets\lemon)"
        );
        assert_eq!(e.kind(), AppErrorKind::ManifestNotFound);
    }

    #[test]
    fn manifest_ambiguous_display() {
        let e = AppError::ManifestAmbiguous {
            name: "foo".to_string(),
            matches: vec!["main/foo".to_string(), "extras/foo".to_string()],
        };
        assert_eq!(
            e.to_string(),
            "manifest not found: \"foo\" is ambiguous (found in: main/foo, extras/foo)"
        );
        assert_eq!(e.kind(), AppErrorKind::ManifestAmbiguous);
    }

    #[test]
    fn bucket_not_installed_display() {
        let e = AppError::BucketNotInstalled {
            name: "extras".to_string(),
        };
        assert_eq!(
            e.to_string(),
            "bucket \"extras\" is not installed; run: glue bucket add extras <repo-url>"
        );
    }

    #[test]
    fn package_not_installed_display() {
        assert_eq!(
            AppError::PackageNotInstalled {
                name: "git".to_string(),
                version: "2.40".to_string(),
            }
            .to_string(),
            "git@2.40 is not installed"
        );
        assert_eq!(
            AppError::PackageNotInstalled {
                name: "git".to_string(),
                version: String::new(),
            }
            .to_string(),
            "package \"git\" is not installed (check with glue list)"
        );
    }

    #[test]
    fn manifest_suggest_display_and_unwrap() {
        let inner = Box::new(AppError::manifest_not_found("x"));
        let s = ManifestSuggest::new(inner, vec!["glue install extras/x".to_string()]);
        assert_eq!(
            s.to_string(),
            "find manifest: manifest not found: x\n\nDid you mean:\n  glue install extras/x"
        );
        let no_hints =
            ManifestSuggest::new(Box::new(AppError::manifest_not_found("y")), Vec::new());
        assert_eq!(no_hints.to_string(), "find manifest: manifest not found: y");
    }

    #[test]
    fn resolve_notice_detection() {
        assert!(is_resolve_notice(&AppError::manifest_not_found("foo")));
        assert!(is_resolve_notice(&AppError::ManifestAmbiguous {
            name: "foo".to_string(),
            matches: vec!["main/foo".to_string(), "extras/foo".to_string()],
        }));
        assert!(is_resolve_notice(&ManifestSuggest::new(
            Box::new(AppError::manifest_not_found("foo")),
            vec!["glue install extras/foo".to_string()]
        )));
        let dl = std::io::Error::other("dependency git: download failed");
        assert!(!is_resolve_notice(&dl));
    }
}
