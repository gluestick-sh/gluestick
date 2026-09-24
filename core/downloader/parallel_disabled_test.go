package downloader

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/gluestick-sh/core/store"
)

func TestDownloaderParallelDisabled(t *testing.T) {
	const total = int64(20 * 1024)
	payload := make([]byte, total)

	var rangeGets atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", strconv.FormatInt(total, 10))
			return
		}
		if rg := r.Header.Get("Range"); rg != "" {
			rangeGets.Add(1)
			start, end, ok := parseRangeSpec(rg)
			if !ok {
				http.Error(w, "bad range", http.StatusRequestedRangeNotSatisfiable)
				return
			}
			chunk := payload[start : end+1]
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, total))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(chunk)
			return
		}
		_, _ = w.Write(payload)
	}))
	defer ts.Close()

	parallelDownloadMinBytes = 10 * 1024
	directStreamMaxBytesOverride = 8 * 1024
	defer func() {
		parallelDownloadMinBytes = 0
		directStreamMaxBytesOverride = 0
	}()

	tmpDir, err := os.MkdirTemp("", "downloader-no-parallel-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	store, err := store.NewStore(filepath.Join(tmpDir, "store"))
	if err != nil {
		t.Fatal(err)
	}
	store.Prereqs()

	d := NewDownloader(store, WithWorkers(4), WithParallelDownload(false))
	results := d.DownloadAll(context.Background(), []Task{{URL: ts.URL, Filename: "big.bin"}})
	if results[0].Error != nil {
		t.Fatalf("download failed: %v", results[0].Error)
	}
	if rangeGets.Load() > 1 {
		t.Fatalf("parallel disabled but got %d range requests", rangeGets.Load())
	}
}
