package extractor

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gluestick-sh/core/store"
)

func TestIngestExtractedDirParallel(t *testing.T) {
	root := t.TempDir()
	extractDir := filepath.Join(root, "out")
	if err := os.MkdirAll(filepath.Join(extractDir, "sub"), 0755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		name := filepath.Join(extractDir, "sub", string(rune('a'+i))+".txt")
		if err := os.WriteFile(name, []byte("data"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	store, err := store.NewStore(filepath.Join(root, "store"))
	if err != nil {
		t.Fatal(err)
	}
	store.Prereqs()

	ext := NewExtractor(store)
	ext.SetWorkers(4)
	files, _, err := ext.ingestExtractedDir(extractDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 8 {
		t.Fatalf("got %d files, want 8", len(files))
	}
}

func TestBuild7zExtractArgsMMT(t *testing.T) {
	ext := NewExtractor(nil)
	ext.SetWorkers(8)
	args := ext.build7zExtractArgs(`C:\out`, `C:\a.7z`, false)
	if args[0] != "-mmt=8" {
		t.Fatalf("args = %v", args)
	}

	progressArgs := ext.build7zExtractArgs(`C:\out`, `C:\a.7z`, true)
	if progressArgs[0] != "-mmt=8" || progressArgs[1] != "-bso0" || progressArgs[2] != "-bsp1" {
		t.Fatalf("progress args = %v", progressArgs)
	}
}
