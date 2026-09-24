package override

import (
	"strings"
	"testing"

	"github.com/gluestick-sh/core/engine/internal/runtime"
	etypes "github.com/gluestick-sh/core/engine/types"
	"github.com/gluestick-sh/core/manifest"
)

func TestResolveManifestOverrides_requestURLOverride(t *testing.T) {
	m, err := manifest.Parse(strings.NewReader(`{
		"version": "1.0.0",
		"url": "https://example.com/old.zip",
		"hash": "abc"
	}`))
	if err != nil {
		t.Fatal(err)
	}
	e := &runtime.Engine{Config: &etypes.EngineConfig{RootDir: t.TempDir()}}
	req := &etypes.InstallRequest{
		DownloadURLOverrides: []string{"https://example.com/req.zip"},
		DownloadHashOverrides: []string{"reqhash"},
	}

	state, err := ResolveManifestOverrides(e, "demo/pkg", "", m, "", req)
	if err != nil {
		t.Fatal(err)
	}
	if !state.URLActive {
		t.Fatal("expected URLActive for request override")
	}
	if got := state.EffectiveM.GetURL(); got != "https://example.com/req.zip" {
		t.Fatalf("url = %q", got)
	}
}

func TestResolveManifestOverrides_nilBucketManifest(t *testing.T) {
	state, err := ResolveManifestOverrides(nil, "demo/pkg", "", nil, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if state.EffectiveM != nil || state.JSONActive || state.JSONStale || state.URLActive {
		t.Fatalf("state = %+v, want empty", state)
	}
}

func TestApplyManifestOverrides_nilEngine(t *testing.T) {
	m, err := manifest.Parse(strings.NewReader(`{"version":"1.0.0","url":"https://example.com/a.zip","hash":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	out, err := ApplyManifestOverrides(nil, "pkg", "", m, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if out != m {
		t.Fatal("expected original manifest when engine is nil")
	}
}
