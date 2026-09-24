package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/gluestick-sh/core/config"
	"github.com/gluestick-sh/core/engine"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type mcpInstallInput struct {
	Package string `json:"package" jsonschema:"package reference to install (bucket/name or name)"`
	Force   bool   `json:"force,omitempty" jsonschema:"force reinstall: skip cache and discard partial downloads"`
}

type mcpUninstallInput struct {
	Package string `json:"package" jsonschema:"installed package to uninstall"`
	Purge   bool   `json:"purge,omitempty" jsonschema:"also remove the cache index entry and store files"`
}

type mcpConfirmInput struct {
	Token string `json:"token" jsonschema:"confirm_token returned by a pending write tool"`
}

type mcpUpdateInput struct {
	Package string `json:"package,omitempty" jsonschema:"package to upgrade; omit when all=true"`
	All     bool   `json:"all,omitempty" jsonschema:"upgrade every outdated package"`
}

type mcpBucketAddInput struct {
	Name string `json:"name" jsonschema:"bucket name (known name like main, or any name with url)"`
	URL  string `json:"url,omitempty" jsonschema:"repository URL; omit for a known bucket"`
}

type mcpBucketUpdateInput struct {
	Name string `json:"name,omitempty" jsonschema:"bucket to update; omit to update all buckets"`
}

// registerGlueWriteTools adds the write tools plus the confirmation endpoint.
func registerGlueWriteTools(server *mcp.Server, eng *engine.Engine, root string) {
	addGlueWriteTool(server, eng, &mcp.Tool{
		Name:        "glue_install",
		Description: "Install a package. Returns a confirm token unless agent.auto_yes is enabled; finish with glue_confirm.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in mcpInstallInput) (*mcp.CallToolResult, any, error) {
		out, err := runMCPInstallCall(mcpAuditContext(ctx, req), eng, root, in)
		return nil, out, err
	})

	addGlueWriteTool(server, eng, &mcp.Tool{
		Name:        "glue_uninstall",
		Description: "Uninstall a package. Returns a confirm token unless agent.auto_yes is enabled; finish with glue_confirm.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in mcpUninstallInput) (*mcp.CallToolResult, any, error) {
		out, err := runMCPUninstallCall(mcpAuditContext(ctx, req), eng, root, in)
		return nil, out, err
	})

	addGlueWriteTool(server, eng, &mcp.Tool{
		Name:        "glue_confirm",
		Description: "Execute a pending write operation using its confirm token (single use, 5 minute TTL).",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in mcpConfirmInput) (*mcp.CallToolResult, any, error) {
		out, err := runMCPConfirm(mcpAuditContext(ctx, req), eng, root, in.Token)
		return nil, out, err
	})

	addGlueWriteTool(server, eng, &mcp.Tool{
		Name:        "glue_update",
		Description: "Upgrade one package or all outdated packages. Returns a confirm token unless agent.auto_yes is enabled.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in mcpUpdateInput) (*mcp.CallToolResult, any, error) {
		out, err := runMCPUpdateCall(mcpAuditContext(ctx, req), eng, root, in)
		return nil, out, err
	})

	addGlueWriteTool(server, eng, &mcp.Tool{
		Name:        "glue_bucket_add",
		Description: "Add a Scoop-compatible bucket. Returns a confirm token unless agent.auto_yes is enabled.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in mcpBucketAddInput) (*mcp.CallToolResult, any, error) {
		out, err := runMCPBucketAddCall(mcpAuditContext(ctx, req), eng, root, in)
		return nil, out, err
	})

	addGlueWriteTool(server, eng, &mcp.Tool{
		Name:        "glue_bucket_update",
		Description: "Update one bucket or all buckets. Returns a confirm token unless agent.auto_yes is enabled.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in mcpBucketUpdateInput) (*mcp.CallToolResult, any, error) {
		out, err := runMCPBucketUpdateCall(mcpAuditContext(ctx, req), eng, root, in)
		return nil, out, err
	})
}

// mcpEvaluate loads the agent policy and decides allow/confirm for op.
func mcpEvaluate(root, op, pkg string) (mcpPolicyDecision, error) {
	settings, err := config.ReadAgent(root)
	if err != nil {
		return mcpPolicyDecision{}, fmt.Errorf("read agent policy: %w", err)
	}
	return decideMCPPolicy(settings, op, pkg), nil
}

