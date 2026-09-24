package downloader

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/gluestick-sh/core/store"
)

func TestDownloaderSingleFile(t *testing.T) {
	// Create test server
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write([]byte("hello, world!"))
	}))
	defer ts.Close()

	// Create cache store
	tmpDir, err := os.MkdirTemp("", "downloader-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	store, err := store.NewStore(filepath.Join(tmpDir, "store"))
	if err != nil {
		t.Fatal(err)
	}
	store.Prereqs()

	// Download
	d := NewDownloader(store)
	results := d.DownloadAll(context.Background(), []Task{
		{URL: ts.URL + "/test.bin", Filename: "test.bin"},
	})

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}

	result := results[0]
	if result.Error != nil {
		t.Fatalf("download failed: %v", result.Error)
	}

	if result.Hash == "" {
		t.Error("expected hash to be set")
	}
}

func TestDownloaderZipStreaming(t *testing.T) {
	// Create a test zip file
	var zipBuf bytes.Buffer
	w := zip.NewWriter(&zipBuf)

	files := []struct{ name, content string }{
		{"bin/test.exe", "EXE CONTENT"},
		{"README.md", "README"},
	}

	for _, f := range files {
		wf, err := w.Create(f.name)
		if err != nil {
			t.Fatal(err)
		}
		wf.Write([]byte(f.content))
	}

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	// Create test server
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/zip")
		w.Write(zipBuf.Bytes())
	}))
	defer ts.Close()

	// Create cache store
	tmpDir, err := os.MkdirTemp("", "downloader-zip-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	store, err := store.NewStore(filepath.Join(tmpDir, "store"))
	if err != nil {
		t.Fatal(err)
	}
	store.Prereqs()

	// Download
	d := NewDownloader(store)
	result := d.download(context.Background(), Task{
		URL:      ts.URL + "/test.zip",
		Filename: "test.zip",
	})

	if result.Error != nil {
		t.Fatalf("download failed: %v", result.Error)
	}

	if result.Hash == "" {
		t.Error("expected zip blob hash")
	}
	if len(result.Files) != 0 {
		t.Errorf("blob-only download should not populate Files, got %d", len(result.Files))
	}
}

func TestConcurrentDownloads(t *testing.T) {
	requestCount := 0
	var mu sync.Mutex

	// Create test server
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requestCount++
		mu.Unlock()

		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write([]byte(fmt.Sprintf("file %s", r.URL.Path)))
	}))
	defer ts.Close()

	// Create cache store
	tmpDir, err := os.MkdirTemp("", "downloader-concurrent-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	store, err := store.NewStore(filepath.Join(tmpDir, "store"))
	if err != nil {
		t.Fatal(err)
	}
	store.Prereqs()

	// Download multiple files concurrently
	d := NewDownloader(store, WithWorkers(2))
	tasks := make([]Task, 5)
	for i := 0; i < 5; i++ {
		tasks[i] = Task{
			URL:      fmt.Sprintf("%s/file%d.bin", ts.URL, i),
			Filename: fmt.Sprintf("file%d.bin", i),
		}
	}

	results := d.DownloadAll(context.Background(), tasks)

	if len(results) != 5 {
		t.Fatalf("expected 5 results, got %d", len(results))
	}

	for i, result := range results {
		if result.Error != nil {
			t.Errorf("result %d error: %v", i, result.Error)
		}
		if result.Hash == "" {
			t.Errorf("result %d: hash not set", i)
		}
	}

	if requestCount != 5 {
		t.Errorf("expected 5 server requests, got %d", requestCount)
	}
}

