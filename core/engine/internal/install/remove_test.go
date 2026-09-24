package install

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRemoveAll_readOnlyFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "nested", "chrome_100_percent.pak")
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("pak"), 0444); err != nil {
		t.Fatal(err)
	}
	if err := RemoveAll(filepath.Join(dir, "nested")); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "nested")); !os.IsNotExist(err) {
		t.Fatalf("expected nested dir removed, stat err=%v", err)
	}
}

func TestRemoveAll_missingPath(t *testing.T) {
	if err := RemoveAll(filepath.Join(t.TempDir(), "missing")); err != nil {
		t.Fatal(err)
	}
}
