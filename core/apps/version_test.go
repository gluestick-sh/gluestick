package apps

import (
	"os"
	"path/filepath"
	"testing"
)

func TestActiveInstallDir_prefersCurrent(t *testing.T) {
	root := t.TempDir()
	pkgRoot := filepath.Join(root, "demo")
	oldVer := filepath.Join(pkgRoot, "1.0.0")
	newVer := filepath.Join(pkgRoot, "2.0.0")
	for _, dir := range []string{oldVer, newVer} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "app.exe"), []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := LinkCurrent(pkgRoot, "1.0.0"); err != nil {
		t.Fatal(err)
	}

	installDir, version, ok := ActiveInstallDir(pkgRoot)
	if !ok || version != "1.0.0" {
		t.Fatalf("ActiveInstallDir = (%q, %q, %v), want 1.0.0", installDir, version, ok)
	}
}

func TestActiveInstallDir_skipsEmptyVersionDir(t *testing.T) {
	root := t.TempDir()
	pkgRoot := filepath.Join(root, "demo")
	emptyVer := filepath.Join(pkgRoot, "1.0.0")
	if err := os.MkdirAll(emptyVer, 0755); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := ActiveInstallDir(pkgRoot); ok {
		t.Fatal("expected no active install dir for empty version directory")
	}
}
