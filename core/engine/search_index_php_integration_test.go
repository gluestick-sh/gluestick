package engine

import (
	"github.com/gluestick-sh/core/bucket"
	"github.com/gluestick-sh/core/engine/internal/runtime"

	"os"
	"path/filepath"
	"testing"
)

func TestRealPHPBucketIndex(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(home, ".glue")
	phpBucket := filepath.Join(root, "buckets", "php")
	if _, err := os.Stat(filepath.Join(phpBucket, "bucket")); err != nil {
		t.Skip("php bucket not installed")
	}

	paths := runtime.BucketManifestPaths(phpBucket, "php")
	if len(paths) == 0 {
		t.Fatal("expected php bucket manifests, got 0 paths")
	}
	t.Logf("manifest paths: %d", len(paths))

	entries := runtime.ScanBucketManifestEntries(phpBucket, "php")
	if len(entries) == 0 {
		t.Fatal("expected indexed php entries, got 0")
	}
	t.Logf("indexed entries: %d", len(entries))

	bucketRegistry, err := bucket.NewRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	bucketRegistry.ReloadFromDisk()

	e := &Engine{Engine: &runtime.Engine{
		Config:         &EngineConfig{RootDir: root},
		BucketRegistry: bucketRegistry,
		SearchIdx:      runtime.NewIndex(),
	}}
	runtime.RebuildSearchIndex(e.Engine)

	counts := e.SearchIdx.CountByBucket(false, nil)
	if counts["php"] == 0 {
		t.Fatalf("expected php in search index, counts=%v", counts)
	}

	matches := e.SearchIdx.ListEntries("php", "php8", false, nil)
	if len(matches) == 0 {
		t.Fatalf("expected php8 matches in php bucket")
	}
}
