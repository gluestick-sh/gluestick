package install

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/gluestick-sh/core/cache"
	"github.com/gluestick-sh/core/store"
)

func TestIndexDirectExtractInstall_archiveOnly(t *testing.T) {
	root := t.TempDir()
	store, err := store.NewStore(filepath.Join(root, "store"))
	if err != nil {
		t.Fatal(err)
	}
	installDir := filepath.Join(root, "apps", "freecad", "1.0.0")
	if err := os.MkdirAll(filepath.Join(installDir, "bin"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(installDir, "bin", "FreeCAD.exe"), []byte("exe"), 0644); err != nil {
		t.Fatal(err)
	}
	archivePath := store.ObjectPath("abc123")
	if err := os.MkdirAll(filepath.Dir(archivePath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(archivePath, []byte("archive"), 0644); err != nil {
		t.Fatal(err)
	}

	files := map[string]string{"old": "stale.txt"}
	var total int64
	count, err := indexDirectExtractInstall(store, installDir, "freecad.7z", "abc123", files, &total, nil)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("count = %d, want 1", count)
	}
	if len(files) != 1 || files["abc123"] != "freecad.7z" {
		t.Fatalf("files = %v", files)
	}
	if total != 3 {
		t.Fatalf("total = %d, want 3", total)
	}
}

func TestRefreshInstalledFilesFromDir_archiveOnlyExtract(t *testing.T) {
	root := t.TempDir()
	store, err := store.NewStore(filepath.Join(root, "store"))
	if err != nil {
		t.Fatal(err)
	}
	installDir := filepath.Join(root, "apps", "zotero", "9.0.5")
	if err := os.MkdirAll(installDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(installDir, "zotero.exe"), []byte("zotero-binary"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(installDir, "post_install.ps1"), []byte("hook"), 0644); err != nil {
		t.Fatal(err)
	}

	files := map[string]string{"abc123": "dl.7z"}
	var total int64
	if err := refreshInstalledFilesFromDir(store, installDir, files, &total); err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("files = %v, want 2 indexed paths", files)
	}
	if total != int64(len("zotero-binary")+len("hook")) {
		t.Fatalf("total = %d, want %d", total, len("zotero-binary")+len("hook"))
	}
}

func TestRefreshInstalledFilesFromDir_prefersCASBlobSize(t *testing.T) {
	root := t.TempDir()
	store, err := store.NewStore(filepath.Join(root, "store"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Prereqs(); err != nil {
		t.Fatal(err)
	}
	installDir := filepath.Join(root, "apps", "tool", "1.0")
	content := []byte("linked-content")
	path := filepath.Join(installDir, "tool.exe")
	if err := os.MkdirAll(installDir, 0755); err != nil {
		t.Fatal(err)
	}
	hash, err := store.Write(bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Link(hash, path); err != nil {
		t.Fatal(err)
	}

	files := map[string]string{}
	var total int64
	if err := refreshInstalledFilesFromDir(store, installDir, files, &total); err != nil {
		t.Fatal(err)
	}
	if total != int64(len(content)) {
		t.Fatalf("total = %d, want %d", total, len(content))
	}
}

func TestShouldExtractFromCache_7zArchiveOnly(t *testing.T) {
	entry := &cache.PackageEntry{
		Files: map[string]string{"abc": "freecad.7z"},
	}
	if !ShouldExtractFromCache(".7z", entry, "freecad.7z", "", nil, "", "") {
		t.Fatal(".7z archive-only cache entry should extract")
	}
}