func TestDownloaderResumeUnknownTotal(t *testing.T) {
	const total = int64(10 * 1024 * 1024) // 10 MiB
	const offset = int64(1024 * 1024)     // 1 MiB
	fullData := bytes.Repeat([]byte("x"), int(total))

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodHead:
			w.Header().Set("Content-Length", strconv.FormatInt(total, 10))
			return
		}

		if rg := r.Header.Get("Range"); rg != "" {
			if rg != fmt.Sprintf("bytes=%d-", offset) {
				t.Errorf("unexpected range header: %s", rg)
			}
			chunk := fullData[offset:]
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, total-1, total))
			w.Header().Set("Content-Length", strconv.FormatInt(int64(len(chunk)), 10))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(chunk)
			return
		}

		t.Errorf("unexpected request: %s", r.Method)
	}))
	defer ts.Close()

	tmpDir, err := os.MkdirTemp("", "downloader-resume-unknown-total-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	store, err := store.NewStore(filepath.Join(tmpDir, "store"))
	if err != nil {
		t.Fatal(err)
	}
	store.Prereqs()

	d := NewDownloader(store)
	task := Task{URL: ts.URL, Filename: "dl.7z"}
	partPath, metaPath := d.partialPaths(task)
	if err := os.MkdirAll(filepath.Dir(partPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(partPath, fullData[:offset], 0644); err != nil {
		t.Fatal(err)
	}
	if err := savePartialMeta(metaPath, partialMeta{URL: task.URL}); err != nil {
		t.Fatal(err)
	}

	results := d.DownloadAll(context.Background(), []Task{task})
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Error != nil {
		t.Fatalf("download failed: %v", results[0].Error)
	}
	if results[0].Size != total {
		t.Fatalf("expected size %d, got %d", total, results[0].Size)
	}
}

func TestDownloaderResumePartial(t *testing.T) {
	fullData := make([]byte, 1000)
	for i := range fullData[:500] {
		fullData[i] = 'a'
	}
	for i := range fullData[500:] {
		fullData[500+i] = 'b'
	}

	var rangedRequests int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rg := r.Header.Get("Range"); rg != "" {
			rangedRequests++
			if rg != "bytes=500-" {
				t.Errorf("unexpected range header: %s", rg)
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes 500-%d/%d", len(fullData)-1, len(fullData)))
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(fullData)-500))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(fullData[500:])
			return
		}

		t.Errorf("expected ranged request, got full download")
	}))
	defer ts.Close()

	tmpDir, err := os.MkdirTemp("", "downloader-resume-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	store, err := store.NewStore(filepath.Join(tmpDir, "store"))
	if err != nil {
		t.Fatal(err)
	}
	store.Prereqs()

	d := NewDownloader(store)
	task := Task{URL: ts.URL, Filename: "resume.bin"}
	partPath, metaPath := d.partialPaths(task)
	if err := os.MkdirAll(filepath.Dir(partPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(partPath, fullData[:500], 0644); err != nil {
		t.Fatal(err)
	}
	if err := savePartialMeta(metaPath, partialMeta{URL: task.URL, TotalSize: int64(len(fullData))}); err != nil {
		t.Fatal(err)
	}

	results := d.DownloadAll(context.Background(), []Task{task})
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Error != nil {
		t.Fatalf("download failed: %v", results[0].Error)
	}
	if rangedRequests != 1 {
		t.Fatalf("expected 1 ranged request, got %d", rangedRequests)
	}
	if results[0].Size != int64(len(fullData)) {
		t.Fatalf("expected size %d, got %d", len(fullData), results[0].Size)
	}
	if _, err := os.Stat(partPath); !os.IsNotExist(err) {
		t.Fatalf("expected partial file removed after success, stat err=%v", err)
	}

	casPath := store.ObjectPath(results[0].Hash)
	got, err := os.ReadFile(casPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, fullData) {
		t.Fatalf("content store blob mismatch")
	}
}

func TestDownloadWithCacheZipStoreReuse(t *testing.T) {
	var zipBuf bytes.Buffer
	w := zip.NewWriter(&zipBuf)
	for _, item := range []struct{ name, content string }{
		{"bin/tool.exe", "tool"},
		{"README.md", "readme"},
	} {
		wf, err := w.Create(item.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := wf.Write([]byte(item.content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	tmpDir, err := os.MkdirTemp("", "downloader-zip-cache-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	store, err := store.NewStore(filepath.Join(tmpDir, "store"))
	if err != nil {
		t.Fatal(err)
	}
	store.Prereqs()

	hash, err := store.Write(bytes.NewReader(zipBuf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}

	d := NewDownloader(store)
	task := Task{
		URL:       "https://example.com/test.zip",
		Filename:  "test.zip",
		HashAlgo:  "sha256",
		HashValue: hash,
	}

	first := d.DownloadWithCache(context.Background(), task, hash, false)
	if first.Error != nil {
		t.Fatalf("first cache hit: %v", first.Error)
	}
	if !first.FromStore {
		t.Fatal("expected FromStore")
	}
	if len(first.Files) != 0 {
		t.Fatalf("expected no member index before adopt, files=%d", len(first.Files))
	}

	toolHash, err := store.Write(bytes.NewReader([]byte("tool")))
	if err != nil {
		t.Fatal(err)
	}
	readmeHash, err := store.Write(bytes.NewReader([]byte("readme")))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{"bin/tool.exe": toolHash, "README.md": readmeHash}
	if err := d.SaveZipMemberIndex(hash, files, 10); err != nil {
		t.Fatal(err)
	}

	second := d.DownloadWithCache(context.Background(), task, hash, false)
	if second.Error != nil {
		t.Fatalf("second cache hit: %v", second.Error)
	}
	if len(second.Files) != 2 {
		t.Fatalf("second files = %d, want 2", len(second.Files))
	}
}

func TestIngestZipFromStoreDedup(t *testing.T) {
	var zipBuf bytes.Buffer
	w := zip.NewWriter(&zipBuf)
	wf, err := w.Create("bin/tool.exe")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wf.Write([]byte("tool payload")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	tmpDir, err := os.MkdirTemp("", "zip-dedup-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	store, err := store.NewStore(filepath.Join(tmpDir, "store"))
	if err != nil {
		t.Fatal(err)
	}
	store.Prereqs()

	hash, err := store.Write(bytes.NewReader(zipBuf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}

	d := NewDownloader(store)
	files, err := d.IngestZipFromStore(context.Background(), hash, "sha256", hash)
	if err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("files = %d, want 1", len(files))
	}

	files2, err := d.IngestZipFromStore(context.Background(), hash, "sha256", hash)
	if err != nil {
		t.Fatalf("dedup ingest: %v", err)
	}
	if len(files2) != 1 {
		t.Fatalf("dedup files = %d, want 1", len(files2))
	}
	for rel, h := range files {
		if files2[rel] != h {
			t.Fatalf("hash mismatch for %s", rel)
		}
	}
}
