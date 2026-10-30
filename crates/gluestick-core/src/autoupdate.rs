//! Build a manifest for a version the bucket never published, from the
//! manifest's own `autoupdate` block.
//!
//! Scoop has two routes to a manifest it has never seen. The first is the
//! bucket's git history (`Manifest.FindHistoricalManifestInGit`, mirrored by
//! [`crate::bucket_history`]). The second is `Manifest.ForVersion` →
//! `Invoke-AutoUpdate`, which rewrites the *current* manifest into the
//! requested version; it is the only route left once the history runs out.
//! This module is that second route.
//!
//! # What is implemented, and what is not
//!
//! `Invoke-AutoUpdate` is PowerShell. It does two things: substitute the
//! `$version`-family variables (`Get-VersionSubstitution`) into the download
//! fields, then resolve the hash through `HashHelper` → `get_hash_for_app`,
//! which either reads a checksum file published next to the release or
//! downloads the artifact and hashes it locally.
//!
//! Only the substitution half needs no script engine, and it carries almost
//! all of the value: on a real bucket (4 870 manifests with `autoupdate`) 89%
//! of URL templates resolve from the version alone. The rest use `$match…`
//! captures that come from a package-specific `checkver` script, which this
//! build does not run — those get a named error instead of a URL with a
//! literal `$matchBuild` in it, because such a URL fails as a 404 much further
//! from the cause.
//!
//! Hash resolution takes the download fallback for every URL, skipping the
//! checksum-file readers entirely. The artifact is fetched anyway, the content
//! store keeps the bytes keyed by their SHA-256, and the install that follows
//! hits the store instead of the network — so reading a published checksum
//! would save the *first* fetch, not a second one, and is not worth the
//! parser surface (`jsonpath`, `xpath`, `rdf`, `metalink`, `fosshub`, …).
//!
//! # One architecture at a time
//!
//! Scoop updates every architecture block and downloads each block's artifact
//! to hash it. Gluestick installs one architecture, so only that block is
//! generated and only its URLs are fetched. The other blocks keep the URLs of
//! the version that was checked out; they are not used unless the selected
//! architecture 404s and the fallback chain moves to them.

use serde_json::{Map, Value};

use crate::manifest::{string_slice_from_value, Manifest};

/// Variables `Get-VersionSubstitution` derives from the version string alone.
///
/// The list omits the `$match…` captures that Scoop adds from a package's
/// `checkver` regex: reproducing those means running that regex script, which
/// this build cannot do. `$matchHead` / `$matchTail` are the exception because
/// Scoop derives them from the version itself (`(?<head>\d+\.\d+(?:\.\d+)?)(?<tail>.*)`),
/// not from the package script.
///
/// Returned longest-name-first so a caller that substitutes in order cannot
/// shorten `$underscoreVersion` to `$u…` by matching `$version` inside it.
pub fn version_substitutions(version: &str) -> Vec<(String, String)> {
    let first = version.split('-').next().unwrap_or("");
    let last = version.rsplit('-').next().unwrap_or("");
    let mut parts = first.split('.');
    let major = parts.next().unwrap_or("");
    let minor = parts.next().unwrap_or("");
    let patch = parts.next().unwrap_or("");
    let build = parts.next().unwrap_or("");

    let mut subs: Vec<(String, String)> = vec![
        ("$version".to_string(), version.to_string()),
        ("$dotVersion".to_string(), normalized(version, '.')),
        ("$underscoreVersion".to_string(), normalized(version, '_')),
        ("$dashVersion".to_string(), normalized(version, '-')),
        ("$cleanVersion".to_string(), normalized(version, '\0')),
        ("$majorVersion".to_string(), major.to_string()),
        ("$minorVersion".to_string(), minor.to_string()),
        ("$patchVersion".to_string(), patch.to_string()),
        ("$buildVersion".to_string(), build.to_string()),
        ("$preReleaseVersion".to_string(), last.to_string()),
    ];
    if let Some((head, tail)) = match_head_tail(version) {
        subs.push(("$matchHead".to_string(), head));
        subs.push(("$matchTail".to_string(), tail));
    }
    subs.sort_by(|a, b| b.0.len().cmp(&a.0.len()).then_with(|| a.0.cmp(&b.0)));
    subs
}

