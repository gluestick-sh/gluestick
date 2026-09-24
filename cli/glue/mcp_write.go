package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/gluestick-sh/core/bucket"
	"github.com/gluestick-sh/core/config"
	"github.com/gluestick-sh/core/engine"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCP write tools (roadmap §4.6.2): install/uninstall never run immediately.
// Unless config.json sets agent.auto_yes (or agent.policy.mode=auto), the tool
// returns a pending confirm token and glue_confirm executes the operation. A
// deny/protected policy hit is refused outright, with or without a token.

// mcpToolError renders the {code,message,hint} contract as the tool's error
// content, so MCP clients can parse policy/confirm failures like CLI --json.
type mcpToolError struct {
	Code    string
	Message string
	Hint    string
}

func (e *mcpToolError) Error() string {
	payload := map[string]string{"code": e.Code, "message": e.Message}
	if e.Hint != "" {
		payload["hint"] = e.Hint
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return e.Message
	}
	return string(encoded)
}

// mcpPolicyDecision is the result of evaluating agent.policy for one operation.
type mcpPolicyDecision struct {
	Allowed bool
	Confirm bool
	Code    string
	Message string
	Hint    string
}

// decideMCPPolicy applies the deny list and protected packages first, then
// decides whether a confirm token is required.
func decideMCPPolicy(settings config.AgentSettings, op, pkg string) mcpPolicyDecision {
	if containsFold(settings.Policy.Deny, op) {
		return mcpPolicyDecision{
			Code:    "denied_by_policy",
			Message: fmt.Sprintf("operation %q is denied by agent.policy.deny", op),
			Hint:    "edit the agent.policy section in config.json to allow it",
		}
	}
	if op == "uninstall" && containsFold(settings.Policy.Protected, packageBaseName(pkg)) {
		return mcpPolicyDecision{
			Code:    "denied_by_policy",
			Message: fmt.Sprintf("package %q is protected by agent.policy.protected", packageBaseName(pkg)),
			Hint:    "remove it from agent.policy.protected (the human CLI is unaffected)",
		}
	}
	confirm := !(settings.AutoYes || settings.Policy.Mode == config.AgentPolicyModeAuto)
	return mcpPolicyDecision{Allowed: true, Confirm: confirm}
}

func containsFold(values []string, want string) bool {
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), want) {
			return true
		}
	}
	return false
}

// mcpActor renders the connecting client's name/version for audit rows.
func mcpActor(req *mcp.CallToolRequest) string {
	if req == nil || req.Session == nil {
		return ""
	}
	params := req.Session.InitializeParams()
	if params == nil || params.ClientInfo == nil {
		return ""
	}
	name, version := params.ClientInfo.Name, params.ClientInfo.Version
	switch {
	case name != "" && version != "":
		return name + "/" + version
	case name != "":
		return name
	default:
		return ""
	}
}

// mcpAuditContext tags the operation with source=mcp and the clientInfo actor,
// so core records who did what (roadmap §4.6.1).
func mcpAuditContext(ctx context.Context, req *mcp.CallToolRequest) context.Context {
	return engine.WithAudit(ctx, engine.AuditInfo{Source: "mcp", Actor: mcpActor(req)})
}

// mcpHeldError is the second gate behind protected: `glue hold` blocks removal
// on the MCP path even when policy allows it.
func mcpHeldError(pkg string) error {
	return &mcpToolError{
		Code:    "denied_by_policy",
		Message: fmt.Sprintf("package %q is held (version lock)", packageBaseName(pkg)),
		Hint:    "run glue unhold to allow removal",
	}
}

// mcpPackageHeld reports whether the package is version-locked (glue hold),
// which blocks removal on the MCP path. A variable so tests can stub the gate.
var mcpPackageHeld = func(eng *engine.Engine, pkg string) bool {
	return eng != nil && eng.IsPackageVersionLocked(packageBaseName(pkg))
}

// mcpTokenTag shortens a confirm token for audit rows (never log the token).
func mcpTokenTag(token string) string {
	token = strings.TrimSpace(token)
	if len(token) > 8 {
		return token[:8]
	}
	return token
}

// mcpRecordDecision writes the confirm/policy decision chain to the audit log
// and emits one stderr summary line (visible in the agent's transcript). Audit
// failures are surfaced as a warning rather than failing the operation.
func mcpRecordDecision(ctx context.Context, eng *engine.Engine, op, pkg, status string, details map[string]any) {
	fmt.Fprintf(os.Stderr, "[glue mcp] %s %s status=%s\n", op, pkg, status)
	if eng == nil {
		return
	}
	if err := eng.RecordAudit(ctx, op, pkg, "", status, details); err != nil {
		fmt.Fprintf(os.Stderr, "[glue mcp] audit warning: %v\n", err)
	}
}

// MCP write inputs.

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
