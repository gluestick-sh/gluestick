package bootstrap

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestHasFull7zip(t *testing.T) {
	root := t.TempDir()
	b := NewBootstrap(root)
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0755); err != nil {
		t.Fatal(err)
	}

	if b.hasFull7zip() {
		t.Fatal("expected false with empty bin")
	}

	if err := os.WriteFile(filepath.Join(bin, "7z.exe"), []byte("exe"), 0755); err != nil {
		t.Fatal(err)
	}
	if b.hasFull7zip() {
		t.Fatal("expected false with 7z.exe only (minimal bootstrap)")
	}

	if err := os.WriteFile(filepath.Join(bin, "7z.dll"), []byte("dll"), 0755); err != nil {
		t.Fatal(err)
	}
	if !b.hasFull7zip() {
		t.Fatal("expected true with 7z.exe and 7z.dll")
	}
}

func TestCleanupFull7zipArtifacts(t *testing.T) {
	root := t.TempDir()
	b := NewBootstrap(root)
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0755); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"7zr.exe", "7z_extra.7z", "7z-installer.exe", "7z.exe", "7z.dll"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	b.cleanupFull7zipArtifacts()

	for _, name := range []string{"7zr.exe", "7z_extra.7z", "7z-installer.exe"} {
		if _, err := os.Stat(filepath.Join(bin, name)); !os.IsNotExist(err) {
			t.Fatalf("%s should be removed", name)
		}
	}
	for _, name := range []string{"7z.exe", "7z.dll"} {
		if _, err := os.Stat(filepath.Join(bin, name)); err != nil {
			t.Fatalf("%s should remain: %v", name, err)
		}
	}
}

func TestCleanupFull7zipArtifacts_skipsWithoutDLL(t *testing.T) {
	root := t.TempDir()
	b := NewBootstrap(root)
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "7zr.exe"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "7z.exe"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	b.cleanupFull7zipArtifacts()

	if _, err := os.Stat(filepath.Join(bin, "7zr.exe")); err != nil {
		t.Fatal("7zr.exe should remain when 7z.dll is missing")
	}
}

func TestCleanupSevenZipSeeds(t *testing.T) {
	root := t.TempDir()
	b := NewBootstrap(root)
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"7zr.exe", "7z-installer.exe", "7z.exe", "7z.dll"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	b.CleanupSevenZipSeeds()

	if _, err := os.Stat(filepath.Join(bin, "7zr.exe")); !os.IsNotExist(err) {
		t.Fatal("7zr.exe should be removed by CleanupSevenZipSeeds")
	}
}

func TestCopyCodecDLLs(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "codecs")
	dest := filepath.Join(root, "out")
	if err := os.MkdirAll(src, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "nsis.dll"), []byte("nsis"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "readme.txt"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	if got := copyCodecDLLs(src, dest); got != 1 {
		t.Fatalf("copied=%d, want 1", got)
	}
	if _, err := os.Stat(filepath.Join(dest, "nsis.dll")); err != nil {
		t.Fatalf("nsis.dll missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "readme.txt")); !os.IsNotExist(err) {
		t.Fatal("non-dll should not be copied")
	}
}

func TestEnsureDark_usesExistingBin(t *testing.T) {
	root := t.TempDir()
	b := NewBootstrap(root)
	wixDir := filepath.Join(root, "bin", "wix")
	if err := os.MkdirAll(wixDir, 0755); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(wixDir, "dark.exe")
	for _, name := range []string{"dark.exe", "wix.dll"} {
		if err := os.WriteFile(filepath.Join(wixDir, name), []byte(name), 0755); err != nil {
			t.Fatal(err)
		}
	}
	got, err := b.EnsureDark(context.Background())
	if err != nil {
		t.Fatalf("EnsureDark: %v", err)
	}
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestEnsureInnounp_usesExistingBin(t *testing.T) {
	root := t.TempDir()
	b := NewBootstrap(root)
	innounpDir := filepath.Join(root, "bin", "innounp")
	if err := os.MkdirAll(innounpDir, 0755); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(innounpDir, "innounp.exe")
	if err := os.WriteFile(want, []byte("innounp"), 0755); err != nil {
		t.Fatal(err)
	}
	got, err := b.EnsureInnounp(context.Background())
	if err != nil {
		t.Fatalf("EnsureInnounp: %v", err)
	}
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestHasBootstrappedDark_rejectsLoneExe(t *testing.T) {
	root := t.TempDir()
	b := NewBootstrap(root)
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "dark.exe"), []byte("dark"), 0755); err != nil {
		t.Fatal(err)
	}
	if b.hasBootstrappedDark() {
		t.Fatal("lone dark.exe without wix.dll should not count as ready")
	}
}
