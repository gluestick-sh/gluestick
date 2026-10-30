//! Configuration management for the Gluestick engine.
//!
//! One JSON document named `config.json`, 2-space indented serialization with
//! `omitempty` semantics, and shallow map overrides keyed by normalized package
//! refs.
//!
//! Configuration belongs to the *program*, not the workspace: an installation
//! keeps its file beside `glue.exe`, so the CLI and the desktop app can be
//! installed, upgraded and removed independently while sharing one set of
//! packages. A standalone binary (a development build, a benchmark harness)
//! has no installation directory and keeps the file in the data root instead.
//! See [`config_dir`].

use std::collections::BTreeMap;
use std::path::{Path, PathBuf};

use serde::{Deserialize, Serialize};
use thiserror::Error;

/// The JSON filename inside the configuration directory.
pub const CONFIG_FILE: &str = "config.json";

/// Environment variable overriding the directory that holds `config.json`.
///
/// For development, tests and sandboxes: it wins over every other location,
/// including a product installation's own directory.
pub const CONFIG_DIR_ENV: &str = "GLUE_CONFIG_DIR";

/// Default background bucket check interval in minutes.
pub const DEFAULT_BUCKET_CHECK_INTERVAL_MINUTES: u64 = 15;

/// Supported bucket check intervals (minutes).
pub const ALLOWED_BUCKET_CHECK_INTERVALS: [u64; 3] = [5, 15, 30];

/// Bucket synchronization mode strings.
pub const BUCKET_SYNC_MODE_MANUAL: &str = "manual";
pub const BUCKET_SYNC_MODE_AUTO: &str = "auto";
pub const DEFAULT_BUCKET_SYNC_MODE: &str = BUCKET_SYNC_MODE_MANUAL;

/// Default ceiling (MiB) for direct streaming when the task can be cancelled.
/// At or below this size a CLI install takes the single-GET path instead of
/// probing for a resumable `.partial` download; larger files stay resumable.
pub const DEFAULT_CANCEL_DIRECT_MAX_MB: u64 = 16;

/// Largest allowed `cancel_direct_max_mb`. 0 disables direct streaming for
/// cancellable tasks, keeping every byte resumable.
pub const MAX_CANCEL_DIRECT_MAX_MB: u64 = 32;

