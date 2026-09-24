package install

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gluestick-sh/core/safepath"
	"github.com/gluestick-sh/core/store"
)

func TestLinkExtractedFiles_rejectsPathTraversal(t *testing.T) {
	root := t.TempDir()
	installDir := filepath.Join(root, "apps", "evil", "1.0.0")
	if err := os.MkdirAll(installDir, 0755); err != nil {
		t.Fatal(err)
	}
	st, err := store.NewStore(filepath.Join(root, "store"))
	if err != nil {
		t.Fatal(err)
	}
	hash := "abc123def4567890abcdef1234567890abcdef1234567890abcdef1234567890"
	writeCASObject(t, st, hash, []byte("payload"))
	files := map[string]string{
		"../../escape.exe": hash,
	}
	_, err = LinkExtractedFiles(st, installDir, "", "", files, nil)
	if err == nil {
		t.Fatal("expected path traversal error")
	}
	if !errors.Is(err, safepath.ErrUnsafePath) {
		t.Fatalf("expected ErrUnsafePath, got %v", err)
	}
}