// runMCPInstallCall either returns a pending confirm token or (auto_yes /
// policy auto) executes the install immediately.
func runMCPInstallCall(ctx context.Context, eng *engine.Engine, root string, in mcpInstallInput) (any, error) {
	pkg := strings.TrimSpace(in.Package)
	if pkg == "" {
		return nil, &mcpToolError{Code: "invalid_request", Message: "package is required"}
	}
	decision, err := mcpEvaluate(root, "install", pkg)
	if err != nil {
		return nil, err
	}
	if !decision.Allowed {
		mcpRecordDecision(ctx, eng, "install", pkg, "denied", map[string]any{"phase": "denied", "reason": decision.Code})
		return nil, &mcpToolError{Code: decision.Code, Message: decision.Message, Hint: decision.Hint}
	}
	if !decision.Confirm {
		mcpRecordDecision(ctx, eng, "install", pkg, "granted", map[string]any{"phase": "granted", "mode": "auto"})
		return executeMCPInstall(ctx, eng, pkg, in.Force)
	}
	mcpRecordDecision(ctx, eng, "install", pkg, "pending", map[string]any{"phase": "confirm_required", "action": "install"})
	token, expiresIn, err := mcpIssueConfirm(root, "install", pkg, in.Force, "")
	if err != nil {
		return nil, err
	}
	return mcpPendingPayload("install", pkg, token, expiresIn), nil
}

// runMCPUninstallCall is the uninstall counterpart of runMCPInstallCall.
func runMCPUninstallCall(ctx context.Context, eng *engine.Engine, root string, in mcpUninstallInput) (any, error) {
	pkg := strings.TrimSpace(in.Package)
	if pkg == "" {
		return nil, &mcpToolError{Code: "invalid_request", Message: "package is required"}
	}
	decision, err := mcpEvaluate(root, "uninstall", pkg)
	if err != nil {
		return nil, err
	}
	if !decision.Allowed {
		mcpRecordDecision(ctx, eng, "uninstall", pkg, "denied", map[string]any{"phase": "denied", "reason": decision.Code})
		return nil, &mcpToolError{Code: decision.Code, Message: decision.Message, Hint: decision.Hint}
	}
	if mcpPackageHeld(eng, pkg) {
		mcpRecordDecision(ctx, eng, "uninstall", pkg, "denied", map[string]any{"phase": "denied", "reason": "held"})
		return nil, mcpHeldError(pkg)
	}
	if !decision.Confirm {
		mcpRecordDecision(ctx, eng, "uninstall", pkg, "granted", map[string]any{"phase": "granted", "mode": "auto"})
		return executeMCPUninstall(ctx, eng, pkg, in.Purge)
	}
	mcpRecordDecision(ctx, eng, "uninstall", pkg, "pending", map[string]any{"phase": "confirm_required", "action": "uninstall"})
	token, expiresIn, err := mcpIssueConfirm(root, "uninstall", pkg, in.Purge, "")
	if err != nil {
		return nil, err
	}
	return mcpPendingPayload("uninstall", pkg, token, expiresIn), nil
}

// runMCPConfirm consumes a token and executes the bound operation. Policy is
// re-evaluated at execution time, so a deny list added after issue still blocks.
func runMCPConfirm(ctx context.Context, eng *engine.Engine, root, token string) (any, error) {
	action, err := mcpConsumeConfirm(root, token)
	if err != nil {
		return nil, err
	}
	decision, err := mcpEvaluate(root, action.Op, action.Package)
	if err != nil {
		return nil, err
	}
	if !decision.Allowed {
		mcpRecordDecision(ctx, eng, action.Op, action.Package, "denied", map[string]any{"phase": "denied", "reason": decision.Code, "token": mcpTokenTag(token)})
		return nil, &mcpToolError{Code: decision.Code, Message: decision.Message, Hint: decision.Hint}
	}
	if action.Op == "uninstall" && mcpPackageHeld(eng, action.Package) {
		mcpRecordDecision(ctx, eng, action.Op, action.Package, "denied", map[string]any{"phase": "denied", "reason": "held", "token": mcpTokenTag(token)})
		return nil, mcpHeldError(action.Package)
	}
	mcpRecordDecision(ctx, eng, action.Op, action.Package, "granted", map[string]any{"phase": "granted", "mode": "confirm_token", "token": mcpTokenTag(token)})
	switch action.Op {
	case "install":
		return executeMCPInstall(ctx, eng, action.Package, action.Flag)
	case "uninstall":
		return executeMCPUninstall(ctx, eng, action.Package, action.Flag)
	case "update":
		return executeMCPUpdate(ctx, eng, action.Package, action.Flag)
	case "bucket_add":
		return executeMCPBucketAdd(ctx, eng, root, action.Package, action.Arg)
	case "bucket_update":
		return executeMCPBucketUpdate(ctx, eng, root, action.Package)
	}
	return nil, &mcpToolError{
		Code:    "invalid_confirm_token",
		Message: "confirm token references an unknown operation",
	}
}

