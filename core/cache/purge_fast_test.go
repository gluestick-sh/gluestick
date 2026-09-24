package cache

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/gluestick-sh/core/store"
)

func TestPurgeAppScanCandidates_skipsIndexed(t *testing.T) {
	indexRefs := map[string]bool{"shared": true}
	got := purgeAppScanCandidates([]string{"shared", "only-freecad"}, indexRefs)
	if len(got) != 1 || got[0] != "only-freecad" {
		t.Fatalf("purgeAppScanCandidates = %v", got)
	}
}

func TestPurgePackage_skipsAppScanWhenUninstalled(t *testing.T) {
	root := testRoot(t)
	store, err := store.NewStore(filepath.Join(root, "store"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Prereqs(); err != nil {
		t.Fatal(err)
	}
	idx, err := NewIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	exclusive, err := store.Write(strings.NewReader("freecad-only"))
	if err != nil {
		t.Fatal(err)
	}
	shared := "shared-hash"
	writeCacheStoreObject(t, store, shared, []byte("shared"))
	if err := idx.Add("freecad", "1.1.1", map[string]string{
		exclusive: "bin/app.exe",
		shared:    "bin/shared.dll",
	}, 100); err != nil {
		t.Fatal(err)
	}
	if err := idx.Add("git", "2.0.0", map[string]string{shared: "bin/shared.dll"}, 10); err != nil {
		t.Fatal(err)
	}

	removed, freed, err := PurgePackage(idx, store, filepath.Join(root, "apps"), "freecad")
	if err != nil {
		t.Fatalf("PurgePackage: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1 exclusive blob", removed)
	}
	if freed <= 0 {
		t.Fatalf("freed = %d", freed)
	}
	if store.Has(exclusive) {
		t.Fatal("exclusive blob should be deleted")
	}
	if !store.Has(shared) {
		t.Fatal("shared blob should remain for git")
	}
}
