package downloader

import (
	"archive/zip"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gluestick-sh/core/store"
)

func TestStreamZipBlobToCAS(t *testing.T) {
	var zipBuf bytes.Buffer
	w := zip.NewWriter(&zipBuf)
	wf, _ := w.Create("hello.txt")
	_, _ = wf.Write([]byte("hello"))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	tmpDir, err := os.MkdirTemp("", "zip-blob-*")
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
	hash, err := d.streamZipBlobToCacheStore(context.Background(), bytes.NewReader(zipBuf.Bytes()), "sha256", "")
	if err != nil {
		t.Fatal(err)
	}
	if !store.Has(hash) {
		t.Fatal("zip blob not stored")
	}
	if idx, ok := loadZipMemberIndex(store.Path(), hash); ok && len(idx.Files) > 0 {
		t.Fatal("blob-only store should not create member index")
	}
}
