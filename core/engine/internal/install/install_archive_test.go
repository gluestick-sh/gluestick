package install

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gluestick-sh/core/cache"
	"github.com/gluestick-sh/core/manifest"
)

func TestFindCacheArchiveHash(t *testing.T) {
	entry := &cache.PackageEntry{
		Files: map[string]string{
			"9d590a7beb702d6d54678a3f1766e1d7cdc2b9faa577f507c26e76cd625a687d": "upm_1.0_windows_amd64.tar.gz",
		},
	}

	got := findCacheArchiveHash(entry, "upm_1.0_windows_amd64.tar.gz", "9d590a7beb702d6d54678a3f1766e1d7cdc2b9faa577f507c26e76cd625a687d")
	if got != "9d590a7beb702d6d54678a3f1766e1d7cdc2b9faa577f507c26e76cd625a687d" {
		t.Fatalf("hash = %q", got)
	}

	wrongName := &cache.PackageEntry{
		Files: map[string]string{
			"9d590a7beb702d6d54678a3f1766e1d7cdc2b9faa577f507c26e76cd625a687d": "590a7beb702d6d54678a3f1766e1d7cdc2b9faa577f507c26e76cd625a687d",
		},
	}
	got = findCacheArchiveHash(wrongName, "upm_1.0_windows_amd64.tar.gz", "9d590a7beb702d6d54678a3f1766e1d7cdc2b9faa577f507c26e76cd625a687d")
	if got != "9d590a7beb702d6d54678a3f1766e1d7cdc2b9faa577f507c26e76cd625a687d" {
		t.Fatalf("hash by expected = %q", got)
	}

	manifestDigest := "6c6c52a4b2648e179f917bdaa7c57e793d18561b380a8bfa025f10cd1b9b2ad1"
	byDigestName := &cache.PackageEntry{
		Files: map[string]string{
			"caskey": manifestDigest,
		},
	}
	got = findCacheArchiveHash(byDigestName, "yazi-x86_64-pc-windows-msvc.zip", manifestDigest)
	if got != "caskey" {
		t.Fatalf("hash by digest filename = %q", got)
	}
}

func TestFlattenExtractDir_backslashManifestPath(t *testing.T) {
	root := t.TempDir()
	installDir := filepath.Join(root, "apps", "pdfsam", "6.0.1")
	nested := filepath.Join(installDir, "pdfsam-basic-6.0.1-windows-x64", "pdfsam")
	if err := os.MkdirAll(nested, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "pdfsam.exe"), []byte("exe"), 0755); err != nil {
		t.Fatal(err)
	}

	extractDir := `pdfsam-basic-6.0.1-windows-x64\pdfsam`
	if err := flattenExtractDir(installDir, extractDir); err != nil {
		t.Fatalf("flattenExtractDir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(installDir, "pdfsam.exe")); err != nil {
		t.Fatalf("pdfsam.exe not flattened: %v", err)
	}
}

func TestInstallExtractDest(t *testing.T) {
	root := t.TempDir()
	got, err := installExtractDest(root, "IDE")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "IDE")
	if got != want {
		t.Fatalf("installExtractDest = %q, want %q", got, want)
	}
	empty, err := installExtractDest(root, "")
	if err != nil {
		t.Fatal(err)
	}
	if empty != root {
		t.Fatal("empty extract_to should use install dir")
	}
	if _, err := installExtractDest(root, "../escape"); err == nil {
		t.Fatal("expected unsafe extract_to error")
	}
}

func TestValidateManifestBins_jetbrainsLayout(t *testing.T) {
	installDir := t.TempDir()
	binDir := filepath.Join(installDir, "IDE", "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "pycharm64.exe"), []byte("exe"), 0755); err != nil {
		t.Fatal(err)
	}
	m := &manifest.Manifest{
		Version: "2026.1.2",
		Bin:     []interface{}{[]interface{}{`IDE\bin\pycharm64.exe`, "pycharm"}},
	}
	if err := validateManifestBins(installDir, m); err != nil {
		t.Fatalf("validateManifestBins: %v", err)
	}
}

func TestCleanInstallDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "stale"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := cleanInstallDir(dir); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected empty dir, got %d entries", len(entries))
	}
}

func TestApplyExtractDirLayout_torBrowserWrapper(t *testing.T) {
	installDir := t.TempDir()
	wrapper := filepath.Join(installDir, "Tor Browser")
	browser := filepath.Join(wrapper, "Browser")
	torData := filepath.Join(wrapper, "TorBrowser", "Data")
	if err := os.MkdirAll(browser, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(torData, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(browser, "firefox.exe"), []byte("exe"), 0755); err != nil {
		t.Fatal(err)
	}
	browserSub := filepath.Join(browser, "browser")
	if err := os.MkdirAll(browserSub, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(browserSub, "omni.ja"), []byte("ja"), 0644); err != nil {
		t.Fatal(err)
	}

	applied, err := applyExtractDirLayout(installDir, "Browser")
	if err != nil {
		t.Fatalf("applyExtractDirLayout: %v", err)
	}
	if !applied {
		t.Fatal("expected extract_dir layout to apply")
	}
	if _, err := os.Stat(filepath.Join(installDir, "firefox.exe")); err != nil {
		t.Fatalf("firefox.exe not at install root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(installDir, "TorBrowser", "Data")); err != nil {
		t.Fatalf("TorBrowser data not at install root: %v", err)
	}

	m := &manifest.Manifest{
		Version:    "15.0.15",
		ExtractDir: "Browser",
		Bin:        []interface{}{[]interface{}{"firefox.exe", "tor-browser"}},
	}
	if err := validateManifestBins(installDir, m); err != nil {
		t.Fatalf("validateManifestBins: %v", err)
	}
}

func TestFlattenExtractDir_browserCaseCollision(t *testing.T) {
	installDir := t.TempDir()
	browserDir := filepath.Join(installDir, "Browser")
	browserSub := filepath.Join(browserDir, "browser")
	if err := os.MkdirAll(browserSub, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(browserDir, "firefox.exe"), []byte("exe"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(browserSub, "omni.ja"), []byte("ja"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := flattenExtractDir(installDir, "Browser"); err != nil {
		t.Fatalf("flattenExtractDir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(installDir, "firefox.exe")); err != nil {
		t.Fatalf("firefox.exe not at install root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(installDir, "browser", "omni.ja")); err != nil {
		t.Fatalf("browser/omni.ja not at install root: %v", err)
	}
}

