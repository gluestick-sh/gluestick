//! Scoop `persist` semantics: deciding whether an entry names a file or a
//! directory (`manifest/persist.go`, `PersistEntry.LooksLikeFile`).

use std::path::Path;

/// Known file extensions for Scoop-style file persist entries.
///
/// A dot in the basename is not enough: Tor Browser persists `profile.default`
/// as a *directory*, so an unknown extension falls through to "directory".
const FILE_EXTENSIONS: &[&str] = &[
    ".ini",
    ".xml",
    ".json",
    ".txt",
    ".conf",
    ".cfg",
    ".yml",
    ".yaml",
    ".toml",
    ".js",
    ".css",
    ".html",
    ".htm",
    ".log",
    ".dat",
    ".db",
    ".sqlite",
    ".sqlite3",
    ".bat",
    ".cmd",
    ".ps1",
    ".reg",
    ".properties",
    ".config",
    ".vnc",
];

/// Reports whether a persist path names a file.
pub fn looks_like_file_persist_path(install_name: &str) -> bool {
    let base = Path::new(install_name)
        .file_name()
        .map(|n| n.to_string_lossy().into_owned())
        .unwrap_or_default();
    if !base.contains('.') {
        return false;
    }
    // A bare dotfile (`.portable`, `.gitconfig`) has no *extension* — the
    // leading dot is part of the name, so `Path::extension` is `None`. Treat
    // it as a file: Windows Terminal ships `.portable`, and classifying it as
    // a directory made the installer script's `Add-Content "$dir\.portable"`
    // fail with ACCESS_DENIED on the directory we had just created.
    if base.starts_with('.') && !base.trim_start_matches('.').contains('.') {
        return true;
    }
    let ext = Path::new(&base)
        .extension()
        .map(|e| e.to_string_lossy().to_lowercase())
        .unwrap_or_default();
    FILE_EXTENSIONS
        .iter()
        .any(|known| known.trim_start_matches('.').eq_ignore_ascii_case(&ext))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn looks_like_file_persist_path_known_extensions() {
        for path in [
            "config.xml",
            "app.ini",
            "settings.json",
            "run.bat",
            "env.ps1",
            "note.TXT",
            "keys.properties",
            // Bare dotfiles: the leading dot is part of the name, not an
            // extension. Windows Terminal persists `.portable` this way.
            ".portable",
            ".gitconfig",
        ] {
            assert!(
                looks_like_file_persist_path(path),
                "{path} should be a file"
            );
        }
    }

    #[test]
    fn looks_like_file_persist_path_directories() {
        // No dot at all.
        assert!(!looks_like_file_persist_path("profile"));
        assert!(!looks_like_file_persist_path("data/cache"));
        // A dot in the basename but an unknown extension — a directory.
        assert!(!looks_like_file_persist_path("profile.default"));
        assert!(!looks_like_file_persist_path("node_modules.old"));
    }

    #[test]
    fn looks_like_file_persist_path_nested_paths() {
        assert!(looks_like_file_persist_path("a/b/c.ini"));
        assert!(!looks_like_file_persist_path("a/b/c"));
    }
}