/// `$Version -replace '[._-]', sep`; `'\0'` means "delete the separator"
/// (`$cleanVersion`).
fn normalized(version: &str, separator: char) -> String {
    version
        .chars()
        .map(|c| match c {
            '.' | '_' | '-' => separator,
            other => other,
        })
        .filter(|c| *c != '\0')
        .collect()
}

/// Scoop's `if ($Version -match '(?<head>\d+\.\d+(?:\.\d+)?)(?<tail>.*)')`.
///
/// Hand-rolled because the only pattern in play is "two or three dotted
/// numbers, then anything", and pulling in a regex engine for it would be the
/// tail wagging the dog.
fn match_head_tail(version: &str) -> Option<(String, String)> {
    let bytes = version.as_bytes();
    let mut i = 0;
    let mut groups = 0;
    loop {
        let start = i;
        while i < bytes.len() && bytes[i].is_ascii_digit() {
            i += 1;
        }
        if i == start {
            // The regex requires a digit run in every group it consumes.
            return (groups >= 2).then(|| (version[..i].to_string(), String::new()));
        }
        groups += 1;
        if groups == 3 {
            break;
        }
        if i < bytes.len() && bytes[i] == b'.' {
            i += 1;
        } else {
            break;
        }
    }
    if groups < 2 {
        return None;
    }
    Some((version[..i].to_string(), version[i..].to_string()))
}

/// Replace every `$name` in `subs`, matching the longest name at each `$`.
///
/// Longest-match is what keeps `$version_windows_amd64` intact: the variable is
/// `$version` and `_windows_amd64` is literal text, even though `$version_…`
/// looks like one identifier. Anything not in `subs` is left untouched so the
/// caller can name it in an error.
pub fn substitute(input: &str, subs: &[(String, String)]) -> String {
    let bytes = input.as_bytes();
    let mut out = String::with_capacity(input.len());
    let mut i = 0;
    while i < bytes.len() {
        if bytes[i] == b'$' {
            let mut best: Option<&(String, String)> = None;
            for entry in subs {
                if starts_with_at(bytes, i, entry.0.as_bytes())
                    && best.is_none_or(|(name, _)| entry.0.len() > name.len())
                {
                    best = Some(entry);
                }
            }
            if let Some((name, value)) = best {
                out.push_str(value);
                i += name.len();
                continue;
            }
        }
        // `i` is a char boundary and `$` is ASCII, so this slice is valid.
        let ch = input[i..].chars().next().expect("i < len");
        out.push(ch);
        i += ch.len_utf8();
    }
    out
}

fn starts_with_at(haystack: &[u8], at: usize, needle: &[u8]) -> bool {
    at + needle.len() <= haystack.len()
        && haystack[at..at + needle.len()].eq_ignore_ascii_case(needle)
}

/// Where a download field has to be written so that install reads it back.
///
/// gluestick's accessors prefer the root `url` / `hash` over the architecture
/// block (see `manifest::accessors`), the opposite of Scoop's `arch_specific`.
/// A generated value written into the other place would therefore be silently
/// ignored, so the target follows the accessor rule rather than wherever
/// `autoupdate` happened to declare the template.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum Target {
    Root,
    Arch,
}

/// Why a version could not be built from `autoupdate`.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum GenerateError {
    /// The manifest has no `autoupdate` block at all.
    NoAutoupdate,
    /// `autoupdate` declares no `url` for the selected architecture.
    NoUrl { arch: String },
    /// The template needs a `$variable` this build cannot compute
    /// (`$matchBuild`, `$matchTag`, …), which Scoop's `checkver` regex would
    /// have produced.
    Unresolved { url: String, variable: String },
}

