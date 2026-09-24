package cache

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gluestick-sh/core/store"
)

func TestPurgePackage_doesNotReindexInstalledPackage(t *testing.T) {
	root := testRoot(t)
	store, err := store.NewStore(filepath.Join(root, "store"))
	if err != nil {
		t.Fatal(err)
	}
	store.Prereqs()

	idx, err := NewIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	hash, err := store.Write(strings.NewReader("winscp-binary"))
	if err != nil {
		t.Fatal(err)
	}

	appsDir := filepath.Join(root, "apps")
	installDir := filepath.Join(appsDir, "winscp", "6.3.5")
	if err := os.MkdirAll(installDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := store.Link(hash, filepath.Join(installDir, "WinSCP.exe")); err != nil {
		t.Fatal(err)
	}

	if err := idx.Add("winscp", "6.3.5", map[string]string{hash: "WinSCP.exe"}, 100); err != nil {
		t.Fatal(err)
	}

	if _, _, err := PurgePackage(idx, store, appsDir, "winscp"); err != nil {
		t.Fatalf("PurgePackage: %v", err)
	}
	if _, ok := idx.Get("winscp"); ok {
		t.Fatal("winscp should stay removed from cache index after purge")
	}
}
