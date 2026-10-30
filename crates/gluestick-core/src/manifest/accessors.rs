//! Field accessors of [`Manifest`](super::Manifest).
//!
//! Parity notes — the "root vs architecture block" precedence differs per
//! field and is mirrored exactly here:
//!
//! | field | precedence |
//! |---|---|
//! | url / hash / extract_dir / extract_to / env_add_path | root first, then block |
//! | depends_pre / depends_post / pre_install / post_install / pre_uninstall / post_uninstall / uninstaller.script / shortcuts | block first, then root |
//! | installer.script | root first, then block's value (even if empty) |
//! | env / env_set | root overlaid by block |
//! | bin | root, then installer.script bin map, then block |
//! | notes | root only |

use super::architecture::{ARCH_32BIT, ARCH_64BIT, ARCH_ARM64};
use super::{
    binaries_from_field, first_string, parse_persist_entries, parse_shortcut_entries,
    string_map_from_value, string_slice_from_value, Manifest, ManifestSuggestion, PersistEntry,
    ShortcutEntry,
};
use serde_json::{Map, Value};
use std::collections::BTreeMap;

impl Manifest {
    /// Primary download URL (root or host-selected block).
    pub fn get_url(&self) -> String {
        let s = first_string(self.url.as_ref());
        if !s.is_empty() {
            return s;
        }
        self.arch_block("")
            .map(|b| first_string(b.get("url")))
            .unwrap_or_default()
    }

    /// All download URLs (mirrors) for the host.
    pub fn get_urls(&self) -> Vec<String> {
        self.get_urls_for_install("")
    }

    /// Download URLs for an override architecture (root wins when present).
    pub fn get_urls_for_install(&self, override_: &str) -> Vec<String> {
        let root = string_slice_from_value(self.url.as_ref());
        if !root.is_empty() {
            return root;
        }
        self.arch_block(override_)
            .map(|b| string_slice_from_value(b.get("url")))
            .unwrap_or_default()
    }

    /// Primary hash value (root or host-selected block).
    pub fn get_hash(&self) -> String {
        let s = first_string(self.hash.as_ref());
        if !s.is_empty() {
            return s;
        }
        self.arch_block("")
            .map(|b| first_string(b.get("hash")))
            .unwrap_or_default()
    }

    /// Hashes for the host, in the same order as [`get_urls`](Self::get_urls).
    pub fn get_hashes(&self) -> Vec<String> {
        self.get_hashes_for_install("")
    }

    /// Hashes for an override architecture (root wins when present).
    pub fn get_hashes_for_install(&self, override_: &str) -> Vec<String> {
        let root = string_slice_from_value(self.hash.as_ref());
        if !root.is_empty() {
            return root;
        }
        self.arch_block(override_)
            .map(|b| string_slice_from_value(b.get("hash")))
            .unwrap_or_default()
    }

    /// Extraction subdirectory for the host.
    pub fn get_extract_dir(&self) -> String {
        self.get_extract_dir_for_install("")
    }

    /// Extraction subdirectory for an override architecture (root wins).
    pub fn get_extract_dir_for_install(&self, override_: &str) -> String {
        if !self.extract_dir.is_empty() {
            return self.extract_dir.clone();
        }
        self.arch_block(override_)
            .and_then(|b| {
                b.get("extract_dir")
                    .and_then(Value::as_str)
                    .map(str::to_string)
            })
            .unwrap_or_default()
    }

    /// Install subdirectory for archive extraction (Scoop `extract_to`).
    pub fn get_extract_to(&self) -> String {
        self.get_extract_to_for_install("")
    }

    /// `extract_to` for an override architecture (root wins).
    pub fn get_extract_to_for_install(&self, override_: &str) -> String {
        if !self.extract_to.is_empty() {
            return self.extract_to.clone();
        }
        self.arch_block(override_)
            .and_then(|b| {
                b.get("extract_to")
                    .and_then(Value::as_str)
                    .map(str::to_string)
            })
            .unwrap_or_default()
    }

    /// License identifier (string form or `{identifier}` object form).
    pub fn get_license(&self) -> String {
        match self.license.as_ref() {
            Some(Value::String(s)) => s.clone(),
            Some(Value::Object(map)) => map
                .get("identifier")
                .and_then(Value::as_str)
                .unwrap_or("")
                .to_string(),
            _ => String::new(),
        }
    }

