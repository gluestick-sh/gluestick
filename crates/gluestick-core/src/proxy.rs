//! HTTP proxy resolution.
//!
//! Three sources, in order: the `proxy` key in `config.json` (`auto`, `direct`
//! or a URL), the conventional environment variables, then the Windows system
//! proxy (WinINET, the "Internet Options" settings that Scoop/aria2 also
//! read). Only HTTP CONNECT proxies are supported; SOCKS would need another
//! protocol implementation and is not used by any of the tools we replace.

use std::path::Path;

/// Conventional variable names, checked in both cases (`HTTPS_PROXY` and
/// `https_proxy`) because Windows and Unix shells disagree.
const HTTPS_VARS: [&str; 2] = ["HTTPS_PROXY", "https_proxy"];
const HTTP_VARS: [&str; 2] = ["HTTP_PROXY", "http_proxy"];
const ALL_VARS: [&str; 2] = ["ALL_PROXY", "all_proxy"];
const NO_PROXY_VARS: [&str; 2] = ["NO_PROXY", "no_proxy"];

/// WinINET settings ("Internet Options → Connections → LAN settings").
#[cfg(windows)]
const WININET_KEY: &str = r"Software\Microsoft\Windows\CurrentVersion\Internet Settings";

/// One CONNECT proxy endpoint.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ProxySpec {
    pub host: String,
    pub port: u16,
    pub username: Option<String>,
    pub password: Option<String>,
    /// Outer TLS to the proxy itself (`https://…`). Not implemented yet; the
    /// connector reports a clear error rather than silently going direct.
    pub tls: bool,
}

/// Resolved proxy settings for both target schemes plus the bypass list.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct ProxySettings {
    pub http: Option<ProxySpec>,
    pub https: Option<ProxySpec>,
    /// Host names (or `.suffix` / `*` / `<local>`) that bypass the proxy.
    pub no_proxy: Vec<String>,
    /// Hosts allowed to use the proxy while [`Self::restrict`] is set.
    pub only_hosts: Vec<String>,
    /// When set, only [`Self::only_hosts`] reach the proxy; every other
    /// connection goes direct. Used by `proxy_packages` so a package can be
    /// routed through a proxy without proxying the whole workspace.
    pub restrict: bool,
}

impl ProxySettings {
    pub fn is_empty(&self) -> bool {
        self.http.is_none() && self.https.is_none()
    }

    /// Whether `host` may use the proxy under the current restriction.
    pub fn allows_proxy(&self, host: &str) -> bool {
        !self.restrict || no_proxy_match(host, &self.only_hosts)
    }

    /// Add a host to the restricted allowlist. Ignored when the list is not
    /// restricted; deduplicated so repeated downloads stay cheap.
    pub fn add_proxy_host(&mut self, host: &str) {
        if !self.restrict {
            return;
        }
        let host = host.trim().trim_end_matches('.').to_ascii_lowercase();
        if !host.is_empty() && !self.only_hosts.iter().any(|entry| entry == &host) {
            self.only_hosts.push(host);
        }
    }

    /// Proxy to use for a target scheme. An `https` target falls back to the
    /// `http` entry when only one proxy is configured (`ALL_PROXY`, a single
    /// system proxy, an explicit `proxy` URL).
    pub fn for_scheme(&self, is_tls: bool) -> Option<&ProxySpec> {
        if is_tls {
            self.https.as_ref().or(self.http.as_ref())
        } else {
            self.http.as_ref()
        }
    }

    /// Whether `host` must be reached directly.
    pub fn bypasses(&self, host: &str) -> bool {
        no_proxy_match(host, &self.no_proxy)
    }

    /// Compact `http://host:port` description for the UI and diagnostics
    /// (credentials omitted). Empty when no proxy is configured.
    pub fn describe(&self) -> String {
        let one = |spec: &ProxySpec| format!("http://{}:{}", spec.host, spec.port);
        match (&self.http, &self.https) {
            (Some(http), Some(https)) if http == https => one(http),
            (Some(http), Some(https)) => format!("http={}; https={}", one(http), one(https)),
            (Some(http), None) => one(http),
            (None, Some(https)) => one(https),
            (None, None) => String::new(),
        }
    }
}

