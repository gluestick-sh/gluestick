//! Self-update *notification* only.
//!
//! Gluestick never downloads, installs or replaces its own executable. Scoop
//! does not update itself either; it tells the user which commands refresh the
//! checkout. Gluestick mirrors only that kind of *hint*: an opt-in endpoint
//! names the latest published version, and one line on stderr suggests running
//! a newer installer. Updating stays the installer's job.
//!
//! The check is off by default. `self_update_check_url` must be set before any
//! store, cache or network access happens, so a default installation makes no
//! outbound call at all. When the key is set, a cache in `<root>/cache` limits
//! the check to once per day, which is also what keeps the short fetch
//! timeouts from delaying more than the first command of the day.
//!
//! Comparison is intentionally not semver: Gluestick only needs to know
//! whether the published number is bigger than the running one.

use std::path::{Path, PathBuf};

use serde::{Deserialize, Serialize};

use crate::downloader::Downloader;
use crate::store::Store;

/// Cache file name inside `<root>/cache`.
const CACHE_FILE: &str = "self-update-check.json";

/// How long a cached answer is trusted before the endpoint is asked again.
///
/// One day matches the "check once per day" cost model in the module docs.
const CACHE_TTL_SECS: i64 = 24 * 60 * 60;

/// Largest response body considered. A version document is a few bytes; the
/// ceiling only exists so a wrong URL cannot stream a whole archive into
/// memory.
const MAX_BODY_BYTES: u64 = 64 * 1024;

/// The cached outcome of one update check.
///
/// Persisted so repeated CLI invocations do not hit the network: the first
/// command of the day pays for the request, every later command reads this.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct LatestVersion {
    pub latest: String,
}

/// A running version and a strictly newer published one.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct UpdateNotice {
    pub current: String,
    pub latest: String,
}

/// One persisted check: when it happened and what it found.
#[derive(Debug, Serialize, Deserialize)]
struct CacheFile {
    /// Unix seconds at which the endpoint answered.
    checked_at: i64,
    #[serde(flatten)]
    version: LatestVersion,
}

/// The running version, as compiled into the binary.
fn current_version() -> &'static str {
    env!("CARGO_PKG_VERSION")
}

/// `<root>/cache/self-update-check.json`.
fn cache_path(root: &Path) -> PathBuf {
    root.join("cache").join(CACHE_FILE)
}

/// Parse `"1.2.3"`, `"v1.2.3"`, `"1.2"` or `"1.2.3-beta1"` into a comparable
/// triple.
///
/// Deliberately not semver. Pre-release ordering, build metadata and the
/// meaning of a fourth component are all irrelevant to "is the published
/// number bigger than mine", so each dot-separated component contributes its
/// leading digits (`3-beta1` counts as `3`) and the scan stops at the first
/// component that does not start with a digit. Missing components are zero,
/// so `"1.2"` compares equal to `"1.2.0"`.
///
/// Returns `None` when the leading component is not numeric — an endpoint that
/// answers with HTML or an error message must not be mistaken for version 0.
pub fn parse_version(s: &str) -> Option<(u64, u64, u64)> {
    let s = s.trim();
    // One optional `v`/`V` prefix, as in `v1.2.3`.
    let s = s
        .strip_prefix('v')
        .or_else(|| s.strip_prefix('V'))
        .unwrap_or(s);
    let mut components = [0u64; 3];
    let mut parsed = 0usize;
    for part in s.split('.').take(3) {
        // Take the leading digit run: `3-beta1` is the third component of
        // `1.2.3-beta1`, while a component starting with a letter ends the
        // scan (`1.2.beta` is (1, 2, 0)).
        let digits: String = part.chars().take_while(char::is_ascii_digit).collect();
        match digits.parse::<u64>() {
            Ok(value) => {
                components[parsed] = value;
                parsed += 1;
            }
            Err(_) => break,
        }
    }
    // Nothing numeric before the first dot means this is not a version at all.
    if parsed == 0 {
        return None;
    }
    Some((components[0], components[1], components[2]))
}

