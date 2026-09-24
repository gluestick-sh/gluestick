package engine

import (
	"github.com/gluestick-sh/core/bucket"
	"github.com/gluestick-sh/core/engine/internal/runtime"

	"os"
	"path/filepath"
	"testing"

	"github.com/gluestick-sh/core/apps"
	"github.com/gluestick-sh/core/cache"
)

func TestCheckPackageUpdatesUsesActiveVersionOnDisk(t *testing.T) {
	root := t.TempDir()
	pkgName := "zotero"
	pkgRoot := filepath.Join(root, "apps", pkgName)
	oldVer := "9.0.4"
	newVer := "9.0.5"
	for _, ver := range []string{oldVer, newVer} {
		dir := filepath.Join(pkgRoot, ver)
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "install.json"), []byte(`{
			"version": "`+ver+`",
			"bucket": "main",
			"manifest": {"version":"`+ver+`"}
		}`), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := apps.LinkCurrent(pkgRoot, newVer); err != nil {
		t.Fatal(err)
	}

	idx, err := cache.NewIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	if err := idx.Add(pkgName, oldVer, map[string]string{"abc": "file.txt"}, 10); err != nil {
		t.Fatal(err)
	}

	bucketsDir := filepath.Join(root, "buckets", "main", "bucket")
	if err := os.MkdirAll(bucketsDir, 0755); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(bucketsDir, pkgName+".json")
	if err := os.WriteFile(manifestPath, []byte(`{"version":"`+newVer+`"}`), 0644); err != nil {
		t.Fatal(err)
	}
	bucketRegistry, err := bucket.NewRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := bucketRegistry.ReloadFromDisk(); err != nil {
		t.Fatal(err)
	}

	e := &Engine{Engine: &runtime.Engine{
		Config:         &EngineConfig{RootDir: root},
		Cache:          idx,
		BucketRegistry: bucketRegistry,
	}}

	updates, err := e.CheckPackageUpdates()
	if err != nil {
		t.Fatalf("CheckPackageUpdates: %v", err)
	}
	if len(updates) != 0 {
		t.Fatalf("updates = %+v, want none when active version matches manifest", updates)
	}

	entry, ok := idx.Get(pkgName)
	if !ok || entry.Version != newVer {
		t.Fatalf("indexed version = %q, want %q after heal", entry.Version, newVer)
	}
}

func TestCheckPackageUpdatesAfterCacheIndexClear(t *testing.T) {
	root := t.TempDir()
	pkgName := "tor-browser"
	installedVer := "14.0.1"
	latestVer := "14.0.2"
	pkgRoot := filepath.Join(root, "apps", pkgName)
	installDir := filepath.Join(pkgRoot, installedVer)
	if err := os.MkdirAll(installDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(installDir, "install.json"), []byte(`{
		"version": "`+installedVer+`",
		"bucket": "extras",
		"manifest": {"version":"`+installedVer+`"}
	}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := apps.LinkCurrent(pkgRoot, installedVer); err != nil {
		t.Fatal(err)
	}

	idx, err := cache.NewIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	if err := idx.ClearAll(); err != nil {
		t.Fatal(err)
	}

	bucketsDir := filepath.Join(root, "buckets", "extras", "bucket")
	if err := os.MkdirAll(bucketsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bucketsDir, pkgName+".json"),
		[]byte(`{"version":"`+latestVer+`","url":"https://example.com/tor-browser.zip","hash":"abc"}`), 0644); err != nil {
		t.Fatal(err)
	}
	bucketRegistry, err := bucket.NewRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := bucketRegistry.ReloadFromDisk(); err != nil {
		t.Fatal(err)
	}

	e := &Engine{Engine: &runtime.Engine{
		Config:         &EngineConfig{RootDir: root},
		Cache:          idx,
		BucketRegistry: bucketRegistry,
	}}

	versions, err := e.installedPackagesForUpdateCheck()
	if err != nil {
		t.Fatalf("installedPackagesForUpdateCheck: %v", err)
	}
	if len(versions) != 1 {
		t.Fatalf("installedPackagesForUpdateCheck = %#v, want one package", versions)
	}

	updates, err := e.CheckPackageUpdates()
	if err != nil {
		t.Fatalf("CheckPackageUpdates: %v", err)
	}
	if len(updates) != 1 {
		t.Fatalf("updates = %+v, want one pending update after cache clear", updates)
	}
	if updates[0].Name != pkgName || updates[0].LatestVersion != latestVer {
		t.Fatalf("updates[0] = %+v, want %s -> %s", updates[0], installedVer, latestVer)
	}
}

func TestInstalledPackagesForUpdateCheckFallsBackToCache(t *testing.T) {
	root := t.TempDir()
	idx, err := cache.NewIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	if err := idx.Add("orphan", "1.0.0", map[string]string{"a": "b"}, 1); err != nil {
		t.Fatal(err)
	}

	e := &Engine{Engine: &runtime.Engine{
		Config: &EngineConfig{RootDir: root},
		Cache:  idx,
	}}

	versions, err := e.installedPackagesForUpdateCheck()
	if err != nil {
		t.Fatalf("installedPackagesForUpdateCheck: %v", err)
	}
	if len(versions) != 1 || versions["orphan"] != "1.0.0" {
		t.Fatalf("versions = %#v, want cache fallback when apps/ is missing", versions)
	}
}

func TestCheckPackageUpdatesNonMainBucket(t *testing.T) {
	root := t.TempDir()
	pkgName := "zotero"
	installedVer := "7.0.0"
	latestVer := "7.1.0"
	pkgRoot := filepath.Join(root, "apps", pkgName)
	installDir := filepath.Join(pkgRoot, installedVer)
	if err := os.MkdirAll(installDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(installDir, "install.json"), []byte(`{
		"version": "`+installedVer+`",
		"bucket": "extras",
		"manifest": {"version":"`+installedVer+`"}
	}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := apps.LinkCurrent(pkgRoot, installedVer); err != nil {
		t.Fatal(err)
	}

	bucketsDir := filepath.Join(root, "buckets", "extras", "bucket")
	if err := os.MkdirAll(bucketsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bucketsDir, pkgName+".json"),
		[]byte(`{"version":"`+latestVer+`","url":"https://example.com/z.zip","hash":"abc"}`), 0644); err != nil {
		t.Fatal(err)
	}
	bucketRegistry, err := bucket.NewRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := bucketRegistry.ReloadFromDisk(); err != nil {
		t.Fatal(err)
	}

	idx, err := cache.NewIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idx.Close() })

	e := &Engine{Engine: &runtime.Engine{
		Config:         &EngineConfig{RootDir: root},
		Cache:          idx,
		BucketRegistry: bucketRegistry,
	}}

	updates, err := e.CheckPackageUpdates()
	if err != nil {
		t.Fatalf("CheckPackageUpdates: %v", err)
	}
	if len(updates) != 1 {
		t.Fatalf("updates = %+v, want one pending update from extras bucket", updates)
	}
	if updates[0].Bucket != "extras" || updates[0].LatestVersion != latestVer {
		t.Fatalf("updates[0] = %+v", updates[0])
	}
}
