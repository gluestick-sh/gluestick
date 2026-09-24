// Package git wraps the git CLI for bucket clone, pull, and update checks.
package git

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gluestick-sh/core/message"
	"github.com/gluestick-sh/core/procutil"
)

// checkUpdateTimeout bounds the network fetch performed while checking for
// bucket updates so a slow or unreachable remote cannot hang the UI.
const checkUpdateTimeout = 15 * time.Second

// newCmd creates a new exec.Cmd with the given name and args, hiding the window on Windows.
func newCmd(name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	procutil.HideWindow(cmd)
	return cmd
}

// newCmdCtx creates a new exec.Cmd with context, name, and args, hiding the window on Windows.
func newCmdCtx(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	procutil.HideWindow(cmd)
	return cmd
}

// Runner executes git commands
type Runner struct {
	gitPath string
	timeout time.Duration
	repoMu  sync.Map // repo dir -> *sync.Mutex
}

// lockRepo acquires a per-repository mutex for the given directory.
// Returns an unlock function that should be called as defer unlock().
func (r *Runner) lockRepo(dir string) func() {
	key := filepath.Clean(dir)
	v, _ := r.repoMu.LoadOrStore(key, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// NewRunner creates a new git command runner
func NewRunner() *Runner {
	return &Runner{
		gitPath: "git", // Assume git is in PATH
		timeout: 5 * time.Minute,
	}
}

// SetGitPath sets the path to the git executable
func (r *Runner) SetGitPath(path string) {
	r.gitPath = path
}

// SetTimeout sets the timeout for git operations
func (r *Runner) SetTimeout(d time.Duration) {
	r.timeout = d
}

// Check checks if git is available
func (r *Runner) Check() error {
	cmd := newCmd(r.gitPath, "--version")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git not found: %w", err)
	}
	if !strings.Contains(string(output), "git version") {
		return fmt.Errorf("git version check failed")
	}
	return nil
}

// Clone clones a repository to a directory
// shallow=true uses --depth=1 for faster cloning
func (r *Runner) Clone(repoURL, destDir string, shallow bool) error {
	return r.CloneWithProgress(repoURL, destDir, shallow, nil)
}

// CloneWithProgress clones a repository and streams git --progress stderr lines.
func (r *Runner) CloneWithProgress(repoURL, destDir string,
	shallow bool,
	onProgress ProgressCallback,
) error {
	args := []string{"clone", "--progress"}
	if shallow {
		args = append(args, "--depth=1")
	}
	args = append(args, repoURL, destDir)

	cmd := newCmd(r.gitPath, args...)
	var stderr bytes.Buffer
	if onProgress != nil {
		writer := &lineBufferWriter{fn: func(line string) {
			msg, pct := parseGitProgressLine(line)
			onProgress(msg, pct)
		}}
		cmd.Stderr = writer
	} else {
		cmd.Stderr = &stderr
	}

	if err := cmd.Run(); err != nil {
		if onProgress == nil {
			return fmt.Errorf("git clone failed: %w\n%s", err, stderr.String())
		}
		return fmt.Errorf("git clone failed: %w", err)
	}

	return nil
}

// Pull fast-forwards the current branch to its upstream after fetching.
// Serialized per repo dir to avoid racing with CheckUpdateStatus fetch.
func (r *Runner) Pull(dir string) error {
	unlock := r.lockRepo(dir)
	defer unlock()

	ctx, cancel := context.WithTimeout(context.Background(), r.timeout)
	defer cancel()

	if err := r.fetchCtx(ctx, dir, false); err != nil {
		return err
	}

	cmd := newCmdCtx(ctx, r.gitPath, "-C", dir, "merge", "--ff-only", "@{upstream}")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("git merge timed out")
		}
		return fmt.Errorf("git merge --ff-only failed: %w\n%s", err, stderr.String())
	}

	return nil
}

// Fetch fetches updates from a repository
func (r *Runner) Fetch(dir string) error {
	unlock := r.lockRepo(dir)
	defer unlock()

	ctx, cancel := context.WithTimeout(context.Background(), r.timeout)
	defer cancel()
	return r.fetchCtx(ctx, dir, false)
}

