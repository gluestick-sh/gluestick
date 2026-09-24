package cache

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gluestick-sh/core/apps"
	"github.com/gluestick-sh/core/store"
)

func TestHashFromStorePath(t *testing.T) {
	storeRoot := filepath.Join(t.TempDir(), "store")
	hash64 := strings.Repeat("a", 64)

	tests := []struct {
		name   string
		path   string
		want   string
		wantOK bool
	}{
		{
			name:   "two-level layout",
			path:   filepath.Join(storeRoot, hash64[:2], hash64[2:]),
			want:   hash64,
			wantOK: true,
		},
		{
			name:   "flat 64-hex file",
			path:   filepath.Join(storeRoot, hash64),
			want:   hash64,
			wantOK: true,
		},
		{
			name:   "prefix directory",
			path:   filepath.Join(storeRoot, "ab"),
			wantOK: false,
		},
		{
			name:   "temp file ignored",
			path:   filepath.Join(storeRoot, ".tmp-foo"),
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := hashFromStorePath(storeRoot, tt.path)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && got != tt.want {
				t.Fatalf("hash = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAppReferencedHashes_emptyInputs(t *testing.T) {
	root := testRoot(t)
	store, err := store.NewStore(filepath.Join(root, "store"))
	if err != nil {
		t.Fatal(err)
	}

	refs, err := AppReferencedHashes("", store)
	if err != nil || len(refs) != 0 {
		t.Fatalf("empty appsDir: refs=%v err=%v", refs, err)
	}

	refs, err = AppReferencedHashes(filepath.Join(root, "apps"), nil)
	if err != nil || len(refs) != 0 {
		t.Fatalf("nil store: refs=%v err=%v", refs, err)
	}

	missing := filepath.Join(root, "no-such-apps")
	refs, err = AppReferencedHashes(missing, store)
	if err != nil || len(refs) != 0 {
		t.Fatalf("missing apps dir: refs=%v err=%v", refs, err)
	}
}

func TestAppReferencedHashes_latestVersionOnly(t *testing.T) {
	root := testRoot(t)
	store, err := store.NewStore(filepath.Join(root, "store"))
	if err != nil {
		t.Fatal(err)
	}
	store.Prereqs()

	currentHash, err := store.Write(strings.NewReader("current-version"))
	if err != nil {
		t.Fatal(err)
	}

	appsDir := filepath.Join(root, "apps")
	oldDir := filepath.Join(appsDir, "tool", "1.0.0")
	newDir := filepath.Join(appsDir, "tool", "2.0.0")
	if err := os.MkdirAll(oldDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(newDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldDir, "old.exe"), []byte("obsolete"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := store.Link(currentHash, filepath.Join(newDir, "tool.exe")); err != nil {
		t.Fatal(err)
	}

	refs, err := AppReferencedHashes(appsDir, store)
	if err != nil {
		t.Fatal(err)
	}
	if !refs[currentHash] {
		t.Fatalf("expected current version hash %s in refs, got %v", currentHash[:8], refs)
	}
	if len(refs) != 1 {
		t.Fatalf("expected exactly one referenced hash, got %d", len(refs))
	}
}

func TestCollectReferencedHashes_unionIndexAndApps(t *testing.T) {
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

	indexOnly := "aaaabbbbccccddddeeeeffff00001111222233334444555566667777888899990000"
	appsOnlyHash, err := store.Write(strings.NewReader("apps-only"))
	if err != nil {
		t.Fatal(err)
	}
	writeCacheStoreObject(t, store, indexOnly, []byte("index-only"))

	if err := idx.Add("pkg-a", "1.0", map[string]string{indexOnly: "a.exe"}, 10); err != nil {
		t.Fatal(err)
	}

	appsDir := filepath.Join(root, "apps")
	installDir := filepath.Join(appsDir, "pkg-b", "1.0")
	if err := os.MkdirAll(installDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := store.Link(appsOnlyHash, filepath.Join(installDir, "b.exe")); err != nil {
		t.Fatal(err)
	}

	refs, err := CollectReferencedHashes(idx, appsDir, store)
	if err != nil {
		t.Fatal(err)
	}
	if !refs[indexOnly] || !refs[appsOnlyHash] {
		t.Fatalf("expected both hashes referenced, got %v", refs)
	}
}

func TestPurgePackage_keepsHashReferencedByApps(t *testing.T) {
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

	hash, err := store.Write(strings.NewReader("shared-binary"))
	if err != nil {
		t.Fatal(err)
	}

	appsDir := filepath.Join(root, "apps")
	installDir := filepath.Join(appsDir, "make", "4.4.1")
	if err := os.MkdirAll(filepath.Join(installDir, "bin"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := store.Link(hash, filepath.Join(installDir, "bin", "make.exe")); err != nil {
		t.Fatal(err)
	}

	if err := idx.Add("upm", "1.0.0", map[string]string{hash: "upm.exe"}, 10); err != nil {
		t.Fatal(err)
	}

	removed, _, err := PurgePackage(idx, store, appsDir, "upm")
	if err != nil {
		t.Fatalf("PurgePackage: %v", err)
	}
	if removed != 0 {
		t.Fatalf("hash still linked under apps/, removed=%d", removed)
	}
	if _, err := os.Stat(store.ObjectPath(hash)); err != nil {
		t.Fatalf("Cache store blobs should remain: %v", err)
	}
}

func TestPurgePackage_emptyAppsDir_indexOnly(t *testing.T) {
	root := testRoot(t)
	store, err := store.NewStore(filepath.Join(root, "store"))
	if err != nil {
		t.Fatal(err)
	}
	idx, err := NewIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	hash := "deadbeef00000000000000000000000000000000000000000000000000000000"
	writeCacheStoreObject(t, store, hash, []byte("solo"))
	if err := idx.Add("solo", "1.0", map[string]string{hash: "solo.exe"}, 4); err != nil {
		t.Fatal(err)
	}

	removed, _, err := PurgePackage(idx, store, "", "solo")
	if err != nil {
		t.Fatalf("PurgePackage: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed=%d, want 1", removed)
	}
	if _, err := os.Stat(store.ObjectPath(hash)); !os.IsNotExist(err) {
		t.Fatal("blob should be deleted when only index referenced it")
	}
}

func TestPurgeOrphanBlobs(t *testing.T) {
	root := testRoot(t)
	store, err := store.NewStore(filepath.Join(root, "store"))
	if err != nil {
		t.Fatal(err)
	}
	idx, err := NewIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	kept := "aaaabbbbccccddddeeeeffff00001111222233334444555566667777888899990000"
	orphan := "1111222233334444555566667777888899990000aaaabbbbccccddddeeeeffff1111"
	writeCacheStoreObject(t, store, kept, []byte("keep"))
	writeCacheStoreObject(t, store, orphan, []byte("orphan"))

	if err := idx.Add("upm", "1.0.0", map[string]string{kept: "upm.exe"}, 4); err != nil {
		t.Fatal(err)
	}

	removed, freed, err := PurgeOrphanBlobs(idx, store, filepath.Join(root, "apps"))
	if err != nil {
		t.Fatalf("PurgeOrphanBlobs: %v", err)
	}
	if removed != 1 || freed != int64(len("orphan")) {
		t.Fatalf("removed=%d freed=%d, want 1 and %d", removed, freed, len("orphan"))
	}
	if _, err := os.Stat(store.ObjectPath(kept)); err != nil {
		t.Fatal("kept blob should remain")
	}
	if _, err := os.Stat(store.ObjectPath(orphan)); !os.IsNotExist(err) {
		t.Fatal("orphan blob should be deleted")
	}
}

func TestPurgeOrphanBlobs_keepsAppsOnlyReference(t *testing.T) {
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

	orphan := "222233334444555566667777888899990000aaaabbbbccccddddeeeeffff00001111"
	appsHash, err := store.Write(strings.NewReader("installed"))
	if err != nil {
		t.Fatal(err)
	}
	writeCacheStoreObject(t, store, orphan, []byte("orphan"))

	appsDir := filepath.Join(root, "apps")
	installDir := filepath.Join(appsDir, "node", "20.0.0")
	if err := os.MkdirAll(installDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := store.Link(appsHash, filepath.Join(installDir, "node.exe")); err != nil {
		t.Fatal(err)
	}

	removed, _, err := PurgeOrphanBlobs(idx, store, appsDir)
	if err != nil {
		t.Fatalf("PurgeOrphanBlobs: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed=%d, want 1 orphan", removed)
	}
	if _, err := os.Stat(store.ObjectPath(appsHash)); err != nil {
		t.Fatal("apps-referenced blob should remain")
	}
	if _, err := os.Stat(store.ObjectPath(orphan)); !os.IsNotExist(err) {
		t.Fatal("orphan should be deleted")
	}
}

func TestPurgeOrphanBlobs_requiresStore(t *testing.T) {
	root := testRoot(t)
	idx, err := NewIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	_, _, err = PurgeOrphanBlobs(idx, nil, filepath.Join(root, "apps"))
	if err == nil {
		t.Fatal("expected error when store is nil")
	}
}

func TestScanInstallDir_skipsHiddenFiles(t *testing.T) {
	root := testRoot(t)
	store, err := store.NewStore(filepath.Join(root, "store"))
	if err != nil {
		t.Fatal(err)
	}
	store.Prereqs()

	installDir := filepath.Join(root, "install")
	if err := os.MkdirAll(installDir, 0755); err != nil {
		t.Fatal(err)
	}

	visibleHash, err := store.Write(strings.NewReader("visible"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Link(visibleHash, filepath.Join(installDir, "tool.exe")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(installDir, ".intermediate.tar"), []byte("hidden"), 0644); err != nil {
		t.Fatal(err)
	}

	files, _, err := scanInstallDir(installDir, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("files = %v, want one visible entry", files)
	}
	if _, ok := files[visibleHash]; !ok {
		t.Fatalf("expected visible hash in map, got %v", files)
	}
}

func TestScanInstallDir_archiveOnlyExtractCountsFileSize(t *testing.T) {
	root := testRoot(t)
	store, err := store.NewStore(filepath.Join(root, "store"))
	if err != nil {
		t.Fatal(err)
	}

	installDir := filepath.Join(root, "install")
	if err := os.MkdirAll(installDir, 0755); err != nil {
		t.Fatal(err)
	}
	payload := []byte("extracted-app")
	if err := os.WriteFile(filepath.Join(installDir, "app.exe"), payload, 0644); err != nil {
		t.Fatal(err)
	}

	files, total, err := scanInstallDir(installDir, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("files = %v", files)
	}
	if total != int64(len(payload)) {
		t.Fatalf("total = %d, want %d", total, len(payload))
	}
}

func TestLatestVersionDir(t *testing.T) {
	entries := []os.DirEntry{
		dirEntry("1.0.0"),
		dirEntry("2.1.0"),
		dirEntry("1.9.9"),
		dirEntry(apps.CurrentLinkName),
		fileEntry("readme.txt"),
	}
	if got := latestVersionDir(entries); got != "2.1.0" {
		t.Fatalf("latestVersionDir = %q, want 2.1.0", got)
	}
	if got := latestVersionDir(nil); got != "" {
		t.Fatalf("empty entries = %q, want empty", got)
	}
}

type fakeDirEntry struct {
	name  string
	isDir bool
}

func (f fakeDirEntry) Name() string               { return f.name }
func (f fakeDirEntry) IsDir() bool                { return f.isDir }
func (f fakeDirEntry) Type() os.FileMode          { return 0 }
func (f fakeDirEntry) Info() (os.FileInfo, error) { return nil, nil }

func dirEntry(name string) os.DirEntry  { return fakeDirEntry{name: name, isDir: true} }
func fileEntry(name string) os.DirEntry { return fakeDirEntry{name: name, isDir: false} }
