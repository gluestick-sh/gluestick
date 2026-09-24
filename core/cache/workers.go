package cache

import "runtime"

const (
	minGCWorkers = 2
	maxGCWorkers = 32
)

// gcWorkers returns a worker count for parallel GC scan/delete (2–32, capped by itemCount).
func gcWorkers(itemCount int) int {
	if itemCount <= 1 {
		return 1
	}
	n := runtime.NumCPU()
	if itemCount > 5000 {
		n *= 4
	} else if itemCount > 1000 {
		n *= 2
	}
	if n < minGCWorkers {
		n = minGCWorkers
	}
	if n > maxGCWorkers {
		n = maxGCWorkers
	}
	if n > itemCount {
		n = itemCount
	}
	return n
}
