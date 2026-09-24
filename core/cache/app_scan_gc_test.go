package cache

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gluestick-sh/core/store"
	"github.com/gluestick-sh/core/message"
)

func TestIndexedFileCountForPackages(t *testing.T) {
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

	addPkg := func(name string, fileCount int) {
		files := make(map[string]string, fileCount)
		for i := 0; i < fileCount; i++ {
			hash, err := store.Write(strings.NewReader(fmt.Sprintf("%s-%d", name, i)))
			if err != nil {
				t.Fatal(err)
			}
			files[hash] = fmt.Sprintf("f%d.bin", i)
		}
		if err := idx.Add(name, "1.0", files, int64(fileCount)); err != nil {
			t.Fatal(err)
		}
	}
	addPkg("freecad", 1200)
	addPkg("tool", 50)
	addPkg("git", 30)

	got, err := idx.IndexedFileCountForPackages([]string{"freecad", "tool", "git"})
	if err != nil {
		t.Fatal(err)
	}
	if got != 1280 {
		t.Fatalf("IndexedFileCountForPackages = %d, want 1280", got)
	}
}

// TestPurgeOrphanBlobs_progressUsesWorkUnits verifies GC percent tracks real work units.
func TestPurgeOrphanBlobs_progressUsesWorkUnits(t *testing.T) {
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
	specs := []struct {
		name      string
		fileCount int
		subdirs   int
	}{
		{name: "freecad", fileCount: 40, subdirs: 20},
		{name: "tool", fileCount: 5, subdirs: 2},
		{name: "git", fileCount: 3, subdirs: 1},
	}

	var wantHashes []string
	for _, spec := range specs {
		installDir := filepath.Join(appsDir, spec.name, "1.0.0")
		if err := os.MkdirAll(installDir, 0755); err != nil {
			t.Fatal(err)
		}
		indexFiles := make(map[string]string, spec.fileCount)
		for i := 0; i < spec.fileCount; i++ {
			sub := filepath.Join(installDir, "data", fmt.Sprintf("d%02d", i%spec.subdirs))
			if err := os.MkdirAll(sub, 0755); err != nil {
				t.Fatal(err)
			}
			hash, err := store.Write(strings.NewReader(fmt.Sprintf("%s-%d", spec.name, i)))
			if err != nil {
				t.Fatal(err)
			}
			wantHashes = append(wantHashes, hash)
			indexFiles[hash] = fmt.Sprintf("data/d%02d/f%d.bin", i%spec.subdirs, i)
			if err := store.Link(hash, filepath.Join(sub, fmt.Sprintf("f%d.bin", i))); err != nil {
				t.Fatal(err)
			}
		}
		orphan := "1111222233334444555566667777888899990000aaaabbbbccccddddeeeeffff1111"
		writeCacheStoreObject(t, store, orphan, []byte("orphan"))
		if err := idx.Add(spec.name, "1.0.0", indexFiles, int64(spec.fileCount)); err != nil {
			t.Fatal(err)
		}
	}

	var events []GCProgressEvent
	_, _, err = PurgeOrphanBlobsWithProgress(idx, store, appsDir, func(ev GCProgressEvent) {
		events = append(events, ev)
		t.Logf("[%.1f%%] %s", ev.Percent, ev.Message())
	})
	if err != nil {
		t.Fatal(err)
	}

	var sawPlan bool
	for i, ev := range events {
		if i > 0 && ev.Percent+0.001 < events[i-1].Percent && ev.Percent < 100 {
			t.Fatalf("progress regressed: %.1f -> %.1f", events[i-1].Percent, ev.Percent)
		}
		if ev.MessageKey == message.GCAppsScanPlan {
			sawPlan = true
			if intArg(ev.MessageArgs, "fileTotal") < 48 {
				t.Fatalf("fileTotal = %v, want >= 48", ev.MessageArgs["fileTotal"])
			}
		}
	}
	if !sawPlan {
		t.Fatal("expected scan plan event")
	}
	if events[len(events)-1].Percent != 100 {
		t.Fatalf("final percent = %.1f, want 100", events[len(events)-1].Percent)
	}
}

func intArg(args map[string]interface{}, key string) int {
	if args == nil {
		return 0
	}
	switch v := args[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	default:
		return 0
	}
}
