package install

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gluestick-sh/core/bucket"
	"github.com/gluestick-sh/core/engine/internal/runtime"
	etypes "github.com/gluestick-sh/core/engine/types"
	"github.com/gluestick-sh/core/extractor"
)

// TestPackageFullNewFlow tests the new phased PackageFull implementation
func TestPackageFullNewFlow(t *testing.T) {
	root := t.TempDir()

	// Create bucket structure
	bucketDir := filepath.Join(root, "buckets", "main", "bucket")
	if err := os.MkdirAll(bucketDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create a simple manifest
	manifestContent := `{
		"version":"1.0.0",
		"description":"test package for phased install",
		"url":"https://example.com/test.zip",
		"hash":"abc123def456",
		"extract_dir":"test"
	}`
	manifestPath := filepath.Join(bucketDir, "testpkg.json")
	if err := os.WriteFile(manifestPath, []byte(manifestContent), 0644); err != nil {
		t.Fatal(err)
	}

	// Initialize bucket registry
	br, err := bucket.NewRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := br.ReloadFromDisk(); err != nil {
		t.Fatal(err)
	}

	// Setup minimal engine - just enough for resolve phase
	e := &runtime.Engine{
		Config:         &etypes.EngineConfig{RootDir: root},
		BucketRegistry: br,
		SearchIdx:      runtime.NewIndex(),
		Extractor:      &extractor.Extractor{},
		// We omit Store, Cache, Downloader to test resolve phase only
	}
	runtime.RebuildSearchIndex(e)

	// Create install request
	ctx := context.Background()
	req := &etypes.InstallRequest{
		Request: etypes.Request{
			Name:  "testpkg",
			Force: false,
		},
	}
	reporter := &mockProgressReporter{}

	// Test that we can at least create and initialize state
	t.Log("Testing state initialization and resolve phase...")
	state := newInstallState(e, ctx, "testpkg", req, reporter)
	defer state.cleanup()

	// Phase 1: Resolve - should work
	err = resolveInstallPhase(state)
	if err != nil {
		t.Fatalf("resolveInstallPhase failed: %v", err)
	}

	// Verify state populated correctly
	if state.pkgName != "testpkg" {
		t.Errorf("pkgName = %q, want testpkg", state.pkgName)
	}
	if state.manifest == nil {
		t.Error("manifest should be loaded")
	}
	if state.targetVersion != "1.0.0" {
		t.Errorf("targetVersion = %q, want 1.0.0", state.targetVersion)
	}

	t.Log("Resolve phase test completed successfully")
	t.Log("Note: Full install test would require Store/Cache/Downloader setup")
}

// TestStateConsistency verifies that installState is properly maintained across phases
func TestStateConsistency(t *testing.T) {
	root := t.TempDir()

	// Create bucket structure
	bucketDir := filepath.Join(root, "buckets", "main", "bucket")
	if err := os.MkdirAll(bucketDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create a manifest
	manifestContent := `{"version":"1.0.0","description":"state test","url":"https://example.com/test.zip","hash":"abc123"}`
	manifestPath := filepath.Join(bucketDir, "statetest.json")
	if err := os.WriteFile(manifestPath, []byte(manifestContent), 0644); err != nil {
		t.Fatal(err)
	}

	// Initialize bucket registry
	br, err := bucket.NewRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := br.ReloadFromDisk(); err != nil {
		t.Fatal(err)
	}

	// Setup engine
	e := &runtime.Engine{
		Config:         &etypes.EngineConfig{RootDir: root},
		BucketRegistry: br,
		SearchIdx:      runtime.NewIndex(),
		Extractor:      &extractor.Extractor{},
	}
	runtime.RebuildSearchIndex(e)

	// Create install state
	ctx := context.Background()
	req := &etypes.InstallRequest{
		Request: etypes.Request{Name: "statetest"},
	}
	reporter := &mockProgressReporter{}

	state := newInstallState(e, ctx, "statetest", req, reporter)
	defer state.cleanup()

	// Phase 1: Resolve - should populate state
	if err := resolveInstallPhase(state); err != nil {
		t.Fatalf("resolveInstallPhase failed: %v", err)
	}

	// Verify state after resolve phase
	if state.pkgName != "statetest" {
		t.Errorf("pkgName = %q, want statetest", state.pkgName)
	}
	if state.manifest == nil {
		t.Error("manifest should be loaded")
	}
	if state.targetVersion != "1.0.0" {
		t.Errorf("targetVersion = %q, want 1.0.0", state.targetVersion)
	}
	if state.manifestPath == "" {
		t.Error("manifestPath should be set")
	}

	// Test that cleanup works properly
	state.cleanup()
	if state.engine == nil {
		t.Error("engine should still be accessible after cleanup")
	}

	t.Log("State consistency test passed")
}
