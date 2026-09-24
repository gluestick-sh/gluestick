package engine

import (
	"github.com/gluestick-sh/core/engine/internal/runtime"

	"os"
	"path/filepath"
	"testing"
)

func TestBucketManifestPathsFlatBucket(t *testing.T) {
	root := t.TempDir()
	bucketDir := filepath.Join(root, "php", "bucket")
	if err := os.MkdirAll(bucketDir, 0755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"php8.3.json", "php8.4.json"} {
		content := `{"version":"1.0.0","description":"PHP","url":"https://example.com/x.zip","hash":"abc"}`
		if err := os.WriteFile(filepath.Join(bucketDir, name), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}

	paths := runtime.BucketManifestPaths(filepath.Join(root, "php"), "php")
	if len(paths) != 2 {
		t.Fatalf("expected 2 manifest paths, got %d", len(paths))
	}
}

func TestBucketManifestPathsNestedBucket(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "main", "bucket", "p")
	if err := os.MkdirAll(nested, 0755); err != nil {
		t.Fatal(err)
	}
	content := `{"version":"1.0.0","description":"Python","url":"https://example.com/x.zip","hash":"abc"}`
	if err := os.WriteFile(filepath.Join(nested, "python.json"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	paths := runtime.BucketManifestPaths(filepath.Join(root, "main"), "main")
	if len(paths) != 1 {
		t.Fatalf("expected 1 nested manifest path, got %d", len(paths))
	}
}

func TestScanBucketManifestEntriesPHPStyle(t *testing.T) {
	root := t.TempDir()
	bucketDir := filepath.Join(root, "bucket")
	if err := os.MkdirAll(bucketDir, 0755); err != nil {
		t.Fatal(err)
	}
	// PHP manifests often omit description; architecture block only.
	content := `{
		"homepage": "https://windows.php.net/",
		"version": "8.3.29",
		"architecture": {
			"64bit": {
				"url": "https://example.com/php.zip",
				"hash": "abc123"
			}
		}
	}`
	if err := os.WriteFile(filepath.Join(bucketDir, "php8.3.json"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	entries := runtime.ScanBucketManifestEntries(root, "php")
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].Name != "php8.3" || entries[0].Version != "8.3.29" {
		t.Fatalf("unexpected entry: %+v", entries[0])
	}
}
