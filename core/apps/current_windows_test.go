//go:build windows

package apps

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLinkCurrentAndRead(t *testing.T) {
	root := t.TempDir()
	pkgRoot := filepath.Join(root, "vim")
	verDir := filepath.Join(pkgRoot, "9.2.0545")
	if err := os.MkdirAll(verDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(verDir, "gvim.exe"), []byte("stub"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := LinkCurrent(pkgRoot, "9.2.0545"); err != nil {
		t.Fatalf("LinkCurrent: %v", err)
	}

	got, err := ReadCurrent(pkgRoot)
	if err != nil {
		t.Fatalf("ReadCurrent: %v", err)
	}
	if got != "9.2.0545" {
		t.Fatalf("ReadCurrent = %q, want 9.2.0545", got)
	}

	viaCurrent := filepath.Join(pkgRoot, CurrentLinkName, "gvim.exe")
	if _, err := os.Stat(viaCurrent); err != nil {
		t.Fatalf("stat via current: %v", err)
	}

	// Switch to another version.
	ver2 := filepath.Join(pkgRoot, "9.2.0600")
	if err := os.MkdirAll(ver2, 0755); err != nil {
		t.Fatal(err)
	}
	if err := LinkCurrent(pkgRoot, "9.2.0600"); err != nil {
		t.Fatalf("LinkCurrent switch: %v", err)
	}
	got, err = ReadCurrent(pkgRoot)
	if err != nil || got != "9.2.0600" {
		t.Fatalf("after switch ReadCurrent = %q err=%v", got, err)
	}
}

func TestEnsureCurrentMigrates(t *testing.T) {
	root := t.TempDir()
	pkgRoot := filepath.Join(root, "7zip")
	for _, ver := range []string{"26.00", "26.01"} {
		dir := filepath.Join(pkgRoot, ver)
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "7z.exe"), []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	ver, err := EnsureCurrent(pkgRoot)
	if err != nil {
		t.Fatalf("EnsureCurrent: %v", err)
	}
	if ver != "26.01" {
		t.Fatalf("EnsureCurrent version = %q, want 26.01", ver)
	}
}
