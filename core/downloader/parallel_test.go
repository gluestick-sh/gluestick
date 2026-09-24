package downloader

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gluestick-sh/core/store"
)

func TestPlanChunks(t *testing.T) {
	if got := planChunks(31<<20, 4); got != nil {
		t.Fatalf("31MiB should not parallel: %v", got)
	}
	if got := planChunks(64<<20, 4); got != nil {
		t.Fatalf("64MiB should not parallel: %v", got)
	}
	const total = 80 << 20
	chunks := planChunks(total, 4)
	if len(chunks) != 4 {
		t.Fatalf("want 4 chunks, got %d", len(chunks))
	}
	if chunks[0].start != 0 || chunks[len(chunks)-1].end != total-1 {
		t.Fatalf("bad coverage: %+v", chunks)
	}
}

func TestDownloaderParallel(t *testing.T) {
	const total = int64(20 * 1024)
	payload := make([]byte, total)
	for i := range payload {
		payload[i] = byte(i % 251)
	}

	var rangeGets atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", strconv.FormatInt(total, 10))
			w.Header().Set("Accept-Ranges", "bytes")
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		rg := r.Header.Get("Range")
		if rg == "" {
			w.Header().Set("Content-Length", strconv.FormatInt(total, 10))
			_, _ = w.Write(payload)
			return
		}
		rangeGets.Add(1)
		start, end, ok := parseRangeSpec(rg)
		if !ok || start < 0 || end >= total || start > end {
			http.Error(w, "bad range", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		chunk := payload[start : end+1]
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, total))
		w.Header().Set("Content-Length", strconv.FormatInt(int64(len(chunk)), 10))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(chunk)
	}))
	defer ts.Close()

	parallelDownloadMinBytes = 10 * 1024
	directStreamMaxBytesOverride = 8 * 1024
	minParallelChunkSizeOverride = 4096
	defer func() {
		parallelDownloadMinBytes = 0
		directStreamMaxBytesOverride = 0
		minParallelChunkSizeOverride = 0
	}()

	tmpDir, err := os.MkdirTemp("", "downloader-parallel-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	store, err := store.NewStore(filepath.Join(tmpDir, "store"))
	if err != nil {
		t.Fatal(err)
	}
	store.Prereqs()

	d := NewDownloader(store, WithWorkers(4), WithParallelDownload(true))
	task := Task{URL: ts.URL, Filename: "big.bin"}

	results := d.DownloadAll(context.Background(), []Task{task})
	if len(results) != 1 {
		t.Fatalf("results: %d", len(results))
	}
	if results[0].Error != nil {
		t.Fatalf("download failed: %v", results[0].Error)
	}
	if results[0].Size != total {
		t.Fatalf("size %d want %d", results[0].Size, total)
	}
	if rangeGets.Load() < 2 {
		t.Fatalf("expected parallel range requests, got %d", rangeGets.Load())
	}
}

func parseRangeSpec(rg string) (start, end int64, ok bool) {
	if !strings.HasPrefix(rg, "bytes=") {
		return 0, 0, false
	}
	_, err := fmt.Sscanf(rg, "bytes=%d-%d", &start, &end)
	return start, end, err == nil
}