/// A manifest rewritten for the requested version, with its hashes pending.
///
/// The URLs are substituted but no hash is filled in yet: the hashes have to
/// come from the bytes each URL serves, which is a download and belongs to the
/// caller (it has the progress sink and the cancel token). [`Self::apply_hashes`]
/// writes the result into the same place install reads it from.
#[derive(Debug, Clone)]
pub struct Generated {
    manifest: Manifest,
    urls: Vec<String>,
    hash_target: Target,
    arch: String,
}

impl Generated {
    /// URLs the caller must download to compute the hashes, in manifest order.
    pub fn urls(&self) -> &[String] {
        &self.urls
    }

    /// Write one `sha256:…` value per URL (same order as [`Self::urls`]).
    pub fn apply_hashes(&mut self, hashes: &[String]) {
        let value = strings_value(hashes);
        match self.hash_target {
            Target::Root => self.manifest.hash = Some(value),
            Target::Arch => match arch_block_mut(&mut self.manifest, &self.arch) {
                Some(block) => {
                    block.insert("hash".to_string(), value);
                }
                // The rule that picked `Arch` requires the block, but falling
                // back keeps a malformed manifest installable instead of
                // silently dropping the hash.
                None => self.manifest.hash = Some(value),
            },
        }
    }

    pub fn into_manifest(self) -> Manifest {
        self.manifest
    }
}

/// Rewrite `m` into `version` (Scoop `Manifest.ForVersion`).
///
/// `arch` is the architecture install selected (`""` when the manifest has no
/// architecture block). Only that block's URLs are generated.
pub fn for_version(m: &Manifest, version: &str, arch: &str) -> Result<Generated, GenerateError> {
    let au = m.autoupdate.as_ref().ok_or(GenerateError::NoAutoupdate)?;
    let subs = version_substitutions(version);

    let root_has_url = !string_slice_from_value(m.url.as_ref()).is_empty();
    // Only a real block counts: install falls back to the root fields when the
    // block is missing, so writing into a block that does not exist would be
    // lost anyway.
    let target = if root_has_url || arch_block(m, arch).is_none() {
        Target::Root
    } else {
        Target::Arch
    };
    let au_arch = au_arch_block(au, arch);

    let (raw_urls, target) = match target {
        // Scoop's `Update-ManifestProperty` also reaches for the block value
        // when the block does not declare the property itself.
        Target::Arch => match au_arch.and_then(|b| b.get("url")).or_else(|| au.get("url")) {
            Some(value) => (string_slice_from_value(Some(value)), Target::Arch),
            None => (Vec::new(), Target::Root),
        },
        Target::Root => (string_slice_from_value(au.get("url")), Target::Root),
    };

    let urls: Vec<String> = raw_urls.iter().map(|u| substitute(u, &subs)).collect();
    if urls.is_empty() {
        return Err(GenerateError::NoUrl {
            arch: arch.to_string(),
        });
    }
    for url in &urls {
        if let Some(variable) = unresolved_variable(url) {
            return Err(GenerateError::Unresolved {
                url: url.clone(),
                variable,
            });
        }
    }

    let mut out = m.clone();
    out.version = version.to_string();
    write_urls(&mut out, target, arch, &urls);
    substitute_fields(&mut out, au, arch, &subs);

    // The hash follows its own accessor rule, which can differ from the URL's:
    // a manifest may carry the URL at the root and the hash in a block.
    let hash_target = if !string_slice_from_value(m.hash.as_ref()).is_empty() {
        Target::Root
    } else if arch_block(m, arch)
        .is_some_and(|b| !string_slice_from_value(b.get("hash")).is_empty())
    {
        Target::Arch
    } else {
        target
    };

    Ok(Generated {
        manifest: out,
        urls,
        hash_target,
        arch: arch.to_string(),
    })
}

