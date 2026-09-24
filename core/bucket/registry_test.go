package bucket

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRegistryReloadFromDisk_nonGitBucket(t *testing.T) {
	root := t.TempDir()
	bucketDir := filepath.Join(root, "buckets", "custom", "bucket")
	if err := os.MkdirAll(bucketDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bucketDir, "demo.json"),
		[]byte(`{"version":"1.0.0","url":"https://example.com/demo.zip","hash":"abc"}`), 0644); err != nil {
		t.Fatal(err)
	}

	reg, err := NewRegistry(root)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if err := reg.ReloadFromDisk(); err != nil {
		t.Fatalf("ReloadFromDisk: %v", err)
	}

	buckets := reg.List()
	if len(buckets) != 1 {
		t.Fatalf("List() = %d buckets, want 1", len(buckets))
	}
	if buckets[0].Name != "custom" {
		t.Fatalf("bucket name = %q", buckets[0].Name)
	}
}

func TestRegistryGetManifestPath(t *testing.T) {
	root := t.TempDir()
	bucketDir := filepath.Join(root, "buckets", "main", "bucket")
	if err := os.MkdirAll(bucketDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bucketDir, "vim.json"),
		[]byte(`{"version":"9.1.0","url":"https://example.com/vim.zip","hash":"abc"}`), 0644); err != nil {
		t.Fatal(err)
	}

	reg, err := NewRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.ReloadFromDisk(); err != nil {
		t.Fatal(err)
	}

	path, m, err := reg.GetManifestPath("vim")
	if err != nil {
		t.Fatalf("GetManifestPath: %v", err)
	}
	if m == nil || m.Version != "9.1.0" {
		t.Fatalf("manifest = %#v", m)
	}
	if filepath.Base(path) != "vim.json" {
		t.Fatalf("path = %q", path)
	}

	_, _, err = reg.GetManifestPath("custom/vim")
	if err == nil {
		t.Fatal("expected error for missing bucket")
	}
}

func TestRegistryReloadFromDisk_missingBucketsDir(t *testing.T) {
	root := t.TempDir()
	reg, err := NewRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.ReloadFromDisk(); err != nil {
		t.Fatalf("ReloadFromDisk on empty root: %v", err)
	}
	if len(reg.List()) != 0 {
		t.Fatalf("expected no buckets, got %d", len(reg.List()))
	}
}
