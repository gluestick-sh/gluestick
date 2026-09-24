package engine

import (
	"github.com/gluestick-sh/core/engine/internal/override"
	"github.com/gluestick-sh/core/engine/internal/runtime"

	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gluestick-sh/core/config"
	"github.com/gluestick-sh/core/manifest"
)

func TestManifestJSONOverrideRoundTrip(t *testing.T) {
	root := t.TempDir()
	bucketDir := filepath.Join(root, "buckets", "main", "bucket")
	if err := os.MkdirAll(bucketDir, 0755); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(bucketDir, "acon.json")
	raw := `{"version":"1.0.0","url":"https://example.com/old.zip","hash":"abc"}`
	if err := os.WriteFile(manifestPath, []byte(raw), 0644); err != nil {
		t.Fatal(err)
	}
	baseHash, err := manifest.HashFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	overrideJSON := `{"version":"2.0.0","url":"https://example.com/new.zip","hash":"def"}`
	if err := config.SetConfigManifestJSONOverride(root, "main/acon", overrideJSON, baseHash); err != nil {
		t.Fatal(err)
	}

	bucketM, err := manifest.ParseFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{Engine: &runtime.Engine{Config: &EngineConfig{RootDir: root}}}
	state, err := override.ResolveManifestOverrides(e.Engine, "main/acon", manifestPath, bucketM, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !state.JSONActive {
		t.Fatal("expected json override active")
	}
	if state.EffectiveM.Version != "2.0.0" {
		t.Fatalf("version = %q", state.EffectiveM.Version)
	}

	if err := os.WriteFile(manifestPath, []byte(`{"version":"3.0.0","url":"https://example.com/v3.zip","hash":"zzz"}`), 0644); err != nil {
		t.Fatal(err)
	}
	state, err = override.ResolveManifestOverrides(e.Engine, "main/acon", manifestPath, bucketM, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !state.JSONStale {
		t.Fatal("expected stale override after bucket update")
	}
	if state.JSONActive {
		t.Fatal("stale override should not be active")
	}
}

func TestSetManifestJSONOverrideClearsWhenUnchanged(t *testing.T) {
	root := t.TempDir()
	bucketDir := filepath.Join(root, "buckets", "main", "bucket")
	if err := os.MkdirAll(bucketDir, 0755); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(bucketDir, "acon.json")
	raw := `{"version":"1.0.0","url":"https://example.com/app.zip","hash":"abc"}`
	if err := os.WriteFile(manifestPath, []byte(raw), 0644); err != nil {
		t.Fatal(err)
	}
	e := &Engine{Engine: &runtime.Engine{Config: &EngineConfig{RootDir: root}}}
	if err := e.SetManifestJSONOverride("main/acon", manifestPath, raw); err != nil {
		t.Fatal(err)
	}
	got, err := config.ReadConfigManifestJSONOverrides(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("expected cleared override, got %#v", got)
	}
}

func TestSetManifestJSONOverrideRejectsInvalidJSON(t *testing.T) {
	root := t.TempDir()
	manifestPath := filepath.Join(root, "demo.json")
	if err := os.WriteFile(manifestPath, []byte(`{"version":"1.0.0","url":"https://example.com/a.zip","hash":"abc"}`), 0644); err != nil {
		t.Fatal(err)
	}
	e := &Engine{Engine: &runtime.Engine{Config: &EngineConfig{RootDir: root}}}
	err := e.SetManifestJSONOverride("main/acon", manifestPath, `{not json`)
	if err == nil || !strings.Contains(err.Error(), "invalid manifest JSON") {
		t.Fatalf("err = %v", err)
	}
}
