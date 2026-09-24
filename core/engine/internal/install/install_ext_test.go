package install

import (
	"testing"

	"github.com/gluestick-sh/core/cache"
	"github.com/gluestick-sh/core/manifest"
)

func TestNormalizeInstallFileExt(t *testing.T) {
	tests := map[string]string{
		".tar.gz":  ".tar",
		".tar.bz2": ".tar",
		".tar.xz":  ".tar",
		".tgz":     ".tar",
		".tar":     ".tar",
		".zip":     ".zip",
		".exe":     ".exe",
	}
	for in, want := range tests {
		if got := normalizeInstallFileExt(in); got != want {
			t.Errorf("normalizeInstallFileExt(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestShouldExtractFromCache_msiHook(t *testing.T) {
	entry := &cache.PackageEntry{Files: map[string]string{"h1": "dl.msi_"}}
	if ShouldExtractFromCache(".msi_", entry, "dl.msi_", "h1", nil, "", "") {
		t.Fatal("dl.msi_ hook install should link blob, not extract from cache")
	}
}

func TestShouldExtractFromCache_preInstall7zHook(t *testing.T) {
	entry := &cache.PackageEntry{Files: map[string]string{"h1": "WPSOffice_12.2.0.21179.exe"}}
	m := &manifest.Manifest{
		PreInstall: []interface{}{
			`Expand-7zipArchive "$dir\$fname" -Switches '-t#'`,
		},
	}
	if ShouldExtractFromCache(".exe", entry, "WPSOffice_12.2.0.21179.exe", "h1", m, "", "wpsoffice") {
		t.Fatal("SFX pre_install hook install should link blob, not extract from cache")
	}
}

func TestInstallNeedsSevenZip(t *testing.T) {
	zipM := &manifest.Manifest{Version: "1"}
	if !installNeedsSevenZip(zipM, "app.zip", ".zip", "") {
		t.Fatal("zip install should need 7z")
	}

	portable := &manifest.Manifest{Bin: []interface{}{"app.exe"}}
	if installNeedsSevenZip(portable, "app.exe", ".exe", "") {
		t.Fatal("portable exe should not need 7z")
	}

	inno := &manifest.Manifest{InnoSetup: true}
	if installNeedsSevenZip(inno, "setup.exe", ".exe", "") {
		t.Fatal("Inno Setup should use innounp, not 7z")
	}

	msiAdmin := &manifest.Manifest{
		ExtractDir: "files",
	}
	if installNeedsSevenZip(msiAdmin, "setup.msi", ".msi", "") {
		t.Fatal("administrative MSI should not need 7z")
	}
}

func TestArchiveTypeForExtract_buffalo(t *testing.T) {
	got := archiveTypeForExtract("buffalo_0.18.14_windows_x86_64.tar.gz", ".tar.gz")
	if got != "tar" {
		t.Fatalf("archiveTypeForExtract = %q, want tar", got)
	}
}
