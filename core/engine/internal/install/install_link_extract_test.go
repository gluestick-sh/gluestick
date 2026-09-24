package install

import (
	"github.com/gluestick-sh/core/engine/internal/catalog"
	"os"
	"path/filepath"
	"testing"

	"github.com/gluestick-sh/core/store"
)

func TestLinkExtractedFiles_extractDirBackslash(t *testing.T) {
	root := t.TempDir()
	store, err := store.NewStore(filepath.Join(root, "store"))
	if err != nil {
		t.Fatal(err)
	}
	hash := "abc123"
	writeCASObject(t, store, hash, []byte("data"))

	installDir := filepath.Join(root, "apps", "pdfsam", "6.0.1")
	extractDir := `pdfsam-basic-6.0.1-windows-x64\pdfsam`
	relPath := "pdfsam-basic-6.0.1-windows-x64/pdfsam/pdfsam.exe"
	linked, err := LinkExtractedFiles(store, installDir, "", extractDir, map[string]string{relPath: hash}, nil)
	if err != nil {
		t.Fatalf("LinkExtractedFiles: %v", err)
	}
	if linked != 1 {
		t.Fatalf("linked=%d want 1", linked)
	}
	if _, err := os.Stat(filepath.Join(installDir, "pdfsam.exe")); err != nil {
		t.Fatalf("pdfsam.exe missing at install root: %v", err)
	}
}

func TestLinkExtractedFiles_success(t *testing.T) {
	root := t.TempDir()
	store, err := store.NewStore(filepath.Join(root, "store"))
	if err != nil {
		t.Fatal(err)
	}
	hash := "abc123"
	writeCASObject(t, store, hash, []byte("data"))

	installDir := filepath.Join(root, "apps", "pkg", "1.0")
	linked, err := LinkExtractedFiles(store, installDir, "", "", map[string]string{"tool.exe": hash}, nil)
	if err != nil {
		t.Fatalf("LinkExtractedFiles: %v", err)
	}
	if linked != 1 {
		t.Fatalf("linked=%d want 1", linked)
	}
}

func TestLinkExtractedFiles_missingBlob(t *testing.T) {
	root := t.TempDir()
	store, err := store.NewStore(filepath.Join(root, "store"))
	if err != nil {
		t.Fatal(err)
	}
	installDir := filepath.Join(root, "apps", "pkg", "1.0")
	_, err = LinkExtractedFiles(store, installDir, "", "", map[string]string{"tool.exe": "missing"}, nil)
	if err == nil {
		t.Fatal("expected error for missing blob")
	}
}

func TestLinkExtractedFiles_noInstallableFiles(t *testing.T) {
	root := t.TempDir()
	store, err := store.NewStore(filepath.Join(root, "store"))
	if err != nil {
		t.Fatal(err)
	}
	installDir := filepath.Join(root, "apps", "pkg", "1.0")
	linked, err := LinkExtractedFiles(store, installDir, "", "", map[string]string{}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if linked != 0 {
		t.Fatalf("linked=%d want 0", linked)
	}
}

func TestLinkExtractedFiles_extractTo(t *testing.T) {
	root := t.TempDir()
	store, err := store.NewStore(filepath.Join(root, "store"))
	if err != nil {
		t.Fatal(err)
	}
	hash := "abc123"
	writeCASObject(t, store, hash, []byte("exe"))

	installDir := filepath.Join(root, "apps", "pycharm", "2026.1.2")
	linked, err := LinkExtractedFiles(store, installDir, "IDE", "", map[string]string{"bin/pycharm64.exe": hash}, nil)
	if err != nil {
		t.Fatalf("LinkExtractedFiles: %v", err)
	}
	if linked != 1 {
		t.Fatalf("linked=%d want 1", linked)
	}
	if _, err := os.Stat(filepath.Join(installDir, "IDE", "bin", "pycharm64.exe")); err != nil {
		t.Fatalf("pycharm64.exe missing under IDE/: %v", err)
	}
}

func TestLinkExtractedFiles_skipsDirectoryPlaceholder(t *testing.T) {
	root := t.TempDir()
	store, err := store.NewStore(filepath.Join(root, "store"))
	if err != nil {
		t.Fatal(err)
	}
	exeHash := "exehash"
	writeCASObject(t, store, exeHash, []byte("MZ"))

	installDir := filepath.Join(root, "apps", "assetstudio", "0.16.53")
	// Simulate legacy ingest: empty folder marker was linked as a file blocking nested paths.
	markerPath := filepath.Join(installDir, "Dependencies", "luadec")
	if err := os.MkdirAll(filepath.Dir(markerPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(markerPath, nil, 0644); err != nil {
		t.Fatal(err)
	}

	linked, err := LinkExtractedFiles(store, installDir, "", "", map[string]string{
		`Dependencies\luadec\`:                      "empty",
		`Dependencies/luadec/lua51/luadec.exe`: exeHash,
	}, nil)
	if err != nil {
		t.Fatalf("LinkExtractedFiles: %v", err)
	}
	if linked != 1 {
		t.Fatalf("linked=%d want 1", linked)
	}
	if _, err := os.Stat(filepath.Join(installDir, "Dependencies", "luadec", "lua51", "luadec.exe")); err != nil {
		t.Fatalf("nested exe missing: %v", err)
	}
}

func TestBucketDirHasManifests(t *testing.T) {
	dir := t.TempDir()
	if catalog.BucketDirHasManifests(dir) {
		t.Fatal("empty dir should not be ready")
	}
	if err := os.WriteFile(filepath.Join(dir, "foo.json"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	if !catalog.BucketDirHasManifests(dir) {
		t.Fatal("dir with manifest should be ready")
	}
}

