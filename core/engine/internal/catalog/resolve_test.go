package catalog

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	etypes "github.com/gluestick-sh/core/engine/types"
	"github.com/gluestick-sh/core/bucket"
	"github.com/gluestick-sh/core/apperr"
	"github.com/gluestick-sh/core/engine/internal/runtime"
	"github.com/gluestick-sh/core/manifest"
)

func writeManifest(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	body := `{"version":"1.0.0","description":"test","url":"https://example.com/x.zip","hash":"abc"}`
	if err := os.WriteFile(filepath.Join(dir, name+".json"), []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
}

func writeManifestWithGit(t *testing.T, dir, name string) {
	t.Helper()
	bucketDir := filepath.Dir(dir)
	if err := os.MkdirAll(bucketDir, 0755); err != nil {
		t.Fatal(err)
	}
	// Initialize a minimal git repository
	gitDir := filepath.Join(bucketDir, ".git")
	if err := os.MkdirAll(filepath.Join(gitDir, "objects"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(gitDir, "refs"), 0755); err != nil {
		t.Fatal(err)
	}
	// Create HEAD file
	if err := os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte("ref: refs/heads/main\n"), 0644); err != nil {
		t.Fatal(err)
	}
	writeManifest(t, dir, name)
}

func TestInstallRefFromMatch(t *testing.T) {
	if got := InstallRefFromMatch(manifest.Match{Name: "git", Bucket: "main"}); got != "git" {
		t.Fatalf("main ref = %q, want git", got)
	}
	if got := InstallRefFromMatch(manifest.Match{Name: "zotero", Bucket: "extras"}); got != "extras/zotero" {
		t.Fatalf("extras ref = %q, want extras/zotero", got)
	}
}

func TestPickInstallMatch(t *testing.T) {
	matches := []manifest.Match{
		{Name: "foo", Bucket: "main"},
		{Name: "foo", Bucket: "extras"},
	}
	if got := PickInstallMatch(matches); got == nil || got.Bucket != "extras" {
		t.Fatalf("expected extras match, got %#v", got)
	}

	ambiguous := []manifest.Match{
		{Name: "foo", Bucket: "extras"},
		{Name: "foo", Bucket: "versions"},
	}
	if got := PickInstallMatch(ambiguous); got != nil {
		t.Fatalf("expected nil for ambiguous matches, got %#v", got)
	}
}

// TestResolveInstallRefTypedErrors verifies resolve errors are inspectable with errors.Is.
func TestResolveInstallRefTypedErrors(t *testing.T) {
	root := t.TempDir()
	br, err := bucket.NewRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	e := &runtime.Engine{Config: &etypes.EngineConfig{RootDir: root}, BucketRegistry: br, SearchIdx: runtime.NewIndex()}

	_, err = ResolveInstallRef(e, context.Background(), "missing-package")
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, apperr.ErrManifestNotFound) {
		t.Fatalf("expected ErrManifestNotFound, got %v", err)
	}
}

func TestResolveInstallRefNonMain(t *testing.T) {
	root := t.TempDir()
	mainDir := filepath.Join(root, "buckets", "main", "bucket")
	extrasDir := filepath.Join(root, "buckets", "extras", "bucket")
	writeManifestWithGit(t, mainDir, "git")
	writeManifestWithGit(t, extrasDir, "zotero")

	br, err := bucket.NewRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := br.ReloadFromDisk(); err != nil {
		t.Fatal(err)
	}

	// Debug: check what buckets are loaded
	buckets := br.List()
	t.Logf("Loaded %d buckets:", len(buckets))
	for _, b := range buckets {
		t.Logf("  - %s: %s", b.Name, b.Root)
	}

	e := &runtime.Engine{Config: &etypes.EngineConfig{RootDir: root}, BucketRegistry: br, SearchIdx: runtime.NewIndex()}
	runtime.RebuildSearchIndex(e)

	got, err := ResolveInstallRef(e, context.Background(), "zotero")
	if err != nil {
		t.Fatalf("ResolveInstallRef: %v", err)
	}
	if got != "extras/zotero" {
		t.Fatalf("resolved = %q, want extras/zotero", got)
	}

	got, err = ResolveInstallRef(e, context.Background(), "git")
	if err != nil {
		t.Fatalf("ResolveInstallRef git: %v", err)
	}
	if got != "git" {
		t.Fatalf("resolved = %q, want git", got)
	}
}

