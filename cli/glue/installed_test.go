package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gluestick-sh/core/apps"
)

func TestInstalledPackage_withCurrentLink(t *testing.T) {
	root := t.TempDir()
	pkgName := "vim"
	version := "9.2.0580"
	pkgRoot := apps.PkgRoot(root, pkgName)
	installDir := filepath.Join(pkgRoot, version)
	if err := os.MkdirAll(installDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := apps.LinkCurrent(pkgRoot, version); err != nil {
		t.Fatal(err)
	}

	gotVer, ok := installedPackage(root, pkgName)
	if !ok || gotVer != version {
		t.Fatalf("installedPackage(%q) = (%q, %v), want (%q, true)", pkgName, gotVer, ok, version)
	}
}

func TestInstalledPackage_notInstalled(t *testing.T) {
	root := t.TempDir()
	if _, ok := installedPackage(root, "missing"); ok {
		t.Fatal("expected missing package to be not installed")
	}
}
