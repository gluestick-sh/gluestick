package downloader

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gluestick-sh/core/store"
)

func TestClearAllPartials_freesBytes(t *testing.T) {
	root := t.TempDir()
	st, err := store.NewStore(filepath.Join(root, "store"))
	if err != nil {
		t.Fatal(err)
	}
	d := NewDownloader(st)
	dir := filepath.Join(st.Path(), ".partial")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	part := filepath.Join(dir, "abcd.part")
	meta := filepath.Join(dir, "abcd.meta.json")
	if err := os.WriteFile(part, []byte("partial-payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(meta, []byte(`{"url":"https://example.com/x"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	removed, freed, err := d.ClearAllPartials()
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 {
		t.Fatalf("removed = %d, want 2", removed)
	}
	want := int64(len("partial-payload") + len(`{"url":"https://example.com/x"}`))
	if freed != want {
		t.Fatalf("freed = %d, want %d", freed, want)
	}
	if _, err := os.Stat(part); !os.IsNotExist(err) {
		t.Fatal("part file should be gone")
	}
}

func TestClearAllPartials_missingDir(t *testing.T) {
	root := t.TempDir()
	st, err := store.NewStore(filepath.Join(root, "store"))
	if err != nil {
		t.Fatal(err)
	}
	d := NewDownloader(st)
	removed, freed, err := d.ClearAllPartials()
	if err != nil || removed != 0 || freed != 0 {
		t.Fatalf("got removed=%d freed=%d err=%v", removed, freed, err)
	}
}