    /// Is the manifest JSON-marked as deprecated?
    pub fn is_deprecated_marked(&self) -> bool {
        match self.deprecated.as_ref() {
            None => false,
            Some(Value::String(s)) => !s.trim().is_empty(),
            Some(_) => true,
        }
    }

    /// Executable patterns for shims (root bin, installer.script map, block).
    pub fn binaries(&self) -> Vec<String> {
        self.binaries_with(self.selected_arch_block())
    }

    pub(crate) fn binaries_with(&self, block: Option<&Map<String, Value>>) -> Vec<String> {
        let bins = binaries_from_field(self.bin.as_ref());
        if !bins.is_empty() {
            return bins;
        }
        // installer.script as a map may carry `bin` (alternative format).
        if let Some(Value::Object(script)) = self.installer.script.as_ref() {
            let bins = binaries_from_field(script.get("bin"));
            if !bins.is_empty() {
                return bins;
            }
        }
        block
            .map(|b| binaries_from_field(b.get("bin")))
            .unwrap_or_default()
    }

    /// Bin entries for the open-program UI. Unlike [`binaries`](Self::binaries),
    /// architecture blocks qualify even without download URLs.
    pub fn launch_binaries(&self) -> Vec<String> {
        self.launch_binaries_with(self.selected_arch_block())
    }

    pub(crate) fn launch_binaries_with(&self, block: Option<&Map<String, Value>>) -> Vec<String> {
        let bins = binaries_from_field(self.bin.as_ref());
        if !bins.is_empty() {
            return bins;
        }
        if let Some(Value::Object(script)) = self.installer.script.as_ref() {
            let bins = binaries_from_field(script.get("bin"));
            if !bins.is_empty() {
                return bins;
            }
        }
        if let Some(bins) = block.map(|b| binaries_from_field(b.get("bin"))) {
            if !bins.is_empty() {
                return bins;
            }
        }
        // Fixed-order scan: arm64 first (parity, independent of host).
        let Some(map) = self.architecture.as_ref() else {
            return Vec::new();
        };
        for arch in [ARCH_ARM64, ARCH_64BIT, ARCH_32BIT] {
            if let Some(bins) = map
                .get(arch)
                .and_then(Value::as_object)
                .map(|b| binaries_from_field(b.get("bin")))
            {
                if !bins.is_empty() {
                    return bins;
                }
            }
        }
        Vec::new()
    }

    /// Start-menu shortcuts for the host (block first, then root).
    pub fn shortcut_entries(&self) -> Vec<ShortcutEntry> {
        self.shortcut_entries_for_install("")
    }

    /// Shortcuts for an override architecture (block first, then root).
    pub fn shortcut_entries_for_install(&self, override_: &str) -> Vec<ShortcutEntry> {
        if let Some(block) = self.arch_block(override_) {
            let entries = parse_shortcut_entries(block.get("shortcuts"));
            if !entries.is_empty() {
                return entries;
            }
        }
        parse_shortcut_entries(self.shortcuts.as_ref())
    }

    /// Shortcuts for the open-program UI (root first, then block, then a
    /// fixed-order arm64→64bit→32bit scan).
    pub fn launch_shortcuts(&self) -> Vec<ShortcutEntry> {
        self.launch_shortcuts_with(self.selected_arch_block())
    }

    pub(crate) fn launch_shortcuts_with(
        &self,
        block: Option<&Map<String, Value>>,
    ) -> Vec<ShortcutEntry> {
        let entries = parse_shortcut_entries(self.shortcuts.as_ref());
        if !entries.is_empty() {
            return entries;
        }
        if let Some(entries) = block.map(|b| parse_shortcut_entries(b.get("shortcuts"))) {
            if !entries.is_empty() {
                return entries;
            }
        }
        let Some(map) = self.architecture.as_ref() else {
            return Vec::new();
        };
        for arch in [ARCH_ARM64, ARCH_64BIT, ARCH_32BIT] {
            if let Some(entries) = map
                .get(arch)
                .and_then(Value::as_object)
                .map(|b| parse_shortcut_entries(b.get("shortcuts")))
            {
                if !entries.is_empty() {
                    return entries;
                }
            }
        }
        Vec::new()
    }

