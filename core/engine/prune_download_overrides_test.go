package engine

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gluestick-sh/core/bucket"
	"github.com/gluestick-sh/core/config"
	"github.com/gluestick-sh/core/engine/internal/runtime"
	"github.com/gluestick-sh/core/manifest"
)

func TestPruneStaleManifestDownloadOverrides(t *testing.T) {
	root := t.TempDir()
	bucketDir := filepath.Join(root, "buckets", "extras", "bucket")
	if err := os.MkdirAll(bucketDir, 0o755); err != nil {
		t.Fatal(err)
	}
	keepPath := filepath.Join(bucketDir, "keep.json")
	dropPath := filepath.Join(bucketDir, "drop.json")
	if err := os.WriteFile(keepPath, []byte(`{"version":"1.0.0","url":"https://example.com/keep.exe","hash":"abc"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dropPath, []byte(`{"version":"1.0.0","url":"https://example.com/drop-old.exe","hash":"abc"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	keepHash, err := manifest.HashFile(keepPath)
	if err != nil {
		t.Fatal(err)
	}
	oldDropHash, err := manifest.HashFile(dropPath)
	if err != nil {
		t.Fatal(err)
	}

	if err := config.SetConfigManifestDownloadOverride(root, "extras/keep",
		[]string{"https://mirror.example/keep.exe"}, nil, keepHash); err != nil {
		t.Fatal(err)
	}
	if err := config.SetConfigManifestDownloadOverride(root, "extras/drop",
		[]string{"https://mirror.example/drop.exe"}, nil, oldDropHash); err != nil {
		t.Fatal(err)
	}
	if err := config.SetConfigManifestDownloadOverride(root, "extras/legacy",
		[]string{"https://mirror.example/legacy.exe"}, nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := config.SetConfigManifestDownloadOverride(root, "extras/gone",
		[]string{"https://mirror.example/gone.exe"}, nil, "deadbeef"); err != nil {
		t.Fatal(err)
	}

	// Simulate bucket upgrade for drop.json only.
	if err := os.WriteFile(dropPath, []byte(`{"version":"2.0.0","url":"https://example.com/drop-new.exe","hash":"def"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	reg, err := bucket.NewRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.ReloadFromDisk(); err != nil {
		t.Fatal(err)
	}
	e := &Engine{Engine: &runtime.Engine{
		Config:         &EngineConfig{RootDir: root},
		BucketRegistry: reg,
	}}

	removed, err := e.PruneStaleManifestDownloadOverrides()
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) < 3 {
		t.Fatalf("removed = %#v, want at least drop/legacy/gone", removed)
	}

	got, err := config.ReadConfigManifestDownloadOverrides(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("remaining = %#v", got)
	}
	if _, ok := got["extras/keep"]; !ok {
		t.Fatalf("fresh override should remain: %#v", got)
	}
}