/// Errors produced by config reads/writes.
#[derive(Debug, Error)]
pub enum ConfigError {
    #[error("read config: {0}")]
    Read(#[source] std::io::Error),
    #[error("parse config: {0}")]
    Parse(#[source] serde_json::Error),
    #[error("write config: {0}")]
    Write(#[source] std::io::Error),
    #[error("configuration location unavailable")]
    RootUnavailable,
    #[error("bucket name is required")]
    BucketNameRequired,
    #[error("package ref is required")]
    PackageRefRequired,
}

pub type Result<T> = std::result::Result<T, ConfigError>;

/// JSON structure of the config file.
#[derive(Debug, Default, Serialize, Deserialize)]
struct ConfigFile {
    #[serde(default, skip_serializing_if = "String::is_empty")]
    github_proxy: String,
    /// Opt-in endpoint that answers with the latest published version. Empty
    /// (the default) disables the update check entirely, so no request is
    /// made.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    self_update_check_url: String,
    /// HTTP proxy: empty/`auto` (environment then system), `direct`, or a URL.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    proxy: String,
    /// Packages whose downloads use `proxy`; every other download is direct.
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    proxy_packages: Vec<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    download_workers: Option<i64>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    bucket_check_interval_minutes: Option<i64>,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    bucket_sync_mode: String,
    #[serde(default, skip_serializing_if = "BTreeMap::is_empty")]
    bucket_descriptions: BTreeMap<String, String>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    hidden_catalog_packages: Vec<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    verbose: Option<bool>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    parallel_download: Option<bool>,
    /// MiB ceiling for the cancellable direct-stream path; 0 forces `.partial`.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    cancel_direct_max_mb: Option<i64>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    color: Option<bool>,
}

/// CLI-managed settings from config.json.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct Basics {
    pub github_proxy: String,
    /// URL of a version document for the self-update *notice*; empty means the
    /// check is off and nothing is ever fetched. See
    /// [`crate::self_update::check`].
    pub self_update_check_url: String,
    /// Packages whose downloads use `proxy`; empty sends every download direct.
    pub proxy_packages: Vec<String>,
    pub verbose: Option<bool>,
    pub parallel_download: Option<bool>,
    /// Parallel download connections (2–16); `None` keeps the default.
    pub download_workers: Option<i64>,
    /// MiB ceiling for the cancellable direct-stream path (0–32); `None`
    /// keeps the default (16). 0 forces every cancellable download through
    /// the resumable `.partial` path.
    pub cancel_direct_max_mb: Option<i64>,
    pub color: Option<bool>,
    /// `auto` syncs bucket checkouts on startup when the interval elapsed;
    /// empty means the default (`manual`).
    pub bucket_sync_mode: String,
    /// Minutes between automatic bucket checks (5/15/30 when set).
    pub bucket_check_interval_minutes: Option<i64>,
}

/// Clamp a bucket sync mode string to a supported value: `auto`
/// (case-insensitive, trimmed) or `manual` (default).
pub fn normalize_bucket_sync_mode(mode: &str) -> &'static str {
    match mode.trim().to_lowercase().as_str() {
        "auto" => BUCKET_SYNC_MODE_AUTO,
        _ => BUCKET_SYNC_MODE_MANUAL,
    }
}

/// Clamp to a supported bucket check interval, defaulting to 15 minutes.
pub fn normalize_bucket_check_interval(minutes: u64) -> u64 {
    if ALLOWED_BUCKET_CHECK_INTERVALS.contains(&minutes) {
        minutes
    } else {
        DEFAULT_BUCKET_CHECK_INTERVAL_MINUTES
    }
}

/// Path to the active `config.json`.
///
/// Resolved through [`config_dir`]: the running program's installation
/// directory, or `root_dir` for a standalone binary.
pub fn path(root_dir: &Path) -> PathBuf {
    config_dir(root_dir).join(CONFIG_FILE)
}

/// Alias for [`path`].
pub fn config_path(root_dir: &Path) -> PathBuf {
    path(root_dir)
}

/// Directory holding the active `config.json`.
///
/// Precedence: `GLUE_CONFIG_DIR`, then the running executable's installation
/// directory (`glue.exe` beside the installer's `.glue-install` marker), then
/// `root_dir`. The installation wins over the workspace because configuration
/// describes the program that edits it; deleting or reinstalling one product
/// must not silently change the other's settings.
pub fn config_dir(root_dir: &Path) -> PathBuf {
    config_dir_in(
        root_dir,
        env_config_dir().as_deref(),
        crate::paths::installed_data_root().as_deref(),
    )
}

/// [`config_dir`] with every environment read passed in, so the precedence is
/// testable without mutating process state.
fn config_dir_in(root_dir: &Path, env_dir: Option<&Path>, install_dir: Option<&Path>) -> PathBuf {
    if let Some(dir) = env_dir {
        return dir.to_path_buf();
    }
    if let Some(dir) = install_dir {
        return dir.to_path_buf();
    }
    root_dir.to_path_buf()
}

fn env_config_dir() -> Option<PathBuf> {
    std::env::var_os(CONFIG_DIR_ENV)
        .filter(|value| !value.is_empty())
        .map(PathBuf::from)
}

fn read_config_file(root_dir: &Path) -> Result<ConfigFile> {
    if root_dir.as_os_str().is_empty() {
        return Ok(ConfigFile::default());
    }
    let primary = path(root_dir);
    let legacy = root_dir.join(CONFIG_FILE);
    read_config_file_from(&primary, (legacy != primary).then_some(legacy.as_path()))
}

/// Reads `primary`, falling back to the workspace copy when it is missing.
///
/// Configuration used to live in the workspace. An installation that has not
/// written its own file yet still reads the old one, and the next write lands
/// beside the program, where it belongs.
fn read_config_file_from(primary: &Path, legacy: Option<&Path>) -> Result<ConfigFile> {
    match std::fs::read(primary) {
        Ok(data) => serde_json::from_slice(&data).map_err(ConfigError::Parse),
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => {
            if let Some(legacy) = legacy {
                match std::fs::read(legacy) {
                    Ok(data) => return serde_json::from_slice(&data).map_err(ConfigError::Parse),
                    Err(e) if e.kind() == std::io::ErrorKind::NotFound => {}
                    Err(e) => return Err(ConfigError::Read(e)),
                }
            }
            Ok(ConfigFile::default())
        }
        Err(e) => Err(ConfigError::Read(e)),
    }
}

fn write_config_file(root_dir: &Path, cfg: &ConfigFile) -> Result<()> {
    if root_dir.as_os_str().is_empty() {
        return Err(ConfigError::RootUnavailable);
    }
    let file = path(root_dir);
    if let Some(dir) = file.parent() {
        std::fs::create_dir_all(dir).map_err(ConfigError::Write)?;
    }
    // Json.MarshalIndent(cfg, "", " ") — 2 spaces, no trailing newline.
    let data = serde_json::to_string_pretty(cfg).expect("config serialization is infallible");
    crate::atomic::write_atomic(&file, data.as_bytes()).map_err(ConfigError::Write)
}

/// Read CLI-managed settings from config.json.
pub fn read_basics(root_dir: &Path) -> Result<Basics> {
    let cfg = read_config_file(root_dir)?;
    Ok(Basics {
        github_proxy: cfg.github_proxy,
        self_update_check_url: cfg.self_update_check_url,
        proxy_packages: cfg.proxy_packages,
        verbose: cfg.verbose,
        parallel_download: cfg.parallel_download,
        download_workers: cfg.download_workers,
        cancel_direct_max_mb: cfg.cancel_direct_max_mb,
        color: cfg.color,
        bucket_sync_mode: cfg.bucket_sync_mode,
        bucket_check_interval_minutes: cfg.bucket_check_interval_minutes,
    })
}

/// Update CLI-managed settings while preserving other config keys.
pub fn write_basics(root_dir: &Path, basics: &Basics) -> Result<()> {
    let mut cfg = read_config_file(root_dir)?;
    cfg.github_proxy = basics.github_proxy.clone();
    cfg.self_update_check_url = basics.self_update_check_url.clone();
    cfg.proxy_packages = basics.proxy_packages.clone();
    cfg.verbose = basics.verbose;
    cfg.parallel_download = basics.parallel_download;
    cfg.download_workers = basics.download_workers;
    cfg.cancel_direct_max_mb = basics
        .cancel_direct_max_mb
        .map(|mb| mb.clamp(0, MAX_CANCEL_DIRECT_MAX_MB as i64));
    cfg.color = basics.color;
    cfg.bucket_sync_mode = if basics.bucket_sync_mode.trim().is_empty() {
        String::new()
    } else {
        normalize_bucket_sync_mode(&basics.bucket_sync_mode).to_string()
    };
    cfg.bucket_check_interval_minutes = basics
        .bucket_check_interval_minutes
        .map(|minutes| normalize_bucket_check_interval(minutes.max(0) as u64) as i64);
    write_config_file(root_dir, &cfg)
}

/// Read `verbose` from config.json (default false).
pub fn read_config_verbose(root_dir: &Path) -> Result<bool> {
    Ok(read_config_file(root_dir)?.verbose.unwrap_or(false))
}

/// Read `github_proxy` from config.json (not env).
pub fn read_config_github_proxy(root_dir: &Path) -> Result<String> {
    Ok(read_config_file(root_dir)?.github_proxy)
}

/// Update `github_proxy` in config.json. Empty value clears the setting.
pub fn write_config_github_proxy(root_dir: &Path, value: &str) -> Result<()> {
    let mut cfg = read_config_file(root_dir)?;
    cfg.github_proxy = value.to_string();
    write_config_file(root_dir, &cfg)
}

/// Read `self_update_check_url` from config.json.
///
/// Empty (or missing) means the self-update notice is disabled, which is the
/// default: no URL, no network call.
pub fn read_config_self_update_check_url(root_dir: &Path) -> Result<String> {
    Ok(read_config_file(root_dir)?.self_update_check_url)
}

/// Update `self_update_check_url` in config.json. Empty value clears the
/// setting and turns the update check off again.
pub fn write_config_self_update_check_url(root_dir: &Path, value: &str) -> Result<()> {
    let mut cfg = read_config_file(root_dir)?;
    cfg.self_update_check_url = value.to_string();
    write_config_file(root_dir, &cfg)
}

/// Read `download_workers` from config.json when set.
pub fn read_config_download_workers(root_dir: &Path) -> Result<Option<i64>> {
    Ok(read_config_file(root_dir)?.download_workers)
}

/// Read `cancel_direct_max_mb` from config.json when set.
pub fn read_config_cancel_direct_max_mb(root_dir: &Path) -> Result<Option<i64>> {
    Ok(read_config_file(root_dir)?.cancel_direct_max_mb)
}

/// Update `cancel_direct_max_mb`; `None` clears it (back to the default).
pub fn write_config_cancel_direct_max_mb(root_dir: &Path, value: Option<i64>) -> Result<()> {
    let mut cfg = read_config_file(root_dir)?;
    cfg.cancel_direct_max_mb = value.map(|mb| mb.clamp(0, MAX_CANCEL_DIRECT_MAX_MB as i64));
    write_config_file(root_dir, &cfg)
}

/// Read the HTTP proxy setting: empty means `auto` (environment, then the
/// Windows system proxy), `direct` disables it, anything else is a proxy URL.
pub fn read_config_proxy(root_dir: &Path) -> Result<String> {
    Ok(read_config_file(root_dir)?.proxy)
}

/// Update the HTTP proxy setting. Empty clears it (back to `auto`).
pub fn write_config_proxy(root_dir: &Path, value: &str) -> Result<()> {
    let mut cfg = read_config_file(root_dir)?;
    cfg.proxy = value.trim().to_string();
    write_config_file(root_dir, &cfg)
}

/// Packages whose downloads must use the `proxy` setting.
///
/// Entries may be a bare package name (`claude-code`) or a bucket-qualified
/// ref (`main/claude-code`); both match the same package. Empty (the default)
/// keeps the proxy global.
pub fn read_config_proxy_packages(root_dir: &Path) -> Result<Vec<String>> {
    let cfg = read_config_file(root_dir)?;
    let mut seen = std::collections::BTreeSet::new();
    let mut out = Vec::new();
    for entry in cfg.proxy_packages {
        let key = short_package_name(&entry);
        if !key.is_empty() && seen.insert(key.clone()) {
            out.push(key);
        }
    }
    Ok(out)
}

/// The package part of a bare name or `bucket/name` ref, lowercased.
fn short_package_name(value: &str) -> String {
    value
        .rsplit('/')
        .next()
        .unwrap_or(value)
        .trim()
        .to_lowercase()
}

/// Whether `package` (a name or `bucket/name` ref) is listed in `entries`.
pub fn package_uses_proxy(entries: &[String], package: &str) -> bool {
    let name = short_package_name(package);
    !name.is_empty()
        && entries
            .iter()
            .any(|entry| short_package_name(entry) == name)
}

/// Update `download_workers` in config.json.
pub fn write_config_download_workers(root_dir: &Path, workers: i64) -> Result<()> {
    let mut cfg = read_config_file(root_dir)?;
    cfg.download_workers = Some(workers);
    write_config_file(root_dir, &cfg)
}

/// Read `parallel_download` from config.json (enabled when unset).
pub fn read_config_parallel_download(root_dir: &Path) -> Result<bool> {
    Ok(read_config_file(root_dir)?
        .parallel_download
        .unwrap_or(true))
}

/// Read `bucket_check_interval_minutes` from config.json when set (normalized).
pub fn read_config_bucket_check_interval(root_dir: &Path) -> Result<Option<u64>> {
    match read_config_file(root_dir)?.bucket_check_interval_minutes {
        Some(m) => Ok(Some(normalize_bucket_check_interval(m.max(0) as u64))),
        None => Ok(None),
    }
}

/// Update `bucket_check_interval_minutes` in config.json (normalized).
pub fn write_config_bucket_check_interval(root_dir: &Path, minutes: u64) -> Result<()> {
    let mut cfg = read_config_file(root_dir)?;
    cfg.bucket_check_interval_minutes = Some(normalize_bucket_check_interval(minutes) as i64);
    write_config_file(root_dir, &cfg)
}

/// Read `bucket_sync_mode` from config.json when set (normalized).
pub fn read_config_bucket_sync_mode(root_dir: &Path) -> Result<Option<&'static str>> {
    let cfg = read_config_file(root_dir)?;
    if cfg.bucket_sync_mode.trim().is_empty() {
        return Ok(None);
    }
    Ok(Some(normalize_bucket_sync_mode(&cfg.bucket_sync_mode)))
}

