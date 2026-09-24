package cache

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gluestick-sh/core/store"
)

func TestScanAppInstallDirsParallel_largePackageUsesSubdirs(t *testing.T) {
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

	appsDir := filepath.Join(root, "apps")
	largeInstall := filepath.Join(appsDir, "freecad", "1.0.0")
	smallInstall := filepath.Join(appsDir, "tool", "1.0.0")
	for _, dir := range []string{largeInstall, smallInstall} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}

	hashes := make([]string, 0, 12)
	indexFiles := make(map[string]string)
	for i := 0; i < 10; i++ {
		sub := filepath.Join(largeInstall, "lib", "mod"+string(rune('a'+i)))
		if err := os.MkdirAll(sub, 0755); err != nil {
			t.Fatal(err)
		}
		hash, err := store.Write(strings.NewReader("large-" + string(rune('a'+i))))
		if err != nil {
			t.Fatal(err)
		}
		hashes = append(hashes, hash)
		indexFiles[hash] = fmt.Sprintf("lib/mod%c/part.bin", 'a'+i)
		if err := store.Link(hash, filepath.Join(sub, "part.bin")); err != nil {
			t.Fatal(err)
		}
	}
	smallHash, err := store.Write(strings.NewReader("small"))
	if err != nil {
		t.Fatal(err)
	}
	hashes = append(hashes, smallHash)
	indexFiles[smallHash] = "tool.exe"
	if err := store.Link(smallHash, filepath.Join(smallInstall, "tool.exe")); err != nil {
		t.Fatal(err)
	}
	if err := idx.Add("freecad", "1.0.0", indexFiles, 11); err != nil {
		t.Fatal(err)
	}

	plan, err := buildAppScanPlan(appsDir, idx)
	if err != nil {
		t.Fatal(err)
	}
	keyIndex, _, err := scanStoreParallel(store, nil)
	if err != nil {
		t.Fatal(err)
	}
	refs, err := scanAppInstallDirsParallel(plan, store, keyIndex, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, hash := range hashes {
		if !refs[hash] {
			t.Fatalf("missing hash %s in refs", hash[:8])
		}
	}
}

func TestScanAppInstallDirsParallel_manySubdirsNoDeadlock(t *testing.T) {
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

	installDir := filepath.Join(root, "apps", "wide", "1.0.0")
	if err := os.MkdirAll(installDir, 0755); err != nil {
		t.Fatal(err)
	}

	const subdirs = 300
	for i := 0; i < subdirs; i++ {
		sub := filepath.Join(installDir, "branch", fmt.Sprintf("dir%03d", i))
		if err := os.MkdirAll(sub, 0755); err != nil {
			t.Fatal(err)
		}
		hash, err := store.Write(strings.NewReader("blob-" + sub))
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Link(hash, filepath.Join(sub, "file.bin")); err != nil {
			t.Fatal(err)
		}
	}

	plan, err := buildAppScanPlan(filepath.Join(root, "apps"), idx)
	if err != nil {
		t.Fatal(err)
	}
	keyIndex, _, err := scanStoreParallel(store, nil)
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		_, err := scanAppInstallDirsParallel(plan, store, keyIndex, nil)
		if err != nil {
			t.Errorf("scan: %v", err)
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("parallel app scan deadlocked with many subdirectories")
	}
}
