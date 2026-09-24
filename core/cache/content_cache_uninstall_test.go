package cache

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/gluestick-sh/core/store"
)

func TestContentCacheSurvivesUninstallRegistryRemoval(t *testing.T) {
	root := testRoot(t)
	idx, err := NewIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	store, err := store.NewStore(filepath.Join(root, "store"))
	if err != nil {
		t.Fatal(err)
	}
	store.Prereqs()

	payload := []byte("godot-binary")
	hash, err := store.Write(bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{hash: "Godot.exe"}
	if err := idx.Add("godot", "4.6.3", files, 1024); err != nil {
		t.Fatal(err)
	}
	installDir := filepath.Join(root, "apps", "godot", "4.6.3")
	if err := idx.AddInstalled("godot", "4.6.3", installDir, 1024, nil); err != nil {
		t.Fatal(err)
	}

	if err := idx.RemoveInstalled("godot"); err != nil {
		t.Fatal(err)
	}
	if _, ok := idx.GetInstalled("godot"); ok {
		t.Fatal("installed registry should be cleared")
	}
	entry, ok := idx.Get("godot")
	if !ok {
		t.Fatal("content cache entry should remain")
	}
	if entry.Version != "4.6.3" || len(entry.Files) != 1 {
		t.Fatalf("unexpected cache entry: %+v", entry)
	}
	if _, ok := idx.Reusable("godot", "4.6.3", store); !ok {
		t.Fatal("expected Reusable after uninstall registry removal")
	}
}

func TestRemoveInstalled_withoutContentCacheRow(t *testing.T) {
	root := testRoot(t)
	idx, err := NewIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	installDir := filepath.Join(root, "apps", "inkscape", "1.4.4")
	if err := os.MkdirAll(installDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := idx.Add("inkscape", "1.4.4", map[string]string{"abc": "inkscape.exe"}, 1024); err != nil {
		t.Fatal(err)
	}
	if err := idx.AddInstalled("inkscape", "1.4.4", installDir, 1024, nil); err != nil {
		t.Fatal(err)
	}

	idx.mu.Lock()
	tx, err := idx.db.Begin()
	idx.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`DELETE FROM packages WHERE name = ?`, "inkscape"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	if err := idx.RemoveInstalled("inkscape"); err != nil {
		t.Fatalf("RemoveInstalled: %v", err)
	}
	if _, ok := idx.GetInstalled("inkscape"); ok {
		t.Fatal("installed registry should be cleared")
	}
}

func TestSyncSkipsContentCacheWithoutDiskInstall(t *testing.T) {
	root := testRoot(t)
	idx, err := NewIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	if err := idx.Add("godot", "4.6.3", map[string]string{"abc": "Godot.exe"}, 1024); err != nil {
		t.Fatal(err)
	}

	if err := idx.SyncInstalledFromPackages(root); err != nil {
		t.Fatal(err)
	}
	if _, ok := idx.GetInstalled("godot"); ok {
		t.Fatal("content cache without disk install must not register as installed")
	}

	installDir := filepath.Join(root, "apps", "godot", "4.6.3")
	if err := os.MkdirAll(installDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(installDir, "Godot.exe"), []byte("godot"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := idx.SyncInstalledFromPackages(root); err != nil {
		t.Fatal(err)
	}
	if _, ok := idx.GetInstalled("godot"); !ok {
		t.Fatal("expected installed registry after disk install appears")
	}
}
