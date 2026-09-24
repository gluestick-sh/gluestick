package extractor

import (
	"os"
	"path/filepath"
	"testing"
)

func TestArchiveExtension(t *testing.T) {
	tests := map[string]string{
		"upm_1.0_windows_amd64.tar.gz": ".tar.gz",
		"tool.tgz":                     ".tar.gz",
		"archive.tar.bz2":              ".tar.bz2",
		"archive.tar.xz":               ".tar.xz",
		"setup.7z.exe":                 ".7z.exe",
		"tool.zip":                     ".zip",
	}
	for name, want := range tests {
		if got := archiveExtension(name); got != want {
			t.Errorf("archiveExtension(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestExtractToDir_tarGz(t *testing.T) {
	sevenZip := os.Getenv("GLUE_7Z")
	if sevenZip == "" {
		sevenZip = filepath.Join(os.Getenv("USERPROFILE"), ".glue", "bin", "7z.exe")
	}
	if _, err := os.Stat(sevenZip); err != nil {
		t.Skip("7z.exe not available")
	}

	archive := filepath.Join(os.Getenv("USERPROFILE"), ".glue", "store", "9d", "590a7beb702d6d54678a3f1766e1d7cdc2b9faa577f507c26e76cd625a687d")
	if _, err := os.Stat(archive); err != nil {
		t.Skip("upm cache store archive not present")
	}

	dest := t.TempDir()
	ext := NewExtractor(nil)
	ext.Set7zPath(sevenZip)

	if err := ext.ExtractToDir(archive, dest, "upm_1.0_windows_amd64.tar.gz"); err != nil {
		t.Fatalf("ExtractToDir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "upm.exe")); err != nil {
		t.Fatalf("upm.exe missing: %v", err)
	}
}
