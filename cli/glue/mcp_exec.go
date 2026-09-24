package main

import (
	"context"
	"fmt"
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
func executeMCPBucketAdd(_ context.Context, eng *engine.Engine, root, name, repoURL string) (any, error) {
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
	return map[string]any{"command": "bucket_add", "ok": true, "name": b.Name, "repo_url": b.RepoURL}, nil
}

// executeMCPBucketUpdate pulls one bucket (or all when name is empty).
func executeMCPBucketUpdate(_ context.Context, eng *engine.Engine, root, name string) (any, error) {
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
	return map[string]any{"command": "bucket_update", "ok": true, "updated": names}, nil
}

func executeMCPInstall(ctx context.Context, eng *engine.Engine, pkg string, force bool) (any, error) {
	result, err := eng.Install(ctx, &engine.InstallRequest{
		Request: engine.Request{Name: pkg, Force: force, Options: map[string]string{}},
	}, engine.NewSilentReporter())
	if failErr := installFailureError(err, result); failErr != nil {
		return nil, failErr
	}
	return map[string]any{"command": "install", "ok": true, "result": result}, nil
}

func executeMCPUninstall(ctx context.Context, eng *engine.Engine, pkg string, purge bool) (any, error) {
	result, err := eng.Uninstall(ctx, &engine.UninstallRequest{
		Request: engine.Request{Name: pkg},
		Purge:   purge,
	}, engine.NewSilentReporter())
	if err != nil {
		return nil, err
	}
	return map[string]any{"command": "uninstall", "ok": true, "result": result}, nil
}
