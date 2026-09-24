package catalog

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gluestick-sh/core/bucket"
	"github.com/gluestick-sh/core/config"
	etypes "github.com/gluestick-sh/core/engine/types"
	"github.com/gluestick-sh/core/engine/internal/runtime"
)

func TestListCatalogPackagesHidesMainBucketPackage(t *testing.T) {
	root := t.TempDir()
	mainDir := filepath.Join(root, "buckets", "main", "deprecated")
	if err := os.MkdirAll(mainDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mainDir, "acorn.json"), []byte(`{
		"version": "1.0.0",
		"description": "acorn",
		"url": "https://example.com/acorn.zip",
		"hash": "abc"
	}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := config.AddConfigHiddenCatalogPackage(root, "acorn"); err != nil {
		t.Fatal(err)
	}

	br, err := bucket.NewRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := br.ReloadFromDisk(); err != nil {
		t.Fatal(err)
	}
	e := &runtime.Engine{
		Config:    &etypes.EngineConfig{RootDir: root},
		BucketRegistry: br,
		SearchIdx: runtime.NewIndex(),
	}
	runtime.RebuildSearchIndex(e)

	page, err := ListCatalogPackages(e, CatalogPackageQuery{
		Bucket:   "main",
		Query:    "acorn",
		Page:     1,
		PageSize: 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 0 {
		t.Fatalf("expected acorn hidden, got %#v", page.Items)
	}
}
