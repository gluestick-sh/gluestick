package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

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

// mcpProtectedOps are the destructive operations whose target name is checked
// against agent.policy.protected: packages for uninstall, buckets for
// bucket_remove.
var mcpProtectedOps = map[string]bool{
	"uninstall":     true,
	"bucket_remove": true,
}

// mcpProtectedNoun names the protected target in the denial message.
func mcpProtectedNoun(op string) string {
	if op == "bucket_remove" {
		return "bucket"
	}
	return "package"
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
	if mcpProtectedOps[op] && containsFold(settings.Policy.Protected, packageBaseName(pkg)) {
		return mcpPolicyDecision{
			Code:    "denied_by_policy",
			Message: fmt.Sprintf("%s %q is protected by agent.policy.protected", mcpProtectedNoun(op), packageBaseName(pkg)),
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