// fetchCtx runs git fetch with context and optional --no-write-fetch-head flag.
// Handles timeout detection and error formatting.
func (r *Runner) fetchCtx(ctx context.Context, dir string, noWriteFetchHead bool) error {
	args := []string{"-C", dir, "fetch"}
	if noWriteFetchHead {
		args = append(args, "--no-write-fetch-head")
	}
	cmd := newCmdCtx(ctx, r.gitPath, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("git fetch timed out")
		}
		detail := strings.TrimSpace(stderr.String())
		if detail != "" {
			detail = strings.Join(strings.Fields(detail), " ")
			return fmt.Errorf("git fetch failed: %w (%s)", err, detail)
		}
		return fmt.Errorf("git fetch failed: %w", err)
	}

	return nil
}

// GetRemoteURL returns the origin remote URL for a repository
func (r *Runner) GetRemoteURL(dir string) (string, error) {
	cmd := newCmd(r.gitPath, "-C", dir, "config", "--get", "remote.origin.url")
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("get remote URL: %w", err)
	}
	return strings.TrimSpace(string(output)), nil
}

// GetCurrentCommit returns the current commit hash
func (r *Runner) GetCurrentCommit(dir string) (string, error) {
	cmd := newCmd(r.gitPath, "-C", dir, "rev-parse", "HEAD")
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("get current commit: %w", err)
	}
	return strings.TrimSpace(string(output)), nil
}

// GetLastCommitDate returns the date of the last commit
func (r *Runner) GetLastCommitDate(dir string) (time.Time, error) {
	cmd := newCmd(r.gitPath, "-C", dir, "log", "-1", "--format=%ci")
	output, err := cmd.Output()
	if err != nil {
		return time.Time{}, fmt.Errorf("get last commit date: %w", err)
	}

	return time.Parse("2006-01-02 15:04:05 -0700", strings.TrimSpace(string(output)))
}

// IsRepository checks if a directory is a git repository
func (r *Runner) IsRepository(dir string) bool {
	cmd := newCmd(r.gitPath, "-C", dir, "rev-parse", "--git-dir")
	return cmd.Run() == nil
}

// UpdateStatus describes local vs upstream commit state after a fetch.
type UpdateStatus struct {
	HasUpdates   bool
	LocalCommit  string
	RemoteCommit string
	OK           bool
	ErrMsg       string
}

// HasUpdates checks if a repository has updates available
// Returns true if the remote has commits that are not in the local branch
func (r *Runner) HasUpdates(dir string) (bool, error) {
	status, err := r.CheckUpdateStatus(dir)
	return status.HasUpdates, err
}

// CheckUpdateStatus fetches upstream and returns local/upstream commit SHAs.
func (r *Runner) CheckUpdateStatus(dir string) (UpdateStatus, error) {
	unlock := r.lockRepo(dir)
	defer unlock()

	ctx, cancel := context.WithTimeout(context.Background(), checkUpdateTimeout)
	defer cancel()

	local, err := r.revParseCtx(ctx, dir, "HEAD")
	if err != nil {
		return UpdateStatus{ErrMsg: fmt.Sprintf("get local commit: %v", err)}, fmt.Errorf("get local commit: %w", err)
	}

	// Do not write FETCH_HEAD so a concurrent pull (after lock release) is not confused.
	if err := r.fetchCtx(ctx, dir, true); err != nil {
		return UpdateStatus{LocalCommit: local, ErrMsg: err.Error()}, err
	}

	remote, err := r.revParseCtx(ctx, dir, "@{upstream}")
	if err != nil {
		return UpdateStatus{LocalCommit: local, ErrMsg: fmt.Sprintf("get remote commit: %v", err)}, fmt.Errorf("get remote commit: %w", err)
	}

	behind, err := r.commitsBehindUpstreamCtx(ctx, dir)
	if err != nil {
		return UpdateStatus{
			LocalCommit:  local,
			RemoteCommit: remote,
			ErrMsg:       err.Error(),
		}, err
	}

	return UpdateStatus{
		HasUpdates:   behind,
		LocalCommit:  local,
		RemoteCommit: remote,
		OK:           true,
	}, nil
}