/// Numeric-triple comparison, no new dependency.
///
/// A version that does not parse never counts as older: a false "a newer
/// version is available" line is worse than no line at all.
pub fn is_newer(latest: &str, current: &str) -> bool {
    match (parse_version(latest), parse_version(current)) {
        (Some(latest), Some(current)) => latest > current,
        _ => false,
    }
}

/// Best-effort check. Returns `None` when the check is disabled, the endpoint
/// is unreachable, nothing changed, or anything else goes wrong — it never
/// fails the caller.
///
/// `force` skips the 24-hour cache; the CLI passes `false`, so a day's worth
/// of commands share one request.
pub fn check(root: &Path, force: bool) -> Option<UpdateNotice> {
    // Step 1, before the store or the network: the URL is the opt-in. An
    // unset key means this build has no server to ask, so a default install
    // stays completely offline. `?` on the read also covers an unreadable
    // config: no notification is the safe answer either way.
    let url = crate::config::read_config_self_update_check_url(root).ok()?;
    let url = url.trim();
    if url.is_empty() {
        return None;
    }

    let now = unix_now();
    let latest = match read_cache(root) {
        Some((checked_at, cached)) if !force && now.saturating_sub(checked_at) < CACHE_TTL_SECS => {
            cached.latest
        }
        _ => {
            let latest = fetch_latest(root, url)?;
            // Best effort: an unwritable cache only means the next command
            // asks again, which is still correct.
            write_cache(
                root,
                &LatestVersion {
                    latest: latest.clone(),
                },
                now,
            );
            latest
        }
    };

    notice_for(latest)
}

/// The cached answer only, with no I/O beyond the cache file and no network.
///
/// `glue doctor` uses this so diagnosing a machine never adds a request; a
/// report can say what the last check found but must not perform a new one.
pub fn cached_notice(root: &Path) -> Option<UpdateNotice> {
    let (_, cached) = read_cache(root)?;
    notice_for(cached.latest)
}

/// Build a notice when `latest` is strictly newer than the running version.
fn notice_for(latest: String) -> Option<UpdateNotice> {
    let current = current_version();
    if is_newer(&latest, current) {
        Some(UpdateNotice {
            current: current.to_string(),
            latest,
        })
    } else {
        None
    }
}

/// Read the cache, if it exists and is readable.
fn read_cache(root: &Path) -> Option<(i64, LatestVersion)> {
    let data = std::fs::read(cache_path(root)).ok()?;
    let cached: CacheFile = serde_json::from_slice(&data).ok()?;
    if cached.version.latest.trim().is_empty() {
        return None;
    }
    Some((cached.checked_at, cached.version))
}

/// Persist one check. Errors are deliberately swallowed: a notification that
/// cannot be remembered must not become a command failure.
fn write_cache(root: &Path, version: &LatestVersion, checked_at: i64) {
    let dir = root.join("cache");
    if std::fs::create_dir_all(&dir).is_err() {
        return;
    }
    let Ok(bytes) = serde_json::to_vec(&CacheFile {
        checked_at,
        version: version.clone(),
    }) else {
        return;
    };
    let _ = crate::atomic::write_atomic(&dir.join(CACHE_FILE), &bytes);
}

/// Fetch the endpoint and extract a version string.
///
/// Uses the downloader's own text fetch so the request inherits the same
/// proxy route and short (3 s connect / 4 s body) timeouts as a download,
/// instead of building a second HTTP agent with different behaviour.
fn fetch_latest(root: &Path, url: &str) -> Option<String> {
    // `root.join("store")` mirrors `Services::new`; the store directory is
    // created here, but only after the key proved the check is enabled.
    let store = Store::new(root.join("store")).ok()?;
    let mut downloader = Downloader::new(store);
    downloader.set_proxy(crate::proxy::resolve(root));
    let body = downloader.fetch_text(url, MAX_BODY_BYTES).ok()?;
    parse_body(&body)
}

/// Extract the version from a response body.
///
/// JSON (`{"version":"1.2.3"}`) is the recommended shape because the endpoint
/// can grow fields later. A plain text file also works: its first non-empty
/// line is the version, so trailing newlines and a trailing comment line do
/// not confuse the parse.
fn parse_body(body: &str) -> Option<String> {
    let trimmed = body.trim();
    if trimmed.is_empty() {
        return None;
    }
    if let Ok(value) = serde_json::from_str::<serde_json::Value>(trimmed) {
        if let Some(version) = value.get("version").and_then(|v| v.as_str()) {
            let version = version.trim();
            if !version.is_empty() {
                return Some(version.to_string());
            }
        }
    }
    trimmed
        .lines()
        .map(str::trim)
        .find(|line| !line.is_empty())
        .map(str::to_string)
}

