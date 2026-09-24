package downloader

import (
	"runtime"
	"testing"
)

func TestNormalizeUserWorkers(t *testing.T) {
	cases := []struct {
		in, want int
	}{
		{0, MinUserWorkers},
		{1, MinUserWorkers},
		{2, 2},
		{3, 2},
		{4, 4},
		{5, 4},
		{6, 6},
		{7, 6},
		{8, 8},
		{9, 8},
	}
	for _, tc := range cases {
		if got := NormalizeUserWorkers(tc.in); got != tc.want {
			t.Fatalf("NormalizeUserWorkers(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestZipIngestWorkers(t *testing.T) {
	if got := zipIngestWorkers(4, 100); got < 4 {
		t.Fatalf("zipIngestWorkers(4, 100) = %d, want >= 4", got)
	}
	if got := zipIngestWorkers(8, 2); got != 2 {
		t.Fatalf("zipIngestWorkers(8, 2) = %d, want 2", got)
	}
	max := 8
	if runtime.GOOS != "windows" {
		max = 12
	}
	if got := zipIngestWorkers(4, 10000); got > max {
		t.Fatalf("zipIngestWorkers capped at %d, got %d", max, got)
	}
}

func TestParallelWorkersForSize(t *testing.T) {
	base := 4
	if got := parallelWorkersForSize(50<<20, base); got != base {
		t.Fatalf("50MiB: got %d want %d", got, base)
	}
	if got := parallelWorkersForSize(80<<20, base); got != base {
		t.Fatalf("80MiB: got %d want %d", got, base)
	}
	if got := parallelWorkersForSize(400<<20, base); got != base {
		t.Fatalf("400MiB: got %d want %d", got, base)
	}
	if got := parallelWorkersForSize(600<<20, 6); got != 6 {
		t.Fatalf("600MiB base 6: got %d want 6", got)
	}
}

func TestPlanChunksRange(t *testing.T) {
	const total = int64(400 << 20)
	offset := total * 56 / 100
	chunks := planChunksRange(offset, total-1, 4)
	if len(chunks) < 2 {
		t.Fatalf("resume chunks: got %d", len(chunks))
	}
	if chunks[0].start != offset {
		t.Fatalf("first chunk start = %d want %d", chunks[0].start, offset)
	}
	if chunks[len(chunks)-1].end != total-1 {
		t.Fatalf("last chunk end = %d want %d", chunks[len(chunks)-1].end, total-1)
	}
}
