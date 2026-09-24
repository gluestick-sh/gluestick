package install

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gluestick-sh/core/cache"
	"github.com/gluestick-sh/core/store"
)

func writeCASObject(t *testing.T, store *store.Store, hash string, content []byte) {
	t.Helper()
	objPath := store.ObjectPath(hash)
	if err := os.MkdirAll(filepath.Dir(objPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(objPath, content, 0644); err != nil {
		t.Fatal(err)
	}
}

func TestLinkFromCache_plainFileLinksDownload(t *testing.T) {
	root := t.TempDir()
	store, err := store.NewStore(filepath.Join(root, "store"))
	if err != nil {
		t.Fatal(err)
	}

	hash := "abc123"
	writeCASObject(t, store, hash, []byte("@echo off"))

	installDir := filepath.Join(root, "apps", "mill", "1.1.6")
	entry := &cache.PackageEntry{
		Files: map[string]string{hash: "mill.bat"},
	}

	linked, err := LinkFromCache(store, installDir, entry, "mill.bat", ".bat", "", "", "", nil, "", "")
	if err != nil {
		t.Fatalf("LinkFromCache: %v", err)
	}
	if linked != 1 {
		t.Fatalf("linked=%d, want 1", linked)
	}
	if _, err := os.Stat(filepath.Join(installDir, "mill.bat")); err != nil {
		t.Fatalf("mill.bat not installed: %v", err)
	}
}

func TestLinkFromCache_archiveSkipsDownloadBlob(t *testing.T) {
	root := t.TempDir()
	store, err := store.NewStore(filepath.Join(root, "store"))
	if err != nil {
		t.Fatal(err)
	}

	archiveHash := "archive"
	innerHash := "inner"
	writeCASObject(t, store, archiveHash, []byte("zip-data"))
	writeCASObject(t, store, innerHash, []byte("tool.exe"))

	installDir := filepath.Join(root, "apps", "tool", "1.0.0")
	entry := &cache.PackageEntry{
		Files: map[string]string{
			archiveHash: "tool.zip",
			innerHash:   "tool.exe",
		},
	}

	linked, err := LinkFromCache(store, installDir, entry, "tool.zip", ".zip", "", "", "", nil, "", "")
	if err != nil {
		t.Fatalf("LinkFromCache: %v", err)
	}
	if linked != 1 {
		t.Fatalf("linked=%d, want 1", linked)
	}
	if _, err := os.Stat(filepath.Join(installDir, "tool.zip")); !os.IsNotExist(err) {
		t.Fatal("archive blob should not be linked into install dir")
	}
	if _, err := os.Stat(filepath.Join(installDir, "tool.exe")); err != nil {
		t.Fatalf("tool.exe not installed: %v", err)
	}
}

func TestLinkFromCache_skipsArchiveByHash(t *testing.T) {
	root := t.TempDir()
	store, err := store.NewStore(filepath.Join(root, "store"))
	if err != nil {
		t.Fatal(err)
	}

	archiveHash := "9d590a7beb702d6d54678a3f1766e1d7cdc2b9faa577f507c26e76cd625a687d"
	innerHash := "inner"
	writeCASObject(t, store, archiveHash, []byte("tar-gz-data"))
	writeCASObject(t, store, innerHash, []byte("tool.exe"))

	installDir := filepath.Join(root, "apps", "upm", "1.0")
	entry := &cache.PackageEntry{
		Files: map[string]string{
			archiveHash: "590a7beb702d6d54678a3f1766e1d7cdc2b9faa577f507c26e76cd625a687d",
			innerHash:   "upm.exe",
		},
	}

	linked, err := LinkFromCache(store, installDir, entry, "upm_1.0_windows_amd64.tar.gz", ".tar", archiveHash, "", "", nil, "", "")
	if err != nil {
		t.Fatalf("LinkFromCache: %v", err)
	}
	if linked != 1 {
		t.Fatalf("linked=%d, want 1", linked)
	}
	if _, err := os.Stat(filepath.Join(installDir, "590a7beb702d6d54678a3f1766e1d7cdc2b9faa577f507c26e76cd625a687d")); !os.IsNotExist(err) {
		t.Fatal("archive blob should not be linked into install dir")
	}
	if _, err := os.Stat(filepath.Join(installDir, "upm.exe")); err != nil {
		t.Fatalf("upm.exe not installed: %v", err)
	}
}

