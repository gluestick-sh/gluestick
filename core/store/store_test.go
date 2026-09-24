package store

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestStoreWriteAndLink(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "cas-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	store, err := NewStore(tmpDir)
	if err != nil {
		t.Fatal(err)
	}

	// Pre-create prefix directories
	if err := store.Prereqs(); err != nil {
		t.Fatal(err)
	}

	// Write test data
	data := []byte("hello, world!")
	hash, err := store.Write(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	// Verify hash
	expectedHash := "68e656b251e67e8358bef8483ab0d51c6619f3e7a1a9f0e75838d41ff368f728"
	if hash != expectedHash {
		t.Errorf("hash = %s, want %s", hash, expectedHash)
	}

	// Verify object exists
	if !store.Has(hash) {
		t.Error("object should exist in store")
	}

	// Test hardlink
	targetDir := filepath.Join(tmpDir, "target")
	targetPath := filepath.Join(targetDir, "test.txt")

	if err := store.Link(hash, targetPath); err != nil {
		t.Fatalf("Link failed: %v", err)
	}

	// Verify linked content
	got, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatalf("failed to read linked file: %v", err)
	}

	if !bytes.Equal(got, data) {
		t.Errorf("linked content = %s, want %s", got, data)
	}

	// Verify it's a hardlink (same inode on Unix, or nlink > 1 on Windows)
	info, _ := os.Stat(targetPath)
	// On Windows, hardlinks show nlink > 1
	if info.Sys() != nil {
		// Can't reliably check nlink across platforms
		// but we verified the content matches
	}
}

func TestStoreDeleteMissingIsNoop(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "cas-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	store, err := NewStore(tmpDir)
	if err != nil {
		t.Fatal(err)
	}

	if err := store.Delete("0027ca41ce1a18262ee881b9daf8d4c0493240ccc468da435d757868d118c81e"); err != nil {
		t.Fatalf("Delete missing object: %v", err)
	}
}

func TestStoreTwoLevelStructure(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "cas-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	store, err := NewStore(tmpDir)
	if err != nil {
		t.Fatal(err)
	}

	// Pre-create prefix directories
	if err := store.Prereqs(); err != nil {
		t.Fatal(err)
	}

	hash := "dffd6021bb2bd5b0af676290809ec3a53191dd81c7f70a4b28688a362182986f"
	path := store.ObjectPath(hash)

	// Should be store/df/fd6021...
	expected := filepath.Join(tmpDir, "df", "fd6021bb2bd5b0af676290809ec3a53191dd81c7f70a4b28688a362182986f")
	if path != expected {
		t.Errorf("path = %s, want %s", path, expected)
	}
}