/// Where the effective proxy came from, for diagnostics and the settings UI.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ProxySource {
    /// The `proxy` key in `config.json`.
    Config,
    /// `HTTPS_PROXY` / `HTTP_PROXY` / `ALL_PROXY`.
    Environment,
    /// The Windows system proxy (WinINET, static `ProxyServer`).
    System,
    /// The system proxy is a PAC script, which this client does not execute.
    SystemPac,
    /// No proxy; connections are direct.
    Direct,
}

/// Proxy settings plus where they came from.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ResolvedProxy {
    pub settings: ProxySettings,
    pub source: ProxySource,
}

/// Resolve proxy settings from config, environment and the system settings.
pub fn resolve(root: &Path) -> ProxySettings {
    resolve_detailed(root).settings
}

/// [`resolve`] with the source that won, so the UI can explain the state.
pub fn resolve_detailed(root: &Path) -> ResolvedProxy {
    let configured = crate::config::read_config_proxy(root).unwrap_or_default();
    let configured = configured.trim();
    if configured.eq_ignore_ascii_case("direct") || configured.eq_ignore_ascii_case("none") {
        return ResolvedProxy {
            settings: ProxySettings::default(),
            source: ProxySource::Direct,
        };
    }
    if !configured.is_empty() && !configured.eq_ignore_ascii_case("auto") {
        return match parse_proxy_url(configured) {
            Some(spec) => ResolvedProxy {
                settings: ProxySettings {
                    http: Some(spec.clone()),
                    https: Some(spec),
                    no_proxy: env_no_proxy(),
                    ..ProxySettings::default()
                },
                source: ProxySource::Config,
            },
            None => ResolvedProxy {
                settings: ProxySettings::default(),
                source: ProxySource::Direct,
            },
        };
    }
    if let Some(settings) = from_env() {
        return ResolvedProxy {
            settings,
            source: ProxySource::Environment,
        };
    }
    let (settings, pac_only) = system_proxy_detailed();
    let source = if settings.is_some() {
        ProxySource::System
    } else if pac_only {
        ProxySource::SystemPac
    } else {
        ProxySource::Direct
    };
    ResolvedProxy {
        settings: settings.unwrap_or_default(),
        source,
    }
}

fn from_env() -> Option<ProxySettings> {
    from_env_with(|name| std::env::var(name).ok())
}

/// Environment-only resolution, split out so it is testable without touching
/// the process environment.
fn from_env_with(lookup: impl Fn(&str) -> Option<String>) -> Option<ProxySettings> {
    let read = |names: &[&str]| {
        names
            .iter()
            .filter_map(|name| lookup(name))
            .map(|value| value.trim().to_string())
            .find(|value| !value.is_empty())
    };
    let all = read(&ALL_VARS).and_then(|value| parse_proxy_url(&value));
    let http = read(&HTTP_VARS)
        .and_then(|value| parse_proxy_url(&value))
        .or_else(|| all.clone());
    let https = read(&HTTPS_VARS)
        .and_then(|value| parse_proxy_url(&value))
        .or_else(|| all.clone());
    if http.is_none() && https.is_none() {
        return None;
    }
    Some(ProxySettings {
        http,
        https,
        no_proxy: read(&NO_PROXY_VARS)
            .map(|value| split_no_proxy(&value))
            .unwrap_or_default(),
        ..ProxySettings::default()
    })
}

fn env_no_proxy() -> Vec<String> {
    NO_PROXY_VARS
        .iter()
        .find_map(|name| std::env::var(name).ok())
        .map(|value| split_no_proxy(&value))
        .unwrap_or_default()
}

/// Windows system proxy (WinINET). Returns the settings (when a static proxy
/// is enabled) and whether a PAC script is configured — PAC is detected but
/// not executed, so the UI can say why automatic mode is not routing.
#[cfg(windows)]
fn system_proxy_detailed() -> (Option<ProxySettings>, bool) {
    let pac_only = reg_string(WININET_KEY, "AutoConfigURL")
        .map(|value| !value.trim().is_empty())
        .unwrap_or(false);
    let enabled = reg_dword(WININET_KEY, "ProxyEnable").unwrap_or(0) != 0;
    if !enabled {
        return (None, pac_only);
    }
    let server = reg_string(WININET_KEY, "ProxyServer").unwrap_or_default();
    if server.trim().is_empty() {
        return (None, pac_only);
    }
    let overrides = reg_string(WININET_KEY, "ProxyOverride").unwrap_or_default();
    (
        Some(settings_from_system_parts(&server, &overrides)),
        pac_only,
    )
}

