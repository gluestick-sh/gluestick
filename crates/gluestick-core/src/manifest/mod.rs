//! Scoop-compatible package manifest parsing and normalization.
//!
//! JSON leniency
//! unknown fields are ignored, `null` behaves
//! like an absent value, and polymorphic Scoop fields (`url`, `hash`, `bin`,
//! `license`, …) accept string-or-array-or-object shapes.

mod accessors;
mod architecture;
mod url;

pub use architecture::{
    architecture_candidates, host_arch, HostInfo, ARCH_32BIT, ARCH_64BIT, ARCH_ARM64,
};
pub use url::{
    is_scoop_archive_alias, is_scoop_msi_alias, is_scoop_msi_hook_install, parse_url,
    should_native_zip_ingest, ParsedURL,
};

use serde::Deserialize;
use serde_json::{Map, Value};
use std::collections::BTreeMap;
use std::path::Path;
use thiserror::Error;

/// Deserializes `null` (and absence, via `#[serde(default)]`) into `T::default()`,
/// where `null` is a no-op for any field type.
fn null_default<'de, D, T>(de: D) -> Result<T, D::Error>
where
    D: serde::Deserializer<'de>,
    T: Deserialize<'de> + Default,
{
    Ok(Option::<T>::deserialize(de)?.unwrap_or_default())
}

/// Scoop writes `depends` either as an array or as a single string
/// (`"depends": "main/ffmpeg"`).
///
/// **Intentional divergence from.** declares `Depends []string` and has
/// no `UnmarshalJSON` on `Manifest` (is only used for
/// the `interface{}`-typed fields such as `env_add_path` / `notes`), so
/// rejects these manifests outright and the package becomes
/// unusable: parse error on install, invisible to search/browse/resolve. That
/// is 115 of 4,928 manifests in a stock catalog set — too many to inherit.
/// Accepting both shapes matches Scoop's own lenient handling and can only turn
/// a hard failure into a working install.
fn string_or_seq<'de, D>(de: D) -> Result<Vec<String>, D::Error>
where
    D: serde::Deserializer<'de>,
{
    let value = Option::<Value>::deserialize(de)?;
    Ok(string_slice_from_value(value.as_ref()))
}

/// Same as [`string_or_seq`] but keeps only the first string.
///
/// Used for `extract_dir` / `extract_to`, where Scoop allows an array with one
/// entry per download URL. Taking the first is the only choice that cannot make
/// a working manifest worse: the single-archive case is unaffected, and the
/// multi-archive case never reads the value (those installs link the archives
/// into the version directory as files instead of extracting them).
fn string_or_first_of_seq<'de, D>(de: D) -> Result<String, D::Error>
where
    D: serde::Deserializer<'de>,
{
    let value = Option::<Value>::deserialize(de)?;
    Ok(string_slice_from_value(value.as_ref())
        .into_iter()
        .next()
        .unwrap_or_default())
}

