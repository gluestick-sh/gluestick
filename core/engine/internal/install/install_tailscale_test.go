package install

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gluestick-sh/core/manifest"
)

func TestExtractDirLookupPaths_tailscaleMSI(t *testing.T) {
	paths := extractDirLookupPaths(`PFiles64\Tailscale`)
	want := []string{
		"PFiles64/Tailscale",
		"Program Files/Tailscale",
		"Tailscale",
	}
	if len(paths) != len(want) {
		t.Fatalf("paths = %v, want %v", paths, want)
	}
	for i, p := range want {
		if paths[i] != p {
			t.Fatalf("paths[%d] = %q, want %q (full %v)", i, paths[i], p, paths)
		}
	}
}

func TestRelPathAfterExtractDir_programFilesAlias(t *testing.T) {
	got := relPathAfterExtractDir(`Program Files/Tailscale/tailscale.exe`, `PFiles64\Tailscale`)
	if got != "tailscale.exe" {
		t.Fatalf("rel = %q, want tailscale.exe", got)
	}
}

func TestApplyExtractDirLayout_programFilesTailscale(t *testing.T) {
	installDir := t.TempDir()
	tsDir := filepath.Join(installDir, "Program Files", "Tailscale")
	if err := os.MkdirAll(tsDir, 0755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"tailscale.exe", "tailscale-ipn.exe", "tailscaled.exe"} {
		if err := os.WriteFile(filepath.Join(tsDir, name), []byte("bin"), 0755); err != nil {
			t.Fatal(err)
		}
	}

	applied, err := applyExtractDirLayout(installDir, `PFiles64\Tailscale`)
	if err != nil {
		t.Fatalf("applyExtractDirLayout: %v", err)
	}
	if !applied {
		t.Fatal("expected extract_dir layout to apply")
	}

	m := &manifest.Manifest{
		Version: "1.98.4",
		Bin:     []interface{}{"tailscale.exe", "tailscale-ipn.exe", "tailscaled.exe"},
	}
	if err := validateManifestBins(installDir, m); err != nil {
		t.Fatalf("validateManifestBins: %v", err)
	}
}

func TestCleanupIncompleteInstall_removesPartialVersionDir(t *testing.T) {
	root := t.TempDir()
	pkgRoot := filepath.Join(root, "apps", "tailscale", "1.98.4")
	if err := os.MkdirAll(pkgRoot, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkgRoot, "partial.txt"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	cleanupIncompleteInstall(nil, root, "tailscale", "1.98.4")
	if _, err := os.Stat(pkgRoot); !os.IsNotExist(err) {
		t.Fatalf("expected version dir removed, stat err = %v", err)
	}
}
