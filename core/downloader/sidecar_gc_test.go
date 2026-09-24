package downloader

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gluestick-sh/core/store"
)

func TestClearStaleSidecarIndexes(t *testing.T) {
	root := t.TempDir()
	st, err := store.NewStore(filepath.Join(root, "store"))
	if err != nil {
		t.Fatal(err)
	}
	st.Prereqs()
	d := NewDownloader(st)

	liveHash, err := st.Write(strings.NewReader("live-blob"))
	if err != nil {
		t.Fatal(err)
	}
	deadHash := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	zipDir := filepath.Join(st.Path(), ".zip-index")
	if err := os.MkdirAll(zipDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := saveZipMemberIndex(st.Path(), liveHash, map[string]string{"a.txt": liveHash}, 9); err != nil {
		t.Fatal(err)
	}
	if err := saveZipMemberIndex(st.Path(), deadHash, map[string]string{"b.txt": deadHash}, 1); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(zipDir, "junk.tmp"), []byte("tmp"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := saveManifestHashAlias(st.Path(), "sha512", "digest-live", liveHash); err != nil {
		t.Fatal(err)
	}
	if err := saveManifestHashAlias(st.Path(), "sha512", "digest-dead", deadHash); err != nil {
		t.Fatal(err)
	}

	removed, _, err := d.ClearStaleSidecarIndexes()
	if err != nil {
		t.Fatal(err)
	}
	if removed < 3 {
		t.Fatalf("removed = %d, want at least dead zip-index, tmp, and dead manifest-hash", removed)
	}
	if _, err := os.Stat(zipMemberIndexPath(st.Path(), liveHash)); err != nil {
		t.Fatalf("live zip-index should remain: %v", err)
	}
	if _, err := os.Stat(zipMemberIndexPath(st.Path(), deadHash)); !os.IsNotExist(err) {
		t.Fatal("dead zip-index should be removed")
	}
	if _, err := os.Stat(manifestHashIndexPath(st.Path(), "sha512", "digest-live")); err != nil {
		t.Fatalf("live manifest-hash should remain: %v", err)
	}
	if _, err := os.Stat(manifestHashIndexPath(st.Path(), "sha512", "digest-dead")); !os.IsNotExist(err) {
		t.Fatal("dead manifest-hash should be removed")
	}
}
