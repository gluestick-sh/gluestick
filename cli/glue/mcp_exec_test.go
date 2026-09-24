package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/gluestick-sh/core/engine"
)

// runGitExec runs git in dir, failing the test on error.
func runGitExec(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// TestExecuteMCPUpdate_noUpdates covers the update execution path without any
// network: a fresh root has nothing to upgrade.
func TestExecuteMCPUpdate_noUpdates(t *testing.T) {
	root := t.TempDir()
	eng, err := engine.NewEngine(&engine.EngineConfig{RootDir: root})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	defer eng.Close()

	out, err := executeMCPUpdate(context.Background(), eng, "", true)
	if err != nil {
		t.Fatalf("executeMCPUpdate: %v", err)
	}
	payload, ok := out.(map[string]any)
	if !ok || payload["upToDate"] != true {
		t.Fatalf("payload = %#v, want upToDate=true", out)
	}
}

// TestExecuteMCPBucketAddUpdate_localRepo covers bucket add/update end to end
// against a local git repository (no network).
func TestExecuteMCPBucketAddUpdate_localRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	runGitExec(t, repo, "init")
	if err := os.MkdirAll(filepath.Join(repo, "bucket"), 0755); err != nil {
		t.Fatalf("mkdir bucket: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "bucket", "demo.json"), []byte(`{"version":"1.0.0"}`), 0644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	runGitExec(t, repo, "add", ".")
	runGitExec(t, repo, "-c", "user.email=test@example.invalid", "-c", "user.name=Glue Test", "commit", "-m", "init")

	url := "file:///" + filepath.ToSlash(repo)
	root := t.TempDir()
	eng, err := engine.NewEngine(&engine.EngineConfig{RootDir: root})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	defer eng.Close()

	if _, err := executeMCPBucketAdd(context.Background(), eng, root, "localtest", url); err != nil {
		t.Fatalf("executeMCPBucketAdd: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "buckets", "localtest", "bucket", "demo.json")); err != nil {
		t.Fatalf("bucket not cloned: %v", err)
	}

	// Second commit, then update must pull it.
	if err := os.WriteFile(filepath.Join(repo, "bucket", "demo2.json"), []byte(`{"version":"1.0.0"}`), 0644); err != nil {
		t.Fatalf("write second manifest: %v", err)
	}
	runGitExec(t, repo, "add", ".")
	runGitExec(t, repo, "-c", "user.email=test@example.invalid", "-c", "user.name=Glue Test", "commit", "-m", "second")

	if _, err := executeMCPBucketUpdate(context.Background(), eng, root, "localtest"); err != nil {
		t.Fatalf("executeMCPBucketUpdate: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "buckets", "localtest", "bucket", "demo2.json")); err != nil {
		t.Fatalf("bucket update did not pull: %v", err)
	}
}
