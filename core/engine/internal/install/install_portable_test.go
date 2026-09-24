package install

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gluestick-sh/core/manifest"
)

func TestEnsureExtractDirFlattened_nestedBin(t *testing.T) {
	root := t.TempDir()
	installDir := filepath.Join(root, "apps", "pdfsam", "6.0.1")
	nested := filepath.Join(installDir, "pdfsam-basic-6.0.1-windows-x64", "pdfsam")
	if err := os.MkdirAll(nested, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "pdfsam.exe"), []byte("exe"), 0755); err != nil {
		t.Fatal(err)
	}

	m := &manifest.Manifest{
		Version:    "6.0.1",
		ExtractDir: `pdfsam-basic-6.0.1-windows-x64\pdfsam`,
		Bin:        "pdfsam.exe",
	}

	flattened, err := ensureExtractDirFlattened(installDir, m, "")
	if err != nil {
		t.Fatalf("ensureExtractDirFlattened: %v", err)
	}
	if !flattened {
		t.Fatal("expected flatten")
	}
	if _, err := os.Stat(filepath.Join(installDir, "pdfsam.exe")); err != nil {
		t.Fatalf("pdfsam.exe not at install root: %v", err)
	}
	if err := validateManifestBins(installDir, m); err != nil {
		t.Fatalf("validateManifestBins: %v", err)
	}
}

func TestEnsureExtractDirFlattened_alreadyFlat(t *testing.T) {
	installDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(installDir, "pdfsam.exe"), []byte("exe"), 0755); err != nil {
		t.Fatal(err)
	}
	m := &manifest.Manifest{
		ExtractDir: `pdfsam-basic-6.0.1-windows-x64\pdfsam`,
		Bin:        "pdfsam.exe",
	}
	flattened, err := ensureExtractDirFlattened(installDir, m, "")
	if err != nil {
		t.Fatal(err)
	}
	if flattened {
		t.Fatal("expected no flatten when bin already at root")
	}
}

func TestEnsureExtractDirFlattened_versionSubdir(t *testing.T) {
	installDir := t.TempDir()
	nested := filepath.Join(installDir, "149.0.7827.10")
	if err := os.MkdirAll(nested, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "chrome.exe"), []byte("exe"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "chrome_elf.dll"), []byte("dll"), 0644); err != nil {
		t.Fatal(err)
	}

	m := &manifest.Manifest{
		ExtractDir: "Chrome-bin",
		Bin:        []interface{}{[]interface{}{"chrome.exe", "chrome"}},
	}
	flattened, err := ensureExtractDirFlattened(installDir, m, "")
	if err != nil {
		t.Fatalf("ensureExtractDirFlattened: %v", err)
	}
	if !flattened {
		t.Fatal("expected flatten from version subdir")
	}
	if _, err := os.Stat(filepath.Join(installDir, "chrome.exe")); err != nil {
		t.Fatalf("chrome.exe not at install root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(installDir, "chrome_elf.dll")); err != nil {
		t.Fatalf("chrome_elf.dll not at install root: %v", err)
	}
	if err := validateManifestBins(installDir, m); err != nil {
		t.Fatalf("validateManifestBins: %v", err)
	}
}

