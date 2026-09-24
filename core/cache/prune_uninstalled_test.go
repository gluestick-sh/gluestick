package cache

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPruneUninstalledPackages_removesStaleIndex(t *testing.T) {
	root := testRoot(t)
	idx, err := NewIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	if err := idx.Add("vim", "9.1.0000", map[string]string{"abc": "vim.exe"}, 1024); err != nil {
		t.Fatal(err)
	}
	if err := idx.Add("git", "2.0.0", map[string]string{"def": "git.exe"}, 2048); err != nil {
		t.Fatal(err)
	}

	installDir := filepath.Join(root, "apps", "git", "2.0.0")
	if err := os.MkdirAll(installDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(installDir, "git.exe"), []byte("git"), 0644); err != nil {
		t.Fatal(err)
	}

	removed, err := idx.PruneUninstalledPackages(root)
	if err != nil {
		t.Fatalf("PruneUninstalledPackages: %v", err)
	}
	if removed != 0 {
		t.Fatalf("removed = %d, want 0 (content cache kept)", removed)
	}
	if _, ok := idx.Get("vim"); !ok {
		t.Fatal("expected vim content cache to remain in index")
	}
	if _, ok := idx.Get("git"); !ok {
		t.Fatal("expected git to remain in index")
	}
}
