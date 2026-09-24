package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gluestick-sh/core/config"
	"github.com/gluestick-sh/core/engine/internal/override"
	"github.com/gluestick-sh/core/engine/internal/runtime"
	"github.com/gluestick-sh/core/manifest"
)

func TestApplyManifestDownloadOverrides_requiresFreshBaseHash(t *testing.T) {
	root := t.TempDir()
	manifestPath := filepath.Join(root, "pkg.json")
	body := []byte(`{
		"version": "3.5.1",
		"url": "https://example.com/old.exe",
		"hash": "abc"
	}`)
	if err := os.WriteFile(manifestPath, body, 0o644); err != nil {
		t.Fatal(err)
	}
	baseHash, err := manifest.HashFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	m, err := manifest.Parse(strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{Engine: &runtime.Engine{Config: &EngineConfig{RootDir: root}}}
	if err := override.SetManifestDownloadOverride(e.Engine, "lemon/1key.run",
		[]string{"https://example.com/new.exe"}, nil, baseHash); err != nil {
		t.Fatal(err)
	}
	out, err := override.ApplyManifestOverrides(e.Engine, "lemon/1key.run", manifestPath, m, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := out.GetURL(); got != "https://example.com/new.exe" {
		t.Fatalf("url = %q", got)
	}

	// Bucket manifest changed → override becomes stale and is ignored.
	if err := os.WriteFile(manifestPath, []byte(`{
		"version": "3.5.2",
		"url": "https://example.com/bucket-new.exe",
		"hash": "def"
	}`), 0o644); err != nil {
		t.Fatal(err)
	}
	m2, err := manifest.ParseFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	out2, err := override.ApplyManifestOverrides(e.Engine, "lemon/1key.run", manifestPath, m2, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := out2.GetURL(); got != "https://example.com/bucket-new.exe" {
		t.Fatalf("stale override still applied: %q", got)
	}
}

func TestManifestDownloadOverrideConfigRoundTrip(t *testing.T) {
	root := t.TempDir()
	if err := config.SetConfigManifestDownloadOverride(root, "Lemon/1key.run",
		[]string{"https://example.com/x.exe"}, []string{"deadbeef"}, "base123"); err != nil {
		t.Fatal(err)
	}
	got, err := config.ReadConfigManifestDownloadOverrides(root)
	if err != nil {
		t.Fatal(err)
	}
	item, ok := got["lemon/1key.run"]
	if !ok || item.URLs[0] != "https://example.com/x.exe" || item.BaseHash != "base123" {
		t.Fatalf("override = %#v, ok=%v", item, ok)
	}
}