/// Update `bucket_sync_mode` in config.json (normalized).
pub fn write_config_bucket_sync_mode(root_dir: &Path, mode: &str) -> Result<()> {
    let mut cfg = read_config_file(root_dir)?;
    cfg.bucket_sync_mode = normalize_bucket_sync_mode(mode).to_string();
    write_config_file(root_dir, &cfg)
}

/// Whether an automatic bucket refresh is due: `bucket_sync_mode` is `auto`
/// and the configured interval has elapsed since the last attempt.
///
/// Shared by the CLI startup path and the desktop scheduler so the two agree
/// on the interval and on the meaning of the stamp file.
pub fn bucket_sync_due(root_dir: &Path) -> bool {
    let mode = read_config_bucket_sync_mode(root_dir).ok().flatten();
    if mode != Some(BUCKET_SYNC_MODE_AUTO) {
        return false;
    }
    let interval = read_config_bucket_check_interval(root_dir)
        .ok()
        .flatten()
        .unwrap_or(DEFAULT_BUCKET_CHECK_INTERVAL_MINUTES);
    let last = std::fs::read_to_string(bucket_check_stamp_path(root_dir))
        .ok()
        .and_then(|text| text.trim().parse::<u64>().ok())
        .unwrap_or(0);
    now_secs().saturating_sub(last) >= interval.saturating_mul(60)
}