#[cfg(not(windows))]
fn system_proxy_detailed() -> (Option<ProxySettings>, bool) {
    (None, false)
}

/// Parse a WinINET `ProxyServer` value: either `host:port` or a per-scheme
/// list such as `http=proxy:8080;https=proxy:8080`.
fn settings_from_system_parts(server: &str, overrides: &str) -> ProxySettings {
    let mut http = None;
    let mut https = None;
    if server.contains('=') {
        for part in server.split(';') {
            let Some((scheme, value)) = part.split_once('=') else {
                continue;
            };
            let spec = parse_proxy_url(value.trim());
            match scheme.trim().to_ascii_lowercase().as_str() {
                "http" => http = spec,
                "https" => https = spec,
                _ => {}
            }
        }
    } else if let Some(spec) = parse_proxy_url(server.trim()) {
        http = Some(spec.clone());
        https = Some(spec);
    }
    ProxySettings {
        http,
        https,
        no_proxy: split_system_overrides(overrides),
        ..ProxySettings::default()
    }
}

/// `ProxyOverride` is `;`-separated and may use the `<local>` token.
fn split_system_overrides(value: &str) -> Vec<String> {
    value
        .split(';')
        .map(|item| item.trim().to_ascii_lowercase())
        .filter(|item| !item.is_empty())
        .collect()
}

fn split_no_proxy(value: &str) -> Vec<String> {
    value
        .split(|c: char| c == ',' || c == ';' || c.is_whitespace())
        .map(|item| item.trim().to_ascii_lowercase())
        .filter(|item| !item.is_empty())
        .collect()
}

/// Parse `http://user:pass@host:8080`, `host:8080`, or a bare host.
///
/// A missing scheme means `http://`. A missing port uses the scheme default
/// (80 for `http`, 443 for `https`).
pub fn parse_proxy_url(value: &str) -> Option<ProxySpec> {
    let value = value.trim();
    if value.is_empty() {
        return None;
    }
    let (scheme, rest) = match value.split_once("://") {
        Some((scheme, rest)) => (scheme.to_ascii_lowercase(), rest),
        None => ("http".to_string(), value),
    };
    if scheme != "http" && scheme != "https" {
        return None;
    }
    let (userinfo, authority) = match rest.rsplit_once('@') {
        Some((userinfo, authority)) => (Some(userinfo), authority),
        None => (None, rest),
    };
    let authority = authority.trim_end_matches('/');
    if authority.is_empty() {
        return None;
    }
    let (host, port) = split_host_port(authority, if scheme == "https" { 443 } else { 80 })?;
    let (username, password) = match userinfo {
        Some(info) => match info.split_once(':') {
            Some((user, pass)) => (Some(user.to_string()), Some(pass.to_string())),
            None => (Some(info.to_string()), None),
        },
        None => (None, None),
    };
    Some(ProxySpec {
        host,
        port,
        username,
        password,
        tls: scheme == "https",
    })
}

/// Split `host:port`, `[v6]:port` or a bare host.
fn split_host_port(authority: &str, default_port: u16) -> Option<(String, u16)> {
    if let Some(rest) = authority.strip_prefix('[') {
        // [::1]:8080
        let (host, tail) = rest.split_once(']')?;
        if host.is_empty() {
            return None;
        }
        let port = match tail.strip_prefix(':') {
            Some(port) => port.parse().ok()?,
            None => default_port,
        };
        return Some((host.to_string(), port));
    }
    match authority.rsplit_once(':') {
        // A lone colon is a host:port split; a port must parse, otherwise the
        // value is malformed rather than a hostname that happens to contain it.
        Some((host, port)) if !host.is_empty() && !port.contains(':') => {
            Some((host.to_string(), port.parse().ok()?))
        }
        Some(_) => None,
        None => Some((authority.to_string(), default_port)),
    }
}

/// Whether `host` matches any entry of the bypass list.
///
/// Entries are lowercase; a leading dot is ignored, an optional `:port` is
/// dropped, `*` bypasses everything and `<local>` bypasses hosts without a dot
/// (the WinINET convention).
pub fn no_proxy_match(host: &str, entries: &[String]) -> bool {
    let host = host.trim_end_matches('.').to_ascii_lowercase();
    if host.is_empty() {
        return false;
    }
    for entry in entries {
        let entry = entry.trim();
        if entry.is_empty() {
            continue;
        }
        if entry == "*" {
            return true;
        }
        if entry == "<local>" {
            if !host.contains('.') {
                return true;
            }
            continue;
        }
        let entry = entry.trim_start_matches('.');
        let entry_host = match entry.rsplit_once(':') {
            Some((name, port)) if port.chars().all(|c| c.is_ascii_digit()) => name,
            _ => entry,
        };
        if host == entry_host || host.ends_with(&format!(".{entry_host}")) {
            return true;
        }
    }
    false
}

