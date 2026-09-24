package downloader

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gluestick-sh/core/store"
)

func TestZipMemberIndexReadyStaleLegacy(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "zip-index-ready-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	store, err := store.NewStore(filepath.Join(tmpDir, "store"))
	if err != nil {
		t.Fatal(err)
	}
	store.Prereqs()

	zipHash := "abc123"
	files := map[string]string{"LICENSE": "911f8f5782931320f5b8d1160a76365b83aea6447ee6c04fa6d5591467db9dad"}
	payload, err := json.Marshal(zipMemberIndex{Files: files, TotalBytes: 10})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(store.Path(), ".zip-index"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(zipMemberIndexPath(store.Path(), zipHash), payload, 0644); err != nil {
		t.Fatal(err)
	}

	idx, ok := loadZipMemberIndex(store.Path(), zipHash)
	if !ok {
		t.Fatal("expected index load")
	}
	if ZipMemberIndexReady(store, idx) {
		t.Fatal("legacy index with missing blobs should not be ready")
	}

	d := NewDownloader(store)
	if _, _, ok := d.ResolveZipMemberIndex(zipHash); ok {
		t.Fatal("expected stale index removed")
	}
	if _, err := os.Stat(zipMemberIndexPath(store.Path(), zipHash)); !os.IsNotExist(err) {
		t.Fatal("expected stale index file removed")
	}
}

func TestZipMemberIndexReadyAdoptFormat(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "zip-index-adopt-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	store, err := store.NewStore(filepath.Join(tmpDir, "store"))
	if err != nil {
		t.Fatal(err)
	}
	store.Prereqs()

	files := map[string]string{"bin/tool.exe": "missinghash"}
	if err := saveZipMemberIndex(store.Path(), "zip1", files, 4); err != nil {
		t.Fatal(err)
	}
	idx, ok := loadZipMemberIndex(store.Path(), "zip1")
	if !ok {
		t.Fatal("expected index")
	}
	if idx.Format != zipMemberIndexFormatAdopt {
		t.Fatalf("format = %d, want %d", idx.Format, zipMemberIndexFormatAdopt)
	}
	if ZipMemberIndexReady(store, idx) {
		t.Fatal("index with missing blobs should not be ready")
	}

	hash, err := store.Write(strings.NewReader("tool"))
	if err != nil {
		t.Fatal(err)
	}
	files["bin/tool.exe"] = hash
	if err := saveZipMemberIndex(store.Path(), "zip1", files, 4); err != nil {
		t.Fatal(err)
	}
	idx, ok = loadZipMemberIndex(store.Path(), "zip1")
	if !ok || !ZipMemberIndexReady(store, idx) {
		t.Fatal("expected ready index when member blobs exist")
	}
}