func TestIsPortableExeInstall(t *testing.T) {
	m := &manifest.Manifest{
		Version: "1.0.0",
		Bin:     "cnping.exe",
		Architecture: map[string]interface{}{
			"64bit": map[string]interface{}{
				"url": "https://github.com/cntools/cnping/releases/download/1.0.0/cnping.exe",
			},
		},
	}
	if !isPortableExeInstall(m, "cnping.exe", "64bit") {
		t.Fatal("cnping.exe should be portable exe install")
	}

	withScript := &manifest.Manifest{
		Bin: "setup.exe",
		Installer: manifest.Installer{
			Script: "innosetup",
		},
	}
	if isPortableExeInstall(withScript, "setup.exe", "") {
		t.Fatal("installer script exe should not be portable")
	}

	withExtract := &manifest.Manifest{
		Bin:        "app.exe",
		ExtractDir: "subdir",
	}
	if isPortableExeInstall(withExtract, "app.zip", "") {
		t.Fatal("extract_dir package should not be portable exe")
	}

	comet := &manifest.Manifest{
		Bin: []interface{}{[]interface{}{"comet-x86_64-pc-windows-msvc.exe", "comet"}},
	}
	if !isPortableExeInstall(comet, "comet-aarch64-pc-windows-msvc.exe", "arm64") {
		t.Fatal("arch-specific exe download should be portable")
	}

	sevenZip := &manifest.Manifest{
		Version: "26.01",
		Bin:     []interface{}{"7z.exe", "7zG.exe", "7zFM.exe"},
		Architecture: map[string]interface{}{
			"arm64": map[string]interface{}{
				"url": "https://example.com/7z2601-arm64.exe",
				"pre_install": []interface{}{
					"Expand-7zipArchive -Path \"$dir\\$fname\" -DestinationPath $dir",
				},
			},
		},
	}
	if isPortableExeInstall(sevenZip, "7z2601-arm64.exe", "arm64") {
		t.Fatal("7zip arm64 sfx should not be portable exe install")
	}

	bitwarden := &manifest.Manifest{
		Version: "2026.5.0",
		Bin:     "Bitwarden.exe",
		PreInstall: []interface{}{
			`Rename-Item "$dir\Bitwarden-Portable-$version.exe" 'Bitwarden.exe'`,
		},
	}
	if !isPortableExeInstall(bitwarden, "Bitwarden-Portable-2026.5.0.exe", "") {
		t.Fatal("bitwarden rename pre_install should link portable exe first")
	}
}

func TestIsPreInstall7zHookInstall_wpsoffice(t *testing.T) {
	wps := &manifest.Manifest{
		Version: "12.2.0.21179",
		PreInstall: []interface{}{
			`Expand-7zipArchive "$dir\$fname" -Switches '-t#'`,
			`Remove-Item "$dir\*" -Exclude '4.7z', '2.7z' -Recurse`,
		},
	}
	if !isPreInstall7zHookInstall(wps, "WPSOffice_12.2.0.21179.exe", "", "wpsoffice") {
		t.Fatal("wpsoffice SFX pre_install should link installer blob")
	}
	sevenZip := &manifest.Manifest{
		Version: "26.01",
		Architecture: map[string]interface{}{
			"arm64": map[string]interface{}{
				"url": "https://example.com/7z2601-arm64.exe",
				"pre_install": []interface{}{
					"$7zrPath = Join-Path $dir '7zr-arm64.exe'",
					"Invoke-ExternalCommand -FilePath $7zrPath -ArgumentList @('x', \"$dir\\$fname\", \"-o$dir\", '-y')",
				},
			},
		},
	}
	if isPreInstall7zHookInstall(sevenZip, "7z2601-arm64.exe", "arm64", "7zip") {
		t.Fatal("7zip arm64 native handler should not use pre_install 7z hook path")
	}
}

func TestResolveInstalledBinPath_archMismatch(t *testing.T) {
	dir := t.TempDir()
	download := "comet-aarch64-pc-windows-msvc.exe"
	if err := os.WriteFile(filepath.Join(dir, download), []byte("exe"), 0755); err != nil {
		t.Fatal(err)
	}
	path, rel, ok := resolveInstalledBinPath(dir, "comet-x86_64-pc-windows-msvc.exe", download)
	if !ok || rel != download {
		t.Fatalf("resolveInstalledBinPath = (%q, %q, %v)", path, rel, ok)
	}
	m := &manifest.Manifest{Bin: []interface{}{[]interface{}{"comet-x86_64-pc-windows-msvc.exe", "comet"}}}
	if err := validateManifestBins(dir, m); err != nil {
		t.Fatalf("validateManifestBins: %v", err)
	}
}
