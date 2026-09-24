package install

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gluestick-sh/core/store"
)

func TestAdoptInstallDirToStore(t *testing.T) {
	root := t.TempDir()
	store, err := store.NewStore(filepath.Join(root, "store"))
	if err != nil {
		t.Fatal(err)
	}
	store.Prereqs()

	installDir := filepath.Join(root, "apps", "tool", "1.0")
	if err := os.MkdirAll(filepath.Join(installDir, "bin"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(installDir, "bin", "tool.exe"), []byte("tool"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(installDir, "README.md"), []byte("readme"), 0644); err != nil {
		t.Fatal(err)
	}

	files, total, err := adoptInstallDirToStore(store, installDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("files = %d, want 2", len(files))
	}
	if total != int64(len("tool")+len("readme")) {
		t.Fatalf("total = %d, want %d", total, len("tool")+len("readme"))
	}
	for _, hash := range files {
		if !store.Has(hash) {
			t.Fatalf("missing adopted blob %s", hash)
		}
	}
}
