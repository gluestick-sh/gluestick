package engine

import (
	"github.com/gluestick-sh/core/bucket"
	"github.com/gluestick-sh/core/engine/internal/catalog"
	"github.com/gluestick-sh/core/engine/internal/runtime"

	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestSearchIndexBucketLifecycle(t *testing.T) {
	root := t.TempDir()
	bucketsDir := filepath.Join(root, "buckets")
	mainDir := filepath.Join(bucketsDir, "main", "bucket")
	if err := os.MkdirAll(mainDir, 0755); err != nil {
		t.Fatal(err)
	}

	writeManifest := func(name, description string) {
		path := filepath.Join(mainDir, name+".json")
		content := `{
  "version": "1.0.0",
  "description": "` + description + `",
  "url": "https://example.com/` + name + `.zip",
  "hash": "abc123"
}`
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}

	writeManifest("git", "The stupid content tracker")
	writeManifest("go", "The Go programming language")

	bucketRegistry, err := bucket.NewRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := bucketRegistry.ReloadFromDisk(); err != nil {
		t.Fatal(err)
	}

	e := &Engine{Engine: &runtime.Engine{Config: &EngineConfig{RootDir: root}, BucketRegistry: bucketRegistry, SearchIdx: runtime.NewIndex()}}
	runtime.RebuildSearchIndex(e.Engine)

	matches := e.SearchIdx.Search("git", nil, nil)
	if len(matches) != 1 {
		t.Fatalf("expected 1 match for git, got %d", len(matches))
	}
	if matches[0].Bucket != "main" || matches[0].Name != "git" {
		t.Fatalf("unexpected match: %+v", matches[0])
	}

	extrasDir := filepath.Join(bucketsDir, "extras", "bucket")
	if err := os.MkdirAll(extrasDir, 0755); err != nil {
		t.Fatal(err)
	}
	writeManifestPath := filepath.Join(extrasDir, "7zip.json")
	if err := os.WriteFile(writeManifestPath, []byte(`{
  "version": "24.08",
  "description": "File archiver",
  "url": "https://example.com/7zip.zip",
  "hash": "def456"
}`), 0644); err != nil {
		t.Fatal(err)
	}

	bucketRegistry.ReloadFromDisk()
	runtime.SyncSearchIndex(e.Engine, false)

	matches = e.SearchIdx.Search("7zip", nil, nil)
	if len(matches) != 1 || matches[0].Bucket != "extras" {
		t.Fatalf("expected extras/7zip match, got %+v", matches)
	}

	if err := os.RemoveAll(filepath.Join(bucketsDir, "extras")); err != nil {
		t.Fatal(err)
	}
	bucketRegistry.ReloadFromDisk()
	runtime.SyncSearchIndex(e.Engine, false)

	matches = e.SearchIdx.Search("7zip", nil, nil)
	if len(matches) != 0 {
		t.Fatalf("expected no matches after bucket removal, got %+v", matches)
	}

	if len(e.SearchIdx.FindExactName("git")) != 1 {
		t.Fatal("expected git to remain indexed in main bucket")
	}
}

func TestCountAvailablePackagesMatchesIndex(t *testing.T) {
	root := t.TempDir()
	bucketsDir := filepath.Join(root, "buckets")
	mainDir := filepath.Join(bucketsDir, "main", "bucket")
	if err := os.MkdirAll(mainDir, 0755); err != nil {
		t.Fatal(err)
	}
	for _, spec := range []struct{ name, desc string }{
		{"git", "content tracker"},
		{"go", "Go language"},
	} {
		content := `{
  "version": "1.0.0",
  "description": "` + spec.desc + `",
  "url": "https://example.com/` + spec.name + `.zip",
  "hash": "abc123"
}`
		if err := os.WriteFile(filepath.Join(mainDir, spec.name+".json"), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}

	bucketRegistry, err := bucket.NewRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	bucketRegistry.ReloadFromDisk()

	e := &Engine{Engine: &runtime.Engine{Config: &EngineConfig{RootDir: root}, BucketRegistry: bucketRegistry, SearchIdx: runtime.NewIndex()}}
	runtime.RebuildSearchIndex(e.Engine)

	if got := e.CountAvailablePackages(false); got != 2 {
		t.Fatalf("CountAvailablePackages() = %d, want 2", got)
	}
	counts := e.PackageCountsByBucket()
	if counts["main"] != 2 {
		t.Fatalf("PackageCountsByBucket()[main] = %d, want 2", counts["main"])
	}
	summaries := e.CatalogBuckets(catalog.CatalogBucketsQuery{})
	if len(summaries) != 1 || summaries[0].PackageCount != 2 {
		t.Fatalf("CatalogBuckets() = %+v, want main=2", summaries)
	}
	if e.CountAvailablePackages(false) != summaries[0].PackageCount {
		t.Fatal("total count should match single bucket count in test fixture")
	}
}

func TestSyncSearchIndexLoadsNewBucketOnly(t *testing.T) {
	root := t.TempDir()
	bucketsDir := filepath.Join(root, "buckets")
	mainDir := filepath.Join(bucketsDir, "main", "bucket")
	if err := os.MkdirAll(mainDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mainDir, "git.json"), []byte(`{
  "version": "1.0.0",
  "description": "content tracker",
  "url": "https://example.com/git.zip",
  "hash": "abc123"
}`), 0644); err != nil {
		t.Fatal(err)
	}

	bucketRegistry, err := bucket.NewRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	bucketRegistry.ReloadFromDisk()

	e := &Engine{Engine: &runtime.Engine{Config: &EngineConfig{RootDir: root}, BucketRegistry: bucketRegistry, SearchIdx: runtime.NewIndex()}}
	runtime.RebuildSearchIndex(e.Engine)
	if !e.SearchIdx.HasLoadedBucket("main") {
		t.Fatal("expected main bucket to be indexed")
	}

	extrasDir := filepath.Join(bucketsDir, "extras", "bucket")
	if err := os.MkdirAll(extrasDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(extrasDir, "7zip.json"), []byte(`{
  "version": "24.08",
  "description": "File archiver",
  "url": "https://example.com/7zip.zip",
  "hash": "def456"
}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := bucketRegistry.ReloadFromDisk(); err != nil {
		t.Fatal(err)
	}

	runtime.SyncSearchIndex(e.Engine, false)

	if !e.SearchIdx.HasLoadedBucket("extras") {
		t.Fatal("expected new extras bucket to be indexed immediately")
	}
	matches := e.SearchIdx.Search("7zip", nil, nil)
	if len(matches) != 1 || matches[0].Bucket != "extras" {
		t.Fatalf("expected extras/7zip, got %+v", matches)
	}
}
func TestSearchIndexDeprecatedFilter(t *testing.T) {
	root := t.TempDir()
	bucketsDir := filepath.Join(root, "buckets")
	lemonDir := filepath.Join(bucketsDir, "lemon")
	if err := os.MkdirAll(filepath.Join(lemonDir, "bucket"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(lemonDir, "deprecated"), 0755); err != nil {
		t.Fatal(err)
	}
	writeManifest := func(path string) {
		if err := os.WriteFile(path, []byte(`{
			"version": "1.0.0",
			"description": "active app",
			"url": "https://example.com/a.zip",
			"hash": "abc"
		}`), 0644); err != nil {
			t.Fatal(err)
		}
	}
	writeManifest(filepath.Join(lemonDir, "bucket", "active.json"))
	writeManifest(filepath.Join(lemonDir, "deprecated", "old.json"))

	bucketRegistry, err := bucket.NewRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := bucketRegistry.ReloadFromDisk(); err != nil {
		t.Fatal(err)
	}
	e := &Engine{Engine: &runtime.Engine{BucketRegistry: bucketRegistry, SearchIdx: runtime.NewIndex()}}
	runtime.RebuildSearchIndex(e.Engine)

	all := e.SearchIdx.ListEntries("lemon", "", false, nil)
	if len(all) != 2 {
		t.Fatalf("listEntries all = %d, want 2", len(all))
	}
	hidden := e.SearchIdx.ListEntries("lemon", "", true, nil)
	if len(hidden) != 1 || hidden[0].Name != "active" {
		t.Fatalf("listEntries hide deprecated = %+v", hidden)
	}
	counts := e.SearchIdx.CountByBucket(true, nil)
	if counts["lemon"] != 1 {
		t.Fatalf("countByBucket hide = %d", counts["lemon"])
	}
}

func TestSearchIndexArchivedInDeprecatedDir(t *testing.T) {
	root := t.TempDir()
	mainDir := filepath.Join(root, "buckets", "main")
	if err := os.MkdirAll(filepath.Join(mainDir, "deprecated"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mainDir, "deprecated", "axel.json"), []byte(`{
		"version": "2.16.1-1",
		"description": "Lightweight download accelerator",
		"url": "https://example.com/axel.zip",
		"hash": "abc"
	}`), 0644); err != nil {
		t.Fatal(err)
	}

	bucketRegistry, err := bucket.NewRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := bucketRegistry.ReloadFromDisk(); err != nil {
		t.Fatal(err)
	}
	e := &Engine{Engine: &runtime.Engine{BucketRegistry: bucketRegistry, SearchIdx: runtime.NewIndex()}}
	runtime.RebuildSearchIndex(e.Engine)

	entries := e.SearchIdx.ListEntries("main", "axel", false, nil)
	if len(entries) != 1 {
		t.Fatalf("entries = %+v", entries)
	}
	if !entries[0].Archived {
		t.Fatal("expected deprecated/ path")
	}
	if !entries[0].BrowseDeprecated() {
		t.Fatal("deprecated/ manifest should be marked for browse")
	}
}

func TestSearchIndexPartialNameMatch(t *testing.T) {
	root := t.TempDir()
	mainDir := filepath.Join(root, "buckets", "main", "bucket")
	if err := os.MkdirAll(mainDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mainDir, "dagger.json"), []byte(`{
  "version": "0.21.6",
  "description": "A portable dev kit for CI/CD pipelines",
  "url": "https://example.com/dagger.zip",
  "hash": "abc123"
}`), 0644); err != nil {
		t.Fatal(err)
	}

	bucketRegistry, err := bucket.NewRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := bucketRegistry.ReloadFromDisk(); err != nil {
		t.Fatal(err)
	}
	e := &Engine{Engine: &runtime.Engine{BucketRegistry: bucketRegistry, SearchIdx: runtime.NewIndex()}}
	runtime.RebuildSearchIndex(e.Engine)

	matches := e.SearchIdx.Search("dag", nil, nil)
	if len(matches) != 1 || matches[0].Name != "dagger" {
		t.Fatalf("expected dagger for dag, got %+v", matches)
	}
}

func TestSearchWaitsForAsyncIndexBuild(t *testing.T) {
	root := t.TempDir()
	mainDir := filepath.Join(root, "buckets", "main", "bucket")
	if err := os.MkdirAll(mainDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mainDir, "dagger.json"), []byte(`{
  "version": "0.21.6",
  "description": "A portable dev kit for CI/CD pipelines",
  "url": "https://example.com/dagger.zip",
  "hash": "abc123"
}`), 0644); err != nil {
		t.Fatal(err)
	}

	bucketRegistry, err := bucket.NewRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := bucketRegistry.ReloadFromDisk(); err != nil {
		t.Fatal(err)
	}
	e := &Engine{Engine: &runtime.Engine{BucketRegistry: bucketRegistry, SearchIdx: runtime.NewIndex()}}

	ready := make(chan struct{})
	go func() {
		runtime.RebuildSearchIndex(e.Engine)
		close(ready)
	}()

	pkgs, err := e.Search(context.Background(), &SearchRequest{Query: "dag"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(pkgs) != 1 || pkgs[0].Name != "dagger" {
		t.Fatalf("Search(dag) = %+v, want dagger", pkgs)
	}
	<-ready
}
