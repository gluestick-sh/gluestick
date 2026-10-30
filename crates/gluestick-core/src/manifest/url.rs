//! Manifest download URL interpretation (Scoop `#/` rename fragments).
//!
//! `Path`/`Fragment` are percent-decoded before taking base names, because the
//! `url` crate keeps them percent-encoded; filenames stay byte-compatible with
//! the manifests.

/// How to fetch and name a Scoop-style download URL.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct ParsedURL {
    /// HTTP URL without fragment.
    pub fetch_url: String,
    /// Local filename (fragment rename or URL basename).
    pub local_name: String,
    /// Lower-case extension for install routing.
    pub extension: String,
}

/// Reports `dl.zip` / `dl.7z` fragments used to extract installers with 7-Zip.
pub fn is_scoop_archive_alias(name: &str) -> bool {
    matches!(
        go_path_base(name).to_ascii_lowercase().as_str(),
        "dl.zip" | "dl.7z"
    )
}

/// Reports `#/dl.msi_` fragments (MSI expanded in pre_install).
pub fn is_scoop_msi_alias(name: &str) -> bool {
    go_path_base(name).eq_ignore_ascii_case("dl.msi_")
}

/// Reports installs that link `dl.msi_` and run Expand-MsiArchive in pre_install.
pub fn is_scoop_msi_hook_install(local_name: &str, file_ext: &str) -> bool {
    is_scoop_msi_alias(local_name) || file_ext.eq_ignore_ascii_case(".msi_")
}

/// Whether the downloader should stream-ingest as ZIP.
///
/// Scoop `dl.zip` aliases are 7z SFX installers, not standard zip archives.
pub fn should_native_zip_ingest(filename: &str, fetch_url: &str) -> bool {
    if is_scoop_archive_alias(filename) {
        return false;
    }
    let lower = filename.to_ascii_lowercase();
    if lower.ends_with(".zip") || lower.ends_with(".nupkg") {
        return true;
    }
    if !fetch_url.is_empty() {
        if let Ok(u) = url::Url::parse(fetch_url) {
            let base = go_path_base(u.path()).to_ascii_lowercase();
            return base.ends_with(".zip") || base.ends_with(".nupkg");
        }
    }
    false
}

/// Interprets a manifest download URL, including Scoop `#/` rename fragments.
/// Shorthand `#dl.zip` (missing slash) is normalized to `#/dl.zip`.
pub fn parse_url(raw: &str) -> Result<ParsedURL, url::ParseError> {
    let normalized = normalize_fragment(raw);
    let u = url::Url::parse(&normalized)?;

    let mut fetch = u.clone();
    fetch.set_fragment(None);
    let fetch_url = fetch.to_string();

    // is percent-decoded.
    let mut local_name = go_path_base(&percent_decode(u.path()));
    if let Some(frag) = u.fragment() {
        let frag = frag.strip_prefix('/').unwrap_or(frag);
        if !frag.is_empty() {
            local_name = go_path_base(&percent_decode(frag));
        }
    }

    let extension = extension_for_name(&local_name);
    Ok(ParsedURL {
        fetch_url,
        local_name,
        extension,
    })
}

/// insert the missing `/` after `#` (shorthand form).
fn normalize_fragment(raw: &str) -> String {
    if raw.contains("#/") {
        return raw.to_string();
    }
    if let Some(i) = raw.find('#') {
        let frag = &raw[i + 1..];
        if !frag.is_empty() && !frag.starts_with('/') {
            let mut s = String::with_capacity(raw.len() + 1);
            s.push_str(&raw[..i]);
            s.push_str("#/");
            s.push_str(frag);
            return s;
        }
    }
    raw.to_string()
}

/// semantics: `""` → `"."`, all-slashes → `"/"`,
/// trailing slashes removed, otherwise the last segment.
fn go_path_base(p: &str) -> String {
    if p.is_empty() {
        return ".".to_string();
    }
    let trimmed = p.trim_end_matches('/');
    if trimmed.is_empty() {
        return "/".to_string();
    }
    match trimmed.rfind('/') {
        Some(i) => trimmed[i + 1..].to_string(),
        None => trimmed.to_string(),
    }
}

/// Minimal percent-decoder (stores decoded Path/Fragment).
fn percent_decode(s: &str) -> String {
    if !s.contains('%') {
        return s.to_string();
    }
    let bytes = s.as_bytes();
    let mut out = Vec::with_capacity(bytes.len());
    let mut i = 0;
    while i < bytes.len() {
        if bytes[i] == b'%' && i + 2 < bytes.len() {
            if let (Some(h), Some(l)) = (hex_val(bytes[i + 1]), hex_val(bytes[i + 2])) {
                out.push(h * 16 + l);
                i += 3;
                continue;
            }
        }
        out.push(bytes[i]);
        i += 1;
    }
    String::from_utf8_lossy(&out).into_owned()
}