/// First `$name` still left in `url` after substitution.
fn unresolved_variable(url: &str) -> Option<String> {
    let bytes = url.as_bytes();
    for i in 0..bytes.len() {
        if bytes[i] != b'$' {
            continue;
        }
        let Some(&next) = bytes.get(i + 1) else {
            continue;
        };
        if !(next.is_ascii_alphabetic() || next == b'_') {
            continue;
        }
        let mut end = i + 1;
        while end < bytes.len() && (bytes[end].is_ascii_alphanumeric() || bytes[end] == b'_') {
            end += 1;
        }
        return Some(url[i..end].to_string());
    }
    None
}

/// Substitute the fields `Invoke-AutoUpdate` touches besides `url` / `hash`.
///
/// Scoop only rewrites a property the manifest already declares, and prefers
/// the root value when there is one — the same root-first rule the accessors
/// use. The list is the union of the keys real buckets put under `autoupdate`
/// (`extract_dir` alone accounts for 454 manifests); `url` / `hash` are handled
/// above because they need a download.
fn substitute_fields(
    m: &mut Manifest,
    au: &Map<String, Value>,
    arch: &str,
    subs: &[(String, String)],
) {
    let au_arch = au_arch_block(au, arch);

    if let Some(Value::String(template)) = au.get("extract_dir") {
        if !m.extract_dir.is_empty() {
            m.extract_dir = substitute(template, subs);
        } else if let Some(block) = arch_block_mut(m, arch) {
            substitute_block_field(block, "extract_dir", au_arch, au, subs);
        }
    }
    if let Some(Value::String(template)) = au.get("extract_to") {
        if !m.extract_to.is_empty() {
            m.extract_to = substitute(template, subs);
        } else if let Some(block) = arch_block_mut(m, arch) {
            substitute_block_field(block, "extract_to", au_arch, au, subs);
        }
    }

    for field in ["bin", "shortcuts", "persist", "notes"] {
        if root_field(m, field).is_some() {
            if let Some(template) = au.get(field) {
                let value = substitute_value(template, subs);
                set_root_field(m, field, value);
            }
            continue;
        }
        if let Some(block) = arch_block_mut(m, arch) {
            substitute_block_field(block, field, au_arch, au, subs);
        }
    }
}

fn substitute_block_field(
    block: &mut Map<String, Value>,
    field: &str,
    au_arch: Option<&Map<String, Value>>,
    au: &Map<String, Value>,
    subs: &[(String, String)],
) {
    if !block.contains_key(field) {
        return;
    }
    let Some(template) = au_arch.and_then(|b| b.get(field)).or_else(|| au.get(field)) else {
        return;
    };
    block.insert(field.to_string(), substitute_value(template, subs));
}

/// Recursive string substitution, mirroring PowerShell's `substitute` walking
/// arrays (a `bin` list or a `shortcuts` table can name `$version` too).
fn substitute_value(value: &Value, subs: &[(String, String)]) -> Value {
    match value {
        Value::String(s) => Value::String(substitute(s, subs)),
        Value::Array(items) => {
            Value::Array(items.iter().map(|v| substitute_value(v, subs)).collect())
        }
        Value::Object(map) => Value::Object(
            map.iter()
                .map(|(k, v)| (k.clone(), substitute_value(v, subs)))
                .collect(),
        ),
        other => other.clone(),
    }
}

fn root_field<'a>(m: &'a Manifest, field: &str) -> Option<&'a Value> {
    match field {
        "bin" => m.bin.as_ref(),
        "shortcuts" => m.shortcuts.as_ref(),
        "persist" => m.persist.as_ref(),
        "notes" => m.notes.as_ref(),
        _ => None,
    }
}

fn set_root_field(m: &mut Manifest, field: &str, value: Value) {
    match field {
        "bin" => m.bin = Some(value),
        "shortcuts" => m.shortcuts = Some(value),
        "persist" => m.persist = Some(value),
        "notes" => m.notes = Some(value),
        _ => {}
    }
}

