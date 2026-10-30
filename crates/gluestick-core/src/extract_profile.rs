//! Opt-in extract sub-phase timing.
//!
//! Set `GLUE_EXTRACT_PROFILE=1` to collect per-thread counters while a ZIP is
//! extracted and print one line to stderr after the extract+adopt span. The
//! `--timings` contract is unchanged.
//!
//! Component durations are summed across extraction workers; `extract_to_dir_ms`
//! is the wall clock, so with N workers the components can exceed it. `write_ms`
//! includes the SHA-256 update and the file write (the sink used by the install
//! path hashes while writing).

use std::cell::RefCell;
use std::sync::OnceLock;
use std::time::{Duration, Instant};

#[derive(Default, Clone, Copy)]
pub struct Counters {
    pub inflate_ns: u64,
    pub crc_ns: u64,
    pub write_ns: u64,
    pub setup_ns: u64,
    pub finish_ns: u64,
    pub bytes: u64,
    pub files: u64,
    pub workers: u64,
}

thread_local! {
    static LOCAL: RefCell<Counters> = RefCell::new(Counters::default());
    static LAST: RefCell<Option<(&'static str, Counters)>> = RefCell::new(None);
}

pub fn enabled() -> bool {
    static ON: OnceLock<bool> = OnceLock::new();
    *ON.get_or_init(|| std::env::var_os("GLUE_EXTRACT_PROFILE").is_some())
}

fn add(update: impl FnOnce(&mut Counters)) {
    if !enabled() {
        return;
    }
    LOCAL.with(|cell| {
        let mut counters = cell.borrow_mut();
        update(&mut counters);
    });
}

/// Start a timed span, or `None` when profiling is disabled. Callers pass the
/// result to an `add_*` function; with the feature off the stub returns `None`
/// and the hot loop never reads the clock.
pub fn start() -> Option<Instant> {
    if enabled() {
        Some(Instant::now())
    } else {
        None
    }
}

pub fn add_inflate(start: Option<Instant>) {
    if let Some(start) = start {
        add(|c| {
            c.inflate_ns = c
                .inflate_ns
                .saturating_add(start.elapsed().as_nanos() as u64)
        });
    }
}

pub fn add_crc(start: Option<Instant>) {
    if let Some(start) = start {
        add(|c| c.crc_ns = c.crc_ns.saturating_add(start.elapsed().as_nanos() as u64));
    }
}

pub fn add_write(start: Option<Instant>) {
    if let Some(start) = start {
        add(|c| c.write_ns = c.write_ns.saturating_add(start.elapsed().as_nanos() as u64));
    }
}

pub fn add_setup(start: Option<Instant>) {
    if let Some(start) = start {
        add(|c| c.setup_ns = c.setup_ns.saturating_add(start.elapsed().as_nanos() as u64));
    }
}

pub fn add_finish(start: Option<Instant>) {
    if let Some(start) = start {
        add(|c| {
            c.finish_ns = c
                .finish_ns
                .saturating_add(start.elapsed().as_nanos() as u64)
        });
    }
}

pub fn add_bytes(bytes: u64) {
    add(|c| c.bytes = c.bytes.saturating_add(bytes));
}

pub fn add_file() {
    add(|c| c.files = c.files.saturating_add(1));
}

pub fn take_local() -> Counters {
    if !enabled() {
        return Counters::default();
    }
    LOCAL.with(|cell| std::mem::take(&mut *cell.borrow_mut()))
}

pub fn merge(target: &mut Counters, source: Counters) {
    target.inflate_ns = target.inflate_ns.saturating_add(source.inflate_ns);
    target.crc_ns = target.crc_ns.saturating_add(source.crc_ns);
    target.write_ns = target.write_ns.saturating_add(source.write_ns);
    target.setup_ns = target.setup_ns.saturating_add(source.setup_ns);
    target.finish_ns = target.finish_ns.saturating_add(source.finish_ns);
    target.bytes = target.bytes.saturating_add(source.bytes);
    target.files = target.files.saturating_add(source.files);
}

/// Store the last ZIP extraction's counters on the calling thread, so the
/// install pipeline can print them alongside adopt/layout wall times.
pub fn store_last(kind: &'static str, counters: Counters) {
    if !enabled() {
        return;
    }
    LAST.with(|cell| *cell.borrow_mut() = Some((kind, counters)));
}

pub fn take_last() -> Option<(&'static str, Counters)> {
    if !enabled() {
        return None;
    }
    LAST.with(|cell| {
        let mut last = cell.borrow_mut();
        last.take()
    })
}

fn ms(duration: Duration) -> f64 {
    duration.as_secs_f64() * 1000.0
}

fn ms_ns(nanoseconds: u64) -> f64 {
    nanoseconds as f64 / 1_000_000.0
}

pub fn print_line(
    kind: &str,
    counters: Option<Counters>,
    extract_to_dir: Duration,
    layout: Duration,
    adopt: Duration,
) {
    if !enabled() {
        return;
    }
    let counters = counters.unwrap_or_default();
    eprintln!(
        "GLUE_EXTRACT_PROFILE kind={kind} workers={} extract_to_dir_ms={:.3} layout_ms={:.3} adopt_ms={:.3} inflate_ms={:.3} crc_ms={:.3} write_ms={:.3} setup_ms={:.3} finish_ms={:.3} bytes={} files={}",
        counters.workers,
        ms(extract_to_dir),
        ms(layout),
        ms(adopt),
        ms_ns(counters.inflate_ns),
        ms_ns(counters.crc_ns),
        ms_ns(counters.write_ns),
        ms_ns(counters.setup_ns),
        ms_ns(counters.finish_ns),
        counters.bytes,
        counters.files,
    );
}
