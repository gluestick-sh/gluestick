//! Scoop architecture selection (arm64/64bit/32bit) and host detection.
//!

use super::{string_slice_from_value, Manifest};
use serde_json::{Map, Value};
use std::sync::OnceLock;

/// Scoop manifest architecture keys.
pub const ARCH_ARM64: &str = "arm64";
pub const ARCH_64BIT: &str = "64bit";
pub const ARCH_32BIT: &str = "32bit";

const WIN11_BUILD: i32 = 22000;

/// Scoop-style architecture preference for this host.
pub fn architecture_candidates(host_arch: &str, win_build: i32) -> Vec<&'static str> {
    match host_arch {
        ARCH_ARM64 => {
            if win_build >= WIN11_BUILD {
                vec![ARCH_ARM64, ARCH_64BIT, ARCH_32BIT]
            } else {
                vec![ARCH_ARM64, ARCH_32BIT]
            }
        }
        ARCH_64BIT => vec![ARCH_64BIT, ARCH_32BIT],
        ARCH_32BIT => vec![ARCH_32BIT],
        _ => vec![ARCH_64BIT, ARCH_32BIT],
    }
}

/// Host facts needed for architecture selection.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct HostInfo {
    /// One of `arm64` / `64bit` / `32bit`.
    pub arch: &'static str,
    /// Windows build number (0 when unknown).
    pub win_build: i32,
}

static HOST: OnceLock<HostInfo> = OnceLock::new();

impl HostInfo {
    /// Detects the host architecture once and caches it
    pub fn detect() -> HostInfo {
        *HOST.get_or_init(|| HostInfo {
            arch: detect_architecture(),
            win_build: detect_windows_build(),
        })
    }
}

/// The Scoop architecture key for this host (`arm64` / `64bit` / `32bit`).
///
/// Single source of truth for the install pipeline and the engine; both need
/// the same key when selecting manifest blocks and download URLs.
pub fn host_arch() -> String {
    HostInfo::detect().arch.to_string()
}

fn detect_architecture() -> &'static str {
    // PROCESSOR_ARCHITEW6432 reflects the native OS when this process runs
    // under WoW64/x64 emulation on ARM64 Windows.
    let native = std::env::var("PROCESSOR_ARCHITEW6432").unwrap_or_default();
    if native.trim().eq_ignore_ascii_case("ARM64") {
        return ARCH_ARM64;
    }
    let arch = std::env::var("PROCESSOR_ARCHITECTURE").unwrap_or_default();
    if arch.trim().eq_ignore_ascii_case("ARM64") {
        ARCH_ARM64
    } else if arch.trim().eq_ignore_ascii_case("AMD64") {
        ARCH_64BIT
    } else if arch.trim().eq_ignore_ascii_case("X86") {
        ARCH_32BIT
    } else {
        ARCH_64BIT
    }
}

#[cfg(windows)]
fn detect_windows_build() -> i32 {
    use std::ffi::c_void;
    use windows_sys::Win32::System::Registry::{RegGetValueW, HKEY_LOCAL_MACHINE, RRF_RT_REG_SZ};

    let subkey: Vec<u16> = "SOFTWARE\\Microsoft\\Windows NT\\CurrentVersion"
        .encode_utf16()
        .chain(std::iter::once(0))
        .collect();
    let value: Vec<u16> = "CurrentBuildNumber"
        .encode_utf16()
        .chain(std::iter::once(0))
        .collect();
    let mut buf = [0u16; 32];
    let mut size = (buf.len() * 2) as u32;
    let status = unsafe {
        RegGetValueW(
            HKEY_LOCAL_MACHINE,
            subkey.as_ptr(),
            value.as_ptr(),
            RRF_RT_REG_SZ,
            std::ptr::null_mut(),
            buf.as_mut_ptr() as *mut c_void,
            &mut size,
        )
    };
    if status != 0 {
        return 0;
    }
    let len = buf.iter().position(|&c| c == 0).unwrap_or(buf.len());
    String::from_utf16(&buf[..len])
        .ok()
        .and_then(|s| s.trim().parse().ok())
        .unwrap_or(0)
}

