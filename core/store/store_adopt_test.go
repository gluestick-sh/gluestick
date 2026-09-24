package store

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestStoreAdopt(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "cas-adopt-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	store, err := NewStore(filepath.Join(tmpDir, "store"))
	if err != nil {
		t.Fatal(err)
	}
	store.Prereqs()

	srcDir := filepath.Join(tmpDir, "apps", "pkg", "1.0")
	if err := os.MkdirAll(srcDir, 0755); err != nil {
		t.Fatal(err)
	}
	srcPath := filepath.Join(srcDir, "tool.exe")
	if err := os.WriteFile(srcPath, []byte("payload"), 0644); err != nil {
		t.Fatal(err)
	}

	hash1, err := store.Adopt(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	if !store.Has(hash1) {
		t.Fatal("expected adopted blob in store")
	}

	hash2, err := store.Adopt(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	if hash1 != hash2 {
		t.Fatalf("hash mismatch: %s vs %s", hash1, hash2)
	}

	got, err := os.ReadFile(store.ObjectPath(hash1))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, []byte("payload")) {
		t.Fatalf("unexpected store content: %q", got)
	}
}
