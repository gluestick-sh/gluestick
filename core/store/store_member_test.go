package store

import (
	"bytes"
	"os"
	"testing"
)

func TestStoreWriteMemberSmallDedup(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "cas-member-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	store, err := NewStore(tmpDir)
	if err != nil {
		t.Fatal(err)
	}
	store.Prereqs()

	data := []byte("small member payload")
	size := int64(len(data))

	hash1, err := store.WriteMember(bytes.NewReader(data), size, false)
	if err != nil {
		t.Fatal(err)
	}
	if !store.Has(hash1) {
		t.Fatal("expected member in store")
	}

	hash2, err := store.WriteMember(bytes.NewReader(data), size, true)
	if err != nil {
		t.Fatal(err)
	}
	if hash1 != hash2 {
		t.Fatalf("hash mismatch: %s vs %s", hash1, hash2)
	}
}

func TestStoreWriteMemberLargeDedup(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "cas-member-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	store, err := NewStore(tmpDir)
	if err != nil {
		t.Fatal(err)
	}
	store.Prereqs()

	data := bytes.Repeat([]byte("x"), maxSmallMemberBytes+1)
	size := int64(len(data))

	hash1, err := store.Write(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}

	hash2, err := store.WriteMember(bytes.NewReader(data), size, true)
	if err != nil {
		t.Fatal(err)
	}
	if hash1 != hash2 {
		t.Fatalf("dedup hash mismatch")
	}
}

func TestStoreWriteMemberLargeNeedsRetry(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "cas-member-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	store, err := NewStore(tmpDir)
	if err != nil {
		t.Fatal(err)
	}
	store.Prereqs()

	data := bytes.Repeat([]byte("y"), maxSmallMemberBytes+1)
	size := int64(len(data))

	_, err = store.WriteMember(bytes.NewReader(data), size, true)
	if err != ErrNeedReaderRetry {
		t.Fatalf("expected ErrNeedReaderRetry, got %v", err)
	}

	hash, err := store.Write(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if !store.Has(hash) {
		t.Fatal("expected stored after retry")
	}
}
