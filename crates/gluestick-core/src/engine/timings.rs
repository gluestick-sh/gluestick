//! Per-phase install timings.
//!
//! Enabled with `glue install --timings`: every install then emits one
//! machine-readable line after the package finishes
//!
//! ```text
//! GLUE_TIMINGS download_ms=... cas_ms=... extract_ms=... link_ms=... shim_ms=... cache_ms=... bootstrap_ms=... total_ms=...
//! ```
//!
//! `cas_ms` is the legacy field name for content-store ingest time. The field
//! order is stable because external benchmark scripts parse by position — see
//! [`parse_line`], which.
//!
//! When timings are off the whole thing costs one `Option::is_none` check per
//! call site (nil-receiver methods do the same).

use std::time::{Duration, Instant};

use crate::downloader::Timing;

/// Which bucket a measured span belongs to.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Bucket {
    /// Archive/script extraction into the content store or install dir.
    Extract,
    /// Hardlinking store objects into `apps/<pkg>/<ver>`.
    Link,
    /// `shim.exe` + shim metadata creation.
    Shim,
    /// Install-record / cache index write.
    Cache,
    /// On-demand helper setup (7z, dark, innounp, git).
    Bootstrap,
}

/// Wall-clock time per install phase for one package
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct PhaseTimings {
    pub download: Duration,
    pub store_ingest: Duration,
    pub extract: Duration,
    pub link: Duration,
    pub shim: Duration,
    pub cache: Duration,
    pub bootstrap: Duration,
}

impl PhaseTimings {
    /// Sum of the buckets (not necessarily the end-to-end wall time).
    pub fn total(&self) -> Duration {
        self.download
            + self.store_ingest
            + self.extract
            + self.link
            + self.shim
            + self.cache
            + self.bootstrap
    }

    /// The `GLUE_TIMINGS ...` line, field order.
    pub fn line(&self) -> String {
        format!(
            "GLUE_TIMINGS download_ms={} cas_ms={} extract_ms={} link_ms={} shim_ms={} cache_ms={} bootstrap_ms={} total_ms={}",
            self.download.as_millis(),
            self.store_ingest.as_millis(),
            self.extract.as_millis(),
            self.link.as_millis(),
            self.shim.as_millis(),
            self.cache.as_millis(),
            self.bootstrap.as_millis(),
            self.total().as_millis(),
        )
    }

    fn bucket_mut(&mut self, bucket: Bucket) -> &mut Duration {
        match bucket {
            Bucket::Extract => &mut self.extract,
            Bucket::Link => &mut self.link,
            Bucket::Shim => &mut self.shim,
            Bucket::Cache => &mut self.cache,
            Bucket::Bootstrap => &mut self.bootstrap,
        }
    }
}

/// Optional timing accumulator. Every method is a no-op when disabled, so call
/// sites stay branch-free and unconditional.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct Timings(Option<PhaseTimings>);

impl Timings {
    /// Timings disabled (the default).
    pub fn off() -> Self {
        Timings(None)
    }

    /// Timings enabled (`--timings`).
    pub fn on() -> Self {
        Timings(Some(PhaseTimings::default()))
    }

    pub fn is_on(&self) -> bool {
        self.0.is_some()
    }

    /// Start marker for a timed span; `None` when timings are off so the
    /// matching [`add`](Self::add) call does nothing.
    pub fn start(&self) -> Option<Instant> {
        self.0.as_ref().map(|_| Instant::now())
    }

    /// Add the span since `since` to `bucket`.
    pub fn add(&mut self, bucket: Bucket, since: Option<Instant>) {
        let (Some(phase), Some(start)) = (self.0.as_mut(), since) else {
            return;
        };
        *phase.bucket_mut(bucket) += start.elapsed();
    }

    /// Absorb per-artifact network and store-ingest time
    /// Takes an iterator of [`Timing`] so the caller
    /// can pass `st.download_results.iter().map(|r| r.timing)` without cloning
    /// the results (which carry non-`Clone` errors).
    pub fn absorb_download(&mut self, timings: impl IntoIterator<Item = Timing>) {
        let Some(phase) = self.0.as_mut() else {
            return;
        };
        for t in timings {
            phase.download += t.network;
            phase.store_ingest += t.store_ingest;
        }
    }

    /// The summary line, or `None` when timings are off.
    pub fn line(&self) -> Option<String> {
        self.0.as_ref().map(|p| p.line())
    }

    /// The accumulated phases (tests / programmatic consumers).
    pub fn phases(&self) -> Option<PhaseTimings> {
        self.0
    }
}

/// Parse a `GLUE_TIMINGS ...` line into `(download_ms, cas_ms, extract_ms,
/// link_ms, shim_ms, cache_ms, bootstrap_ms, total_ms)`
/// Returns `None` on any malformed field.
pub fn parse_line(line: &str) -> Option<[u64; 8]> {
    let rest = line.strip_prefix("GLUE_TIMINGS ")?;
    let fields = [
        "download_ms=",
        "cas_ms=",
        "extract_ms=",
        "link_ms=",
        "shim_ms=",
        "cache_ms=",
        "bootstrap_ms=",
        "total_ms=",
    ];
    let mut out = [0u64; 8];
    let mut rest = rest.trim();
    for (i, field) in fields.iter().enumerate() {
        let tail = rest.strip_prefix(field)?;
        let end = tail.find(' ').unwrap_or(tail.len());
        out[i] = tail[..end].parse().ok()?;
        rest = tail[end..].trim_start();
    }
    if rest.is_empty() {
        Some(out)
    } else {
        None
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn off_timings_record_nothing() {
        let mut t = Timings::off();
        let start = t.start();
        assert!(start.is_none());
        t.add(Bucket::Link, start);
        assert_eq!(t.phases(), None);
        assert_eq!(t.line(), None);
        assert!(!t.is_on());
    }

    #[test]
    fn on_timings_accumulate_and_emit_go_field_order() {
        let mut t = Timings::on();
        let start = t.start();
        assert!(start.is_some());
        t.add(Bucket::Extract, start);
        t.add(Bucket::Link, start);
        let line = t.line().expect("enabled timings emit a line");
        let parsed = parse_line(&line).expect("own line parses");
        assert_eq!(parsed[7], t.phases().unwrap().total().as_millis() as u64);
        assert!(line.starts_with("GLUE_TIMINGS download_ms="));
    }

    #[test]
    fn total_is_the_sum_of_buckets() {
        let p = PhaseTimings {
            download: Duration::from_millis(10),
            store_ingest: Duration::from_millis(20),
            extract: Duration::from_millis(30),
            link: Duration::from_millis(40),
            shim: Duration::from_millis(5),
            cache: Duration::from_millis(2),
            bootstrap: Duration::from_millis(1),
        };
        assert_eq!(p.total(), Duration::from_millis(108));
        assert_eq!(
            p.line(),
            "GLUE_TIMINGS download_ms=10 cas_ms=20 extract_ms=30 link_ms=40 shim_ms=5 cache_ms=2 bootstrap_ms=1 total_ms=108"
        );
    }

    #[test]
    fn parse_line_rejects_malformed_input() {
        assert!(parse_line("download_ms=1").is_none());
        assert!(parse_line("GLUE_TIMINGS download_ms=x cas_ms=0 extract_ms=0 link_ms=0 shim_ms=0 cache_ms=0 bootstrap_ms=0 total_ms=0").is_none());
        assert!(parse_line(
            "GLUE_TIMINGS download_ms=1 cas_ms=0 extract_ms=0 link_ms=0 shim_ms=0 cache_ms=0 bootstrap_ms=0"
        )
        .is_none());
    }
}
