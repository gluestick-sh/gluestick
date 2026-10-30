//! Archive member path normalization — paths of members *inside* ZIP/7z
//! archives (not on-disk archive file paths).
//!
//! Archive member names written by Windows tools use backslashes, and this
//! crate targets the Windows layout (Scoop-compatible), so normalization to
//! forward slashes is unconditional.

use std::collections::BTreeMap;

/// NormalizeMember converts archive entry paths to forward-slash form for
/// maps and install layout.
pub fn normalize_member(name: &str) -> String {
    name.trim().replace('\\', "/")
}

/// Last path segment of a normalized (forward-slash) path. Empty for paths
/// ending in a separator.
fn base_of(normalized: &str) -> &str {
    match normalized.rfind('/') {
        Some(i) => &normalized[i + 1..],
        None => normalized,
    }
}

/// IsDirectoryPlaceholder reports .NET-style zero-byte folder markers
/// (e.g. "runtimes\\win-x64\\").
pub fn is_directory_placeholder(name: &str, size: u64) -> bool {
    let name = name.trim();
    let trimmed = name.trim_end_matches(['/', '\\']);
    if trimmed.is_empty() {
        return true;
    }
    if name.len() > trimmed.len() {
        return true;
    }
    if size == 0 {
        let normalized = normalize_member(trimmed);
        if !base_of(&normalized).contains('.') && name.contains(['/', '\\']) {
            return true;
        }
    }
    false
}

/// IsDirectoryPlaceholderName is for link-time filtering when entry size is
/// unknown.
pub fn is_directory_placeholder_name(name: &str) -> bool {
    let name = name.trim();
    let trimmed = name.trim_end_matches(['/', '\\']);
    if trimmed.is_empty() {
        return true;
    }
    name.len() > trimmed.len()
}

/// Depth returns the number of path segments (1 for "app.exe", 3 for
/// "a/b/c.exe").
pub fn depth(rel_path: &str) -> usize {
    let rel_path = normalize_member(rel_path);
    let rel_path = rel_path.trim_matches('/');
    if rel_path.is_empty() {
        return 0;
    }
    rel_path.matches('/').count() + 1
}

/// SingleRootPrefix reports whether every member shares exactly one top-level
/// directory (typical 7z/zip layout). The returned prefix includes a trailing
/// slash, e.g. "FreeCAD/".
pub fn single_root_prefix(members: &BTreeMap<String, String>) -> Option<String> {
    if members.is_empty() {
        return None;
    }
    let mut root = String::new();
    for rel_path in members.keys() {
        let rel = normalize_member(rel_path);
        let rel = rel.trim_matches('/');
        if rel.is_empty() || is_directory_placeholder_name(rel_path) {
            continue;
        }
        // A member directly at the root means there is no single shared prefix.
        let i = rel.find('/')?;
        let top = &rel[..i];
        if root.is_empty() {
            root = top.to_string();
        } else if top != root {
            return None;
        }
    }
    if root.is_empty() {
        return None;
    }
    Some(format!("{root}/"))
}

/// StripRootPrefix returns a copy of members with a shared top-level directory
/// removed. An empty prefix returns the members unchanged.
pub fn strip_root_prefix(
    members: &BTreeMap<String, String>,
    prefix: &str,
) -> BTreeMap<String, String> {
    let prefix = normalize_member(prefix);
    if prefix.is_empty() {
        return members.clone();
    }
    let mut out = BTreeMap::new();
    for (rel_path, hash) in members {
        let mut rel = normalize_member(rel_path);
        if let Some(after) = rel.strip_prefix(&prefix) {
            rel = after.to_string();
        }
        if rel.is_empty() || is_directory_placeholder_name(&rel) {
            continue;
        }
        out.insert(rel, hash.clone());
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    fn map(pairs: &[(&str, &str)]) -> BTreeMap<String, String> {
        pairs
            .iter()
            .map(|(k, v)| (k.to_string(), v.to_string()))
            .collect()
    }

    #[test]
    fn is_directory_placeholder_cases() {
        // Placeholder entries have a trailing separator and a zero size.
        assert!(is_directory_placeholder(r"runtimes\win-x64\", 0));
        assert!(is_directory_placeholder(r"Dependencies\luadec\", 0));
        assert!(!is_directory_placeholder("AssetStudioGUI.exe", 100));
        assert!(!is_directory_placeholder(
            r"Dependencies\luadec\lua51\luadec.exe",
            100
        ));
        // Name-only form ignores size: trailing separators always mark a dir.
        assert!(is_directory_placeholder("dir/", 4096));
        assert!(!is_directory_placeholder("dir", 4096));
        // Zero-byte, no extension, in a subdirectory → placeholder.
        assert!(is_directory_placeholder("bin/README", 0));
        assert!(!is_directory_placeholder("bin/readme.txt", 0));
        // A top-level extensionless file is a real file.
        assert!(!is_directory_placeholder("README", 0));
        // Whitespace is trimmed first.
        assert!(is_directory_placeholder("  dir/  ", 0));
    }

    #[test]
    fn normalize_member_converts_backslashes() {
        assert_eq!(
            normalize_member(r"Dependencies\luadec\lua51\luadec.exe"),
            "Dependencies/luadec/lua51/luadec.exe"
        );
        assert_eq!(normalize_member("  a/b  "), "a/b");
        assert_eq!(normalize_member(""), "");
    }

    #[test]
    fn is_directory_placeholder_name_cases() {
        assert!(is_directory_placeholder_name("dir/"));
        assert!(is_directory_placeholder_name(r"runtimes\win-x64\"));
        assert!(!is_directory_placeholder_name("app.exe"));
        assert!(is_directory_placeholder_name("   "));
    }

    #[test]
    fn depth_counts_segments() {
        assert_eq!(depth("app.exe"), 1);
        assert_eq!(depth("a/b/c.exe"), 3);
        assert_eq!(depth("/a/b/"), 2);
        assert_eq!(depth(""), 0);
        assert_eq!(depth("///"), 0);
        assert_eq!(depth(r"a\b\c"), 3);
    }

    #[test]
    fn single_root_prefix_and_strip() {
        let members = map(&[
            ("FreeCAD/bin/FreeCAD.exe", "h1"),
            ("FreeCAD/bin/FreeCADCmd.exe", "h2"),
            ("FreeCAD/Mod/", "h3"),
        ]);
        let prefix = single_root_prefix(&members);
        assert_eq!(prefix.as_deref(), Some("FreeCAD/"));
        let stripped = strip_root_prefix(&members, prefix.as_deref().unwrap());
        assert_eq!(
            stripped.get("bin/FreeCAD.exe").map(String::as_str),
            Some("h1")
        );
        assert_eq!(
            stripped.get("bin/FreeCADCmd.exe").map(String::as_str),
            Some("h2")
        );
        // Directory placeholder entry dropped after stripping.
        assert!(!stripped.contains_key("Mod/"));

        let mixed = map(&[("a/x", "h1"), ("b/y", "h2")]);
        assert!(single_root_prefix(&mixed).is_none());
        // Flat layout (no shared root) → no prefix.
        let flat = map(&[("x", "h1"), ("y", "h2")]);
        assert!(single_root_prefix(&flat).is_none());
        // Empty map → no prefix.
        assert!(single_root_prefix(&BTreeMap::new()).is_none());
        // Empty prefix returns members unchanged.
        let same = strip_root_prefix(&members, "");
        assert_eq!(same, members);
    }
}