    /// Whether the manifest declares bins and/or shortcuts.
    pub fn has_launch_definitions(&self) -> bool {
        !self.launch_binaries().is_empty() || !self.launch_shortcuts().is_empty()
    }

    /// Root `depends` (architecture-level depends is not a Scoop concept).
    pub fn depends(&self) -> &[String] {
        &self.depends
    }

    /// `depends_pre` from the host block, else root.
    pub fn depends_pre_list(&self) -> Vec<String> {
        self.depends_pre_with(self.arch_block(""))
    }

    pub(crate) fn depends_pre_with(&self, block: Option<&Map<String, Value>>) -> Vec<String> {
        if let Some(deps) = block.map(|b| string_slice_from_value(b.get("depends_pre"))) {
            if !deps.is_empty() {
                return deps;
            }
        }
        self.depends_pre.clone()
    }

    /// `depends_post` from the host block, else root.
    pub fn depends_post_list(&self) -> Vec<String> {
        self.depends_post_with(self.arch_block(""))
    }

    pub(crate) fn depends_post_with(&self, block: Option<&Map<String, Value>>) -> Vec<String> {
        if let Some(deps) = block.map(|b| string_slice_from_value(b.get("depends_post"))) {
            if !deps.is_empty() {
                return deps;
            }
        }
        self.depends_post.clone()
    }

    /// Manifest env vars for an override architecture (root overlaid by block).
    pub fn env_vars_for_install(&self, override_: &str) -> BTreeMap<String, String> {
        let mut out = self.env.clone().unwrap_or_default();
        if let Some(block) = self.arch_block(override_) {
            for (k, v) in string_map_from_value(block.get("env")) {
                out.insert(k, v);
            }
        }
        out
    }

    /// `env_set` vars for an override architecture (root overlaid by block).
    pub fn env_set_for_install(&self, override_: &str) -> BTreeMap<String, String> {
        let mut out = self.env_set.clone().unwrap_or_default();
        if let Some(block) = self.arch_block(override_) {
            for (k, v) in string_map_from_value(block.get("env_set")) {
                out.insert(k, v);
            }
        }
        out
    }

    /// PATH directories from `env_add_path` (root first, then block).
    pub fn env_add_paths(&self) -> Vec<String> {
        self.env_add_paths_with(self.arch_block(""))
    }

    pub(crate) fn env_add_paths_with(&self, block: Option<&Map<String, Value>>) -> Vec<String> {
        let root = string_slice_from_value(self.env_add_path.as_ref());
        if !root.is_empty() {
            return root;
        }
        block
            .map(|b| string_slice_from_value(b.get("env_add_path")))
            .unwrap_or_default()
    }

    /// Manifest notes (root only; string or []string).
    pub fn get_notes(&self) -> Vec<String> {
        string_slice_from_value(self.notes.as_ref())
    }

    /// `pre_install` for an override architecture (block first, then root).
    pub fn pre_install_hooks_for_install(&self, override_: &str) -> Vec<String> {
        if let Some(block) = self.arch_block(override_) {
            let hooks = string_slice_from_value(block.get("pre_install"));
            if !hooks.is_empty() {
                return hooks;
            }
        }
        string_slice_from_value(self.pre_install.as_ref())
    }

    /// `persist` entries for an override architecture (block first, then root).
    ///
    /// an arch block's `persist` wins
    /// only when it parses to at least one entry.
    pub fn persist_entries_for_install(&self, override_: &str) -> Vec<PersistEntry> {
        if let Some(block) = self.arch_block(override_) {
            let entries = parse_persist_entries(block.get("persist"));
            if !entries.is_empty() {
                return entries;
            }
        }
        parse_persist_entries(self.persist.as_ref())
    }

    /// Root-level `persist` entries.
    pub fn persist_entries(&self) -> Vec<PersistEntry> {
        self.persist_entries_for_install("")
    }

