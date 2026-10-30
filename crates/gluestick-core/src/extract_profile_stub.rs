//! No-op stub for builds without the `extract-profile` feature.
//!
//! Keeps the same API as the real module, but `start()` returns `None` and
//! every `add_*` is empty, so the inflate hot loop does not read the clock.

use std::time::{Duration, Instant};

#[derive(Default, Clone, Copy)]
pub struct Counters {
    pub workers: u64,
}

pub fn start() -> Option<Instant> {
    None
}

pub fn add_inflate(_start: Option<Instant>) {}
pub fn add_crc(_start: Option<Instant>) {}
pub fn add_write(_start: Option<Instant>) {}
pub fn add_setup(_start: Option<Instant>) {}
pub fn add_finish(_start: Option<Instant>) {}
pub fn add_bytes(_bytes: u64) {}
pub fn add_file() {}

pub fn take_local() -> Counters {
    Counters::default()
}

pub fn merge(_target: &mut Counters, _source: Counters) {}

pub fn store_last(_kind: &'static str, _counters: Counters) {}

pub fn take_last() -> Option<(&'static str, Counters)> {
    None
}

pub fn print_line(
    _kind: &str,
    _counters: Option<Counters>,
    _extract_to_dir: Duration,
    _layout: Duration,
    _adopt: Duration,
) {
}
