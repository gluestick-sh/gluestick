//! GC worker-count sizing.
//!
//! `gc_workers` returns a worker count for parallel GC scan/delete (2–32,
//! capped by itemCount and scaled by item volume).

const MIN_GC_WORKERS: usize = 2;
pub(crate) const MAX_GC_WORKERS: usize = 32;

/// Stack size for cache scan/GC worker threads.
///
/// A Rust thread's default 2 MiB × 32 workers would mean 64 MiB of stack per
/// pool (Windows commits stack pages eagerly). Worker closures never recurse
/// deeper than the directory tree (bounded by path length: well under a
/// hundred frames vs. 512 KiB), so 512 KiB is ample and keeps parallel scans
/// memory-light.
pub(crate) const WORKER_STACK_SIZE: usize = 512 * 1024;

/// Worker count for a parallel GC scan/delete pass over `item_count` items.
///
/// Below 2 items the scan is serial (1 worker); otherwise the CPU count
/// scaled ×4 for large item counts (> 5000) or ×2 (> 1000), clamped to 2–32
/// and capped at `item_count`.
pub fn gc_workers(item_count: usize) -> usize {
    if item_count <= 1 {
        return 1;
    }
    let mut n = std::thread::available_parallelism()
        .map(|p| p.get())
        .unwrap_or(1);
    if item_count > 5000 {
        n *= 4;
    } else if item_count > 1000 {
        n *= 2;
    }
    n = n.clamp(MIN_GC_WORKERS, MAX_GC_WORKERS);
    if n > item_count {
        n = item_count;
    }
    n
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn gc_workers_rules() {
        assert_eq!(gc_workers(0), 1);
        assert_eq!(gc_workers(1), 1);
        let mid = gc_workers(100);
        assert!((MIN_GC_WORKERS..=MAX_GC_WORKERS).contains(&mid));
        // Item count caps the worker count.
        assert_eq!(gc_workers(2), 2);
        assert_eq!(gc_workers(3), 3);
        // Large counts still respect the cap.
        assert!(gc_workers(10_000) <= MAX_GC_WORKERS);
    }
}