/// Record that a bucket refresh was attempted now.
///
/// Stamped even when a pull failed: the interval bounds how often a broken
/// remote is retried, instead of every later command retrying it.
pub fn stamp_bucket_check(root_dir: &Path) {
    let path = bucket_check_stamp_path(root_dir);
    if let Some(dir) = path.parent() {
        let _ = std::fs::create_dir_all(dir);
    }
    let _ = crate::atomic::write_atomic(&path, now_secs().to_string().as_bytes());
}

/// `<root>/cache/bucket_last_check` — the auto-sync interval's clock.
fn bucket_check_stamp_path(root_dir: &Path) -> PathBuf {
    root_dir.join("cache").join("bucket_last_check")
}

fn now_secs() -> u64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|elapsed| elapsed.as_secs())
        .unwrap_or(0)
}

/// Read user-defined bucket descriptions from config.json.
pub fn read_config_bucket_descriptions(root_dir: &Path) -> Result<BTreeMap<String, String>> {
    let cfg = read_config_file(root_dir)?;
    let mut out = BTreeMap::new();
    for (name, desc) in cfg.bucket_descriptions {
        let name = name.trim().to_string();
        let desc = desc.trim().to_string();
        if name.is_empty() || desc.is_empty() {
            continue;
        }
        out.insert(name, desc);
    }
    Ok(out)
}