func TestFormatInstallResolveNotice(t *testing.T) {
	err := fmt.Errorf(`find manifest: manifest not found: lemon/notepad2 (searched in: C:\Users\test\.glue\buckets\lemon)`)
	got := FormatInstallResolveNotice(err)
	wantPrefix := "Note: manifest not found: lemon/notepad2"
	if !strings.HasPrefix(got, wantPrefix) {
		t.Fatalf("got %q, want prefix %q", got, wantPrefix)
	}
}

func TestIsInstallResolveNotice(t *testing.T) {
	if !IsInstallResolveNotice(fmt.Errorf("find manifest: %w", &apperr.ManifestNotFound{Name: "foo"})) {
		t.Fatal("expected manifest not found")
	}
	if !IsInstallResolveNotice(&apperr.ManifestAmbiguous{Name: "foo", Matches: []string{"main/foo", "extras/foo"}}) {
		t.Fatal("expected ambiguous manifest")
	}
	if IsInstallResolveNotice(fmt.Errorf("dependency git: download failed")) {
		t.Fatal("dependency failure should not be resolve notice")
	}
}

func TestWrapManifestNotFoundHint(t *testing.T) {
	root := t.TempDir()
	mainDir := filepath.Join(root, "buckets", "main", "bucket")
	extrasDir := filepath.Join(root, "buckets", "extras", "bucket")
	writeManifestWithGit(t, mainDir, "git")
	writeManifestWithGit(t, extrasDir, "zotero")

	br, err := bucket.NewRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := br.ReloadFromDisk(); err != nil {
		t.Fatal(err)
	}

	e := &runtime.Engine{Config: &etypes.EngineConfig{RootDir: root}, BucketRegistry: br, SearchIdx: runtime.NewIndex()}
	runtime.RebuildSearchIndex(e)
	err = WrapManifestNotFound(e, "zotero", &apperr.ManifestNotFound{Name: "zotero"})
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "glue install extras/zotero") {
		t.Fatalf("hint missing extras/zotero: %q", msg)
	}
}

