package cache

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gluestick-sh/core/store"
	"github.com/gluestick-sh/core/message"
)

func TestPurgePackage_progressUsesWorkUnits(t *testing.T) {
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
	if err := os.MkdirAll(appsDir, 0755); err != nil {
		t.Fatal(err)
	}

	indexFiles := make(map[string]string, 8)
	for i := 0; i < 8; i++ {
		hash, err := store.Write(strings.NewReader("blob-" + string(rune('a'+i))))
		if err != nil {
			t.Fatal(err)
		}
		indexFiles[hash] = "f" + string(rune('a'+i)) + ".bin"
	}
	if err := idx.Add("tool", "1.0.0", indexFiles, 8); err != nil {
		t.Fatal(err)
	}

	var events []GCProgressEvent
	removed, _, err := PurgePackageWithProgress(idx, store, appsDir, "tool", func(ev GCProgressEvent) {
		events = append(events, ev)
		t.Logf("[%.1f%%] %s", ev.Percent, ev.Message())
	})
	if err != nil {
		t.Fatal(err)
	}
	if removed != 8 {
		t.Fatalf("removed=%d, want 8", removed)
	}

	var sawPrepare bool
	for i, ev := range events {
		if i > 0 && ev.Percent+0.001 < events[i-1].Percent && ev.Percent < 100 {
			t.Fatalf("progress regressed: %.1f -> %.1f", events[i-1].Percent, ev.Percent)
		}
		if ev.MessageKey == message.PurgePrepare {
			sawPrepare = true
		}
	}
	if !sawPrepare {
		t.Fatal("expected prepare event")
	}
	if events[len(events)-1].Percent != 100 {
		t.Fatalf("final percent = %.1f, want 100", events[len(events)-1].Percent)
	}
}
