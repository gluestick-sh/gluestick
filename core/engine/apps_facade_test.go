package engine

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gluestick-sh/core/apps"
)

func TestListInstalledAllVersions(t *testing.T) {
	root := t.TempDir()
	pnpmRoot := apps.PkgRoot(root, "pnpm")
	for _, ver := range []string{"11.6.0", "11.7.0"} {
		dir := filepath.Join(pnpmRoot, ver)
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "stub"), nil, 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := apps.LinkCurrent(pnpmRoot, "11.7.0"); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(apps.PkgRoot(root, "freecad"), 0755); err != nil {
		t.Fatal(err)
	}

	eng, err := NewEngine(&EngineConfig{RootDir: root})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	got, err := eng.ListInstalledAllVersions(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "pnpm" || got[0].Current != "11.7.0" {
		t.Fatalf("ListInstalledAllVersions = %#v", got)
	}
}

func TestEnsureInstalledVersion(t *testing.T) {
	root := t.TempDir()
	pkgRoot := apps.PkgRoot(root, "vim")
	dir := filepath.Join(pkgRoot, "9.2")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "vim.exe"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	ver, ok := EnsureInstalledVersion(root, "vim")
	if !ok || ver != "9.2" {
		t.Fatalf("EnsureInstalledVersion = (%q, %v)", ver, ok)
	}
	current, err := apps.ReadCurrent(pkgRoot)
	if err != nil || current != "9.2" {
		t.Fatalf("current = %q, err = %v", current, err)
	}
}