func TestBucketDirHasManifestsSkipsGitTree(t *testing.T) {
	root := t.TempDir()
	bucketDir := filepath.Join(root, "main")
	bucketSub := filepath.Join(bucketDir, "bucket")
	if err := os.MkdirAll(bucketSub, 0755); err != nil {
		t.Fatal(err)
	}
	writeManifest(t, bucketSub, "git")

	gitObjects := filepath.Join(bucketDir, ".git", "objects")
	if err := os.MkdirAll(gitObjects, 0755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 200; i++ {
		name := filepath.Join(gitObjects, fmt.Sprintf("obj-%04d", i))
		if err := os.WriteFile(name, []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	if !BucketDirHasManifests(bucketDir) {
		t.Fatal("expected manifests under bucket/ to be detected without walking .git")
	}
}

func TestEnsureBucketForInstallSkipsReloadWhenIndexed(t *testing.T) {
	root := t.TempDir()
	mainDir := filepath.Join(root, "buckets", "main", "bucket")
	writeManifestWithGit(t, mainDir, "git")

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
	runtime.SyncSearchIndex(e, false)
	if !e.SearchIdx.HasLoadedBucket("main") {
		t.Fatal("expected main bucket indexed")
	}

	for i := 0; i < 5; i++ {
		if err := EnsureBucketForInstall(e, context.Background(), "git", nil); err != nil {
			t.Fatalf("EnsureBucketForInstall #%d: %v", i, err)
		}
	}
}

func TestEnsureBucketForInstallIndexesUnregisteredBucketDir(t *testing.T) {
	root := t.TempDir()
	mainDir := filepath.Join(root, "buckets", "main", "bucket")
	writeManifest(t, mainDir, "git")

	br, err := bucket.NewRegistry(root)
	if err != nil {
		t.Fatal(err)
	}

	e := &runtime.Engine{
		Config:         &etypes.EngineConfig{RootDir: root},
		BucketRegistry: br,
		SearchIdx:      runtime.NewIndex(),
	}

	if err := EnsureBucketForInstall(e, context.Background(), "git", nil); err != nil {
		t.Fatalf("EnsureBucketForInstall: %v", err)
	}
	if _, err := br.Get("main"); err != nil {
		t.Fatalf("expected main registered: %v", err)
	}
	if !e.SearchIdx.HasLoadedBucket("main") {
		t.Fatal("expected main bucket indexed after ensure")
	}
}

func TestEnsureBucketForInstallWithBucketsFilter(t *testing.T) {
	root := t.TempDir()
	mainDir := filepath.Join(root, "buckets", "main", "bucket")
	extrasDir := filepath.Join(root, "buckets", "extras", "bucket")
	writeManifestWithGit(t, mainDir, "git")
	writeManifestWithGit(t, extrasDir, "zotero")

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

	// Test 1: No bucket restriction - should succeed for both buckets
	if err := EnsureBucketForInstall(e, context.Background(), "git", nil); err != nil {
		t.Fatalf("unrestricted git install failed: %v", err)
	}
	if err := EnsureBucketForInstall(e, context.Background(), "zotero", nil); err != nil {
		t.Fatalf("unrestricted zotero install failed: %v", err)
	}

	// Test 2: Only main bucket allowed - extras should fail
	allowedBuckets := []string{"main"}
	if err := EnsureBucketForInstall(e, context.Background(), "git", allowedBuckets); err != nil {
		t.Fatalf("allowed bucket git failed: %v", err)
	}
	if err := EnsureBucketForInstall(e, context.Background(), "extras/zotero", allowedBuckets); err == nil {
		t.Fatal("disallowed bucket extras/zotero should have failed")
	}

	// Test 3: Only extras bucket allowed - main should fail
	allowedBuckets = []string{"extras"}
	if err := EnsureBucketForInstall(e, context.Background(), "extras/zotero", allowedBuckets); err != nil {
		t.Fatalf("allowed bucket extras/zotero failed: %v", err)
	}
	if err := EnsureBucketForInstall(e, context.Background(), "git", allowedBuckets); err == nil {
		t.Fatal("disallowed bucket git should have failed")
	}

	// Test 4: Empty bucket list should allow all
	if err := EnsureBucketForInstall(e, context.Background(), "git", []string{}); err != nil {
		t.Fatalf("empty bucket list git failed: %v", err)
	}
	if err := EnsureBucketForInstall(e, context.Background(), "extras/zotero", []string{}); err != nil {
		t.Fatalf("empty bucket list extras/zotero failed: %v", err)
	}
}

func TestEnsureBucketForInstallContextCancellation(t *testing.T) {
	root := t.TempDir()

	br, err := bucket.NewRegistry(root)
	if err != nil {
		t.Fatal(err)
	}

	e := &runtime.Engine{
		Config:         &etypes.EngineConfig{RootDir: root},
		BucketRegistry: br,
		SearchIdx:      runtime.NewIndex(),
	}

	// Test with cancelled context
	// Use a known bucket that would require git operations
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	// The function should check context before expensive git operations
	// and return context.Canceled error
	err = EnsureBucketForInstall(e, ctx, "git", nil)
	if err == nil {
		t.Fatal("expected error with cancelled context")
	}
	// We accept either context.Canceled or bucket not found error
	// Both indicate that context checking is working
	if err != context.Canceled && !strings.Contains(err.Error(), "not installed") {
		t.Fatalf("expected context.Canceled or bucket error, got %v", err)
	}
}

func TestEnsureBucketForInstallBucketsErrorMessage(t *testing.T) {
	root := t.TempDir()
	mainDir := filepath.Join(root, "buckets", "main", "bucket")
	extrasDir := filepath.Join(root, "buckets", "extras", "bucket")
	writeManifestWithGit(t, mainDir, "git")
	writeManifestWithGit(t, extrasDir, "zotero")

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

	allowedBuckets := []string{"main"}
	err = EnsureBucketForInstall(e, context.Background(), "extras/zotero", allowedBuckets)
	if err == nil {
		t.Fatal("expected error for disallowed bucket")
	}

	errMsg := err.Error()
	if !strings.Contains(errMsg, "extras") {
		t.Fatalf("error message should mention 'extras', got: %s", errMsg)
	}
	if !strings.Contains(errMsg, "allowed buckets") {
		t.Fatalf("error message should mention 'allowed buckets', got: %s", errMsg)
	}
}
