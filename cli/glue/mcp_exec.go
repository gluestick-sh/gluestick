package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/gluestick-sh/core/bucket"
	"github.com/gluestick-sh/core/engine"
)

func mcpPendingPayload(action, pkg, token string, expiresIn int) map[string]any {
	return map[string]any{
		"status":        "pending",
		"confirm_token": token,
		"expires_in":    expiresIn,
		"action":        action,
		"package":       pkg,
		"message":       "call glue_confirm with confirm_token to execute",
	}
}

// executeMCPUpdate upgrades the named package (or everything with all=true).
func executeMCPUpdate(ctx context.Context, eng *engine.Engine, pkg string, all bool) (any, error) {
	updates, err := eng.CheckPackageUpdates()
	if err != nil {
		return nil, fmt.Errorf("check updates: %w", err)
	}
	targets := make([]engine.PackageUpdate, 0, len(updates))
	for _, update := range updates {
		if all || strings.EqualFold(update.Name, packageBaseName(pkg)) {
			targets = append(targets, update)
		}
	}
	if len(targets) == 0 {
		return map[string]any{"command": "update", "ok": true, "upToDate": true, "updated": []string{}, "count": 0}, nil
	}
	updated := []string{}
	failures := []string{}
	for _, update := range targets {
		ref := updateInstallRef(update)
		if _, err := eng.Install(ctx, &engine.InstallRequest{
			Request: engine.Request{Name: ref, Force: true, Options: map[string]string{}},
		}, engine.NewSilentReporter()); err != nil {
			failures = append(failures, ref+": "+err.Error())
			continue
		}
		updated = append(updated, ref)
	}
	if len(failures) > 0 {
		return nil, fmt.Errorf("update failed: %s", strings.Join(failures, "; "))
	}
	return map[string]any{"command": "update", "ok": true, "updated": updated, "count": len(updated)}, nil
}

// executeMCPBucketAdd clones a bucket (known name or explicit URL) and indexes it.
func executeMCPBucketAdd(ctx context.Context, eng *engine.Engine, root, name, repoURL string) (any, error) {
	br, err := bucket.NewRegistry(root)
	if err != nil {
		return nil, fmt.Errorf("bucket registry: %w", err)
	}
	if err := br.EnsureGit(); err != nil {
		return nil, fmt.Errorf("git not available: %w", err)
	}
	br.ReloadFromDisk()
	if existing, err := br.Get(name); err == nil {
		return map[string]any{
			"command": "bucket_add", "ok": true, "name": existing.Name,
			"repo_url": existing.RepoURL, "alreadyInstalled": true,
		}, nil
	}
	if repoURL == "" {
		repoURL, _ = bucket.GetKnownBucketURL(name)
	}
	if repoURL == "" {
		return nil, &mcpToolError{Code: "bucket_unknown", Message: fmt.Sprintf("unknown bucket %q", name), Hint: "provide url for a custom bucket"}
	}
	b, err := br.Add(name, repoURL)
	if err != nil {
		return nil, err
	}
	if eng != nil {
		eng.LoadSearchIndexBucket(b.Name)
	}
	recordMCPBucketActivity(ctx, eng, "bucket_add", b.Name)
	return map[string]any{"command": "bucket_add", "ok": true, "name": b.Name, "repo_url": b.RepoURL}, nil
}

// executeMCPBucketUpdate pulls one bucket (or all when name is empty).
func executeMCPBucketUpdate(ctx context.Context, eng *engine.Engine, root, name string) (any, error) {
	br, err := bucket.NewRegistry(root)
	if err != nil {
		return nil, fmt.Errorf("bucket registry: %w", err)
	}
	if err := br.EnsureGit(); err != nil {
		return nil, fmt.Errorf("git not available: %w", err)
	}
	br.ReloadFromDisk()
	names := []string{}
	if name != "" {
		names = []string{name}
	}
	if err := br.UpdateSilent(names); err != nil {
		return nil, err
	}
	if eng != nil {
		eng.ReloadBuckets(true)
	}
	label := name
	if label == "" {
		label = "*" // all buckets, matching the CLI's activity label
	}
	recordMCPBucketActivity(ctx, eng, "bucket_update", label)
	return map[string]any{"command": "bucket_update", "ok": true, "updated": names}, nil
}

// executeMCPBucketRemove deletes a bucket checkout and drops it from the search
// index. The audit row carries the MCP source/actor from ctx, so the trail says
// which agent removed which bucket.
func executeMCPBucketRemove(ctx context.Context, eng *engine.Engine, root, name string) (any, error) {
	br, err := bucket.NewRegistry(root)
	if err != nil {
		return nil, fmt.Errorf("bucket registry: %w", err)
	}
	if err := br.EnsureGit(); err != nil {
		return nil, fmt.Errorf("git not available: %w", err)
	}
	br.ReloadFromDisk()
	if err := br.Remove(name); err != nil {
		code, hint := jsonErrorInfo(err)
		return nil, &mcpToolError{Code: code, Message: err.Error(), Hint: hint}
	}
	if eng != nil {
		eng.RemoveSearchIndexBucket(name)
	}
	recordMCPBucketActivity(ctx, eng, "bucket_remove", name)
	return jsonCommandResult{Command: "bucket_remove", OK: true, Results: []jsonResultItem{{Ref: name}}}, nil
}

// recordMCPBucketActivity writes the completion row for an MCP bucket
// operation. The CLI reaches the same operations through
// syncEngineBucketsAfter*, which has no context to carry source/actor; MCP
// passes the request context so the row reads source=mcp + client actor.
func recordMCPBucketActivity(ctx context.Context, eng *engine.Engine, op, label string) {
	if eng == nil {
		return
	}
	if err := eng.RecordAudit(ctx, op, label, "", "success", map[string]any{}); err != nil {
		fmt.Fprintf(os.Stderr, "[glue mcp] audit warning: %v\n", err)
	}
}

func executeMCPInstall(ctx context.Context, eng *engine.Engine, pkg string, force bool) (any, error) {
	result, err := eng.Install(ctx, &engine.InstallRequest{
		Request: engine.Request{Name: pkg, Force: force, Options: map[string]string{}},
	}, engine.NewSilentReporter())
	if failErr := installFailureError(err, result); failErr != nil {
		return nil, failErr
	}
	return jsonCommandResult{Command: "install", OK: true, Results: []jsonResultItem{jsonResultItemFromInstall(pkg, result, nil)}}, nil
}

func executeMCPUninstall(ctx context.Context, eng *engine.Engine, pkg string, purge bool) (any, error) {
	result, err := eng.Uninstall(ctx, &engine.UninstallRequest{
		Request: engine.Request{Name: pkg},
		Purge:   purge,
	}, engine.NewSilentReporter())
	if err != nil {
		return nil, err
	}
	return jsonCommandResult{Command: "uninstall", OK: true, Results: []jsonResultItem{jsonResultItemFromInstall(pkg, result, nil)}}, nil
}