/// Store or clear a bucket description override in config.json.
pub fn set_config_bucket_description(root_dir: &Path, name: &str, description: &str) -> Result<()> {
    let name = name.trim();
    if name.is_empty() {
        return Err(ConfigError::BucketNameRequired);
    }
    let description = description.trim();
    let mut cfg = read_config_file(root_dir)?;
    if description.is_empty() {
        cfg.bucket_descriptions.remove(name);
    } else {
        cfg.bucket_descriptions
            .insert(name.to_string(), description.to_string());
    }
    write_config_file(root_dir, &cfg)
}

/// Canonicalize a catalog package ref to `bucket/name` (defaults to `main`).
pub fn normalize_catalog_package_ref(pkg_ref: &str) -> String {
    let key = pkg_ref.trim().to_lowercase();
    if key.is_empty() {
        return String::new();
    }
    if !key.contains('/') {
        format!("main/{key}")
    } else {
        key
    }
}

/// Read user-hidden catalog package refs (`bucket/name`).
pub fn read_config_hidden_catalog_packages(root_dir: &Path) -> Result<Vec<String>> {
    let cfg = read_config_file(root_dir)?;
    let mut seen = std::collections::BTreeSet::new();
    let mut out = Vec::new();
    for ref_ in cfg.hidden_catalog_packages {
        let key = normalize_catalog_package_ref(&ref_);
        if !key.is_empty() && seen.insert(key.clone()) {
            out.push(key);
        }
    }
    Ok(out)
}

/// Record a catalog package ref to hide from browse/search.
pub fn add_config_hidden_catalog_package(root_dir: &Path, pkg_ref: &str) -> Result<()> {
    let key = normalize_catalog_package_ref(pkg_ref);
    if key.is_empty() {
        return Err(ConfigError::PackageRefRequired);
    }
    let mut cfg = read_config_file(root_dir)?;
    let already = cfg
        .hidden_catalog_packages
        .iter()
        .any(|existing| normalize_catalog_package_ref(existing) == key);
    if !already {
        cfg.hidden_catalog_packages.push(key);
    }
    write_config_file(root_dir, &cfg)
}

// ---------------------------------------------------------------------------
// GitHub mirror support (github_mirror.go)
// ---------------------------------------------------------------------------

/// GitHub mirror prefixes from env or config.
/// Precedence: `GITHUB_PROXY` env, then config.json `github_proxy`.
///
/// With nothing stored, the built-in mirror list is used: mirrors are on by
/// default. The [`DIRECT_PROXY_MARKER`] value is an explicit opt-out and keeps
/// downloads on the direct GitHub URL.
pub fn load_proxies(root_dir: &Path) -> Vec<String> {
    let env = env_github_proxy();
    if !env.is_empty() {
        return split_proxies(&env);
    }
    match read_config_file(root_dir) {
        Ok(cfg) if !cfg.github_proxy.trim().is_empty() => split_proxies(&cfg.github_proxy),
        _ => split_proxies(&default_proxy_list()),
    }
}

/// The built-in GitHub mirrors as a config value, in preference order.
///
/// This is what the settings dialog offers as "restore defaults" and what
/// [`load_proxies`] falls back to when no preference is stored.
pub fn default_proxy_list() -> String {
    PROXY_PROBE_CANDIDATES.join(",")
}

/// `GITHUB_PROXY` from the environment (trimmed).
pub fn env_github_proxy() -> String {
    std::env::var("GITHUB_PROXY")
        .unwrap_or_default()
        .trim()
        .to_string()
}

/// Parse a comma-separated proxy list (+ trim + drop empty).
///
/// The [`DIRECT_PROXY_MARKER`] token means "no proxy" — it is how the first-run
/// detector records that direct access won, without re-probing on every run.
pub fn split_proxies(value: &str) -> Vec<String> {
    value
        .split(',')
        .map(str::trim)
        .filter(|s| !s.is_empty() && !s.eq_ignore_ascii_case(DIRECT_PROXY_MARKER))
        .map(String::from)
        .collect()
}

