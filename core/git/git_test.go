package git

import (
	"errors"
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

// testRepoURL is the real remote every network-dependent test in this package
// clones. AGENTS.md treats a GitHub hiccup as environment noise, so those tests
// must skip instead of failing.
const testRepoURL = "https://github.com/ScoopInstaller/Main.git"

// networkFailureMarkers are git/remote messages meaning "the network or the
// remote was unavailable" rather than a defect in our git usage.
var networkFailureMarkers = []string{
	"unable to access",
	"could not resolve host",
	"failed to connect",
	"connection reset",
	"connection timed out",
	"operation timed out",
	"deadline exceeded",
	"the remote end hung up",
	"early eof",
	"rpc failed",
	"tls",
	"ssl",
	"502 ",
	"503 ",
	"504 ",
	"service unavailable",
	"rate limit",
}

// isNetworkFailure reports whether err looks like a transient network/remote
// failure. "repository not found" and other definitive remote answers stay
// false so a moved fixture URL cannot silently skip forever.
func isNetworkFailure(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, marker := range networkFailureMarkers {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// cloneFixtureOrSkip clones the shared test remote into destDir: an unreachable
// remote skips the test, any other error fails it.
func cloneFixtureOrSkip(t *testing.T, r *Runner, destDir string) {
	t.Helper()
	if err := r.Clone(testRepoURL, destDir, true); err != nil {
		if isNetworkFailure(err) {
			t.Skipf("network unavailable, skipping remote clone: %v", err)
		}
		t.Fatalf("Clone(%s) failed: %v", testRepoURL, err)
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
	cloneFixtureOrSkip(t, r, destDir)

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
	cloneFixtureOrSkip(t, r, destDir)

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
	repoURL := testRepoURL

	r := NewRunner()

	// First call should clone
	if err := r.CloneOrPull(repoURL, destDir, true); err != nil {
		if isNetworkFailure(err) {
			t.Skipf("network unavailable, skipping remote clone: %v", err)
		}
		t.Fatalf("CloneOrPull (first) failed: %v", err)
	}

	if !r.IsRepository(destDir) {
		t.Error("expected repository after CloneOrPull")
	}

	// Second call should pull
	if err := r.CloneOrPull(repoURL, destDir, true); err != nil {
		if isNetworkFailure(err) {
			t.Skipf("network unavailable while pulling: %v", err)
		}
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
	cloneFixtureOrSkip(t, r, destDir)

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
	cloneFixtureOrSkip(t, r, destDir)

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
	cloneFixtureOrSkip(t, r, destDir)

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

// TestIsNetworkFailure pins the classifier that decides between "environment
// noise, skip" and "real defect, fail" for the remote-clone tests. It runs
// offline so the policy itself is always covered.
func TestIsNetworkFailure(t *testing.T) {
	network := []string{
		"git clone failed: exit status 128\nfatal: unable to access 'https://github.com/ScoopInstaller/Main.git/': Could not resolve host: github.com",
		"git clone failed: exit status 128\nfatal: unable to access 'https://github.com/x/y.git/': Failed to connect to github.com port 443: Connection timed out",
		"error: RPC failed; curl 56 OpenSSL SSL_read: Connection reset by peer, errno 10054",
		"fatal: the remote end hung up unexpectedly",
		"fatal: unable to access 'https://github.com/x/y.git/': The requested URL returned error: 503 ",
		"context deadline exceeded",
	}
	for _, msg := range network {
		if !isNetworkFailure(errors.New(msg)) {
			t.Errorf("isNetworkFailure(%q) = false, want true", msg)
		}
	}

	defects := []string{
		"git clone failed: exit status 128\nfatal: repository 'https://github.com/gluestick-sh/nope.git/' not found",
		"git clone failed: exit status 128\nfatal: destination path 'repo' already exists and is not an empty directory.",
		"git not found: exec: \"git\": executable file not found in %PATH%",
		"fatal: not a git repository (or any of the parent directories): .git",
		"git pull failed: exit status 1\nfatal: refusing to merge unrelated histories",
	}
	for _, msg := range defects {
		if isNetworkFailure(errors.New(msg)) {
			t.Errorf("isNetworkFailure(%q) = true, want false", msg)
		}
	}

	if isNetworkFailure(nil) {
		t.Error("isNetworkFailure(nil) = true, want false")
	}
}
