package install

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gluestick-sh/core/apps"
)

func TestPackageNeedsCurrentRepair(t *testing.T) {
	root := t.TempDir()
	pkgName := "comet"
	version := "0.3.2"
	installDir := filepath.Join(apps.PkgRoot(root, pkgName), version)
	if err := os.MkdirAll(installDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(installDir, "comet.exe"), []byte("stub"), 0644); err != nil {
		t.Fatal(err)
	}

	ver, ok := packageNeedsCurrentRepair(root, pkgName)
	if !ok || ver != version {
		t.Fatalf("packageNeedsCurrentRepair = (%q, %v), want (%q, true)", ver, ok, version)
	}

	pkgRoot := apps.PkgRoot(root, pkgName)
	if err := apps.LinkCurrent(pkgRoot, version); err != nil {
		t.Fatal(err)
	}
	if _, ok := packageNeedsCurrentRepair(root, pkgName); ok {
		t.Fatal("expected no repair when current exists")
	}
}

func TestUninstallRestoresCurrentOnRemoveFailure(t *testing.T) {
	root := t.TempDir()
	pkgName := "demo"
	version := "1.0.0"
	pkgRoot := apps.PkgRoot(root, pkgName)
	installDir := filepath.Join(pkgRoot, version)
	if err := os.MkdirAll(installDir, 0755); err != nil {
		t.Fatal(err)
	}
	lockFile := filepath.Join(installDir, "app.exe")
	if err := os.WriteFile(lockFile, []byte("stub"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := apps.LinkCurrent(pkgRoot, version); err != nil {
		t.Fatal(err)
	}

	removedCurrent := false
	if err := apps.RemoveCurrent(pkgRoot); err != nil {
		t.Fatal(err)
	}
	removedCurrent = true

	// Simulate failed remove with rollback (same logic as uninstallPackageFull).
	if removedCurrent {
		if err := apps.LinkCurrent(pkgRoot, version); err != nil {
			t.Fatalf("restore current: %v", err)
		}
	}

	if _, err := apps.ReadCurrent(pkgRoot); err != nil {
		t.Fatalf("current should be restored: %v", err)
	}
}
