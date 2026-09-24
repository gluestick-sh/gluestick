// Bucket git health for the readiness view: dubious-ownership errors block
// every git command glue runs in a bucket, and an unpinned local autocrlf lets
// later global config flips dirty a bucket checkout. Glue-scoped by design —
// --fix trusts/pins only these directories, never the user's global config.
package engine

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/gluestick-sh/core/bucket"
	"github.com/gluestick-sh/core/message"
)

// agentGit runs git in dir (with -C), bounded so a hung credential helper
// cannot stall the doctor run.
func agentGit(ctx context.Context, dir string, args ...string) (string, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

// agentFirstLine trims git output to its first line for compact details.
func agentFirstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	return s
}

// ProbeGitBucket classifies one bucket checkout: "", "ownership"
// (dubious-ownership error, cured by safe.directory), "unpinned" (no local
// core.autocrlf), or "error" (any other git failure). detail carries a short
// human string for the fail path. Exported so `glue doctor --fix` re-uses the
// exact probe the check runs.
func ProbeGitBucket(ctx context.Context, dir string) (kind, detail string) {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, stderr, err := agentGit(ctx, dir, "rev-parse", "--is-inside-work-tree"); err != nil {
		if strings.Contains(strings.ToLower(stderr), "dubious") {
			return "ownership", agentFirstLine(stderr)
		}
		if line := agentFirstLine(stderr); line != "" {
			return "error", line
		}
		return "error", "git rev-parse failed"
	}
	// --local only: a value inherited from the global config counts as unpinned.
	if out, _, err := agentGit(ctx, dir, "config", "--local", "--get", "core.autocrlf"); err == nil && strings.TrimSpace(out) != "" {
		return "", ""
	}
	return "unpinned", "local core.autocrlf not set"
}

// agentCheckGitConfig inspects every registered bucket checkout. Skips when no
// bucket exists yet (the blocking buckets check covers that) or git is absent.
func (e *Engine) agentCheckGitConfig(ctx context.Context) DoctorCheck {
	c := DoctorCheck{ID: message.AgentCheckGitConfig, Level: AgentLevelAdvisory}
	buckets := []*bucket.Bucket{}
	if e.BucketRegistry != nil {
		buckets = e.BucketRegistry.List()
	}
	if len(buckets) == 0 {
		c.Status = AgentStatusSkipped
		c.DetailKey = message.AgentGitConfigNoBuckets
		c.DetailText = "add a bucket first"
		return c
	}
	if _, err := exec.LookPath("git"); err != nil {
		c.Status = AgentStatusSkipped
		c.DetailKey = message.AgentGitConfigNoGit
		c.DetailText = "git not on PATH"
		return c
	}
	ownership, unpinned, failed := []string{}, []string{}, []string{}
	scanned := 0
	for _, b := range buckets {
		if b.Root == "" {
			continue
		}
		scanned++
		switch kind, _ := ProbeGitBucket(ctx, b.Root); kind {
		case "ownership":
			ownership = append(ownership, b.Name)
		case "unpinned":
			unpinned = append(unpinned, b.Name)
		case "error":
			failed = append(failed, b.Name)
		}
	}
	issues := len(ownership) + len(unpinned) + len(failed)
	c.Data = map[string]any{
		"buckets_scanned": scanned,
		"ownership":       ownership,
		"unpinned":        unpinned,
		"errors":          failed,
	}
	c.DetailText = fmt.Sprintf("%d bucket(s), %d issue(s)", scanned, issues)
	if issues == 0 {
		c.OK = true
		c.Status = AgentStatusPass
		c.DetailKey = message.AgentGitConfigOK
		return c
	}
	c.Status = AgentStatusFail
	c.DetailKey = message.AgentGitConfigBad
	c.HintKey = message.AgentHintGitConfig
	c.Hint = doctorHint(c.HintKey)
	return c
}
