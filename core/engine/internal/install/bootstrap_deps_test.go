package install

import (
	"os"
	"path/filepath"
	"testing"

	etypes "github.com/gluestick-sh/core/engine/types"
	"github.com/gluestick-sh/core/engine/internal/runtime"
	"github.com/gluestick-sh/core/manifest"
)

func TestManifestMayNeedDark(t *testing.T) {
	if !ManifestMayNeedDark(&manifest.Manifest{
		PreInstall: `Expand-DarkArchive "$dir\$fname" -DestinationPath "$dir\.tmp"`,
	}) {
		t.Fatal("expected Expand-DarkArchive hook to need dark")
	}
	if ManifestMayNeedDark(&manifest.Manifest{PreInstall: "Expand-7zipArchive -Path $fname"}) {
		t.Fatal("7zip-only manifest should not need dark")
	}
}

func TestCatalogNeedsDark(t *testing.T) {
	root := t.TempDir()
	mainDir := filepath.Join(root, "buckets", "main", "bucket")
	if err := os.MkdirAll(mainDir, 0755); err != nil {
		t.Fatal(err)
	}
	content := `{
  "version": "1.0.0",
  "description": "example",
  "url": "https://example.com/setup.exe",
  "hash": "abc123",
  "pre_install": "Expand-DarkArchive \"$dir\\$fname\" -DestinationPath \"$dir\\.tmp\""
}`
	if err := os.WriteFile(filepath.Join(mainDir, "example.json"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	e, err := runtime.NewEngine(&etypes.EngineConfig{RootDir: root})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	if !CatalogNeedsDark(e) {
		t.Fatal("expected catalog to need dark")
	}
}

func TestResolveGitPath_findsGit(t *testing.T) {
	root := t.TempDir()
	want := filepath.Join(root, "bin", "git", "mingw64", "bin", "git.exe")
	if err := os.MkdirAll(filepath.Dir(want), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(want, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	e, err := runtime.NewEngine(&etypes.EngineConfig{RootDir: root})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	got, err := ResolveGitPath(e)
	if err != nil {
		t.Fatalf("ResolveGitPath: %v", err)
	}
	if got == "" {
		t.Fatal("expected non-empty git path")
	}
}

func TestResolveSevenZipPath_prefersGlueBin(t *testing.T) {
	root := t.TempDir()
	want := filepath.Join(root, "bin", "7z.exe")
	if err := os.MkdirAll(filepath.Dir(want), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(want, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	e, err := runtime.NewEngine(&etypes.EngineConfig{RootDir: root})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	got, err := ResolveSevenZipPath(e)
	if err != nil {
		t.Fatalf("ResolveSevenZipPath: %v", err)
	}
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