#[cfg(not(windows))]
fn detect_windows_build() -> i32 {
    0
}

impl Manifest {
    /// Picks the manifest architecture block for an explicit host (test seam;
    /// public methods use [`HostInfo::detect`]).
    pub fn select_architecture_for_host(&self, host_arch: &str, win_build: i32) -> &'static str {
        let Some(map) = self.architecture.as_ref() else {
            return "";
        };
        for arch in architecture_candidates(host_arch, win_build) {
            if let Some(block) = map.get(arch).and_then(Value::as_object) {
                if arch_block_is_selectable(block) {
                    return arch;
                }
            }
        }
        ""
    }

    /// Selected architecture block key for this host ("" when none applies).
    pub fn selected_architecture(&self) -> &'static str {
        let host = HostInfo::detect();
        self.select_architecture_for_host(host.arch, host.win_build)
    }

    /// Lists manifest architecture keys that are selectable (have download
    /// URLs or arch-only metadata), in fixed Scoop display order.
    pub fn available_architectures(&self) -> Vec<&'static str> {
        let Some(map) = self.architecture.as_ref() else {
            return Vec::new();
        };
        [ARCH_ARM64, ARCH_64BIT, ARCH_32BIT]
            .into_iter()
            .filter(|arch| {
                map.get(*arch)
                    .and_then(Value::as_object)
                    .map(arch_block_is_selectable)
                    .unwrap_or(false)
            })
            .collect()
    }

    /// Override when valid, otherwise host-selected architecture.
    pub fn architecture_for_install<'a>(&'a self, override_: &'a str) -> &'a str {
        if !override_.is_empty() {
            if let Some(block) = self
                .architecture
                .as_ref()
                .and_then(|m| m.get(override_))
                .and_then(Value::as_object)
            {
                if arch_block_is_selectable(block) {
                    return override_;
                }
            }
            return self.selected_architecture();
        }
        self.selected_architecture()
    }

    /// Next host-preferred architecture with a download URL after `current`.
    /// Used when the preferred arch's URL 404s.
    pub fn fallback_architecture(&self, current: &str) -> &'static str {
        let host = HostInfo::detect();
        self.fallback_architecture_for_host(current, host.arch, host.win_build)
    }

    /// [`fallback_architecture`](Self::fallback_architecture) with an explicit
    /// host (test seam).
    pub fn fallback_architecture_for_host(
        &self,
        current: &str,
        host_arch: &str,
        win_build: i32,
    ) -> &'static str {
        if self.architecture.is_none() || current.is_empty() {
            return "";
        }
        let Some(map) = self.architecture.as_ref() else {
            return "";
        };
        let mut seen_current = false;
        for arch in architecture_candidates(host_arch, win_build) {
            if !seen_current {
                if arch == current {
                    seen_current = true;
                }
                continue;
            }
            if let Some(block) = map.get(arch).and_then(Value::as_object) {
                if arch_block_has_url(block) {
                    return arch;
                }
            }
        }
        ""
    }

    /// Raw `url` list of one architecture block (empty when absent).
    pub fn urls_for_architecture(&self, arch: &str) -> Vec<String> {
        self.architecture
            .as_ref()
            .and_then(|m| m.get(arch))
            .and_then(Value::as_object)
            .map(|block| string_slice_from_value(block.get("url")))
            .unwrap_or_default()
    }

    /// Architecture block for an override key (empty override = host pick).
    ///
    /// Parity: a present-but-unselectable override block yields `None` (no
    /// fallthrough to the host pick) — callers fall back to root fields.
    pub(crate) fn arch_block_for_host(
        &self,
        override_: &str,
        host_arch: &str,
        win_build: i32,
    ) -> Option<&Map<String, Value>> {
        if !override_.is_empty() {
            let block = self.architecture.as_ref()?.get(override_)?.as_object()?;
            return arch_block_is_selectable(block).then_some(block);
        }
        let arch = self.select_architecture_for_host(host_arch, win_build);
        if arch.is_empty() {
            return None;
        }
        self.architecture.as_ref()?.get(arch)?.as_object()
    }

    /// [`arch_block_for_host`](Self::arch_block_for_host) with the detected host.
    pub(crate) fn arch_block(&self, override_: &str) -> Option<&Map<String, Value>> {
        let host = HostInfo::detect();
        self.arch_block_for_host(override_, host.arch, host.win_build)
    }

    /// Host-selected architecture block (override = "").
    pub(crate) fn selected_arch_block(&self) -> Option<&Map<String, Value>> {
        self.arch_block("")
    }
}