/// Manifest parsing errors (message parity with the implementation).
#[derive(Debug, Error)]
pub enum ManifestError {
    #[error("open manifest: {0}")]
    Open(#[source] std::io::Error),
    #[error("read manifest: {0}")]
    Read(#[source] std::io::Error),
    #[error("parse manifest JSON: {0}")]
    Json(#[from] serde_json::Error),
    #[error("manifest missing required field: version")]
    MissingVersion,
    #[error("manifest missing required field: url")]
    MissingUrl,
    #[error("manifest missing required field: hash")]
    MissingHash,
}

/// One catalogue hit for a package name.
///
/// Used by catalog resolution to decide which bucket to install from; the
/// `manifest` field is intentionally omitted — Rust callers re-read the file
/// from disk when they actually need the parsed manifest.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct Match {
    pub name: String,
    pub bucket: String,
    pub description: String,
    pub version: String,
}

/// A Scoop-compatible package manifest.
#[derive(Debug, Clone, Default, Deserialize)]
pub struct Manifest {
    #[serde(default, deserialize_with = "null_default")]
    pub version: String,
    #[serde(default, deserialize_with = "null_default")]
    pub description: String,
    #[serde(default, deserialize_with = "null_default")]
    pub homepage: String,
    /// string or `{identifier, url}`
    #[serde(default)]
    pub license: Option<Value>,
    /// string or []string
    #[serde(default)]
    pub url: Option<Value>,
    /// string or []string
    #[serde(default)]
    pub hash: Option<Value>,
    #[serde(default, deserialize_with = "null_default")]
    pub installer: Installer,
    #[serde(default, deserialize_with = "null_default")]
    pub extractor: String,
    #[serde(default, deserialize_with = "string_or_seq")]
    pub depends: Vec<String>,
    #[serde(default, deserialize_with = "string_or_seq")]
    pub depends_pre: Vec<String>,
    #[serde(default, deserialize_with = "string_or_seq")]
    pub depends_post: Vec<String>,
    /// label -> install ref
    #[serde(default)]
    pub suggest: Option<Value>,
    #[serde(default)]
    pub env: Option<BTreeMap<String, String>>,
    #[serde(default)]
    pub env_set: Option<BTreeMap<String, String>>,
    /// string or []string
    #[serde(default)]
    pub pre_install: Option<Value>,
    /// string or []string
    #[serde(default)]
    pub post_install: Option<Value>,
    /// string or []string
    #[serde(default)]
    pub pre_uninstall: Option<Value>,
    /// string or []string
    #[serde(default)]
    pub post_uninstall: Option<Value>,
    #[serde(default)]
    pub uninstaller: Uninstaller,
    /// string, []string, or []map (name/exe/alias/shim)
    #[serde(default)]
    pub bin: Option<Value>,
    /// [[target, label, args?, icon?], …]
    #[serde(default)]
    pub shortcuts: Option<Value>,
    /// string or []string
    #[serde(default)]
    pub env_add_path: Option<Value>,
    /// Inno Setup installer; extract with innounp
    #[serde(default, deserialize_with = "null_default")]
    pub innosetup: bool,
    /// Arch key -> block (null/absent ≈ nil map; `{}` is non-nil)
    #[serde(default)]
    pub architecture: Option<Map<String, Value>>,
    /// subdirectory in archive to extract from, then flatten.
    ///
    /// Scoop also allows an array here — one entry per download URL — which
    /// rejects (`ExtractDir string`), hiding the package from every command.
    /// We take the first element: for a single-archive manifest that is exactly
    /// the declared value, and for a genuine multi-archive manifest the value is
    /// unused anyway (multi-artifact installs link the archives as files and
    /// never extract them). See `string_or_first_of_seq`.
    #[serde(default, deserialize_with = "string_or_first_of_seq")]
    pub extract_dir: String,
    /// subdirectory under the install dir to extract into (same array form and
    /// rationale as `extract_dir`).
    #[serde(default, deserialize_with = "string_or_first_of_seq")]
    pub extract_to: String,
    /// string, []string, or [install, data] pairs
    #[serde(default)]
    pub persist: Option<Value>,
    /// string or []string
    #[serde(default)]
    pub notes: Option<Value>,
    /// string or object
    #[serde(default)]
    pub deprecated: Option<Value>,
    #[serde(default)]
    pub autoupdate: Option<Map<String, Value>>,
    /// Gluestick extension (unknown to Scoop; ignored there).
    #[serde(default, rename = "versionProbe")]
    pub version_probe: Option<VersionProbe>,
}

/// `installer` block.
#[derive(Debug, Clone, Default, Deserialize)]
pub struct Installer {
    /// string or []string (or a map with a `bin` key — see `binaries()`)
    #[serde(default)]
    pub script: Option<Value>,
    /// string or []string. Real buckets write `"args": "-sasl"` (a single
    /// token) in a handful of manifests; declares `Args []string` and fails
    /// the whole parse, hiding the package from every command. See
    /// `string_or_seq` for the rationale.
    #[serde(default, deserialize_with = "string_or_seq")]
    pub args: Vec<String>,
    #[serde(default, deserialize_with = "null_default")]
    pub keep: bool,
    #[serde(default, deserialize_with = "null_default")]
    pub changes: Vec<String>,
}

/// `uninstaller` block.
#[derive(Debug, Clone, Default, Deserialize)]
pub struct Uninstaller {
    /// string or []string
    #[serde(default)]
    pub script: Option<Value>,
}

/// (Gluestick extension): how to read a runtime-installed version.
#[derive(Debug, Clone, Default, Deserialize)]
pub struct VersionProbe {
    /// Path relative to the install directory (preferred probe target).
    #[serde(default, deserialize_with = "null_default")]
    pub file: String,
    /// Ordered strategies: "fileversion", "command". Empty = defaults.
    #[serde(default, deserialize_with = "null_default")]
    pub strategies: Vec<String>,
    #[serde(default, deserialize_with = "null_default")]
    pub args: Vec<String>,
    /// Optional regex with one capture group for command stdout.
    #[serde(default, deserialize_with = "null_default")]
    pub pattern: String,
    /// Field selector when probing a JSON sidecar.
    #[serde(default, deserialize_with = "null_default", rename = "jsonKey")]
    pub json_key: String,
}

/// One start-menu shortcut (`shortcuts` entry).
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct ShortcutEntry {
    pub target: String,
    pub label: String,
    pub args: String,
    pub icon: String,
}

/// A manifest `persist` entry.
///
/// Maps a path under the install dir to a path under `persist/<app>`; both
/// default to each other when only one is given.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct PersistEntry {
    pub install: String,
    pub data: String,
}

impl PersistEntry {
    /// The path relative to the app version directory.
    pub fn install_name(&self) -> &str {
        if self.install.is_empty() {
            &self.data
        } else {
            &self.install
        }
    }