fn hex_val(b: u8) -> Option<u8> {
    match b {
        b'0'..=b'9' => Some(b - b'0'),
        b'a'..=b'f' => Some(b - b'a' + 10),
        b'A'..=b'F' => Some(b - b'A' + 10),
        _ => None,
    }
}

/// lower-case extension with multi-part special cases.
fn extension_for_name(name: &str) -> String {
    if is_scoop_msi_alias(name) {
        return ".msi_".to_string();
    }
    let lower = name.to_ascii_lowercase();
    for (suffix, ext) in [
        (".7z.exe", ".7z.exe"),
        (".tar.gz", ".tar.gz"),
        (".tar.bz2", ".tar.bz2"),
        (".tar.xz", ".tar.xz"),
        (".tgz", ".tar"),
    ] {
        if lower.ends_with(suffix) {
            return ext.to_string();
        }
    }
    // Path.Ext: suffix from the final '.' of the final element.
    let base = go_path_base(&lower);
    match base.rfind('.') {
        Some(i) => base[i..].to_string(),
        None => String::new(),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parse_url_freedroidrpg_shorthand_alias() {
        let raw = "https://ftp.osuosl.org/pub/freedroid/freedroidRPG-1.0/freedroidRPG-1.0-win32-x86-64.exe#dl.zip";
        let p = parse_url(raw).unwrap();
        assert_eq!(
            p.fetch_url,
            "https://ftp.osuosl.org/pub/freedroid/freedroidRPG-1.0/freedroidRPG-1.0-win32-x86-64.exe"
        );
        assert_eq!(p.local_name, "dl.zip");
        assert_eq!(p.extension, ".zip");
        // Scoop dl.zip aliases are 7z SFX installers — not native zip.
        assert!(!should_native_zip_ingest(&p.local_name, &p.fetch_url));
    }

    #[test]
    fn parse_url_scoop_fragment() {
        let p = parse_url("https://example.org/program.exe#/dl.7z").unwrap();
        assert_eq!(p.fetch_url, "https://example.org/program.exe");
        assert_eq!(p.local_name, "dl.7z");
        assert_eq!(p.extension, ".7z");
        assert!(is_scoop_archive_alias("dl.7z"));
    }

    #[test]
    fn parse_url_plain_zip() {
        let p = parse_url("https://example.org/app-1.0.zip").unwrap();
        assert_eq!(p.local_name, "app-1.0.zip");
        assert_eq!(p.extension, ".zip");
        assert!(should_native_zip_ingest(&p.local_name, &p.fetch_url));
    }

    #[test]
    fn parse_url_libreoffice_msi_alias() {
        let raw = "https://download.documentfoundation.org/libreoffice/stable/26.2.4/win/x86_64/LibreOffice_26.2.4_Win_x86-64.msi#/dl.msi_";
        let p = parse_url(raw).unwrap();
        assert_eq!(p.local_name, "dl.msi_");
        assert_eq!(p.extension, ".msi_");
        assert!(is_scoop_msi_hook_install(&p.local_name, &p.extension));
    }

    #[test]
    fn parse_url_rename_exe() {
        let p = parse_url("https://example.org/v1.0#/pngcrush.exe").unwrap();
        assert_eq!(p.local_name, "pngcrush.exe");
        assert_eq!(p.extension, ".exe");
    }

    #[test]
    fn go_path_base_matches_go() {
        // Verified against path.Base via probe.
        assert_eq!(go_path_base(""), ".");
        assert_eq!(go_path_base("/"), "/");
        assert_eq!(go_path_base("//"), "/");
        assert_eq!(go_path_base("/a/"), "a");
        assert_eq!(go_path_base("a//b"), "b");
        assert_eq!(go_path_base("a/./b"), "b");
        assert_eq!(go_path_base("x/y/z"), "z");
        assert_eq!(go_path_base("a/"), "a");
    }

    #[test]
    fn percent_decoded_local_name_matches_go() {
        // Url.URL.Path is decoded → local name keeps the real space.
        let p = parse_url("https://ex.org/my%20tool.zip").unwrap();
        assert_eq!(p.local_name, "my tool.zip");
        assert_eq!(p.extension, ".zip");
        // Fetch URL keeps re-encoded String form.
        assert_eq!(p.fetch_url, "https://ex.org/my%20tool.zip");
    }

    #[test]
    fn extension_special_cases() {
        assert_eq!(extension_for_name("pkg.tar.gz"), ".tar.gz");
        assert_eq!(extension_for_name("pkg.tar.bz2"), ".tar.bz2");
        assert_eq!(extension_for_name("pkg.tar.xz"), ".tar.xz");
        assert_eq!(extension_for_name("pkg.tgz"), ".tar");
        assert_eq!(extension_for_name("tool.7z.exe"), ".7z.exe");
        assert_eq!(extension_for_name("dl.msi_"), ".msi_");
        assert_eq!(extension_for_name("noext"), "");
    }
}