/// Seconds since the Unix epoch, or 0 when the clock is before it.
fn unix_now() -> i64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|elapsed| elapsed.as_secs().min(i64::MAX as u64) as i64)
        .unwrap_or(0)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::testutil::TempDir;

    /// Write the cache by hand so tests never depend on a network fetch.
    fn write_cache_at(root: &Path, checked_at: i64, latest: &str) {
        let dir = root.join("cache");
        std::fs::create_dir_all(&dir).unwrap();
        let body = format!(r#"{{"checked_at":{checked_at},"latest":"{latest}"}}"#);
        std::fs::write(dir.join(CACHE_FILE), body).unwrap();
    }

    #[test]
    fn parse_version_handles_plain_and_v_prefixed() {
        for value in ["1.2.3", "v1.2.3", "V1.2.3"] {
            assert_eq!(parse_version(value), Some((1, 2, 3)), "{value}");
        }
    }

    #[test]
    fn parse_version_pads_missing_components() {
        assert_eq!(parse_version("1"), Some((1, 0, 0)));
        assert_eq!(parse_version("1.2"), Some((1, 2, 0)));
        assert_eq!(parse_version("1.2.3-beta1"), Some((1, 2, 3)));
    }

    #[test]
    fn parse_version_rejects_non_numeric() {
        for value in ["", "beta", "v"] {
            assert_eq!(parse_version(value), None, "{value}");
        }
    }

    #[test]
    fn is_newer_compares_numerically() {
        assert!(is_newer("0.10.0", "0.9.9"));
        assert!(!is_newer("0.9.9", "0.10.0"));
        assert!(!is_newer("1.0.0", "1.0.0"));
    }

    /// The default must be silent *and* offline: no URL means the store is
    /// never opened and the cache is never written, which is only true if the
    /// check returns before either happens.
    #[test]
    fn disabled_url_makes_no_request() {
        let root = TempDir::new("glue-self-update-disabled");
        assert!(check(root.path(), true).is_none());
        assert!(!root.join("cache").join(CACHE_FILE).exists());
        assert!(!root.join("store").exists());
    }

    #[test]
    fn fresh_cache_reports_a_newer_version_without_network() {
        let root = TempDir::new("glue-self-update-fresh");
        // The URL only gets the check past the opt-in gate; a fresh cache must
        // answer without opening a connection (port 1 is never contacted).
        crate::config::write_config_self_update_check_url(root.path(), "http://127.0.0.1:1/")
            .unwrap();
        write_cache_at(root.path(), unix_now(), "99.0.0");

        let notice = check(root.path(), false).expect("a newer version is announced");
        assert_eq!(notice.latest, "99.0.0");
        assert_eq!(notice.current, env!("CARGO_PKG_VERSION"));

        // The running version, and anything older, is not news.
        write_cache_at(root.path(), unix_now(), env!("CARGO_PKG_VERSION"));
        assert!(check(root.path(), false).is_none());
        write_cache_at(root.path(), unix_now(), "0.0.1");
        assert!(check(root.path(), false).is_none());
    }

    /// A stale cache triggers a refresh. A URL that cannot even be parsed as
    /// an HTTP request fails before any socket is opened, so the test stays
    /// offline; the assertion is only that the caller gets `None`.
    #[test]
    fn stale_cache_is_ignored_offline() {
        let root = TempDir::new("glue-self-update-stale");
        crate::config::write_config_self_update_check_url(root.path(), "not a valid url").unwrap();
        write_cache_at(root.path(), unix_now() - 3 * 24 * 3600, "99.0.0");

        assert!(check(root.path(), false).is_none());
        // A failed refresh must not stamp the stale entry as fresh.
        let raw = std::fs::read_to_string(root.join("cache").join(CACHE_FILE)).unwrap();
        assert!(raw.contains("99.0.0"), "{raw}");
    }
}