/// Standard base64 (RFC 4648) for `Proxy-Authorization: Basic`.
pub(crate) fn base64_encode(input: &[u8]) -> String {
    const TABLE: &[u8; 64] = b"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";
    let mut out = String::with_capacity(input.len().div_ceil(3) * 4);
    for chunk in input.chunks(3) {
        let b0 = chunk[0] as u32;
        let b1 = chunk.get(1).copied().unwrap_or(0) as u32;
        let b2 = chunk.get(2).copied().unwrap_or(0) as u32;
        let triple = (b0 << 16) | (b1 << 8) | b2;
        out.push(TABLE[(triple >> 18) as usize & 0x3f] as char);
        out.push(TABLE[(triple >> 12) as usize & 0x3f] as char);
        out.push(if chunk.len() > 1 {
            TABLE[(triple >> 6) as usize & 0x3f] as char
        } else {
            '='
        });
        out.push(if chunk.len() > 2 {
            TABLE[triple as usize & 0x3f] as char
        } else {
            '='
        });
    }
    out
}

/// Read a `REG_DWORD` from `HKCU\<path>`.
#[cfg(windows)]
fn reg_dword(path: &str, name: &str) -> Option<u32> {
    use windows_sys::Win32::System::Registry::{RegGetValueW, HKEY_CURRENT_USER, RRF_RT_REG_DWORD};
    let key = wide(path);
    let value = wide(name);
    let mut data: u32 = 0;
    let mut size = std::mem::size_of::<u32>() as u32;
    // SAFETY: both strings are NUL-terminated; `data`/`size` describe a valid
    // 4-byte buffer for a REG_DWORD read.
    let status = unsafe {
        RegGetValueW(
            HKEY_CURRENT_USER,
            key.as_ptr(),
            value.as_ptr(),
            RRF_RT_REG_DWORD,
            std::ptr::null_mut(),
            (&mut data as *mut u32).cast(),
            &mut size,
        )
    };
    (status == 0).then_some(data)
}

/// Read a `REG_SZ`/`REG_EXPAND_SZ` from `HKCU\<path>`.
#[cfg(windows)]
fn reg_string(path: &str, name: &str) -> Option<String> {
    use windows_sys::Win32::System::Registry::{
        RegGetValueW, HKEY_CURRENT_USER, RRF_RT_REG_EXPAND_SZ, RRF_RT_REG_SZ,
    };
    let key = wide(path);
    let value = wide(name);
    // WinINET proxy values are short; a larger buffer would be unusual.
    let mut buf = vec![0u8; 8192];
    let mut size = buf.len() as u32;
    // SAFETY: `key`/`value` are NUL-terminated and `buf` is `size` bytes.
    let status = unsafe {
        RegGetValueW(
            HKEY_CURRENT_USER,
            key.as_ptr(),
            value.as_ptr(),
            RRF_RT_REG_SZ | RRF_RT_REG_EXPAND_SZ,
            std::ptr::null_mut(),
            buf.as_mut_ptr().cast(),
            &mut size,
        )
    };
    if status != 0 || size < 2 {
        return None;
    }
    buf.truncate(size as usize);
    let wide: Vec<u16> = buf
        .as_chunks::<2>()
        .0
        .iter()
        .map(|pair| u16::from_le_bytes(*pair))
        .collect();
    let text = String::from_utf16_lossy(&wide);
    let text = text.trim_end_matches('\0').trim().to_string();
    (!text.is_empty()).then_some(text)
}