    /// The path relative to `persist/<app>`.
    pub fn data_name(&self) -> &str {
        if self.data.is_empty() {
            &self.install
        } else {
            &self.data
        }
    }

    /// Reports Scoop-style *file* persist paths.
    ///
    /// Paths whose basename contains a dot are not always files: Tor Browser
    /// persists `profile.default` as a directory. Only known file extensions
    /// count; otherwise the entry is treated as a directory so a missing path
    /// is created on install.
    pub fn looks_like_file(&self) -> bool {
        crate::persist::looks_like_file_persist_path(self.install_name())
    }
}

/// Parses a manifest `persist` field into entries.
///
/// Accepts a bare path string or an array mixing paths and `[install, data]`
/// pairs; elements with no install path are dropped.
pub(crate) fn parse_persist_entries(raw: Option<&Value>) -> Vec<PersistEntry> {
    let Some(value) = raw else {
        return Vec::new();
    };
    match value {
        Value::String(path) => persist_entry_from_path(path).into_iter().collect(),
        Value::Array(items) => items
            .iter()
            .filter_map(|item| match item {
                Value::String(path) => persist_entry_from_path(path),
                Value::Array(pair) => persist_entry_from_pair(pair),
                _ => None,
            })
            .collect(),
        _ => Vec::new(),
    }
}

/// A single persist path becomes both the install and the data name.
fn persist_entry_from_path(path: &str) -> Option<PersistEntry> {
    if path.is_empty() {
        return None;
    }
    Some(PersistEntry {
        install: path.to_string(),
        data: path.to_string(),
    })
}

/// An `[install, data]` pair; a missing or empty data path falls back to
/// the install path.
fn persist_entry_from_pair(pair: &[Value]) -> Option<PersistEntry> {
    let install = pair.first().and_then(Value::as_str)?;
    if install.is_empty() {
        return None;
    }
    let data = pair
        .get(1)
        .and_then(Value::as_str)
        .filter(|d| !d.is_empty())
        .unwrap_or(install);
    Some(PersistEntry {
        install: install.to_string(),
        data: data.to_string(),
    })
}

/// A soft recommendation from `suggest` (label -> bucket/pkg ref).
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct ManifestSuggestion {
    pub label: String,
    pub ref_: String,
}

/// Parses a manifest from raw JSON bytes.
pub fn parse(data: &[u8]) -> Result<Manifest, ManifestError> {
    let m: Manifest = serde_json::from_slice(data)?;
    validate_required(&m)?;
    Ok(m)
}

/// Parses a manifest from a UTF-8 string.
pub fn parse_str(data: &str) -> Result<Manifest, ManifestError> {
    parse(data.as_bytes())
}

/// Reads and parses a manifest from a JSON file.
pub fn parse_file(path: impl AsRef<Path>) -> Result<Manifest, ManifestError> {
    use std::io::Read;
    let path = path.as_ref();
    let mut f = std::fs::File::open(path).map_err(ManifestError::Open)?;
    let mut buf = String::new();
    f.read_to_string(&mut buf).map_err(ManifestError::Read)?;
    parse_str(&buf)
}

fn validate_required(m: &Manifest) -> Result<(), ManifestError> {
    if m.version.is_empty() {
        return Err(ManifestError::MissingVersion);
    }
    // URL might live in an architecture block.
    if m.get_url().is_empty() && m.architecture.is_none() {
        return Err(ManifestError::MissingUrl);
    }
    // Hash might live in an architecture block or be unneeded for some extractors.
    if m.get_hash().is_empty() && m.extractor != "noproxy" && m.architecture.is_none() {
        return Err(ManifestError::MissingHash);
    }
    Ok(())
}

// ---------------------------------------------------------------------------
// Polymorphic-field helpers (stringSliceFromInterface / getStringOrFirst / …)
// ---------------------------------------------------------------------------

/// Normalizes Scoop fields that may be a string or a string array.
/// Empty strings are dropped from arrays.
pub(crate) fn string_slice_from_value(v: Option<&Value>) -> Vec<String> {
    match v {
        Some(Value::String(s)) => {
            if s.is_empty() {
                Vec::new()
            } else {
                vec![s.clone()]
            }
        }
        Some(Value::Array(items)) => items
            .iter()
            .filter_map(|item| match item {
                Value::String(s) if !s.is_empty() => Some(s.clone()),
                _ => None,
            })
            .collect(),
        _ => Vec::new(),
    }
}

/// first string of a string-or-array field.
/// Absent / non-string / empty-array yield `""`; a leading non-string array
/// element yields `""` as well.
pub(crate) fn first_string(v: Option<&Value>) -> String {
    match v {
        Some(Value::String(s)) => s.clone(),
        Some(Value::Array(items)) => match items.first() {
            Some(Value::String(s)) => s.clone(),
            _ => String::new(),
        },
        _ => String::new(),
    }
}

/// keeps only string values of a map field.
pub(crate) fn string_map_from_value(v: Option<&Value>) -> BTreeMap<String, String> {
    let mut out = BTreeMap::new();
    if let Some(Value::Object(map)) = v {
        for (k, item) in map {
            if let Value::String(s) = item {
                out.insert(k.clone(), s.clone());
            }
        }
    }
    out
}

/// entries are `[target, label, args?, icon?]` arrays;
/// entries with a missing/non-string or blank target are skipped. `args`/`icon`
/// are taken verbatim (no trimming), target/label are trimmed.
pub(crate) fn parse_shortcut_entries(raw: Option<&Value>) -> Vec<ShortcutEntry> {
    let Some(Value::Array(items)) = raw else {
        return Vec::new();
    };
    let mut out = Vec::new();
    for item in items {
        let Some(entry) = item.as_array() else {
            continue;
        };
        if entry.len() < 2 {
            continue;
        }
        let target = entry[0].as_str().unwrap_or("").trim().to_string();
        if target.is_empty() {
            continue;
        }
        let label = entry[1].as_str().unwrap_or("").trim().to_string();
        let mut sc = ShortcutEntry {
            target,
            label,
            ..Default::default()
        };
        if let Some(args) = entry.get(2) {
            sc.args = args.as_str().unwrap_or("").to_string();
        }
        if let Some(icon) = entry.get(3) {
            sc.icon = icon.as_str().unwrap_or("").to_string();
        }
        out.push(sc);
    }
    out
}

/// normalizes `bin` into entries (`"exe"` or `"[exe,alias]"`).
pub(crate) fn binaries_from_field(bin: Option<&Value>) -> Vec<String> {
    match bin {
        Some(Value::String(s)) => {
            if s.is_empty() {
                Vec::new()
            } else {
                vec![s.clone()]
            }
        }
        Some(Value::Array(items)) => items.iter().filter_map(bin_entry_from_element).collect(),
        _ => Vec::new(),
    }
}

/// Converts one `bin` array element. An empty string is kept so the array
/// length is preserved; unsupported element types are skipped.
fn bin_entry_from_element(item: &Value) -> Option<String> {
    match item {
        // Keeps empty strings from array elements (unlike the scalar form).
        Value::String(s) => Some(s.clone()),
        Value::Array(pair) => bin_entry_from_pair(pair),
        Value::Object(obj) => bin_entry_from_object(obj),
        _ => None,
    }
}

/// `[path, alias]` pair; a missing or non-string alias is ignored.
fn bin_entry_from_pair(pair: &[Value]) -> Option<String> {
    let exe = pair.first()?.as_str()?;
    let alias = pair.get(1).and_then(Value::as_str).unwrap_or("");
    Some(bin_pattern(exe, alias))
}

/// `{name|file, shim|alias}` object.
fn bin_entry_from_object(obj: &Map<String, Value>) -> Option<String> {
    let exe = bin_map_string(obj, &["name", "file"])?;
    let alias = bin_map_string(obj, &["shim", "alias"]).unwrap_or_default();
    Some(bin_pattern(&exe, &alias))
}

fn bin_map_string(obj: &Map<String, Value>, keys: &[&str]) -> Option<String> {
    for key in keys {
        if let Some(s) = obj.get(*key).and_then(Value::as_str) {
            if !s.is_empty() {
                return Some(s.to_string());
            }
        }
    }
    None
}

/// Renders an executable, pairing an alias when one is set.
fn bin_pattern(exe: &str, alias: &str) -> String {
    if alias.is_empty() {
        exe.to_string()
    } else {
        format!("[{exe},{alias}]")
    }
}

/// `IsDeprecatedManifestPath`: does `manifest_path` live under `deprecated/`?
pub fn is_deprecated_manifest_path(bucket_root: &Path, manifest_path: &Path) -> bool {
    use std::path::Component;
    let Ok(rel) = manifest_path.strip_prefix(bucket_root) else {
        return false;
    };
    let mut comps = rel.components();
    let Some(Component::Normal(first)) = comps.next() else {
        return false;
    };
    first.to_string_lossy().eq_ignore_ascii_case("deprecated") && comps.next().is_some()
}

/// `IsDeprecatedLocation`: archived (`deprecated/`) or JSON-marked deprecation.
pub fn is_deprecated_location(
    bucket_root: &Path,
    manifest_path: &Path,
    m: Option<&Manifest>,
) -> bool {
    if is_deprecated_manifest_path(bucket_root, manifest_path) {
        return true;
    }
    m.map(|m| m.is_deprecated_marked()).unwrap_or(false)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parse_basic_manifest() {
        let m = parse_str(
            r#"{
                "version": "1.0.0",
                "description": "Test package",
                "homepage": "https://example.com",
                "license": "MIT",
                "url": "https://example.com/download/test-1.0.0.zip",
                "hash": "abc123",
                "extractor": "zip"
            }"#,
        )
        .unwrap();
        assert_eq!(m.version, "1.0.0");
        assert_eq!(m.get_url(), "https://example.com/download/test-1.0.0.zip");
        assert_eq!(m.get_hash(), "abc123");
        assert_eq!(m.get_license(), "MIT");
    }

