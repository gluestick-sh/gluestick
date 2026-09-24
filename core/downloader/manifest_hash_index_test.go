package downloader

import (
	"bytes"
	"context"
	"crypto/sha512"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/gluestick-sh/core/store"
)

func TestDownloadWithCacheSha512StoreReuse(t *testing.T) {
	payload := []byte("godot-like zip payload for sha512 reuse test")
	sum512 := sha512.Sum512(payload)
	manifestDigest := hex.EncodeToString(sum512[:])

	tmpDir, err := os.MkdirTemp("", "downloader-sha512-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	store, err := store.NewStore(filepath.Join(tmpDir, "store"))
	if err != nil {
		t.Fatal(err)
	}
	store.Prereqs()

	casHash, err := store.Write(bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	if err := saveManifestHashAlias(store.Path(), "sha512", manifestDigest, casHash); err != nil {
		t.Fatal(err)
	}

	d := NewDownloader(store)
	task := Task{
		URL:       "https://example.com/Godot.zip",
		Filename:  "Godot.zip",
		HashAlgo:  "sha512",
		HashValue: manifestDigest,
	}

	result := d.DownloadWithCache(context.Background(), task, manifestDigest, false)
	if result.Error != nil {
		t.Fatalf("DownloadWithCache: %v", result.Error)
	}
	if !result.FromStore {
		t.Fatal("expected FromStore for sha512 manifest alias")
	}
	if result.Hash != casHash {
		t.Fatalf("hash = %q, want %q", result.Hash, casHash)
	}
}

func TestLookupStoreBlobSha256Unchanged(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "downloader-sha256-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	store, err := store.NewStore(filepath.Join(tmpDir, "store"))
	if err != nil {
		t.Fatal(err)
	}
	store.Prereqs()

	payload := []byte("sha256 blob")
	hash, err := store.Write(bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}

	d := NewDownloader(store)
	task := Task{HashAlgo: "sha256", HashValue: hash}
	got, ok := d.lookupStoreBlob(task)
	if !ok || got != hash {
		t.Fatalf("lookupStoreBlob = %q, %v; want %q, true", got, ok, hash)
	}
}