    /// `post_install` for an override architecture (block first, then root).
    pub fn post_install_hooks_for_install(&self, override_: &str) -> Vec<String> {
        if let Some(block) = self.arch_block(override_) {
            let hooks = string_slice_from_value(block.get("post_install"));
            if !hooks.is_empty() {
                return hooks;
            }
        }
        string_slice_from_value(self.post_install.as_ref())
    }

    /// `pre_uninstall` for an override architecture (block first, then root).
    pub fn pre_uninstall_hooks_for_install(&self, override_: &str) -> Vec<String> {
        if let Some(block) = self.arch_block(override_) {
            let hooks = string_slice_from_value(block.get("pre_uninstall"));
            if !hooks.is_empty() {
                return hooks;
            }
        }
        string_slice_from_value(self.pre_uninstall.as_ref())
    }

    /// `post_uninstall` for an override architecture (block first, then root).
    pub fn post_uninstall_hooks_for_install(&self, override_: &str) -> Vec<String> {
        if let Some(block) = self.arch_block(override_) {
            let hooks = string_slice_from_value(block.get("post_uninstall"));
            if !hooks.is_empty() {
                return hooks;
            }
        }
        string_slice_from_value(self.post_uninstall.as_ref())
    }

    /// `installer.script` lines (root first, then the block's value — note the
    /// block result is returned even when empty, unlike the hook accessors).
    pub fn installer_script_hooks_for_install(&self, override_: &str) -> Vec<String> {
        let root = string_slice_from_value(self.installer.script.as_ref());
        if !root.is_empty() {
            return root;
        }
        self.arch_block(override_)
            .and_then(|b| {
                b.get("installer")
                    .and_then(Value::as_object)
                    .map(|inst| string_slice_from_value(inst.get("script")))
            })
            .unwrap_or_default()
    }

    /// Whether the manifest defines `installer.script`.
    pub fn has_installer_script_for_install(&self, override_: &str) -> bool {
        !self
            .installer_script_hooks_for_install(override_)
            .is_empty()
    }

    /// `uninstaller.script` lines (block first, then root).
    pub fn uninstaller_script_hooks_for_install(&self, override_: &str) -> Vec<String> {
        if let Some(block) = self.arch_block(override_) {
            if let Some(inst) = block.get("uninstaller").and_then(Value::as_object) {
                let hooks = string_slice_from_value(inst.get("script"));
                if !hooks.is_empty() {
                    return hooks;
                }
            }
        }
        string_slice_from_value(self.uninstaller.script.as_ref())
    }

    /// `suggest` entries (label -> bucket/pkg ref).
    pub fn suggestions(&self) -> Vec<ManifestSuggestion> {
        let Some(Value::Object(map)) = self.suggest.as_ref() else {
            return Vec::new();
        };
        let mut out = Vec::new();
        for (label, v) in map {
            let Some(ref_) = v.as_str() else { continue };
            let ref_ = ref_.trim();
            if ref_.is_empty() {
                continue;
            }
            out.push(ManifestSuggestion {
                label: label.clone(),
                ref_: ref_.to_string(),
            });
        }
        out
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::manifest::{architecture::ARCH_64BIT, parse_str};

    #[test]
    fn binaries_alias_forms() {
        let m = parse_str(
            r#"{
                "version": "1.0.0",
                "url": "https://example.com/x.zip",
                "hash": "abc",
                "bin": [
                    "plain.exe",
                    ["nested.exe", "nested-alias"],
                    {"file": "map.exe", "alias": "map-alias"},
                    {"name": "shim.exe", "shim": "shim-cmd"}
                ]
            }"#,
        )
        .unwrap();
        assert_eq!(
            m.binaries(),
            vec![
                "plain.exe",
                "[nested.exe,nested-alias]",
                "[map.exe,map-alias]",
                "[shim.exe,shim-cmd]"
            ]
        );
    }

