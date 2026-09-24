package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeCatalogPackageRef(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"acorn", "main/acorn"},
		{"main/acorn", "main/acorn"},
		{"Main/Acorn", "main/acorn"},
		{"lemon/1key.run", "lemon/1key.run"},
	}
	for _, tc := range tests {
		if got := NormalizeCatalogPackageRef(tc.in); got != tc.want {
			t.Fatalf("NormalizeCatalogPackageRef(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestHiddenCatalogPackageMainBucketKey(t *testing.T) {
	root := t.TempDir()
	if err := AddConfigHiddenCatalogPackage(root, "acorn"); err != nil {
		t.Fatal(err)
	}
	hidden, err := ReadConfigHiddenCatalogPackages(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := hidden["main/acorn"]; !ok {
		t.Fatalf("hidden = %#v", hidden)
	}
}

func TestReadWriteBasics(t *testing.T) {
	root := t.TempDir()
	verbose := true
	if err := WriteBasics(root, &Basics{
		GitHubProxy: "https://mirror.test/",
		Verbose:     &verbose,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := ReadBasics(root)
	if err != nil {
		t.Fatal(err)
	}
	if got.GitHubProxy != "https://mirror.test/" || got.Verbose == nil || !*got.Verbose {
		t.Fatalf("got %#v", got)
	}
}

func TestUnsetVerboseRemovesKey(t *testing.T) {
	root := t.TempDir()
	on := true
	if err := WriteBasics(root, &Basics{Verbose: &on}); err != nil {
		t.Fatal(err)
	}
	got, err := ReadBasics(root)
	if err != nil {
		t.Fatal(err)
	}
	got.Verbose = nil
	if err := WriteBasics(root, got); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(Path(root))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "verbose") {
		t.Fatalf("expected verbose key removed, got %s", data)
	}
}

func TestWriteReadConfigGitHubProxy(t *testing.T) {
	root := t.TempDir()
	if err := WriteConfigGitHubProxy(root, "https://ghproxy.net/"); err != nil {
		t.Fatalf("WriteConfigGitHubProxy: %v", err)
	}
	got, err := ReadConfigGitHubProxy(root)
	if err != nil {
		t.Fatalf("ReadConfigGitHubProxy: %v", err)
	}
	if got != "https://ghproxy.net/" {
		t.Fatalf("got %q", got)
	}
	if err := WriteConfigGitHubProxy(root, ""); err != nil {
		t.Fatalf("clear: %v", err)
	}
	got, err = ReadConfigGitHubProxy(root)
	if err != nil {
		t.Fatalf("ReadConfigGitHubProxy after clear: %v", err)
	}
	if got != "" {
		t.Fatalf("want empty, got %q", got)
	}
}

func TestWriteReadConfigDownloadWorkers(t *testing.T) {
	root := t.TempDir()
	if err := WriteConfigDownloadWorkers(root, 6); err != nil {
		t.Fatalf("WriteConfigDownloadWorkers: %v", err)
	}
	got, ok, err := ReadConfigDownloadWorkers(root)
	if err != nil {
		t.Fatalf("ReadConfigDownloadWorkers: %v", err)
	}
	if !ok || got != 6 {
		t.Fatalf("got %d ok=%v", got, ok)
	}
}

func TestWriteReadConfigBucketCheckInterval(t *testing.T) {
	root := t.TempDir()
	if err := WriteConfigBucketCheckInterval(root, 5); err != nil {
		t.Fatalf("WriteConfigBucketCheckInterval: %v", err)
	}
	got, ok, err := ReadConfigBucketCheckInterval(root)
	if err != nil {
		t.Fatalf("ReadConfigBucketCheckInterval: %v", err)
	}
	if !ok || got != 5 {
		t.Fatalf("got %d ok=%v", got, ok)
	}
	if NormalizeBucketCheckInterval(99) != DefaultBucketCheckIntervalMinutes {
		t.Fatalf("normalize unexpected value")
	}
}

func TestWriteReadConfigBucketSyncMode(t *testing.T) {
	root := t.TempDir()
	if err := WriteConfigBucketSyncMode(root, BucketSyncModeAuto); err != nil {
		t.Fatalf("WriteConfigBucketSyncMode: %v", err)
	}
	got, ok, err := ReadConfigBucketSyncMode(root)
	if err != nil {
		t.Fatalf("ReadConfigBucketSyncMode: %v", err)
	}
	if !ok || got != BucketSyncModeAuto {
		t.Fatalf("got %q ok=%v", got, ok)
	}
	if NormalizeBucketSyncMode("AUTO") != BucketSyncModeAuto {
		t.Fatalf("normalize auto")
	}
	if NormalizeBucketSyncMode("unknown") != BucketSyncModeManual {
		t.Fatalf("normalize manual default")
	}
}

func TestWriteReadConfigBucketDescriptions(t *testing.T) {
	root := t.TempDir()
	if err := SetConfigBucketDescription(root, "lemon", "Community bucket with extra apps"); err != nil {
		t.Fatalf("SetConfigBucketDescription: %v", err)
	}
	got, err := ReadConfigBucketDescriptions(root)
	if err != nil {
		t.Fatalf("ReadConfigBucketDescriptions: %v", err)
	}
	if got["lemon"] != "Community bucket with extra apps" {
		t.Fatalf("got %q", got["lemon"])
	}
	if err := SetConfigBucketDescription(root, "lemon", ""); err != nil {
		t.Fatalf("clear: %v", err)
	}
	got, err = ReadConfigBucketDescriptions(root)
	if err != nil {
		t.Fatalf("ReadConfigBucketDescriptions after clear: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("want empty map, got %#v", got)
	}
}

func TestWriteConfigPreservesOtherKeys(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config.json")
	if err := os.WriteFile(path, []byte(`{"verbose":true,"parallel_download":false}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := WriteConfigGitHubProxy(root, "https://mirror.test/"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)
	if !containsAll(body, `"verbose": true`, `"parallel_download": false`, `"github_proxy": "https://mirror.test/"`) {
		t.Fatalf("unexpected config: %s", body)
	}
}

func containsAll(s string, parts ...string) bool {
	for _, p := range parts {
		if !contains(s, p) {
			return false
		}
	}
	return true
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
func TestPruneManifestDownloadOverrides(t *testing.T) {
	root := t.TempDir()
	if err := SetConfigManifestDownloadOverride(root, "extras/keep", []string{"https://example.com/keep.exe"}, nil, "hash-keep"); err != nil {
		t.Fatal(err)
	}
	if err := SetConfigManifestDownloadOverride(root, "extras/drop", []string{"https://example.com/drop.exe"}, nil, "hash-old"); err != nil {
		t.Fatal(err)
	}
	if err := SetConfigManifestDownloadOverride(root, "extras/legacy", []string{"https://example.com/legacy.exe"}, nil, ""); err != nil {
		t.Fatal(err)
	}

	removed, err := PruneManifestDownloadOverrides(root, func(pkgRef string, item ManifestDownloadOverride) bool {
		return pkgRef == "extras/keep" && item.BaseHash == "hash-keep"
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 2 {
		t.Fatalf("removed = %#v, want 2 entries", removed)
	}

	got, err := ReadConfigManifestDownloadOverrides(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("remaining = %#v", got)
	}
	if _, ok := got["extras/keep"]; !ok {
		t.Fatalf("keep entry missing: %#v", got)
	}
}