#[cfg(windows)]
fn wide(value: &str) -> Vec<u16> {
    value.encode_utf16().chain(std::iter::once(0)).collect()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_plain_host_port() {
        let spec = parse_proxy_url("127.0.0.1:8888").unwrap();
        assert_eq!(spec.host, "127.0.0.1");
        assert_eq!(spec.port, 8888);
        assert!(!spec.tls);
        assert!(spec.username.is_none());
    }

    #[test]
    fn parses_scheme_credentials_and_ipv6() {
        let spec = parse_proxy_url("http://user:pa%20ss@proxy.example:3128").unwrap();
        assert_eq!(spec.host, "proxy.example");
        assert_eq!(spec.port, 3128);
        assert_eq!(spec.username.as_deref(), Some("user"));
        assert_eq!(spec.password.as_deref(), Some("pa%20ss"));

        let v6 = parse_proxy_url("http://[::1]:8080").unwrap();
        assert_eq!(v6.host, "::1");
        assert_eq!(v6.port, 8080);
    }

    #[test]
    fn default_ports_follow_the_scheme() {
        assert_eq!(parse_proxy_url("http://proxy").unwrap().port, 80);
        let tls = parse_proxy_url("https://proxy").unwrap();
        assert_eq!(tls.port, 443);
        assert!(tls.tls);
    }

    #[test]
    fn rejects_nonsense() {
        assert!(parse_proxy_url("").is_none());
        assert!(parse_proxy_url("socks5://proxy:1080").is_none());
        assert!(parse_proxy_url("host:notaport").is_none());
    }

    #[test]
    fn env_precedence_and_all_proxy_fallback() {
        let env = |name: &str| match name {
            "HTTPS_PROXY" => Some("http://secure:8080".to_string()),
            "ALL_PROXY" => Some("http://fallback:3128".to_string()),
            "NO_PROXY" => Some("localhost, .example.com".to_string()),
            _ => None,
        };
        let settings = from_env_with(env).unwrap();
        // HTTPS_PROXY wins for https, ALL_PROXY covers http.
        assert_eq!(settings.https.as_ref().unwrap().port, 8080);
        assert_eq!(settings.http.as_ref().unwrap().port, 3128);
        assert_eq!(settings.no_proxy, vec!["localhost", ".example.com"]);
    }

    #[test]
    fn restricted_proxy_only_allows_listed_hosts() {
        let mut settings = ProxySettings {
            https: Some(parse_proxy_url("127.0.0.1:7897").unwrap()),
            restrict: true,
            ..ProxySettings::default()
        };
        assert!(!settings.allows_proxy("storage.googleapis.com"));
        settings.add_proxy_host("storage.googleapis.com");
        assert!(settings.allows_proxy("storage.googleapis.com"));
        assert!(settings.allows_proxy("cdn.storage.googleapis.com"));
        assert!(!settings.allows_proxy("github.com"));

        // Unrestricted settings keep the global behaviour.
        settings.restrict = false;
        settings.only_hosts.clear();
        assert!(settings.allows_proxy("github.com"));
    }

    #[test]
    fn no_proxy_matching() {
        let entries = vec![
            "localhost".to_string(),
            ".example.com".to_string(),
            "api.test:8443".to_string(),
            "<local>".to_string(),
        ];
        assert!(no_proxy_match("localhost", &entries));
        assert!(no_proxy_match("a.example.com", &entries));
        assert!(no_proxy_match("EXAMPLE.com.", &entries));
        assert!(no_proxy_match("api.test", &entries));
        assert!(no_proxy_match("printer", &entries)); // <local>: no dot
        assert!(!no_proxy_match("github.com", &entries));
        assert!(no_proxy_match("github.com", &["*".to_string()]));
    }

    #[test]
    fn wininet_server_forms() {
        let single = settings_from_system_parts("proxy:8080", "localhost;<local>");
        assert_eq!(single.http.as_ref().unwrap().port, 8080);
        assert_eq!(single.https.as_ref().unwrap().port, 8080);
        assert_eq!(single.no_proxy, vec!["localhost", "<local>"]);

        let per_scheme =
            settings_from_system_parts("http=http-proxy:3128;https=https-proxy:8443", "");
        assert_eq!(per_scheme.http.as_ref().unwrap().port, 3128);
        assert_eq!(per_scheme.https.as_ref().unwrap().port, 8443);
    }

    #[test]
    fn base64_matches_known_vectors() {
        assert_eq!(base64_encode(b""), "");
        assert_eq!(base64_encode(b"f"), "Zg==");
        assert_eq!(base64_encode(b"fo"), "Zm8=");
        assert_eq!(base64_encode(b"foo"), "Zm9v");
        assert_eq!(base64_encode(b"user:pass"), "dXNlcjpwYXNz");
    }
}