    #[test]
    fn binaries_array_variants_drop_unsupported() {
        let m = parse_str(
            r#"{
                "version": "1.0.0",
                "url": "https://example.com/x.zip",
                "hash": "abc",
                "bin": [
                    ["single.exe"],
                    ["aliased.exe", 42],
                    ["", "empty-path"],
                    42,
                    true,
                    null,
                    {"alias": "no-executable"},
                    {}
                ]
            }"#,
        )
        .unwrap();
        assert_eq!(
            m.binaries(),
            vec!["single.exe", "aliased.exe", "[,empty-path]"]
        );
    }

    #[test]
    fn binaries_from_architecture_block() {
        let m = parse_str(
            r#"{
                "version": "4.3.3",
                "url": null,
                "hash": null,
                "bin": null,
                "architecture": {
                    "64bit": {
                        "url": "https://example.com/R-4.3.3-win.exe",
                        "hash": "md5:abc",
                        "bin": ["bin\\x64\\R.exe", "bin\\x64\\Rcmd.exe", ["bin\\x64\\Rgui.exe", "R"]]
                    }
                }
            }"#,
        )
        .unwrap();
        // Explicit block via the test seam (host-independent).
        let block = m.arch_block_for_host(ARCH_64BIT, ARCH_64BIT, 22000);
        assert_eq!(
            m.binaries_with(block),
            vec![
                "bin\\x64\\R.exe",
                "bin\\x64\\Rcmd.exe",
                "[bin\\x64\\Rgui.exe,R]"
            ]
        );
        // launch variants share the same block behavior.
        assert_eq!(m.launch_binaries_with(block), m.binaries_with(block));
    }

    #[test]
    fn launch_binaries_prefers_arm64_in_fixed_order() {
        // Constructed via serde (no urls → would fail manifest::parse validation).
        let m: Manifest = serde_json::from_value(serde_json::json!({
            "version": "1",
            "architecture": {
                "64bit": {"bin": ["x64.exe"]},
                "arm64": {"bin": ["arm.exe"]}
            }
        }))
        .unwrap();
        assert_eq!(m.launch_binaries_with(None), vec!["arm.exe"]);
    }

    #[test]
    fn shortcut_entries_block_first_then_root() {
        let m = parse_str(
            r#"{
                "version": "1",
                "url": "https://e/x.zip",
                "hash": "h",
                "shortcuts": [["root.exe", "Root"]],
                "architecture": {
                    "64bit": {"url": "https://e/x64.zip", "shortcuts": [["block.exe", "Block", "--arg"]]}
                }
            }"#,
        )
        .unwrap();
        let entries = m.shortcut_entries_for_install("64bit");
        assert_eq!(entries.len(), 1);
        assert_eq!(entries[0].target, "block.exe");
        assert_eq!(entries[0].args, "--arg");
        // Invalid override → root fallback.
        let entries = m.shortcut_entries_for_install("bogus");
        assert_eq!(entries[0].target, "root.exe");
    }

    #[test]
    fn env_merge_block_overrides_root() {
        let m = parse_str(
            r#"{
                "version": "1",
                "url": "https://e/x.zip",
                "hash": "h",
                "env": {"A": "root-a", "B": "root-b"},
                "architecture": {"64bit": {"url": "https://e/x64.zip", "env": {"B": "block-b"}}}
            }"#,
        )
        .unwrap();
        let env = m.env_vars_for_install("64bit");
        assert_eq!(env.get("A").map(String::as_str), Some("root-a"));
        assert_eq!(env.get("B").map(String::as_str), Some("block-b"));
    }

    #[test]
    fn installer_and_uninstaller_hook_precedence() {
        let m = parse_str(
            r#"{
                "version": "1",
                "url": "https://e/x.zip",
                "hash": "h",
                "installer": {"script": "root-installer"},
                "uninstaller": {"script": "root-uninstaller"},
                "architecture": {
                    "64bit": {
                        "url": "https://e/x64.zip",
                        "installer": {"script": "block-installer"},
                        "uninstaller": {"script": "block-uninstaller"}
                    }
                }
            }"#,
        )
        .unwrap();
        // installer.script: ROOT first.
        assert_eq!(
            m.installer_script_hooks_for_install("64bit"),
            vec!["root-installer"]
        );
        assert!(m.has_installer_script_for_install("64bit"));
        // uninstaller.script: BLOCK first.
        assert_eq!(
            m.uninstaller_script_hooks_for_install("64bit"),
            vec!["block-uninstaller"]
        );
        // Unknown override → block nil → both fall to root.
        assert_eq!(
            m.uninstaller_script_hooks_for_install("bogus"),
            vec!["root-uninstaller"]
        );
    }
}
