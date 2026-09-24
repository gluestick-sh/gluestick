package install

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gluestick-sh/core/apps"
	"github.com/gluestick-sh/core/manifest"
)

func TestLocalVersionReadyToActivate(t *testing.T) {
	root := t.TempDir()
	pkgName := "demo"
	oldVer := "1.0.0"
	newVer := "2.0.0"
	m := &manifest.Manifest{
		Version: newVer,
		Bin:     "demo.exe",
	}

	oldDir := filepath.Join(apps.PkgRoot(root, pkgName), oldVer)
	newDir := filepath.Join(apps.PkgRoot(root, pkgName), newVer)
	for _, dir := range []string{oldDir, newDir} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(oldDir, "demo.exe"), []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(newDir, "demo.exe"), []byte("new"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(newDir, "install.json"), []byte(`{"version":"2.0.0","bucket":"main"}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := apps.LinkCurrent(apps.PkgRoot(root, pkgName), oldVer); err != nil {
		t.Fatal(err)
	}

	if !LocalVersionReadyToActivate(root, pkgName, newVer, m) {
		t.Fatal("expected completed local version to be activatable")
	}
	if !LocalVersionReadyToActivate(root, pkgName, oldVer, m) {
		t.Fatal("orphan install with bin on disk should be activatable")
	}

	emptyVer := "3.0.0"
	emptyDir := filepath.Join(apps.PkgRoot(root, pkgName), emptyVer)
	if err := os.MkdirAll(emptyDir, 0755); err != nil {
		t.Fatal(err)
	}
	if LocalVersionReadyToActivate(root, pkgName, emptyVer, m) {
		t.Fatal("empty version dir should not be activatable")
	}
}

func TestActiveInstallVersion(t *testing.T) {
	root := t.TempDir()
	pkgName := "demo"
	ver := "1.2.3"
	dir := filepath.Join(apps.PkgRoot(root, pkgName), ver)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "demo.exe"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := apps.LinkCurrent(apps.PkgRoot(root, pkgName), ver); err != nil {
		t.Fatal(err)
	}
	if got := ActiveInstallVersion(root, pkgName); got != ver {
		t.Fatalf("got %q want %q", got, ver)
	}
}
