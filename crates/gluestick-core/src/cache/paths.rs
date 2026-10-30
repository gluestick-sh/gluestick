//! Install-path helpers.

/// Paths that should not be linked into `apps/` or indexed: empty paths and
/// any path whose basename starts with `.`.
pub fn is_hidden_install_path(rel_path: &str) -> bool {
    if rel_path.is_empty() {
        return true;
    }
    // Last path segment, both separators.
    let name = rel_path.rsplit(['/', '\\']).next().unwrap_or(rel_path);
    name.starts_with('.')
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn hidden_install_paths() {
        assert!(is_hidden_install_path(""));
        assert!(is_hidden_install_path(".git"));
        assert!(is_hidden_install_path("sub/.hidden"));
        assert!(is_hidden_install_path("sub\\.hidden"));
        assert!(!is_hidden_install_path("bin"));
        assert!(!is_hidden_install_path("a/b.exe"));
        // Dot only at a parent level is fine — basename decides.
        assert!(!is_hidden_install_path(".config/app/file.txt"));
    }
}