    /// `"depends": "main/ffmpeg"` (bare string) must not reject the manifest.
    /// rejects it (`Depends []string`), which makes 115 stock manifests
    /// unusable; see the `string_or_seq` rationale.
    #[test]
    fn depends_accepts_a_bare_string() {
        let m = parse_str(
            r#"{
                "version": "1.0.0",
                "url": "https://example.com/a.zip",
                "hash": "h",
                "depends": "main/ffmpeg",
                "depends_pre": "main/7zip",
                "depends_post": ["main/git", "main/vim"]
            }"#,
        )
        .unwrap();
        assert_eq!(m.depends, vec!["main/ffmpeg"]);
        assert_eq!(m.depends_pre, vec!["main/7zip"]);
        assert_eq!(m.depends_post, vec!["main/git", "main/vim"]);
    }

    /// `"installer": {"file": "Install.exe", "args": "-sasl"}` — a single
    /// token, not an array. rejects the whole manifest; see `string_or_seq`.
    #[test]
    fn installer_args_accepts_a_bare_string() {
        let m = parse_str(
            r#"{
                "version": "1.0.0",
                "url": "https://example.com/a.exe",
                "hash": "h",
                "installer": {"file": "Install.exe", "args": "-sasl"}
            }"#,
        )
        .unwrap();
        assert_eq!(m.installer.args, vec!["-sasl"]);
    }

    /// `installer: {"file": "Install.exe", "args": null}` — "no args", not an
    /// error.
    #[test]
    fn installer_args_accepts_array_and_null() {
        let m = parse_str(
            r#"{
                "version": "1.0.0",
                "url": "https://example.com/a.exe",
                "hash": "h",
                "installer": {"file": "Install.exe", "args": ["/S", "/D=$dir"]}
            }"#,
        )
        .unwrap();
        assert_eq!(m.installer.args, vec!["/S", "/D=$dir"]);

        let m = parse_str(
            r#"{
                "version": "1.0.0",
                "url": "https://example.com/a.exe",
                "hash": "h",
                "installer": {"file": "Install.exe", "args": null}
            }"#,
        )
        .unwrap();
        assert!(m.installer.args.is_empty());
    }

    /// `extract_dir` written as an array — one entry per URL (haskellx,
    /// pkg-config, …). rejects the whole manifest; we keep the first value.
    #[test]
    fn extract_dir_and_extract_to_accept_arrays() {
        let m = parse_str(
            r#"{
                "version": "1.0.0",
                "url": ["https://example.com/a.zip", "https://example.com/b.zip"],
                "hash": ["h1", "h2"],
                "extract_dir": ["bin", ""],
                "extract_to": ["", "usr\\local\\wbin"]
            }"#,
        )
        .unwrap();
        assert_eq!(m.extract_dir, "bin");
        // An empty array entry means "this URL has no subdir", so the value
        // picked is the first *non-empty* one.
        assert_eq!(m.extract_to, "usr\\local\\wbin");
        // The plain string form is unchanged.
        let m = parse_str(
            r#"{
                "version": "1.0.0",
                "url": "https://example.com/a.zip",
                "hash": "h",
                "extract_dir": "ghc-7.8.3",
                "extract_to": "data"
            }"#,
        )
        .unwrap();
        assert_eq!(m.extract_dir, "ghc-7.8.3");
        assert_eq!(m.extract_to, "data");
        // `null` still means "absent".
        let m = parse_str(
            r#"{
                "version": "1.0.0",
                "url": "https://example.com/a.zip",
                "hash": "h",
                "extract_dir": null,
                "extract_to": null
            }"#,
        )
        .unwrap();
        assert!(m.extract_dir.is_empty());
        assert!(m.extract_to.is_empty());
    }

    #[test]
    fn parse_manifest_with_array_url() {
        let m = parse_str(
            r#"{
                "version": "1.0.0",
                "url": ["https://example.com/test-1.0.0-x64.zip", "https://example.com/test-1.0.0-arm64.zip"],
                "hash": ["abc123", "def456"]
            }"#,
        )
        .unwrap();
        assert_eq!(m.get_url(), "https://example.com/test-1.0.0-x64.zip");
        assert_eq!(m.get_hash(), "abc123");
        assert_eq!(m.get_urls().len(), 2);
    }

    #[test]
    fn parse_manifest_missing_required() {
        for (json, want) in [
            (
                r#"{"url": "https://example.com/test.zip", "hash": "abc123"}"#,
                "version",
            ),
            (r#"{"version": "1.0.0", "hash": "abc123"}"#, "url"),
            (
                r#"{"version": "1.0.0", "url": "https://example.com/test.zip"}"#,
                "hash",
            ),
        ] {
            let err = parse_str(json).expect_err("expected error");
            assert!(
                err.to_string().contains(want),
                "error {err} should mention {want}"
            );
        }
    }

    #[test]
    fn null_fields_behave_like_go() {
        // Encoding/json treats null as a no-op — url:null + architecture passes.
        let m = parse_str(
            r#"{
                "version": "4.3.3",
                "url": null,
                "hash": null,
                "bin": null,
                "installer": null,
                "depends": null,
                "architecture": {"64bit": {"url": "https://example.com/R.exe", "hash": "md5:abc"}}
            }"#,
        )
        .unwrap();
        assert_eq!(m.version, "4.3.3");
        assert!(m.bin.is_none());
        assert!(m.depends.is_empty());
        assert!(m.installer.script.is_none());
    }

    #[test]
    fn explicit_empty_architecture_object_passes_required_url_check() {
        // Architecture non-nil (even {}) skips the root-url requirement.
        let m = parse_str(r#"{"version": "1.0.0", "architecture": {}}"#).unwrap();
        assert_eq!(m.version, "1.0.0");
        assert!(m.architecture.is_some());
    }

    #[test]
    fn license_string_and_object_forms() {
        let m = parse_str(
            r#"{"version":"1","url":"https://e/x.zip","hash":"h","license":{"identifier":"MIT","url":"https://e/mit"}}"#,
        )
        .unwrap();
        assert_eq!(m.get_license(), "MIT");
        let m = parse_str(
            r#"{"version":"1","url":"https://e/x.zip","hash":"h","license":"Apache-2.0"}"#,
        )
        .unwrap();
        assert_eq!(m.get_license(), "Apache-2.0");
    }

    #[test]
    fn env_add_paths_string_or_array() {
        let m =
            parse_str(r#"{"version":"1","url":"https://e/x.zip","hash":"h","env_add_path":"cmd"}"#)
                .unwrap();
        assert_eq!(m.env_add_paths(), vec!["cmd"]);
        let m = parse_str(r#"{"version":"1","url":"https://e/x.zip","hash":"h","env_add_path":["bin","usr\\bin"]}"#).unwrap();
        assert_eq!(m.env_add_paths(), vec!["bin", "usr\\bin"]);
    }

    #[test]
    fn notes_string_or_array() {
        let m = parse_str(
            r#"{"version":"1","url":"https://e/x.zip","hash":"h","notes":"run install.reg"}"#,
        )
        .unwrap();
        assert_eq!(m.get_notes(), vec!["run install.reg"]);
        let m =
            parse_str(r#"{"version":"1","url":"https://e/x.zip","hash":"h","notes":["a","b"]}"#)
                .unwrap();
        assert_eq!(m.get_notes(), vec!["a", "b"]);
    }

    #[test]
    fn suggestions_from_object() {
        let m = parse_str(
            r#"{"version":"1.0.0","url":"https://e/x.zip","hash":"abc","suggest":{"Java Runtime Environment":"java/temurin-jre"}}"#,
        )
        .unwrap();
        let got = m.suggestions();
        assert_eq!(got.len(), 1);
        assert_eq!(got[0].label, "Java Runtime Environment");
        assert_eq!(got[0].ref_, "java/temurin-jre");
    }

    #[test]
    fn deprecated_marked_and_location() {
        let deprecated_m = parse_str(
            r#"{"version":"1.0.0","url":"https://e/x.zip","hash":"abc","deprecated":"use other-app instead"}"#,
        )
        .unwrap();
        assert!(deprecated_m.is_deprecated_marked());
        let m = parse_str(r#"{"version":"1.0.0","url":"https://e/x.zip","hash":"abc"}"#).unwrap();
        assert!(!m.is_deprecated_marked());

        use std::path::{Path, PathBuf};
        let root = PathBuf::from(r"C:\buckets\lemon");
        let deprecated_path = PathBuf::from(r"C:\buckets\lemon\deprecated\1key.run.json");
        let active_path = PathBuf::from(r"C:\buckets\lemon\bucket\1key.run.json");
        assert!(is_deprecated_manifest_path(&root, &deprecated_path));
        assert!(!is_deprecated_manifest_path(&root, &active_path));
        // Active path + JSON-marked manifest is still deprecated.
        assert!(is_deprecated_location(
            &root,
            &active_path,
            Some(&deprecated_m)
        ));
        assert!(!is_deprecated_location(&root, &active_path, Some(&m)));
        assert!(!is_deprecated_location(
            Path::new(r"C:\other"),
            &active_path,
            None
        ));
    }

    #[test]
    fn parse_file_errors() {
        let err = parse_file(r"C:\definitely\missing\manifest.json").expect_err("open should fail");
        assert!(matches!(err, ManifestError::Open(_)));
    }

    #[test]
    fn pre_install_hooks_arch_block() {
        let m = parse_str(
            r#"{
                "version": "26.01",
                "architecture": {
                    "arm64": {
                        "url": "https://example.com/7z-arm64.exe",
                        "pre_install": ["Invoke-ExternalCommand $7zr @('x')"]
                    }
                }
            }"#,
        )
        .unwrap();
        let hooks = m.pre_install_hooks_for_install("arm64");
        assert_eq!(hooks.len(), 1);
        assert!(hooks[0].contains("Invoke-ExternalCommand"));
        // No 64bit block → no hooks from anywhere.
        assert!(m.pre_install_hooks_for_install("64bit").is_empty());
    }

    #[test]
    fn version_probe_field_parses() {
        let m = parse_str(
            r#"{
                "version": "1",
                "url": "https://e/x.zip",
                "hash": "h",
                "versionProbe": {"file": "app.exe", "strategies": ["fileversion"], "jsonKey": "version"}
            }"#,
        )
        .unwrap();
        let probe = m.version_probe.as_ref().unwrap();
        assert_eq!(probe.file, "app.exe");
        assert_eq!(probe.strategies, vec!["fileversion"]);
        assert_eq!(probe.json_key, "version");
    }
}
