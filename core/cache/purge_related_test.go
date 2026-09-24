package cache

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gluestick-sh/core/store"
)

func TestPackagesSharingHashes(t *testing.T) {
	root := testRoot(t)
	store, err := store.NewStore(filepath.Join(root, "store"))
	if err != nil {
		t.Fatal(err)
	}
	idx, err := NewIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	shared := "shared-hash"
	writeCacheStoreObject(t, store, shared, []byte("shared"))
	if err := idx.Add("nodejs", "1.0.0", map[string]string{shared: "lib.dll", "only-node": "node.exe"}, 10); err != nil {
		t.Fatal(err)
	}
	if err := idx.Add("git", "2.0.0", map[string]string{shared: "lib.dll"}, 10); err != nil {
		t.Fatal(err)
	}

	related := idx.PackagesSharingHashes([]string{shared, "only-node", "missing"}, "nodejs")
	if len(related) != 1 || related[0] != "git" {
		t.Fatalf("related=%v, want [git]", related)
	}
}

func TestAppsReferenceCandidateHashes_skipsUnrelatedPackages(t *testing.T) {
	root := testRoot(t)
	store, err := store.NewStore(filepath.Join(root, "store"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Prereqs(); err != nil {
		t.Fatal(err)
	}

	hash := "abc123"
	writeCacheStoreObject(t, store, hash, []byte("node"))

	appsDir := filepath.Join(root, "apps")
	nodeTarget := filepath.Join(appsDir, "nodejs", "20.0.0", "node.exe")
	otherTarget := filepath.Join(appsDir, "git", "2.0.0", "git.exe")
	for _, target := range []string{nodeTarget, otherTarget} {
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Link(hash, nodeTarget); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(otherTarget, []byte("unrelated"), 0644); err != nil {
		t.Fatal(err)
	}

	refs, err := AppsReferenceCandidateHashes(appsDir, store, nil, "nodejs", []string{hash}, nil, nil, purgeScanBudget{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !refs[hash] {
		t.Fatal("expected nodejs install to reference hash")
	}
}