/// Does the block have download URLs?
pub(crate) fn arch_block_has_url(block: &Map<String, Value>) -> bool {
    !string_slice_from_value(block.get("url")).is_empty()
}

/// Blocks used for install: download URL, arch-only metadata
/// (extract_dir/extract_to/hash), or installer/uninstaller hooks that only
/// exist per-architecture.
///
/// Scoop selects an architecture block whenever the key exists. narrower
/// check stops at URL/extract/hash, which silently drops manifests whose arch
/// block carries only `installer.script` (e.g. `extras/hxd`): the script then
/// never runs and the raw installer is deployed unexpanded. Intentional
/// divergence, limited to the keys that drive the installer hooks.
pub(crate) fn arch_block_is_selectable(block: &Map<String, Value>) -> bool {
    if arch_block_has_url(block) {
        return true;
    }
    for key in ["extract_dir", "extract_to"] {
        if let Some(s) = block.get(key).and_then(Value::as_str) {
            if !s.trim().is_empty() {
                return true;
            }
        }
    }
    if !string_slice_from_value(block.get("hash")).is_empty() {
        return true;
    }
    ["installer", "uninstaller", "pre_install", "post_install"]
        .iter()
        .any(|key| block.get(*key).is_some_and(|value| !value.is_null()))
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::manifest::parse_str;

    fn arch_manifest() -> Manifest {
        parse_str(
            r#"{
                "version": "1.0.0",
                "architecture": {
                    "64bit": {"url": "https://example.com/x64.zip", "hash": "h64"},
                    "arm64": {"url": "https://example.com/arm64.zip", "hash": "harm"},
                    "32bit": {"url": "https://example.com/x86.zip", "hash": "h32"}
                }
            }"#,
        )
        .unwrap()
    }

    #[test]
    fn architecture_candidates_arm64_win11() {
        assert_eq!(
            architecture_candidates(ARCH_ARM64, 22000),
            vec![ARCH_ARM64, ARCH_64BIT, ARCH_32BIT]
        );
    }

    #[test]
    fn select_architecture_for_host() {
        let m = arch_manifest();
        let cases = [
            (
                ARCH_ARM64,
                22000,
                ARCH_ARM64,
                "https://example.com/arm64.zip",
            ),
            (
                ARCH_ARM64,
                19041,
                ARCH_ARM64,
                "https://example.com/arm64.zip",
            ),
            (ARCH_64BIT, 22000, ARCH_64BIT, "https://example.com/x64.zip"),
            (ARCH_32BIT, 0, ARCH_32BIT, "https://example.com/x86.zip"),
        ];
        for (host, build, want_arch, want_url) in cases {
            let arch = m.select_architecture_for_host(host, build);
            assert_eq!(arch, want_arch, "host={host} build={build}");
            assert_eq!(m.urls_for_architecture(want_arch), vec![want_url]);
        }
    }

    #[test]
    fn architecture_for_install_override() {
        let m = arch_manifest();
        assert_eq!(m.architecture_for_install(ARCH_64BIT), ARCH_64BIT);
        assert_eq!(
            m.get_urls_for_install(ARCH_64BIT),
            vec!["https://example.com/x64.zip"]
        );
        assert_eq!(
            m.available_architectures(),
            vec![ARCH_ARM64, ARCH_64BIT, ARCH_32BIT]
        );
    }

    #[test]
    fn installer_script_only_arch_block_is_selectable() {
        let m = parse_str(
            r#"{
                "version": "1.0.0",
                "url": "https://example.com/root.zip",
                "hash": "h",
                "architecture": {
                    "64bit": {
                        "installer": {
                            "script": ["Expand-InnoArchive \"$dir\\setup.exe\" -Removal"]
                        }
                    }
                }
            }"#,
        )
        .unwrap();
        assert_eq!(m.select_architecture_for_host(ARCH_64BIT, 0), ARCH_64BIT);
        assert_eq!(
            m.select_architecture_for_host(ARCH_ARM64, 22000),
            ARCH_64BIT,
            "an arm64 host falls back to the selectable 64bit block"
        );
        assert!(m.has_installer_script_for_install(ARCH_64BIT));
    }

    #[test]
    fn empty_arch_block_is_not_selectable() {
        let m = parse_str(
            r#"{"version":"1.0.0","url":"https://example.com/root.zip","hash":"h",
                "architecture":{"64bit":{}}}"#,
        )
        .unwrap();
        assert_eq!(m.select_architecture_for_host(ARCH_64BIT, 0), "");
    }

    #[test]
    fn select_architecture_extract_dir_only() {
        let m = parse_str(
            r#"{
                "version": "1.6.4.0",
                "url": "https://example.com/ultravnc.zip",
                "architecture": {
                    "64bit": {"extract_dir": "x64"},
                    "32bit": {"extract_dir": "x86"}
                }
            }"#,
        )
        .unwrap();
        let arch = m.select_architecture_for_host(ARCH_64BIT, 22000);
        assert_eq!(arch, ARCH_64BIT);
        assert_eq!(m.get_extract_dir_for_install(arch), "x64");
        // Root url still wins over the block (GetURLsForInstall parity).
        assert_eq!(
            m.get_urls_for_install(arch),
            vec!["https://example.com/ultravnc.zip"]
        );
    }

    #[test]
    fn select_architecture_arm64_falls_back_to_64bit_on_win11() {
        let m = parse_str(
            r#"{
                "version": "1.0.0",
                "architecture": {"64bit": {"url": "https://example.com/x64.zip", "hash": "h64"}}
            }"#,
        )
        .unwrap();
        assert_eq!(
            m.select_architecture_for_host(ARCH_ARM64, 22000),
            ARCH_64BIT
        );
        // Pre-Win11 arm64 only considers arm64/32bit → nothing selectable.
        assert_eq!(m.select_architecture_for_host(ARCH_ARM64, 19041), "");
    }

    #[test]
    fn fallback_architecture_after_404() {
        let m = parse_str(
            r#"{
                "version": "1.0.0",
                "architecture": {
                    "arm64": {"url": "https://example.com/arm64.exe"},
                    "64bit": {"url": "https://example.com/x64.exe"},
                    "32bit": {"url": "https://example.com/x86.exe"}
                }
            }"#,
        )
        .unwrap();
        assert_eq!(
            m.fallback_architecture_for_host(ARCH_ARM64, ARCH_ARM64, 22000),
            ARCH_64BIT
        );
        assert_eq!(
            m.fallback_architecture_for_host(ARCH_64BIT, ARCH_ARM64, 22000),
            ARCH_32BIT
        );
        assert_eq!(
            m.fallback_architecture_for_host(ARCH_32BIT, ARCH_ARM64, 22000),
            ""
        );
        assert_eq!(m.fallback_architecture_for_host("", ARCH_ARM64, 22000), "");
    }

    #[test]
    fn host_info_detect_is_sane() {
        let host = HostInfo::detect();
        assert!(matches!(host.arch, ARCH_ARM64 | ARCH_64BIT | ARCH_32BIT));
    }
}
