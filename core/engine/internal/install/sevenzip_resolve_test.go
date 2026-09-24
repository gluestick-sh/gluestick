package install

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gluestick-sh/core/apps"
	"github.com/gluestick-sh/core/engine/internal/runtime"
	etypes "github.com/gluestick-sh/core/engine/types"
	"github.com/gluestick-sh/core/extractor"
	"github.com/gluestick-sh/core/store"
)

func TestResolveLocalSevenZip_prefersGlueBin(t *testing.T) {
	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatal(err)
	}
	sevenZ := filepath.Join(binDir, "7z.exe")
	if err := os.WriteFile(sevenZ, []byte("7z"), 0755); err != nil {
		t.Fatal(err)
	}
	if got := ResolveLocalSevenZip(root); got != sevenZ {
		t.Fatalf("ResolveLocalSevenZip() = %q, want %q", got, sevenZ)
	}
}

func TestResolveLocalSevenZip_prefers7zrFromInstall(t *testing.T) {
	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatal(err)
	}
	sevenZR := filepath.Join(binDir, "7zr.exe")
	if err := os.WriteFile(sevenZR, []byte("7zr"), 0755); err != nil {
		t.Fatal(err)
	}
	if got := ResolveLocalSevenZip(root); got != sevenZR {
		t.Fatalf("ResolveLocalSevenZip() = %q, want %q", got, sevenZR)
	}
}

func TestResolveLocalSevenZip_prefersInstalledFullOverMinimalBin(t *testing.T) {
	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatal(err)
	}
	minimal := filepath.Join(binDir, "7z.exe")
	if err := os.WriteFile(minimal, []byte("7za"), 0755); err != nil {
		t.Fatal(err)
	}

	pkgRoot := filepath.Join(root, "apps", "7zip")
	appDir := filepath.Join(pkgRoot, "26.02")
	if err := os.MkdirAll(appDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appDir, "7z.exe"), []byte("7z"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appDir, "7z.dll"), []byte("dll"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := apps.LinkCurrent(pkgRoot, "26.02"); err != nil {
		t.Fatalf("LinkCurrent: %v", err)
	}

	want := filepath.Join(pkgRoot, "current", "7z.exe")
	got := ResolveLocalSevenZip(root)
	if got != want {
		t.Fatalf("ResolveLocalSevenZip() = %q, want full %q (not minimal %q)", got, want, minimal)
	}
	if got := ResolveFullLocalSevenZip(root); got != want {
		t.Fatalf("ResolveFullLocalSevenZip() = %q, want %q", got, want)
	}
}

func TestResolveFullLocalSevenZip_skipsMinimalBin(t *testing.T) {
	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "7z.exe"), []byte("7za"), 0755); err != nil {
		t.Fatal(err)
	}
	if got := ResolveFullLocalSevenZip(root); got != "" {
		t.Fatalf("ResolveFullLocalSevenZip() = %q, want empty for minimal 7za", got)
	}
}

func TestResolveFullLocalSevenZip_acceptsBinWithDll(t *testing.T) {
	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatal(err)
	}
	full := filepath.Join(binDir, "7z.exe")
	if err := os.WriteFile(full, []byte("7z"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "7z.dll"), []byte("dll"), 0755); err != nil {
		t.Fatal(err)
	}
	if got := ResolveFullLocalSevenZip(root); got != full {
		t.Fatalf("ResolveFullLocalSevenZip() = %q, want %q", got, full)
	}
}

func TestSetSevenZipFromLocal_skipsBootstrap(t *testing.T) {
	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatal(err)
	}
	sevenZ := filepath.Join(binDir, "7z.exe")
	if err := os.WriteFile(sevenZ, []byte("7z"), 0755); err != nil {
		t.Fatal(err)
	}
	store, err := store.NewStore(filepath.Join(root, "store"))
	if err != nil {
		t.Fatal(err)
	}
	e := &runtime.Engine{
		Config:    &etypes.EngineConfig{RootDir: root},
		Extractor: extractor.NewExtractor(store),
	}
	if !SetSevenZipFromLocal(e) {
		t.Fatal("expected local 7z to be configured")
	}
	if e.Extractor.SevenZipPath() != sevenZ {
		t.Fatalf("SevenZipPath = %q, want %q", e.Extractor.SevenZipPath(), sevenZ)
	}
}

func TestSetFullSevenZipFromLocal_rejectsMinimal(t *testing.T) {
	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatal(err)
	}
	minimal := filepath.Join(binDir, "7z.exe")
	if err := os.WriteFile(minimal, []byte("7za"), 0755); err != nil {
		t.Fatal(err)
	}
	store, err := store.NewStore(filepath.Join(root, "store"))
	if err != nil {
		t.Fatal(err)
	}
	e := &runtime.Engine{
		Config:    &etypes.EngineConfig{RootDir: root},
		Extractor: extractor.NewExtractor(store),
	}
	e.Extractor.Set7zPath(minimal)
	if SetFullSevenZipFromLocal(e) {
		t.Fatal("expected minimal 7za to be rejected")
	}
}
