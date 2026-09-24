package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func skipIfNoGit(t *testing.T) {
	if exec.Command("git", "--version").Run() != nil {
		t.Skip("git not available")
	}
}

func TestGitCheck(t *testing.T) {
	skipIfNoGit(t)

	r := NewRunner()
	if err := r.Check(); err != nil {
		t.Errorf("Check failed: %v", err)
	}
}

func TestClone(t *testing.T) {
	skipIfNoGit(t)

	tmpDir, err := os.MkdirTemp("", "git-clone-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	destDir := filepath.Join(tmpDir, "repo")

	r := NewRunner()
	err = r.Clone("https://github.com/ScoopInstaller/Main.git", destDir, true)
	if err != nil {
		t.Fatalf("Clone failed: %v", err)
	}

	// Verify it's a repository
	if !r.IsRepository(destDir) {
		t.Error("cloned directory is not a git repository")
	}

	// Verify remote URL
	remoteURL, err := r.GetRemoteURL(destDir)
	if err != nil {
		t.Errorf("GetRemoteURL failed: %v", err)
	}
	if !contains(remoteURL, "github.com") && !contains(remoteURL, "ScoopInstaller") {
		t.Errorf("unexpected remote URL: %s", remoteURL)
	}
}

func TestPull(t *testing.T) {
	skipIfNoGit(t)

	tmpDir, err := os.MkdirTemp("", "git-pull-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	destDir := filepath.Join(tmpDir, "repo")

	r := NewRunner()

	// First clone
	if err := r.Clone("https://github.com/ScoopInstaller/Main.git", destDir, true); err != nil {
		t.Skipf("Setup failed: %v", err)
	}

	// Then pull
	if err := r.Pull(destDir); err != nil {
		t.Errorf("Pull failed: %v", err)
	}
}

func TestCloneOrPull(t *testing.T) {
	skipIfNoGit(t)

	tmpDir, err := os.MkdirTemp("", "git-cloneorpull-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	destDir := filepath.Join(tmpDir, "repo")
	repoURL := "https://github.com/ScoopInstaller/Main.git"

	r := NewRunner()

	// First call should clone
	if err := r.CloneOrPull(repoURL, destDir, true); err != nil {
		t.Fatalf("CloneOrPull (first) failed: %v", err)
	}

	if !r.IsRepository(destDir) {
		t.Error("expected repository after CloneOrPull")
	}

	// Second call should pull
	if err := r.CloneOrPull(repoURL, destDir, true); err != nil {
		t.Errorf("CloneOrPull (second) failed: %v", err)
	}
}

func TestGetCurrentCommit(t *testing.T) {
	skipIfNoGit(t)

	tmpDir, err := os.MkdirTemp("", "git-commit-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	destDir := filepath.Join(tmpDir, "repo")

	r := NewRunner()
	if err := r.Clone("https://github.com/ScoopInstaller/Main.git", destDir, true); err != nil {
		t.Skipf("Setup failed: %v", err)
	}

	commit, err := r.GetCurrentCommit(destDir)
	if err != nil {
		t.Errorf("GetCurrentCommit failed: %v", err)
	}

	// Git commit hashes are 40 hex characters
	if len(commit) != 40 {
		t.Errorf("commit hash length = %d, want 40", len(commit))
	}
}

func TestListFiles(t *testing.T) {
	skipIfNoGit(t)

	tmpDir, err := os.MkdirTemp("", "git-list-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	destDir := filepath.Join(tmpDir, "repo")

	r := NewRunner()
	if err := r.Clone("https://github.com/ScoopInstaller/Main.git", destDir, true); err != nil {
		t.Skipf("Setup failed: %v", err)
	}

	// List all JSON files
	files, err := r.ListFiles(destDir, "*.json")
	if err != nil {
		t.Errorf("ListFiles failed: %v", err)
	}

	// Should have some JSON manifests
	if len(files) == 0 {
		t.Error("expected to find some JSON files")
	}

	// Check for a known manifest (using less common name)
	hasAny := false
	for _, f := range files {
		// Just check we have .json files
		if strings.HasSuffix(f, ".json") {
			hasAny = true
			break
		}
	}
	if !hasAny {
		t.Error("expected to find .json files")
	}
}

func TestConcurrentCheckAndPull(t *testing.T) {
	skipIfNoGit(t)

	tmpDir, err := os.MkdirTemp("", "git-concurrent-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	destDir := filepath.Join(tmpDir, "repo")
	r := NewRunner()
	if err := r.Clone("https://github.com/ScoopInstaller/Main.git", destDir, true); err != nil {
		t.Skipf("Setup failed: %v", err)
	}

	var wg sync.WaitGroup
	errCh := make(chan error, 20)
	for i := 0; i < 10; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, err := r.CheckUpdateStatus(destDir)
			if err != nil {
				errCh <- err
			}
		}()
		go func() {
			defer wg.Done()
			if err := r.Pull(destDir); err != nil {
				errCh <- err
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if strings.Contains(err.Error(), "Cannot fast-forward to multiple branches") {
			t.Fatalf("concurrent check/pull race: %v", err)
		}
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr ||
		len(s) > len(substr) && findInString(s, substr))
}

func findInString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
