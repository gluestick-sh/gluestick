package engine

import (
	"path/filepath"
	"testing"

	"github.com/gluestick-sh/core/cache"
	"github.com/gluestick-sh/core/engine/internal/runtime"
	"github.com/gluestick-sh/core/store"
)

func TestListCachePackagesAndSummary(t *testing.T) {
	root := t.TempDir()
	store, err := store.NewStore(filepath.Join(root, "store"))
	if err != nil {
		t.Fatal(err)
	}
	idx, err := cache.NewIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	if err := idx.Add("alpha", "1.0", map[string]string{"aa": "f1"}, 100); err != nil {
		t.Fatal(err)
	}
	if err := idx.Add("beta", "2.0", map[string]string{"bb": "f2"}, 250); err != nil {
		t.Fatal(err)
	}

	e := &Engine{Engine: &runtime.Engine{
		Config: &EngineConfig{RootDir: root},
		Store:  store,
		Cache:  idx,
	}}

	list := e.ListCachePackages()
	if len(list) != 2 {
		t.Fatalf("len(ListCachePackages) = %d, want 2", len(list))
	}
	if list[0].Name != "alpha" || list[1].Name != "beta" {
		t.Fatalf("order = %q, %q", list[0].Name, list[1].Name)
	}

	sum := e.CacheSummary()
	if sum.PackageCount != 2 || sum.TotalSize != 350 || sum.TotalFiles != 2 {
		t.Fatalf("summary = %+v", sum)
	}
}