// runMCPUpdateCall gates package upgrades behind the same confirm flow.
func runMCPUpdateCall(ctx context.Context, eng *engine.Engine, root string, in mcpUpdateInput) (any, error) {
	pkg := strings.TrimSpace(in.Package)
	if pkg == "" && !in.All {
		return nil, &mcpToolError{Code: "invalid_request", Message: "provide package or all=true"}
	}
	decision, err := mcpEvaluate(root, "update", pkg)
	if err != nil {
		return nil, err
	}
	if !decision.Allowed {
		mcpRecordDecision(ctx, eng, "update", pkg, "denied", map[string]any{"phase": "denied", "reason": decision.Code})
		return nil, &mcpToolError{Code: decision.Code, Message: decision.Message, Hint: decision.Hint}
	}
	if !decision.Confirm {
		mcpRecordDecision(ctx, eng, "update", pkg, "granted", map[string]any{"phase": "granted", "mode": "auto"})
		return executeMCPUpdate(ctx, eng, pkg, in.All)
	}
	mcpRecordDecision(ctx, eng, "update", pkg, "pending", map[string]any{"phase": "confirm_required", "action": "update"})
	token, expiresIn, err := mcpIssueConfirm(root, "update", pkg, in.All, "")
	if err != nil {
		return nil, err
	}
	return mcpPendingPayload("update", pkg, token, expiresIn), nil
}

// runMCPBucketAddCall gates bucket cloning behind the same confirm flow.
func runMCPBucketAddCall(ctx context.Context, eng *engine.Engine, root string, in mcpBucketAddInput) (any, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, &mcpToolError{Code: "invalid_request", Message: "name is required"}
	}
	decision, err := mcpEvaluate(root, "bucket_add", name)
	if err != nil {
		return nil, err
	}
	if !decision.Allowed {
		mcpRecordDecision(ctx, eng, "bucket_add", name, "denied", map[string]any{"phase": "denied", "reason": decision.Code})
		return nil, &mcpToolError{Code: decision.Code, Message: decision.Message, Hint: decision.Hint}
	}
	if !decision.Confirm {
		mcpRecordDecision(ctx, eng, "bucket_add", name, "granted", map[string]any{"phase": "granted", "mode": "auto"})
		return executeMCPBucketAdd(ctx, eng, root, name, in.URL)
	}
	mcpRecordDecision(ctx, eng, "bucket_add", name, "pending", map[string]any{"phase": "confirm_required", "action": "bucket_add"})
	token, expiresIn, err := mcpIssueConfirm(root, "bucket_add", name, false, strings.TrimSpace(in.URL))
	if err != nil {
		return nil, err
	}
	return mcpPendingPayload("bucket_add", name, token, expiresIn), nil
}

// runMCPBucketUpdateCall gates bucket git pulls behind the same confirm flow.
func runMCPBucketUpdateCall(ctx context.Context, eng *engine.Engine, root string, in mcpBucketUpdateInput) (any, error) {
	name := strings.TrimSpace(in.Name)
	decision, err := mcpEvaluate(root, "bucket_update", name)
	if err != nil {
		return nil, err
	}
	if !decision.Allowed {
		mcpRecordDecision(ctx, eng, "bucket_update", name, "denied", map[string]any{"phase": "denied", "reason": decision.Code})
		return nil, &mcpToolError{Code: decision.Code, Message: decision.Message, Hint: decision.Hint}
	}
	if !decision.Confirm {
		mcpRecordDecision(ctx, eng, "bucket_update", name, "granted", map[string]any{"phase": "granted", "mode": "auto"})
		return executeMCPBucketUpdate(ctx, eng, root, name)
	}
	mcpRecordDecision(ctx, eng, "bucket_update", name, "pending", map[string]any{"phase": "confirm_required", "action": "bucket_update"})
	token, expiresIn, err := mcpIssueConfirm(root, "bucket_update", name, false, "")
	if err != nil {
		return nil, err
	}
	return mcpPendingPayload("bucket_update", name, token, expiresIn), nil
}
