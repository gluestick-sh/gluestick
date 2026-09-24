package install

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gluestick-sh/core/store"
	"github.com/gluestick-sh/core/manifest"
)

func TestUltraVNCInstallLayout(t *testing.T) {
	raw := `{
		"version": "1.6.4.0",
		"url": "https://example.com/ultravnc.zip",
		"hash": "sha256:0000000000000000000000000000000000000000000000000000000000000000",
		"architecture": {
			"64bit": { "extract_dir": "x64" },
			"32bit": { "extract_dir": "x86" }
		},
		"bin": ["vncviewer.exe", "winvnc.exe"]
	}`
	m, err := manifest.Parse(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	arch := m.SelectedArchitecture()
	if arch == "" {
		t.Fatal("expected architecture selection for extract_dir-only blocks")
	}
	if m.GetExtractDirForInstall(arch) != "x64" && m.GetExtractDirForInstall(arch) != "x86" {
		t.Fatalf("unexpected extract_dir for arch %q", arch)
	}

	root := t.TempDir()
	store, err := store.NewStore(filepath.Join(root, "store"))
	if err != nil {
		t.Fatal(err)
	}
	hash := "abc123"
	writeCASObject(t, store, hash, []byte("exe"))
	installDir := filepath.Join(root, "apps", "ultravnc", "1.6.4.0")
	extractDir := m.GetExtractDirForInstall(arch)
	linked, err := LinkExtractedFiles(store, installDir, "", extractDir, map[string]string{
		extractDir + "/vncviewer.exe": hash,
		extractDir + "/winvnc.exe":    hash,
	}, nil)
	if err != nil {
		t.Fatalf("LinkExtractedFiles: %v", err)
	}
	if linked != 2 {
		t.Fatalf("linked=%d want 2", linked)
	}
	if _, err := os.Stat(filepath.Join(installDir, "vncviewer.exe")); err != nil {
		t.Fatalf("vncviewer.exe missing at install root: %v", err)
	}
	if err := validateManifestBins(installDir, m); err != nil {
		t.Fatalf("validateManifestBins: %v", err)
	}
}
