package downloader

import (
	"runtime"
)

const (
	// DefaultWorkers is the default parallel range download worker count (>64 MiB files).
	DefaultWorkers = 4
	// MinUserWorkers is the minimum user-configurable parallel download worker count.
	MinUserWorkers = 2
	// MaxUserWorkers is the maximum user-configurable parallel download worker count.
	MaxUserWorkers = 8
	// UserWorkersStep is the step size for user worker settings (2, 4, 6, 8).
	UserWorkersStep = 2
	// maxParallelConnections caps HTTP range connections for a single file.
	maxParallelConnections = 16
)

// NormalizeUserWorkers snaps n to a valid user worker count (2–8, step 2).
func NormalizeUserWorkers(n int) int {
	if n <= MinUserWorkers {
		return MinUserWorkers
	}
	if n >= MaxUserWorkers {
		return MaxUserWorkers
	}
	rem := (n - MinUserWorkers) % UserWorkersStep
	if rem == 0 {
		return n
	}
	lower := n - rem
	upper := lower + UserWorkersStep
	if upper-n < n-lower {
		return upper
	}
	return lower
}

// parallelWorkersForSize returns parallel range connections for a file of total bytes.
func parallelWorkersForSize(total int64, base int) int {
	if base < 1 {
		base = 1
	}
	if total < effectiveParallelMinBytes() {
		return base
	}
	w := base
	maxChunks := int(total / effectiveMinChunkSize())
	if maxChunks < 2 {
		return base
	}
	if w > maxChunks {
		w = maxChunks
	}
	if w > maxParallelConnections {
		w = maxParallelConnections
	}
	if w < 2 {
		return 2
	}
	return w
}

// zipIngestWorkers returns parallelism for zip member cache store ingest.
// Windows small-file cache store writes thrash past ~8 workers; keep ingest near download workers.
func zipIngestWorkers(downloadWorkers, memberCount int) int {
	if memberCount <= 1 {
		return 1
	}
	maxWorkers := 8
	if runtime.GOOS != "windows" {
		maxWorkers = 12
	}
	n := runtime.NumCPU()
	if n < downloadWorkers {
		n = downloadWorkers
	}
	if n > maxWorkers {
		n = maxWorkers
	}
	if n > memberCount {
		n = memberCount
	}
	if n < 1 {
		n = 1
	}
	return n
}