/// Config value that means "no proxy": the first-run detector stores it when
/// direct GitHub access wins, so the probe does not run again.
pub const DIRECT_PROXY_MARKER: &str = "direct";

/// Proxy prefixes measured by the first-run detector. Order only breaks ties;
/// the fastest candidate is stored in `github_proxy`.
pub const PROXY_PROBE_CANDIDATES: &[&str] = &[
    "https://gh-proxy.com",
    "https://ghfast.top",
    "https://ghfile.geekertao.top",
    "https://ghproxy.net",
];

/// Small, stable GitHub asset (~600 KB) used to measure each candidate.
///
/// Reuses [`crate::bootstrap::INNOUNP_URL`] rather than carrying its own copy:
/// the two have to stay in step, and a probe URL that 404s because upstream
/// moved the file would make every mirror look equally broken. `bootstrap`
/// pins it to a commit, so it does not drift.
pub const PROXY_PROBE_URL: &str = crate::bootstrap::INNOUNP_URL;

/// Report whether `url` is a GitHub-hosted HTTPS asset that can be mirrored.
pub fn is_github_url(url: &str) -> bool {
    const PREFIXES: &[&str] = &[
        "https://github.com/",
        "https://raw.githubusercontent.com/",
        "https://objects.githubusercontent.com/",
        "https://codeload.github.com/",
    ];
    PREFIXES.iter().any(|prefix| url.starts_with(prefix))
}

