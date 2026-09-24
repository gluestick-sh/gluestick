package engine_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gluestick-sh/core/apps"
	"github.com/gluestick-sh/core/config"
	"github.com/gluestick-sh/core/engine"
	"github.com/gluestick-sh/core/snapshot"
)

func TestExportCoreSnapshotEmptyRoot(t *testing.T) {
	root := t.TempDir()
	eng, err := engine.NewEngine(&engine.EngineConfig{RootDir: root})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	defer eng.Close()

	snap, err := eng.ExportCoreSnapshot(snapshot.Meta{Source: snapshot.SourceManual})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Kind != snapshot.Kind {
		t.Fatalf("kind = %q", snap.Kind)
	}
	if snap.Device.DeviceID == "" {
		t.Fatal("missing deviceId")
	}
	if snap.Core.Packages == nil {
		t.Fatal("packages should be non-nil slice")
	}
}

func TestDiffAndApplyDryRunConfig(t *testing.T) {
	root := t.TempDir()
	eng, err := engine.NewEngine(&engine.EngineConfig{RootDir: root})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	defer eng.Close()

	if err := config.WriteConfigGitHubProxy(root, ""); err != nil {
		t.Fatal(err)
	}
	workers := 8
	target := &snapshot.Snapshot{
		SchemaVersion: snapshot.SchemaVersion,
		Kind:          snapshot.Kind,
		ID:            "test_snapshot",
		CreatedAt:     snapshot.NowRFC3339(),
		Device: snapshot.Device{
			DeviceID: "aabbccddeeff00112233445566778899",
			OS:       "windows",
			Arch:     "arm64",
		},
		Core: snapshot.CoreState{
			Config: snapshot.Config{
				GitHubProxy:     "https://mirror.test/",
				DownloadWorkers: &workers,
				BucketSyncMode:  "auto",
			},
		},
	}

	plan, err := eng.DiffCoreSnapshot(target, snapshot.ApplyOptions{Mode: snapshot.ApplyModeInstallMissing})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.ConfigChanges) == 0 {
		t.Fatal("expected config changes")
	}

	dry, err := eng.ApplyCoreSnapshot(context.Background(), target, snapshot.ApplyOptions{DryRun: true}, engine.NewSilentReporter())
	if err != nil {
		t.Fatal(err)
	}
	if dry.Empty() {
		t.Fatal("dry-run plan should not be empty")
	}
	proxy, _ := config.ReadConfigGitHubProxy(root)
	if proxy != "" {
		t.Fatalf("dry-run mutated proxy to %q", proxy)
	}

	plan, err = eng.ApplyCoreSnapshot(context.Background(), target, snapshot.ApplyOptions{}, engine.NewSilentReporter())
	if err != nil {
		t.Fatal(err)
	}
	if plan.Empty() {
		// After apply, a second export should match; plan from apply was non-empty before mutation.
	}
	proxy, err = config.ReadConfigGitHubProxy(root)
	if err != nil {
		t.Fatal(err)
	}
	if proxy != "https://mirror.test/" {
		t.Fatalf("proxy = %q", proxy)
	}
	gotWorkers, ok, err := config.ReadConfigDownloadWorkers(root)
	if err != nil || !ok || gotWorkers != 8 {
		t.Fatalf("workers = %d ok=%v err=%v", gotWorkers, ok, err)
	}
	mode, ok, err := config.ReadConfigBucketSyncMode(root)
	if err != nil || !ok || mode != "auto" {
		t.Fatalf("mode = %q ok=%v err=%v", mode, ok, err)
	}

	// Idempotent: second apply is empty.
	again, err := eng.ApplyCoreSnapshot(context.Background(), target, snapshot.ApplyOptions{}, engine.NewSilentReporter())
	if err != nil {
		t.Fatal(err)
	}
	if !again.Empty() {
		t.Fatalf("second apply plan = %#v", again)
	}
}

func TestExportWriteReadRoundTrip(t *testing.T) {
	root := t.TempDir()
	eng, err := engine.NewEngine(&engine.EngineConfig{RootDir: root})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	if err := config.WriteConfigGitHubProxy(root, "https://export-mirror/"); err != nil {
		t.Fatal(err)
	}
	snap, err := eng.ExportCoreSnapshot(snapshot.Meta{Notes: "test"})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "snapshot.json")
	if err := snapshot.WriteFile(path, snap); err != nil {
		t.Fatal(err)
	}
	loaded, err := snapshot.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Core.Config.GitHubProxy != "https://export-mirror/" {
		t.Fatalf("proxy = %q", loaded.Core.Config.GitHubProxy)
	}
	if loaded.Device.DeviceID != snap.Device.DeviceID {
		t.Fatalf("deviceId mismatch")
	}
}

func TestExportCapturesAllVersions(t *testing.T) {
	root := t.TempDir()
	pkgRoot := filepath.Join(root, "apps", "vivaldi")
	for _, ver := range []string{"6.0.0", "7.0.0"} {
		dir := filepath.Join(pkgRoot, ver)
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "stub"), []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	// Link current via apps helper through NewEngine's tree — write junction with apps.LinkCurrent
	if err := os.WriteFile(filepath.Join(pkgRoot, "6.0.0", "install.json"), []byte(`{"version":"6.0.0","bucket":"extras"}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkgRoot, "7.0.0", "install.json"), []byte(`{"version":"7.0.0","bucket":"extras"}`), 0644); err != nil {
		t.Fatal(err)
	}

	if err := apps.LinkCurrent(pkgRoot, "7.0.0"); err != nil {
		t.Fatal(err)
	}

	eng, err := engine.NewEngine(&engine.EngineConfig{RootDir: root})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	snap, err := eng.ExportCoreSnapshot(snapshot.Meta{})
	if err != nil {
		t.Fatal(err)
	}
	var versions []string
	var currentMarked int
	for _, p := range snap.Core.Packages {
		if p.Name == "vivaldi" {
			versions = append(versions, p.Version)
			if p.Bucket != "extras" {
				t.Fatalf("bucket = %q", p.Bucket)
			}
			if p.Current {
				currentMarked++
				if p.Version != "7.0.0" {
					t.Fatalf("current version = %q", p.Version)
				}
			}
		}
	}
	if len(versions) != 2 {
		t.Fatalf("versions = %#v, packages = %#v", versions, snap.Core.Packages)
	}
	if currentMarked != 1 {
		t.Fatalf("currentMarked = %d, packages = %#v", currentMarked, snap.Core.Packages)
	}
}

func TestApplyRejectsReconcile(t *testing.T) {
	root := t.TempDir()
	eng, err := engine.NewEngine(&engine.EngineConfig{RootDir: root})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	target := &snapshot.Snapshot{
		SchemaVersion: snapshot.SchemaVersion,
		Kind:          snapshot.Kind,
		ID:            "test_x",
		CreatedAt:     snapshot.NowRFC3339(),
		Device:        snapshot.Device{DeviceID: "aabbccddeeff00112233445566778899", OS: "windows", Arch: "arm64"},
	}
	_, err = eng.ApplyCoreSnapshot(context.Background(), target, snapshot.ApplyOptions{Mode: snapshot.ApplyModeReconcile}, nil)
	if err == nil {
		t.Fatal("expected reconcile rejection")
	}
}
