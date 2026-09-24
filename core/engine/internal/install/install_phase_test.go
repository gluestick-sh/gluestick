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
	"github.com/gluestick-sh/core/manifest"
	"github.com/gluestick-sh/core/progress"
)

// TestNewInstallState verifies installState initialization
func TestNewInstallState(t *testing.T) {
	root := t.TempDir()
	e := &runtime.Engine{
		Config:    &etypes.EngineConfig{RootDir: root},
		Extractor: &extractor.Extractor{},
	}
	ctx := context.Background()
	req := &etypes.InstallRequest{Request: etypes.Request{Name: "test"}}
	reporter := &mockProgressReporter{}

	state := newInstallState(e, ctx, "test", req, reporter)

	if state.engine != e {
		t.Error("engine not set")
	}
	if state.ctx == nil {
		t.Error("context not set")
	}
	if state.req != req {
		t.Error("request not set")
	}
	if state.pkgRef != "test" {
		t.Error("pkgRef not set")
	}
	if state.installedFiles == nil {
		t.Error("installedFiles not initialized")
	}
	// Note: prog.Bytes is initially nil and gets set during fetch phase
}

// TestResolveInstallPhase_BucketNotFound tests bucket not found error
func TestResolveInstallPhase_BucketNotFound(t *testing.T) {
	root := t.TempDir()
	e := &runtime.Engine{
		Config:         &etypes.EngineConfig{RootDir: root},
		BucketRegistry: &bucket.Registry{},
	}

	state := &installState{
		engine:         e,
		ctx:            context.Background(),
		req:            &etypes.InstallRequest{Request: etypes.Request{Name: "nonexistent/pkg"}},
		pkgRef:         "nonexistent/pkg",
		lookupRef:      "nonexistent/pkg",
		reporter:       &mockProgressReporter{},
		installedFiles: make(map[string]string),
	}

	err := resolveInstallPhase(state)
	if err == nil {
		t.Fatal("expected error for nonexistent bucket")
	}
	if err.Error() == "" {
		t.Fatal("error message should not be empty")
	}
	t.Logf("Got expected error: %v", err)
}

// TestResolveInstallPhase_WithValidManifest tests successful manifest resolution
func TestResolveInstallPhase_WithValidManifest(t *testing.T) {
	root := t.TempDir()

	// Create bucket structure
	bucketDir := filepath.Join(root, "buckets", "main", "bucket")
	if err := os.MkdirAll(bucketDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create a simple manifest
	manifestContent := `{"version":"1.0.0","description":"test","url":"https://example.com/test.zip","hash":"abc123"}`
	manifestPath := filepath.Join(bucketDir, "test.json")
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

	e := &runtime.Engine{
		Config:         &etypes.EngineConfig{RootDir: root},
		BucketRegistry: br,
		SearchIdx:      runtime.NewIndex(),
	}
	runtime.RebuildSearchIndex(e)

	state := &installState{
		engine:         e,
		ctx:            context.Background(),
		req:            &etypes.InstallRequest{Request: etypes.Request{Name: "test"}},
		pkgRef:         "test",
		lookupRef:      "test",
		reporter:       &mockProgressReporter{},
		installedFiles: make(map[string]string),
		prog:           progress.Handler{},
	}

	err = resolveInstallPhase(state)
	if err != nil {
		t.Fatalf("resolveInstallPhase failed: %v", err)
	}

	// Verify state populated correctly
	if state.pkgName != "test" {
		t.Errorf("pkgName = %q, want test", state.pkgName)
	}
	if state.manifest == nil {
		t.Error("manifest not loaded")
	}
	if state.manifestPath == "" {
		t.Error("manifestPath not set")
	}
	if state.targetVersion != "1.0.0" {
		t.Errorf("targetVersion = %q, want 1.0.0", state.targetVersion)
	}
}

// TestFetchInstallPhase_NoURLs tests manifest with no URLs
func TestFetchInstallPhase_NoURLs(t *testing.T) {
	state := &installState{
		engine:         &runtime.Engine{},
		ctx:            context.Background(),
		req:            &etypes.InstallRequest{Request: etypes.Request{Name: "test"}},
		pkgRef:         "test",
		reporter:       &mockProgressReporter{},
		installedFiles: make(map[string]string),
		prog:           progress.Handler{},
		manifest:       &manifest.Manifest{Version: "1.0.0"},
	}

	err := fetchInstallPhase(state)
	if err == nil {
		t.Fatal("expected error for manifest with no URLs")
	}
	if err.Error() != "manifest has no download URLs" {
		t.Errorf("wrong error message: %v", err)
	}
}

// Mock implementations for testing

type mockProgressReporter struct {
	events []etypes.ProgressEvent
}

func (m *mockProgressReporter) ReportProgress(event etypes.ProgressEvent) {
	m.events = append(m.events, event)
}

// Test that state cleanup works correctly
func TestInstallState_Cleanup(t *testing.T) {
	root := t.TempDir()

	// Create a mock extractor that implements the interface
	mockExtract := &extractor.Extractor{}
	e := &runtime.Engine{
		Config:    &etypes.EngineConfig{RootDir: root},
		Extractor: mockExtract,
	}

	ctx, cancel := context.WithCancel(context.Background())
	state := newInstallState(e, ctx, "test", &etypes.InstallRequest{}, &mockProgressReporter{})
	cancel()

	// Cleanup should be safe
	state.cleanup()
}
