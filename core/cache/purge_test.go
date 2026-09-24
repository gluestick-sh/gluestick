package cache

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gluestick-sh/core/store"
)

func writeCacheStoreObject(t *testing.T, store *store.Store, hash string, content []byte) {
	t.Helper()
	objPath := store.ObjectPath(hash)
	if err := os.MkdirAll(filepath.Dir(objPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(objPath, content, 0644); err != nil {
		t.Fatal(err)
	}
}

func TestIndex_ClearAll_keepsSchema(t *testing.T) {
	root := testRoot(t)
	idx, err := NewIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	if err := idx.Add("upm", "1.0.0", map[string]string{"h1": "upm.exe"}, 100); err != nil {
		t.Fatal(err)
	}
	if err := idx.Add("git", "2.0.0", map[string]string{"h2": "git.exe"}, 200); err != nil {
		t.Fatal(err)
	}

	if err := idx.ClearAll(); err != nil {
		t.Fatalf("ClearAll: %v", err)
	}
	if len(idx.List()) != 0 {
		t.Fatalf("expected empty index, got %d packages", len(idx.List()))
	}
	if idx.GetFilesForPackage("upm") != nil {
		t.Error("package_files should be cleared via cascade")
	}
}

func TestPurgePackage_removesUnreferencedCacheStoreObject(t *testing.T) {
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

	hash := "abc123"
	writeCacheStoreObject(t, store, hash, []byte("upm-binary"))
	if err := idx.Add("upm", "1.0.0", map[string]string{hash: "upm.exe"}, 10); err != nil {
		t.Fatal(err)
	}

	removed, freed, err := PurgePackage(idx, store, filepath.Join(root, "apps"), "upm")
	if err != nil {
		t.Fatalf("PurgePackage: %v", err)
	}
	if removed != 1 || freed != 10 {
		t.Fatalf("removed=%d freed=%d, want 1 and 10", removed, freed)
	}
	if _, ok := idx.Get("upm"); ok {
		t.Fatal("package should be removed from index")
	}
	if _, err := os.Stat(store.ObjectPath(hash)); !os.IsNotExist(err) {
		t.Fatal("Cache store blobs should be deleted")
	}
}

func TestPurgePackage_keepsSharedCacheStoreObject(t *testing.T) {
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
	writeCacheStoreObject(t, store, shared, []byte("shared-lib"))
	if err := idx.Add("upm", "1.0.0", map[string]string{shared: "lib.dll"}, 10); err != nil {
		t.Fatal(err)
	}
	if err := idx.Add("git", "2.0.0", map[string]string{shared: "lib.dll"}, 10); err != nil {
		t.Fatal(err)
	}

	removed, _, err := PurgePackage(idx, store, filepath.Join(root, "apps"), "upm")
	if err != nil {
		t.Fatalf("PurgePackage: %v", err)
	}
	if removed != 0 {
		t.Fatalf("shared hash should be kept, removed=%d", removed)
	}
	if _, ok := idx.Get("upm"); ok {
		t.Fatal("upm should be removed from index")
	}
	if _, ok := idx.Get("git"); !ok {
		t.Fatal("git should remain in index")
	}
	if _, err := os.Stat(store.ObjectPath(shared)); err != nil {
		t.Fatalf("shared cache store objects should remain: %v", err)
	}
}

func TestPurgePackage_notInIndex(t *testing.T) {
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

	if _, _, err := PurgePackage(idx, store, filepath.Join(root, "apps"), "missing"); err == nil {
		t.Fatal("expected error for missing package")
	}
}

func TestPurgePackage_missingCacheStoreObject(t *testing.T) {
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

	present := "abc123"
	writeCacheStoreObject(t, store, present, []byte("present"))
	missing := "0027ca41ce1a18262ee881b9daf8d4c0493240ccc468da435d757868d118c81e"
	if err := idx.Add("upm", "1.0.0", map[string]string{
		present: present + ".exe",
		missing: "missing.exe",
	}, 10); err != nil {
		t.Fatal(err)
	}

	removed, _, err := PurgePackage(idx, store, filepath.Join(root, "apps"), "upm")
	if err != nil {
		t.Fatalf("PurgePackage: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed=%d, want 1 (missing blob should be skipped)", removed)
	}
	if _, ok := idx.Get("upm"); ok {
		t.Fatal("package should be removed from index")
	}
}
