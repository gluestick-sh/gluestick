package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gluestick-sh/core/manifest"
)

func TestBuildManifestInspect(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "demo.json")
	raw := `{"version":"1.0.0","url":"https://example.com/demo.zip","hash":"abc"}`
	if err := os.WriteFile(path, []byte(raw), 0644); err != nil {
		t.Fatal(err)
	}
	m, err := manifest.ParseFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := buildManifestInspectFromModel(path, m)
	if err != nil {
		t.Fatal(err)
	}
	if info.ManifestPath != path {
		t.Fatalf("path = %q", info.ManifestPath)
	}
	if !strings.Contains(info.ManifestJSON, `"version": "1.0.0"`) {
		t.Fatalf("json = %q", info.ManifestJSON)
	}
	if len(info.DownloadURLs) != 1 || info.DownloadURLs[0] != "https://example.com/demo.zip" {
		t.Fatalf("urls = %v", info.DownloadURLs)
	}
}

func TestFormatManifestJSONTruncates(t *testing.T) {
	data := make([]byte, maxManifestInspectBytes+10)
	for i := range data {
		data[i] = '{'
	}
	data[0] = '{'
	data[len(data)-1] = '}'
	out := formatManifestJSON(data)
	if !strings.Contains(out, "truncated") {
		t.Fatalf("expected truncation marker, len=%d", len(out))
	}
}

func TestBuildManifestInspectFromModel(t *testing.T) {
	m := &manifest.Manifest{
		Version: "2.0.0",
		URL:     "https://example.com/app.zip",
		Hash:    "deadbeef",
	}
	info, err := buildManifestInspectFromModel("/apps/demo/2.0.0/install.json", m)
	if err != nil {
		t.Fatal(err)
	}
	if info.Version != "2.0.0" {
		t.Fatalf("version = %q", info.Version)
	}
	if !strings.Contains(info.ManifestJSON, `"version"`) {
		t.Fatalf("json = %q", info.ManifestJSON)
	}
}
