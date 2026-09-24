package install

import (
	"github.com/gluestick-sh/core/engine/internal/runtime"
	etypes "github.com/gluestick-sh/core/engine/types"

	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestIsInnounpHelperPackage(t *testing.T) {
	if !isInnounpHelperPackage("innounp") || !isInnounpHelperPackage(" Innounp ") {
		t.Fatal("expected innounp to be a helper package")
	}
	if isInnounpHelperPackage("python") {
		t.Fatal("python should not be an innounp helper package")
	}
}

func TestEnsureInnounpWithProfSkipsWhenNotNeeded(t *testing.T) {
	e := &runtime.Engine{Config: &etypes.EngineConfig{RootDir: t.TempDir()}}
	path, err := ensureInnounpWithProf(e, context.Background(), nil, false, "python")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if path != "" {
		t.Fatalf("expected empty path, got %q", path)
	}
}

func TestResolveInnounpPrefersBootstrapBin(t *testing.T) {
	root := t.TempDir()
	bootstrap := filepath.Join(root, "bin", "innounp", "innounp.exe")
	if err := os.MkdirAll(filepath.Dir(bootstrap), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bootstrap, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	got, err := resolveInnounp(root)
	if err != nil {
		t.Fatalf("resolveInnounp: %v", err)
	}
	if got != bootstrap {
		t.Fatalf("got %q, want %q", got, bootstrap)
	}
}
