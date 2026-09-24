package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gluestick-sh/core/bucket"
	"github.com/gluestick-sh/core/engine/internal/runtime"
)

func writeDependsManifest(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if body == "" {
		body = `{"version":"1.0.0","url":"https://example.com/x.zip","hash":"abc"}`
	}
	if err := os.WriteFile(filepath.Join(dir, name+".json"), []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestPlanInstall_missingDepends(t *testing.T) {
	root := t.TempDir()
	mainDir := filepath.Join(root, "buckets", "main", "bucket")
	writeDependsManifest(t, mainDir, "git", "")
	writeDependsManifest(t, mainDir, "7zip", "")

	body := `{"version":"2.0.0","depends":["7zip"],"url":"https://example.com/app.zip","hash":"abc"}`
	writeDependsManifest(t, mainDir, "app", body)

	br, err := bucket.NewRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := br.ReloadFromDisk(); err != nil {
		t.Fatal(err)
	}

	r, err := runtime.NewEngine(&EngineConfig{RootDir: root})
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{Engine: r}
	defer e.Close()
	runtime.RebuildSearchIndex(e.Engine)

	plan, err := e.PlanInstall(context.Background(), "app")
	if err != nil {
		t.Fatalf("PlanInstall: %v", err)
	}
	if len(plan.Depends) != 1 || plan.Depends[0].Ref != "7zip" {
		t.Fatalf("depends = %#v, want 7zip", plan.Depends)
	}
}

func TestPlanInstall_dependsPreAndPost(t *testing.T) {
	root := t.TempDir()
	mainDir := filepath.Join(root, "buckets", "main", "bucket")
	writeDependsManifest(t, mainDir, "pre", "")
	writeDependsManifest(t, mainDir, "post", "")

	body := `{"version":"2.0.0","depends_pre":["pre"],"depends_post":["post"],"url":"https://example.com/app.zip","hash":"abc"}`
	writeDependsManifest(t, mainDir, "app", body)

	br, err := bucket.NewRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := br.ReloadFromDisk(); err != nil {
		t.Fatal(err)
	}

	r, err := runtime.NewEngine(&EngineConfig{RootDir: root})
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{Engine: r}
	defer e.Close()
	runtime.RebuildSearchIndex(e.Engine)

	plan, err := e.PlanInstall(context.Background(), "app")
	if err != nil {
		t.Fatalf("PlanInstall: %v", err)
	}
	if len(plan.Depends) != 2 {
		t.Fatalf("depends = %#v, want pre and post", plan.Depends)
	}
	refs := map[string]bool{plan.Depends[0].Ref: true, plan.Depends[1].Ref: true}
	if !refs["pre"] || !refs["post"] {
		t.Fatalf("depends refs = %#v, want pre and post", plan.Depends)
	}
}
