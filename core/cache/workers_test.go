package cache

import "testing"

func TestGCWorkers(t *testing.T) {
	if got := gcWorkers(0); got != 1 {
		t.Fatalf("gcWorkers(0) = %d, want 1", got)
	}
	if got := gcWorkers(1); got != 1 {
		t.Fatalf("gcWorkers(1) = %d, want 1", got)
	}
	if got := gcWorkers(100); got < minGCWorkers || got > maxGCWorkers {
		t.Fatalf("gcWorkers(100) = %d, want %d–%d", got, minGCWorkers, maxGCWorkers)
	}
}
