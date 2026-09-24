package install

import (
	"github.com/gluestick-sh/core/engine/internal/runtime"

	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gluestick-sh/core/manifest"
)

func TestIsSevenZipArm64SFXPreInstall(t *testing.T) {
	hooks := []string{
		`$7zr = Join-Path $env:TMP '7zr.exe'`,
		`Invoke-ExternalCommand $7zr @('x', "$dir\$fname", "-o$dir", '-y') | Out-Null`,
	}
	if !isSevenZipArm64SFXPreInstall("7zip", "arm64", hooks) {
		t.Fatal("expected 7zip arm64 sfx pre_install")
	}
	if isSevenZipArm64SFXPreInstall("7zip", "64bit", hooks) {
		t.Fatal("64bit should not match arm64 handler")
	}
	if isSevenZipArm64SFXPreInstall("git", "arm64", hooks) {
		t.Fatal("other packages should not match")
	}
}

func TestCleanupSevenZipSFXArtifacts(t *testing.T) {
	dir := t.TempDir()
	download := "7z2601-arm64.exe"
	for _, name := range []string{"Uninstall.exe", download, "7z.exe", "leftover-arm64.exe"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	cleanupSevenZipSFXArtifacts(dir, download)
	for _, name := range []string{"Uninstall.exe", download, "leftover-arm64.exe"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			t.Fatalf("expected %s removed", name)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "7z.exe")); err != nil {
		t.Fatal("expected 7z.exe kept")
	}
}

func TestApplySevenZipArm64SFXInstall_alreadyExtracted(t *testing.T) {
	dir := t.TempDir()
	m := &manifest.Manifest{Bin: []interface{}{"7z.exe", "7zG.exe", "7zFM.exe"}}
	if err := os.WriteFile(filepath.Join(dir, "7z.exe"), []byte("bin"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "7zG.exe"), []byte("bin"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "7zFM.exe"), []byte("bin"), 0755); err != nil {
		t.Fatal(err)
	}
	download := "7z2601-arm64.exe"
	e := &runtime.Engine{}
	if err := applySevenZipArm64SFXInstall(e, context.Background(), dir, download, "", m); err != nil {
		t.Fatalf("applySevenZipArm64SFXInstall without sfx on disk: %v", err)
	}
}

func TestArchiveHashForDownload(t *testing.T) {
	files := map[string]string{
		"abc123": "7z2601-arm64.exe",
		"def456": "7z.exe",
	}
	if got := archiveHashForDownload(files, "7z2601-arm64.exe"); got != "abc123" {
		t.Fatalf("archiveHashForDownload() = %q, want abc123", got)
	}
}

func TestNormalizeSevenZipArm64Names(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"_7z.exe", "_7zG.exe", "_7zFM.exe", "_7zip.dll", "_7zip.chm"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := normalizeSevenZipArm64Names(dir); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"7z.exe", "7zG.exe", "7zFM.exe", "7-zip.dll", "7-zip.chm"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("expected %s: %v", name, err)
		}
	}
}

func TestRepairSevenZipArm64Layout_noopWithoutUnderscore(t *testing.T) {
	dir := t.TempDir()
	if err := repairSevenZipArm64Layout(dir); err != nil {
		t.Fatal(err)
	}
}
