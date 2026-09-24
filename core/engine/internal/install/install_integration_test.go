package install

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gluestick-sh/core/apperr"
	"github.com/gluestick-sh/core/bucket"
	"github.com/gluestick-sh/core/engine/internal/catalog"
	"github.com/gluestick-sh/core/engine/internal/runtime"
	etypes "github.com/gluestick-sh/core/engine/types"
	"github.com/gluestick-sh/core/extractor"
)

// TestResolveFetchIntegration tests the complete resolve → fetch flow
func TestResolveFetchIntegration(t *testing.T) {
	root := t.TempDir()

	// Create bucket structure
	bucketDir := filepath.Join(root, "buckets", "main", "bucket")
	if err := os.MkdirAll(bucketDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create a manifest with URLs
	manifestContent := `{
		"version":"1.0.0",
		"description":"test package",
		"url":"https://example.com/test.zip",
		"hash":"abc123def456",
		"extract_dir":"test"
	}`
	manifestPath := filepath.Join(bucketDir, "myapp.json")
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

	// Setup engine with minimal components
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
		Request: etypes.Request{
			Name:  "myapp",
			Force: false,
		},
	}
	reporter := &mockProgressReporter{}

	state := newInstallState(e, ctx, "myapp", req, reporter)
	defer state.cleanup()

	// Phase 1: Resolve
	t.Log("Running resolveInstallPhase...")
	err = resolveInstallPhase(state)
	if err != nil {
		t.Fatalf("resolveInstallPhase failed: %v", err)
	}

	// Verify resolve phase output
	if state.pkgName != "myapp" {
		t.Errorf("pkgName = %q, want myapp", state.pkgName)
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
	if state.installArch == "" {
		t.Log("installArch is empty (expected for 64-bit)")
	}

	// Phase 2: Fetch preparation (will fail due to missing Cache, but we can test URL parsing)
	t.Log("Testing fetch preparation...")

	// Manually test the URL parsing part that doesn't require Cache
	urls := state.manifest.GetURLsForInstall(state.installArch)
	if len(urls) == 0 {
		t.Fatal("manifest should have URLs")
	}

	hashes := state.manifest.GetHashesForInstall(state.installArch)
	state.urlHashPairs = buildURLHashPairs(urls, hashes)
	state.multiArtifact = manifestUsesMultiArtifactURLs(urls, hashes)

	if len(state.urlHashPairs) == 0 {
		t.Error("URL-hash pairs should be built")
	}
	if state.multiArtifact {
		t.Error("Single URL should not be multi-artifact")
	}

	t.Log("Integration test completed successfully - resolve phase works correctly")
}

func TestValidateInstallTarget_manifestNotFound(t *testing.T) {
	root := t.TempDir()
	e := &runtime.Engine{
		Config:    &etypes.EngineConfig{RootDir: root},
		Extractor: &extractor.Extractor{},
	}
	br, err := bucket.NewRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	e.BucketRegistry = br

	req := &etypes.InstallRequest{Request: etypes.Request{Name: "missing-app"}}
	err = catalog.ValidateInstallTarget(e, context.Background(), req)
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, apperr.ErrManifestNotFound) {
		t.Fatalf("expected ErrManifestNotFound, got %v", err)
	}
}

// TestInstallStateLifecycle tests complete state lifecycle
func TestInstallStateLifecycle(t *testing.T) {
	root := t.TempDir()

	e := &runtime.Engine{
		Config:    &etypes.EngineConfig{RootDir: root},
		Extractor: &extractor.Extractor{},
	}

	ctx := context.Background()
	req := &etypes.InstallRequest{Request: etypes.Request{Name: "test"}}
	reporter := &mockProgressReporter{}

	// Create state
	state := newInstallState(e, ctx, "test", req, reporter)

	// Verify initial state
	if state.installedFiles == nil {
		t.Error("installedFiles should be initialized")
	}
	if len(state.installedFiles) != 0 {
		t.Error("installedFiles should start empty")
	}

	// Simulate adding some files
	state.installedFiles["hash1"] = "file1.exe"
	state.installedFiles["hash2"] = "file2.dll"

	if len(state.installedFiles) != 2 {
		t.Errorf("installedFiles count = %d, want 2", len(state.installedFiles))
	}

	// Test cleanup
	state.cleanup()

	// Verify cleanup doesn't crash and state is still accessible
	if state.installedFiles == nil {
		t.Error("installedFiles should still exist after cleanup")
	}
}
