package engine

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gluestick-sh/core/apps"
)

func TestParsePkgRef(t *testing.T) {
	tests := []struct {
		ref      string
		wantName string
		wantVer  string
	}{
		{"uv@0.11.16", "uv", "0.11.16"},
		{"main/uv@0.11.16", "uv", "0.11.16"},
		{"vim@9.2.0580", "vim", "9.2.0580"},
		{"vim", "vim", ""},
		{" main/uv@0.11.16 ", "uv", "0.11.16"},
		{"extras/foo@1.0", "foo", "1.0"},
		{`bucket\pkg@2.0`, "pkg", "2.0"},
	}
	for _, tt := range tests {
		name, ver := ParsePkgRef(tt.ref)
		if name != tt.wantName || ver != tt.wantVer {
			t.Errorf("ParsePkgRef(%q) = (%q, %q), want (%q, %q)", tt.ref, name, ver, tt.wantName, tt.wantVer)
		}
	}
}

func TestInstalledPackageWithoutCurrentLink(t *testing.T) {
	root := t.TempDir()
	pkgName := "php85"
	version := "8.5.6"
	installDir := filepath.Join(apps.PkgRoot(root, pkgName), version)
	if err := os.MkdirAll(installDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(installDir, "php.exe"), []byte("stub"), 0644); err != nil {
		t.Fatal(err)
	}

	gotVer, ok := installedPackage(root, pkgName)
	if !ok {
		t.Fatal("expected installedPackage to detect version dir without current link")
	}
	if gotVer != version {
		t.Fatalf("got version %q want %q", gotVer, version)
	}

	currentPath := filepath.Join(apps.PkgRoot(root, pkgName), apps.CurrentLinkName)
	if _, err := os.Lstat(currentPath); err == nil {
		t.Fatal("installedPackage should not create current link")
	}
}

func TestInstalledPackageWithCurrentLink(t *testing.T) {
	root := t.TempDir()
	pkgName := "php85"
	version := "8.5.6"
	pkgRoot := apps.PkgRoot(root, pkgName)
	installDir := filepath.Join(pkgRoot, version)
	if err := os.MkdirAll(installDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(installDir, "php.exe"), []byte("stub"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := apps.LinkCurrent(pkgRoot, version); err != nil {
		t.Fatal(err)
	}

	gotVer, ok := installedPackage(root, pkgName)
	if !ok || gotVer != version {
		t.Fatalf("got (%q, %v) want (%q, true)", gotVer, ok, version)
	}
}