// revParseCtx runs git rev-parse with context to resolve a reference to its commit SHA.
// Returns the trimmed commit SHA string or an error.
func (r *Runner) revParseCtx(ctx context.Context, dir, ref string) (string, error) {
	cmd := newCmdCtx(ctx, r.gitPath, "-C", dir, "rev-parse", ref)
	output, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

// commitsBehindUpstreamCtx checks if the local branch is behind upstream using git rev-list.
// Returns true if there are commits in upstream that are not in HEAD.
func (r *Runner) commitsBehindUpstreamCtx(ctx context.Context, dir string) (bool, error) {
	cmd := newCmdCtx(ctx, r.gitPath, "-C", dir, "rev-list", "--count", "--left-right", "@{upstream}...HEAD")
	output, err := cmd.Output()
	if err != nil {
		return false, fmt.Errorf("check for updates: %w", err)
	}

	// Output format: "2\t0" means upstream is 2 commits ahead of HEAD
	parts := strings.Split(strings.TrimSpace(string(output)), "\t")
	if len(parts) != 2 {
		return false, fmt.Errorf("unexpected rev-list output: %s", output)
	}

	behind := strings.TrimSpace(parts[0])
	return behind != "0" && behind != "", nil
}

// GetBranch returns the current branch name
func (r *Runner) GetBranch(dir string) (string, error) {
	cmd := newCmd(r.gitPath, "-C", dir, "branch", "--show-current")
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("get branch: %w", err)
	}
	return strings.TrimSpace(string(output)), nil
}

// CheckoutBranch checks out a specific branch
func (r *Runner) CheckoutBranch(dir, branch string) error {
	cmd := newCmd(r.gitPath, "-C", dir, "checkout", branch)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("checkout branch: %w\n%s", err, stderr.String())
	}

	return nil
}

const lsRemoteProbeTimeout = 12 * time.Second

// LsRemote probes whether a remote Git repository is reachable (git ls-remote).
func (r *Runner) LsRemote(ctx context.Context, repoURL string) error {
	if r.gitPath == "" {
		r.gitPath = "git"
	}
	if ctx == nil {
		ctx = context.Background()
	}
	probeCtx, cancel := context.WithTimeout(ctx, lsRemoteProbeTimeout)
	defer cancel()
	cmd := newCmdCtx(probeCtx, r.gitPath, "ls-remote", "-q", repoURL, "HEAD")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git ls-remote %s: %w", repoURL, err)
	}
	return nil
}

// CloneOrPull clones a repository if it doesn't exist, or pulls if it does.
func (r *Runner) CloneOrPull(repoURL, destDir string, shallow bool) error {
	return r.CloneOrPullWithProgress(repoURL, destDir, shallow, nil)
}

// CloneOrPullWithProgress clones or pulls with optional git progress callbacks.
func (r *Runner) CloneOrPullWithProgress(repoURL,
	destDir string,
	shallow bool,
	onProgress ProgressCallback,
) error {
	if r.IsRepository(destDir) {
		remoteURL, err := r.GetRemoteURL(destDir)
		if err == nil {
			normRemote := strings.TrimSuffix(remoteURL, ".git")
			normRepo := strings.TrimSuffix(repoURL, ".git")
			if normRemote == normRepo {
				if onProgress != nil {
					onProgress(message.FormatEN(message.GitPulling, nil), 0)
				}
				return r.Pull(destDir)
			}
		}
	}

	return r.CloneWithProgress(repoURL, destDir, shallow, onProgress)
}

// ListFiles lists all files matching a pattern in a directory
// Returns relative paths from the directory root
func (r *Runner) ListFiles(dir, pattern string) ([]string, error) {
	args := []string{"-C", dir, "ls-files", pattern}
	cmd := newCmd(r.gitPath, args...)

	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("list files: %w", err)
	}

	if len(output) == 0 {
		return []string{}, nil
	}

	return strings.Split(strings.TrimSpace(string(output)), "\n"), nil
}
