package cache

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/gluestick-sh/core/store"
)

func TestFileRefKey_hardlinkMatchesStore(t *testing.T) {
	root := t.TempDir()
	store, err := store.NewStore(filepath.Join(root, "store"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Prereqs(); err != nil {
		t.Fatal(err)
	}

	hash, err := store.Write(bytes.NewReader([]byte("linked-content")))
	if err != nil {
		t.Fatal(err)
	}

	target := filepath.Join(root, "apps", "demo", "1.0.0", "bin.exe")
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		t.Fatal(err)
	}
	if err := store.Link(hash, target); err != nil {
		t.Fatal(err)
	}

	storeKey, ok := fileRefKeyForPath(store.ObjectPath(hash))
	if !ok {
		t.Fatal("store file ref key")
	}
	appKey, ok := fileRefKeyForPath(target)
	if !ok {
		t.Fatal("app file ref key")
	}
	if storeKey != appKey {
		t.Fatalf("hardlink keys differ: store=%q app=%q", storeKey, appKey)
	}
}

func TestAppsReferenceCandidateHashes_priorityPackage(t *testing.T) {
	root := t.TempDir()
	store, err := store.NewStore(filepath.Join(root, "store"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Prereqs(); err != nil {
		t.Fatal(err)
	}

	hash, err := store.Write(bytes.NewReader([]byte("keep-me")))
	if err != nil {
		t.Fatal(err)
	}

	appsDir := filepath.Join(root, "apps")
	target := filepath.Join(appsDir, "nodejs", "20.0.0", "node.exe")
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		t.Fatal(err)
	}
	if err := store.Link(hash, target); err != nil {
		t.Fatal(err)
	}

	refs, err := AppsReferenceCandidateHashes(appsDir, store, nil, "nodejs", []string{hash}, nil, nil, purgeScanBudget{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !refs[hash] {
		t.Fatal("expected installed hash to remain referenced")
	}
}
