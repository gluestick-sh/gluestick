package cache

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gluestick-sh/core/store"
)

func testRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "cache-index-test-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func TestNewIndex_createsDatabase(t *testing.T) {
	root := testRoot(t)

	idx, err := NewIndex(root)
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	defer idx.Close()

	dbPath := filepath.Join(root, "cache", "index.db")
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("index.db not created: %v", err)
	}
}

func TestIndex_addGetListRemove(t *testing.T) {
	root := testRoot(t)
	idx, err := NewIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	files := map[string]string{
		"abc123": "bin/tool.exe",
		"def456": "lib/foo.dll",
	}
	if err := idx.Add("make", "4.4.1", files, 4096); err != nil {
		t.Fatalf("Add: %v", err)
	}

	entry, ok := idx.Get("make")
	if !ok {
		t.Fatal("Get: package not found")
	}
	if entry.Version != "4.4.1" {
		t.Errorf("version = %q, want 4.4.1", entry.Version)
	}
	if entry.Size != 4096 {
		t.Errorf("size = %d, want 4096", entry.Size)
	}
	if len(entry.Files) != 2 {
		t.Fatalf("files count = %d, want 2", len(entry.Files))
	}
	if entry.Files["abc123"] != "bin/tool.exe" {
		t.Errorf("unexpected file mapping: %v", entry.Files)
	}

	hashes := idx.GetFilesForPackage("make")
	if len(hashes) != 2 {
		t.Fatalf("GetFilesForPackage: got %d hashes, want 2", len(hashes))
	}

	all := idx.List()
	if len(all) != 1 {
		t.Fatalf("List: got %d packages, want 1", len(all))
	}
	if len(all["make"].Files) != 2 {
		t.Fatalf("List files count = %d, want 2", len(all["make"].Files))
	}

	meta := idx.ListPackages()
	if len(meta) != 1 || meta["make"].Files != nil {
		t.Fatalf("ListPackages: got %+v", meta)
	}
	counts, err := idx.PackageFileCounts()
	if err != nil {
		t.Fatalf("PackageFileCounts: %v", err)
	}
	if counts["make"] != 2 {
		t.Fatalf("PackageFileCounts[make] = %d, want 2", counts["make"])
	}

	// Replace package version and files
	newFiles := map[string]string{"xyz789": "make.exe"}
	if err := idx.Add("make", "4.4.2", newFiles, 2048); err != nil {
		t.Fatalf("Add replace: %v", err)
	}
	entry, ok = idx.Get("make")
	if !ok || entry.Version != "4.4.2" {
		t.Fatalf("after replace: %+v, ok=%v", entry, ok)
	}
	if len(entry.Files) != 1 || entry.Files["xyz789"] != "make.exe" {
		t.Errorf("replaced files: %v", entry.Files)
	}

	if err := idx.Remove("make"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, ok := idx.Get("make"); ok {
		t.Error("package still present after Remove")
	}
	if idx.GetFilesForPackage("make") != nil {
		t.Error("expected nil hashes after Remove")
	}
}

func TestIndex_migrateFromJSON(t *testing.T) {
	root := testRoot(t)

	jsonPath := filepath.Join(root, "cache-index.json")
	payload := map[string]any{
		"packages": map[string]any{
			"git": map[string]any{
				"version":   "2.45.0",
				"installed": "2024-01-15T10:00:00Z",
				"size":      1024,
				"files": map[string]string{
					"hash1": "cmd/git.exe",
				},
			},
		},
	}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(jsonPath, data, 0644); err != nil {
		t.Fatal(err)
	}

	idx, err := NewIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	if _, err := os.Stat(jsonPath); err == nil {
		t.Error("cache-index.json should be renamed after migration")
	}
	backup := jsonPath + ".bak"
	if _, err := os.Stat(backup); err != nil {
		t.Errorf("expected backup at %s: %v", backup, err)
	}

	entry, ok := idx.Get("git")
	if !ok {
		t.Fatal("migrated package not found")
	}
	if entry.Version != "2.45.0" || entry.Size != 1024 {
		t.Errorf("entry = %+v", entry)
	}
	if entry.Files["hash1"] != "cmd/git.exe" {
		t.Errorf("files = %v", entry.Files)
	}
}

func TestIndex_reusable(t *testing.T) {
	root := testRoot(t)
	storeRoot := filepath.Join(root, "store")
	if err := os.MkdirAll(storeRoot, 0755); err != nil {
		t.Fatal(err)
	}
	store, err := store.NewStore(storeRoot)
	if err != nil {
		t.Fatal(err)
	}

	idx, err := NewIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	if _, ok := idx.Reusable("upm", "1.0.0", store); ok {
		t.Fatal("expected no reusable entry before install")
	}

	hash := "abc123"
	objPath := store.ObjectPath(hash)
	if err := os.MkdirAll(filepath.Dir(objPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(objPath, []byte("cached"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := idx.Add("upm", "1.0.0", map[string]string{hash: "upm.exe"}, 6); err != nil {
		t.Fatal(err)
	}

	if _, ok := idx.Reusable("upm", "1.0.0", store); !ok {
		t.Fatal("expected reusable entry")
	}
	if _, ok := idx.Reusable("upm", "9.9.9", store); ok {
		t.Fatal("wrong version should not be reusable")
	}

	_ = store.Delete(hash)
	if _, ok := idx.Reusable("upm", "1.0.0", store); ok {
		t.Fatal("missing cache store object should not be reusable")
	}
}

func TestIndex_getMissing(t *testing.T) {
	root := testRoot(t)
	idx, err := NewIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	if _, ok := idx.Get("nonexistent"); ok {
		t.Error("expected false for missing package")
	}
	if idx.GetFilesForPackage("nonexistent") != nil {
		t.Error("expected nil hashes for missing package")
	}
}

func TestIndex_rebuildFromApps(t *testing.T) {
	root := testRoot(t)
	store, err := store.NewStore(filepath.Join(root, "store"))
	if err != nil {
		t.Fatal(err)
	}
	store.Prereqs()

	appsDir := filepath.Join(root, "apps")
	installDir := filepath.Join(appsDir, "make", "4.4.1")
	if err := os.MkdirAll(installDir, 0755); err != nil {
		t.Fatal(err)
	}

	hash, err := store.Write(strings.NewReader("tool-binary"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Link(hash, filepath.Join(installDir, "bin", "make.exe")); err != nil {
		t.Fatal(err)
	}

	idx, err := NewIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	count, err := idx.Rebuild(appsDir, store, nil)
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if count != 1 {
		t.Fatalf("Rebuild count = %d, want 1", count)
	}

	entry, ok := idx.Get("make")
	if !ok {
		t.Fatal("expected make in index after rebuild")
	}
	if entry.Version != "4.4.1" {
		t.Errorf("version = %q, want 4.4.1", entry.Version)
	}
	if len(entry.Files) != 1 {
		t.Fatalf("files = %d, want 1", len(entry.Files))
	}
}

func TestSyncInstalledFromPackages_preservesVersionLock(t *testing.T) {
	root := testRoot(t)
	idx, err := NewIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	if err := idx.Add("vim", "9.1.0000", map[string]string{"abc": "vim.exe"}, 1024); err != nil {
		t.Fatalf("Add: %v", err)
	}
	installDir := filepath.Join(root, "apps", "vim", "9.1.0000")
	if err := os.MkdirAll(installDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(installDir, "vim.exe"), []byte("vim"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := idx.AddInstalled("vim", "9.1.0000", installDir, 1024, nil); err != nil {
		t.Fatalf("AddInstalled: %v", err)
	}
	if err := idx.SetVersionLocked("vim", true); err != nil {
		t.Fatalf("SetVersionLocked: %v", err)
	}

	if err := idx.SyncInstalledFromPackages(root); err != nil {
		t.Fatalf("SyncInstalledFromPackages: %v", err)
	}

	inst, ok := idx.GetInstalled("vim")
	if !ok {
		t.Fatal("GetInstalled: vim not found after sync")
	}
	locked, ok := inst.Metadata["versionLocked"].(bool)
	if !ok || !locked {
		t.Fatalf("version lock lost after sync: %v", inst.Metadata)
	}
}

func TestSetVersionLocked_afterAddInstalledWithNilMetadata(t *testing.T) {
	root := testRoot(t)
	idx, err := NewIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	if err := idx.Add("vim", "9.1.0000", map[string]string{"abc": "vim.exe"}, 1024); err != nil {
		t.Fatalf("Add: %v", err)
	}
	installDir := filepath.Join(root, "apps", "vim", "9.1.0000")
	if err := idx.AddInstalled("vim", "9.1.0000", installDir, 1024, nil); err != nil {
		t.Fatalf("AddInstalled: %v", err)
	}

	if err := idx.SetVersionLocked("vim", true); err != nil {
		t.Fatalf("SetVersionLocked: %v", err)
	}

	inst, ok := idx.GetInstalled("vim")
	if !ok {
		t.Fatal("GetInstalled: vim not found")
	}
	locked, ok := inst.Metadata["versionLocked"].(bool)
	if !ok || !locked {
		t.Fatalf("versionLocked = %v, want true", inst.Metadata["versionLocked"])
	}
}

func TestGetActivityLog_and_QueryInstallHistory(t *testing.T) {
	root := testRoot(t)
	idx, err := NewIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	if err := idx.RecordActivity("install", "demo", "1.0", "success", map[string]interface{}{"source": "activity"}); err != nil {
		t.Fatalf("RecordActivity: %v", err)
	}
	if err := idx.Add("demo", "1.0", map[string]string{"abc": "demo.exe"}, 100); err != nil {
		t.Fatalf("Add: %v", err)
	}
	installDir := filepath.Join(root, "apps", "demo", "1.0")
	if err := idx.AddInstalled("demo", "1.0", installDir, 100, nil); err != nil {
		t.Fatalf("AddInstalled: %v", err)
	}

	activity, err := idx.GetActivityLog("demo", 10)
	if err != nil {
		t.Fatalf("GetActivityLog: %v", err)
	}
	if len(activity) != 1 {
		t.Fatalf("GetActivityLog: got %d rows, want 1", len(activity))
	}
	if activity[0]["operation"] != "install" {
		t.Fatalf("activity operation = %v", activity[0]["operation"])
	}
	details, _ := activity[0]["details"].(map[string]interface{})
	if details["source"] != "activity" {
		t.Fatalf("activity details = %v", details)
	}

	installHist, err := idx.QueryInstallHistory("demo", 10)
	if err != nil {
		t.Fatalf("QueryInstallHistory: %v", err)
	}
	if len(installHist) != 1 {
		t.Fatalf("QueryInstallHistory: got %d rows, want 1", len(installHist))
	}
	if installHist[0]["operation"] != "install" {
		t.Fatalf("install_history operation = %v", installHist[0]["operation"])
	}
}
