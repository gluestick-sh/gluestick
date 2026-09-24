package engine

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gluestick-sh/core/engine/internal/catalog"
	"github.com/gluestick-sh/core/manifest"
)

func TestManifestMayNeedInnounp(t *testing.T) {
	if !catalog.ManifestMayNeedInnounp(&manifest.Manifest{InnoSetup: true}) {
		t.Fatal("expected innosetup manifest to need innounp")
	}
	if catalog.ManifestMayNeedInnounp(&manifest.Manifest{PreInstall: "Expand-7zipArchive -Path $fname"}) {
		t.Fatal("7zip-only manifest should not need innounp")
	}
	m := &manifest.Manifest{}
	m.Installer.Script = "Expand-InnoArchive -Path \"$dir\\$fname\""
	if !catalog.ManifestMayNeedInnounp(m) {
		t.Fatal("expected installer script with Expand-InnoArchive to need innounp")
	}
}

func TestCatalogNeedsInnounp(t *testing.T) {
	root := t.TempDir()
	mainDir := filepath.Join(root, "buckets", "main", "bucket")
	if err := os.MkdirAll(mainDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mainDir, "example.json"), []byte(`{
  "version": "1.0.0",
  "description": "example",
  "url": "https://example.com/example.exe",
  "hash": "abc123",
  "innosetup": true
}`), 0644); err != nil {
		t.Fatal(err)
	}

	e, err := NewEngine(&EngineConfig{RootDir: root})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	if !e.CatalogNeedsInnounp() {
		t.Fatal("expected catalog to need innounp")
	}
}

func TestResolveInnounpPath_prefersGlueBin(t *testing.T) {
	root := t.TempDir()
	want := filepath.Join(root, "bin", "innounp", "innounp.exe")
	if err := os.MkdirAll(filepath.Dir(want), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(want, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	e, err := NewEngine(&EngineConfig{RootDir: root})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	got, err := e.ResolveInnounpPath()
	if err != nil {
		t.Fatalf("ResolveInnounpPath: %v", err)
	}
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
