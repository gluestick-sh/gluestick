package downloader

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gluestick-sh/core/store"
)

func TestHasPartialResume(t *testing.T) {
	dir := t.TempDir()
	store, err := store.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	d := NewDownloader(store)
	task := Task{URL: "https://example.com/pkg.tar.gz", Filename: "pkg.tar.gz"}

	if d.hasPartialResume(task) {
		t.Fatal("expected no partial initially")
	}

	partPath, _ := d.partialPaths(task)
	if err := os.MkdirAll(filepath.Dir(partPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(partPath, []byte("partial"), 0644); err != nil {
		t.Fatal(err)
	}
	if !d.hasPartialResume(task) {
		t.Fatal("expected partial detected")
	}
	if result, ok := d.tryDirectStream(context.Background(), task); ok && result.Error == nil {
		t.Fatal("should not direct stream when partial exists")
	}
}