/// URLs to try for downloading `url`: each proxy prefix prepended to the full
/// GitHub URL. Non-GitHub URLs are returned unchanged. An empty proxy prefix
/// means the original URL (direct).
pub fn mirror_urls(url: &str, proxies: &[String]) -> Vec<String> {
    if !is_github_url(url) || proxies.is_empty() {
        return vec![url.to_string()];
    }
    let mut seen = std::collections::BTreeSet::new();
    let mut out = Vec::new();
    for proxy in proxies {
        let candidate = if proxy.is_empty() {
            url.to_string()
        } else {
            format!("{}/{url}", proxy.trim_end_matches('/'))
        };
        if seen.insert(candidate.clone()) {
            out.push(candidate);
        }
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::testutil::TempDir;

    fn temp_root(tag: &str) -> TempDir {
        TempDir::new(&format!("glue-config-test-{tag}"))
    }

    #[test]
    fn config_dir_precedence_is_env_then_installation_then_root() {
        let root = Path::new(r"C:\data\.glue");
        let env = Path::new(r"C:\env-config");
        let install = Path::new(r"C:\Programs\Gluestick CLI");

        assert_eq!(config_dir_in(root, Some(env), Some(install)), env);
        assert_eq!(config_dir_in(root, None, Some(install)), install);
        assert_eq!(config_dir_in(root, None, None), root);
    }

    #[test]
    fn a_legacy_workspace_config_is_read_until_the_program_writes_one() {
        let root = temp_root("legacy-config");
        let program = temp_root("legacy-program");
        let primary = program.join(CONFIG_FILE);
        let legacy = root.join(CONFIG_FILE);
        std::fs::write(&legacy, r#"{"github_proxy":"https://legacy.example"}"#).unwrap();

        let cfg = read_config_file_from(&primary, Some(&legacy)).unwrap();
        assert_eq!(cfg.github_proxy, "https://legacy.example");

        // Once the program has its own file, that one wins.
        std::fs::write(&primary, r#"{"github_proxy":"https://program.example"}"#).unwrap();
        let cfg = read_config_file_from(&primary, Some(&legacy)).unwrap();
        assert_eq!(cfg.github_proxy, "https://program.example");
    }

    #[test]
    fn normalize_sync_mode() {
        assert_eq!(normalize_bucket_sync_mode("auto"), "auto");
        assert_eq!(normalize_bucket_sync_mode(" AUTO "), "auto");
        assert_eq!(normalize_bucket_sync_mode(""), "manual");
        assert_eq!(normalize_bucket_sync_mode("bogus"), "manual");
    }

    #[test]
    fn normalize_interval() {
        assert_eq!(normalize_bucket_check_interval(5), 5);
        assert_eq!(normalize_bucket_check_interval(15), 15);
        assert_eq!(normalize_bucket_check_interval(30), 30);
        assert_eq!(normalize_bucket_check_interval(7), 15);
        assert_eq!(normalize_bucket_check_interval(0), 15);
    }

    #[test]
    fn catalog_ref_defaults_bucket() {
        assert_eq!(normalize_catalog_package_ref("git"), "main/git");
        assert_eq!(normalize_catalog_package_ref("Extras/VLC"), "extras/vlc");
        assert_eq!(normalize_catalog_package_ref(" "), "");
    }

    #[test]
    fn basics_roundtrip_preserves_other_keys() {
        let root = temp_root("basics");
        std::fs::write(
            path(&root),
            r#"{"github_proxy":"https://mirror.example","bucket_sync_mode":"auto","verbose":true}"#,
        )
        .unwrap();
        let basics = read_basics(&root).unwrap();
        assert_eq!(basics.github_proxy, "https://mirror.example");
        assert_eq!(basics.verbose, Some(true));

        // A CLI settings write starts from the parsed basics (as `config set`
        // does), so unrelated keys such as `bucket_sync_mode` survive.
        let mut out = read_basics(&root).unwrap();
        out.github_proxy = String::new();
        out.verbose = Some(false);
        out.parallel_download = None;
        out.color = Some(true);
        write_basics(&root, &out).unwrap();
        let raw = std::fs::read_to_string(path(&root)).unwrap();
        // github_proxy cleared (omitempty), bucket_sync_mode preserved.
        assert!(!raw.contains("github_proxy"), "raw = {raw}");
        assert!(
            raw.contains("\"bucket_sync_mode\": \"auto\""),
            "raw = {raw}"
        );
        assert!(raw.contains("\"verbose\": false"), "raw = {raw}");
        assert!(raw.contains("\"color\": true"), "raw = {raw}");
        assert!(!raw.contains("parallel_download"), "raw = {raw}");
    }

    #[test]
    fn cancel_direct_max_mb_roundtrip_and_clamps() {
        let root = temp_root("cancel-direct");
        let mut basics = read_basics(&root).unwrap();
        basics.cancel_direct_max_mb = Some(16);
        write_basics(&root, &basics).unwrap();
        assert_eq!(read_config_cancel_direct_max_mb(&root).unwrap(), Some(16));
        let raw = std::fs::read_to_string(path(&root)).unwrap();
        assert!(raw.contains("\"cancel_direct_max_mb\": 16"), "raw = {raw}");

        // 0 disables the direct path; anything above the cap is clamped.
        basics.cancel_direct_max_mb = Some(0);
        write_basics(&root, &basics).unwrap();
        assert_eq!(read_config_cancel_direct_max_mb(&root).unwrap(), Some(0));
        basics.cancel_direct_max_mb = Some(999);
        write_basics(&root, &basics).unwrap();
        assert_eq!(
            read_config_cancel_direct_max_mb(&root).unwrap(),
            Some(MAX_CANCEL_DIRECT_MAX_MB as i64)
        );
    }

    #[test]
    fn cancel_direct_max_mb_helper_writes_and_clears() {
        let root = temp_root("cancel-direct-helper");
        write_config_cancel_direct_max_mb(&root, Some(8)).unwrap();
        assert_eq!(read_config_cancel_direct_max_mb(&root).unwrap(), Some(8));
        write_config_cancel_direct_max_mb(&root, Some(999)).unwrap();
        assert_eq!(
            read_config_cancel_direct_max_mb(&root).unwrap(),
            Some(MAX_CANCEL_DIRECT_MAX_MB as i64)
        );
        write_config_cancel_direct_max_mb(&root, None).unwrap();
        assert_eq!(read_config_cancel_direct_max_mb(&root).unwrap(), None);
    }

    #[test]
    fn missing_file_reads_as_empty() {
        let root = temp_root("missing");
        assert_eq!(read_config_github_proxy(&root).unwrap(), "");
        assert_eq!(read_config_download_workers(&root).unwrap(), None);
        assert!(!read_config_verbose(&root).unwrap());
    }

    #[test]
    fn download_workers_roundtrip() {
        let root = temp_root("workers");
        write_config_download_workers(&root, 6).unwrap();
        assert_eq!(read_config_download_workers(&root).unwrap(), Some(6));
    }

    #[test]
    fn bucket_interval_normalized_on_rw() {
        let root = temp_root("interval");
        write_config_bucket_check_interval(&root, 99).unwrap();
        assert_eq!(read_config_bucket_check_interval(&root).unwrap(), Some(15));
    }

    #[test]
    fn bucket_sync_mode_normalized_on_rw() {
        let root = temp_root("syncmode");
        assert_eq!(read_config_bucket_sync_mode(&root).unwrap(), None);
        write_config_bucket_sync_mode(&root, "AUTO").unwrap();
        assert_eq!(read_config_bucket_sync_mode(&root).unwrap(), Some("auto"));
        write_config_bucket_sync_mode(&root, "junk").unwrap();
        assert_eq!(read_config_bucket_sync_mode(&root).unwrap(), Some("manual"));
    }

    #[test]
    fn bucket_descriptions_set_and_clear() {
        let root = temp_root("bucketdesc");
        set_config_bucket_description(&root, "main", "Official packages").unwrap();
        let descs = read_config_bucket_descriptions(&root).unwrap();
        assert_eq!(descs.get("main").unwrap(), "Official packages");
        set_config_bucket_description(&root, "main", "").unwrap();
        assert!(read_config_bucket_descriptions(&root).unwrap().is_empty());
        assert!(matches!(
            set_config_bucket_description(&root, "  ", "x"),
            Err(ConfigError::BucketNameRequired)
        ));
    }

    #[test]
    fn hidden_catalog_packages_dedupe() {
        let root = temp_root("hidden");
        add_config_hidden_catalog_package(&root, "git").unwrap();
        add_config_hidden_catalog_package(&root, "Main/Git").unwrap();
        add_config_hidden_catalog_package(&root, "extras/vlc").unwrap();
        let hidden = read_config_hidden_catalog_packages(&root).unwrap();
        assert_eq!(hidden, vec!["main/git", "extras/vlc"]);
        assert!(matches!(
            add_config_hidden_catalog_package(&root, " "),
            Err(ConfigError::PackageRefRequired)
        ));
    }

    #[test]
    fn proxy_packages_are_normalized_and_match_short_names() {
        let root = temp_root("proxypkgs");
        std::fs::write(
            path(&root),
            r#"{"proxy":"http://127.0.0.1:7897","proxy_packages":["Claude-Code"," main/claude-code ",""]}"#,
        )
        .unwrap();
        let packages = read_config_proxy_packages(&root).unwrap();
        assert_eq!(packages, vec!["claude-code"]);
        assert!(package_uses_proxy(&packages, "claude-code"));
        assert!(package_uses_proxy(&packages, "main/claude-code"));
        assert!(!package_uses_proxy(&packages, "git"));
    }

    /// The first-run GitHub probe saves `github_proxy` through `write_basics`;
    /// that rewrite must not drop the package proxy list.
    #[test]
    fn writing_basics_preserves_proxy_packages() {
        let root = temp_root("proxypkgs-basics");
        std::fs::write(
            path(&root),
            r#"{"proxy":"http://127.0.0.1:7897","proxy_packages":["claude-code"]}"#,
        )
        .unwrap();
        let mut basics = read_basics(&root).unwrap();
        basics.github_proxy = "direct".to_string();
        write_basics(&root, &basics).unwrap();

        let raw = std::fs::read_to_string(path(&root)).unwrap();
        assert!(raw.contains("\"github_proxy\": \"direct\""), "raw = {raw}");
        assert_eq!(
            read_config_proxy_packages(&root).unwrap(),
            vec!["claude-code"]
        );
    }

    #[test]
    fn split_proxies_full_split() {
        // Strings.Split: every comma separates; empties dropped after trim.
        assert_eq!(
            split_proxies("https://a, https://b ,https://c"),
            vec!["https://a", "https://b", "https://c"]
        );
        assert_eq!(
            split_proxies("https://a,,https://b"),
            vec!["https://a", "https://b"]
        );
        assert!(split_proxies("").is_empty());
        assert!(split_proxies(" , ").is_empty());
    }

    #[test]
    fn github_url_detection() {
        assert!(is_github_url("https://github.com/a/b"));
        assert!(is_github_url("https://raw.githubusercontent.com/a/b"));
        assert!(is_github_url("https://objects.githubusercontent.com/a"));
        assert!(is_github_url("https://codeload.github.com/a/b"));
        assert!(!is_github_url("https://example.com/"));
        assert!(!is_github_url("http://github.com/a"));
    }

    #[test]
    fn split_proxies_drops_the_direct_marker() {
        // The first-run detector stores `direct` when no mirror is worth it.
        assert!(split_proxies(DIRECT_PROXY_MARKER).is_empty());
        assert!(split_proxies("  DIRECT  ").is_empty());
        assert_eq!(
            split_proxies("https://a.example, direct ,https://b.example"),
            vec![
                "https://a.example".to_string(),
                "https://b.example".to_string()
            ]
        );
    }

    #[test]
    fn mirror_url_expansion() {
        let url = "https://github.com/a/b/releases/x.zip";
        assert_eq!(mirror_urls(url, &[]), vec![url]);
        assert_eq!(
            mirror_urls(
                url,
                &[
                    "https://m1.example/".to_string(),
                    "https://m2.example".to_string()
                ]
            ),
            vec![
                "https://m1.example/"
                    .to_string()
                    .trim_end_matches('/')
                    .to_string()
                    + "/"
                    + url,
                "https://m2.example/"
                    .to_string()
                    .trim_end_matches('/')
                    .to_string()
                    + "/"
                    + url
            ]
        );
        // Empty prefix = direct URL; duplicates dropped.
        assert_eq!(
            mirror_urls(url, &[String::new(), String::new()]),
            vec![url.to_string()]
        );
        // Non-GitHub unchanged.
        let other = "https://example.com/x.zip";
        assert_eq!(mirror_urls(other, &["https://m".to_string()]), vec![other]);
    }
}