fn write_urls(m: &mut Manifest, target: Target, arch: &str, urls: &[String]) {
    let value = strings_value(urls);
    match target {
        Target::Root => m.url = Some(value),
        Target::Arch => match arch_block_mut(m, arch) {
            Some(block) => {
                block.insert("url".to_string(), value);
            }
            None => m.url = Some(value),
        },
    }
}

fn strings_value(values: &[String]) -> Value {
    if values.len() == 1 {
        Value::String(values[0].clone())
    } else {
        Value::Array(values.iter().cloned().map(Value::String).collect())
    }
}

fn arch_block<'a>(m: &'a Manifest, arch: &str) -> Option<&'a Map<String, Value>> {
    if arch.is_empty() {
        return None;
    }
    m.architecture.as_ref()?.get(arch)?.as_object()
}

fn arch_block_mut<'a>(m: &'a mut Manifest, arch: &str) -> Option<&'a mut Map<String, Value>> {
    if arch.is_empty() {
        return None;
    }
    m.architecture.as_mut()?.get_mut(arch)?.as_object_mut()
}

fn au_arch_block<'a>(au: &'a Map<String, Value>, arch: &str) -> Option<&'a Map<String, Value>> {
    if arch.is_empty() {
        return None;
    }
    au.get("architecture")?.get(arch)?.as_object()
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::manifest;

    fn manifest(json: &str) -> Manifest {
        manifest::parse_str(json).expect("test manifest parses")
    }

    fn generate(json: &str, version: &str, arch: &str) -> Result<Generated, GenerateError> {
        for_version(&manifest(json), version, arch)
    }

    #[test]
    fn version_variables_match_scoop() {
        let subs = version_substitutions("1.2.3-beta.4");
        let value = |name: &str| {
            subs.iter()
                .find(|(key, _)| key == name)
                .map(|(_, v)| v.clone())
                .unwrap_or_default()
        };
        assert_eq!(value("$version"), "1.2.3-beta.4");
        assert_eq!(value("$dotVersion"), "1.2.3.beta.4");
        assert_eq!(value("$underscoreVersion"), "1_2_3_beta_4");
        assert_eq!(value("$dashVersion"), "1-2-3-beta-4");
        assert_eq!(value("$cleanVersion"), "123beta4");
        assert_eq!(value("$majorVersion"), "1");
        assert_eq!(value("$minorVersion"), "2");
        assert_eq!(value("$patchVersion"), "3");
        assert_eq!(value("$buildVersion"), "");
        assert_eq!(value("$preReleaseVersion"), "beta.4");
        assert_eq!(value("$matchHead"), "1.2.3");
        assert_eq!(value("$matchTail"), "-beta.4");
    }

    #[test]
    fn match_head_needs_two_dotted_numbers() {
        assert_eq!(match_head_tail("7"), None);
        assert_eq!(
            match_head_tail("1.2"),
            Some(("1.2".to_string(), String::new()))
        );
        // Greedy three-group head, everything else is the tail.
        assert_eq!(
            match_head_tail("1.2.3.4"),
            Some(("1.2.3".to_string(), ".4".to_string()))
        );
    }

    /// The dominant real-world shape: `$version` glued to a literal suffix
    /// (`$version_windows_amd64` is not a variable name).
    #[test]
    fn version_keeps_its_literal_suffix() {
        let subs = version_substitutions("2.0.0");
        assert_eq!(
            substitute("https://e/$version_windows_amd64.zip", &subs),
            "https://e/2.0.0_windows_amd64.zip"
        );
        assert_eq!(
            substitute("https://e/$underscoreVersion/$version.zip", &subs),
            "https://e/2_0_0/2.0.0.zip"
        );
    }

    #[test]
    fn generates_the_root_url() {
        let generated = generate(
            r#"{"version":"1.0.0","url":"https://e/x-1.0.0.zip","hash":"h",
                "autoupdate":{"url":"https://e/x-$version.zip"}}"#,
            "2.0.0",
            "",
        )
        .expect("generates");
        let m = generated.into_manifest();
        assert_eq!(m.get_url(), "https://e/x-2.0.0.zip");
        assert_eq!(m.version, "2.0.0");
    }

    #[test]
    fn generates_only_the_selected_architecture() {
        let json = r#"{
            "version":"1.0.0",
            "architecture":{
                "64bit":{"url":"https://e/x64-1.0.0.zip","hash":"h"},
                "32bit":{"url":"https://e/x86-1.0.0.zip","hash":"h"}
            },
            "autoupdate":{"architecture":{
                "64bit":{"url":"https://e/x64-$version.zip"},
                "32bit":{"url":"https://e/x86-$version.zip"}
            }}
        }"#;
        let generated = generate(json, "2.0.0", "64bit").expect("generates");
        let m = generated.into_manifest();
        assert_eq!(
            m.get_urls_for_install("64bit"),
            vec!["https://e/x64-2.0.0.zip"]
        );
        // The unselected block is left alone rather than half-generated.
        assert_eq!(
            m.get_urls_for_install("32bit"),
            vec!["https://e/x86-1.0.0.zip"]
        );
    }

    #[test]
    fn unresolved_checkver_variable_is_named() {
        let err = generate(
            r#"{"version":"1.0.0","url":"https://e/x.zip","hash":"h",
                "autoupdate":{"url":"https://e/x-$matchBuild.zip"}}"#,
            "2.0.0",
            "",
        )
        .expect_err("needs checkver");
        assert_eq!(
            err,
            GenerateError::Unresolved {
                url: "https://e/x-$matchBuild.zip".to_string(),
                variable: "$matchBuild".to_string(),
            }
        );
    }

    #[test]
    fn extract_dir_follows_the_version() {
        let generated = generate(
            r#"{"version":"1.0.0","url":"https://e/x.zip","hash":"h",
                "extract_dir":"app-1.0.0",
                "autoupdate":{"url":"https://e/x.zip","extract_dir":"app-$version"}}"#,
            "2.0.0",
            "",
        )
        .expect("generates");
        assert_eq!(generated.into_manifest().extract_dir, "app-2.0.0");
    }

    /// The hash is written where the accessors read it: root when the manifest
    /// has a root hash, otherwise the block that owns the URL.
    #[test]
    fn hashes_land_where_the_install_reads_them() {
        let json = r#"{
            "version":"1.0.0",
            "architecture":{"64bit":{"url":"https://e/x64-1.0.0.zip","hash":"old"}},
            "autoupdate":{"architecture":{"64bit":{"url":"https://e/x64-$version.zip"}}}
        }"#;
        let mut generated = generate(json, "2.0.0", "64bit").expect("generates");
        generated.apply_hashes(&["sha256:abc".to_string()]);
        let m = generated.into_manifest();
        assert_eq!(m.get_hashes_for_install("64bit"), vec!["sha256:abc"]);
        assert!(m.hash.is_none(), "no root hash was declared");

        let json = r#"{"version":"1.0.0","url":"https://e/x-1.0.0.zip","hash":"old",
            "autoupdate":{"url":"https://e/x-$version.zip"}}"#;
        let mut generated = generate(json, "2.0.0", "").expect("generates");
        generated.apply_hashes(&["sha256:def".to_string()]);
        assert_eq!(generated.into_manifest().get_hashes(), vec!["sha256:def"]);
    }

    #[test]
    fn missing_autoupdate_is_an_error() {
        assert_eq!(
            generate(
                r#"{"version":"1.0.0","url":"https://e/x.zip","hash":"h"}"#,
                "2.0.0",
                ""
            )
            .expect_err("no autoupdate"),
            GenerateError::NoAutoupdate
        );
    }
}
