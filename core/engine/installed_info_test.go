package engine

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gluestick-sh/core/apps"
	"github.com/gluestick-sh/core/manifest"
	"github.com/gluestick-sh/core/shim"
)

func TestGetInstalledPackageDetail_installed(t *testing.T) {
	root := t.TempDir()
	pkgName := "vim"
	version := "9.2.0"
	pkgRoot := apps.PkgRoot(root, pkgName)
	installDir := filepath.Join(pkgRoot, version)
	if err := os.MkdirAll(installDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(installDir, "vim.exe"), []byte("vim"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := apps.LinkCurrent(pkgRoot, version); err != nil {
		t.Fatal(err)
	}

	eng, err := NewEngine(&EngineConfig{RootDir: root})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	if err := eng.Cache.Add(pkgName, version, map[string]string{"h1": "vim.exe"}, 3); err != nil {
		t.Fatal(err)
	}

	shimsMeta := filepath.Join(root, "shims-meta")
	if err := os.MkdirAll(shimsMeta, 0755); err != nil {
		t.Fatal(err)
	}
	cfg := shim.Config{
		Name: "vim",
		Path: filepath.Join(installDir, "vim.exe"),
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(shimsMeta, "vim.json"), data, 0644); err != nil {
		t.Fatal(err)
	}

	detail, err := eng.GetInstalledPackageDetail(pkgName)
	if err != nil {
		t.Fatalf("GetInstalledPackageDetail: %v", err)
	}
	if detail.Version != version {
		t.Fatalf("version = %q, want %q", detail.Version, version)
	}
	if detail.InstallPath != installDir {
		t.Fatalf("install path = %q", detail.InstallPath)
	}
	if detail.FileCount != 1 || detail.Size != 3 {
		t.Fatalf("cache stats = %d files, %d bytes", detail.FileCount, detail.Size)
	}
	if len(detail.Shims) != 1 || detail.Shims[0] != "vim" {
		t.Fatalf("shims = %v", detail.Shims)
	}
}

func TestGetInstalledPackageDetail_notInstalled(t *testing.T) {
	root := t.TempDir()
	eng, err := NewEngine(&EngineConfig{RootDir: root})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	if _, err := eng.GetInstalledPackageDetail("missing"); err == nil {
		t.Fatal("expected error for missing package")
	} else if !errors.Is(err, ErrPackageNotInstalled) {
		t.Fatalf("expected ErrPackageNotInstalled, got %v", err)
	}
}

func TestResetPackage_switchesCurrent(t *testing.T) {
	root := t.TempDir()
	pkgName := "git"
	pkgRoot := apps.PkgRoot(root, pkgName)
	for _, ver := range []string{"2.44.0", "2.45.0"} {
		dir := filepath.Join(pkgRoot, ver)
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "git.exe"), []byte(ver), 0644); err != nil {
			t.Fatal(err)
		}
		if err := apps.SaveInstallRecord(dir, "main", &manifest.Manifest{Version: ver, Bin: []interface{}{"git.exe"}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := apps.LinkCurrent(pkgRoot, "2.44.0"); err != nil {
		t.Fatal(err)
	}

	eng, err := NewEngine(&EngineConfig{RootDir: root})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	if err := eng.ResetPackage(pkgName + "@2.45.0"); err != nil {
		t.Fatalf("ResetPackage: %v", err)
	}
	current, err := apps.ReadCurrent(pkgRoot)
	if err != nil || current != "2.45.0" {
		t.Fatalf("current = %q, err = %v", current, err)
	}
}

func TestClearCacheIndex(t *testing.T) {
	root := t.TempDir()
	eng, err := NewEngine(&EngineConfig{RootDir: root})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	if err := eng.Cache.Add("vim", "9.0", map[string]string{"a": "vim.exe"}, 10); err != nil {
		t.Fatal(err)
	}
	cleared, err := eng.ClearCacheIndex([]string{"vim", "missing"})
	if err != nil {
		t.Fatal(err)
	}
	if cleared != 1 {
		t.Fatalf("cleared = %d, want 1", cleared)
	}
	if _, ok := eng.Cache.Get("vim"); ok {
		t.Fatal("vim should be removed from index")
	}
}
