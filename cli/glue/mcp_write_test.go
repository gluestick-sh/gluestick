package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gluestick-sh/core/config"
	"github.com/gluestick-sh/core/engine"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// writeMCPAgentConfig writes a config.json with the given agent section.
func writeMCPAgentConfig(t *testing.T, root, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, "config.json"), []byte(body), 0644); err != nil {
		t.Fatalf("write config.json: %v", err)
	}
}

// TestDecideMCPPolicy pins the §4.6.2 rules: deny/protected always win over
// auto_yes, and the default mode requires confirmation.
func TestDecideMCPPolicy(t *testing.T) {
	defaults := config.DefaultAgentSettings()

	if d := decideMCPPolicy(defaults, "install", "nodejs"); !d.Allowed || !d.Confirm {
		t.Fatalf("defaults: %+v, want allowed+confirm", d)
	}

	autoYes := defaults
	autoYes.AutoYes = true
	if d := decideMCPPolicy(autoYes, "uninstall", "nodejs"); !d.Allowed || d.Confirm {
		t.Fatalf("auto_yes: %+v, want allowed without confirm", d)
	}

	denied := defaults
	denied.Policy.Deny = []string{"uninstall"}
	if d := decideMCPPolicy(denied, "uninstall", "nodejs"); d.Allowed || d.Code != "denied_by_policy" {
		t.Fatalf("deny: %+v, want denied_by_policy", d)
	}
	// auto_yes must not bypass the deny list.
	denied.AutoYes = true
	if d := decideMCPPolicy(denied, "uninstall", "nodejs"); d.Allowed {
		t.Fatalf("deny + auto_yes: %+v, want still denied", d)
	}

	protected := defaults
	protected.Policy.Protected = []string{"nodejs"}
	if d := decideMCPPolicy(protected, "uninstall", "main/nodejs"); d.Allowed || d.Code != "denied_by_policy" {
		t.Fatalf("protected uninstall: %+v, want denied_by_policy", d)
	}
	if d := decideMCPPolicy(protected, "install", "nodejs"); !d.Allowed {
		t.Fatalf("protected install: %+v, want allowed", d)
	}

	// A protected bucket cannot be removed by an agent, even with auto_yes.
	protectedBucket := defaults
	protectedBucket.Policy.Protected = []string{"main"}
	if d := decideMCPPolicy(protectedBucket, "bucket_remove", "main"); d.Allowed || d.Code != "denied_by_policy" {
		t.Fatalf("protected bucket_remove: %+v, want denied_by_policy", d)
	}
	if d := decideMCPPolicy(protectedBucket, "bucket_add", "main"); !d.Allowed {
		t.Fatalf("protected bucket_add: %+v, want allowed", d)
	}
	protectedBucket.AutoYes = true
	if d := decideMCPPolicy(protectedBucket, "bucket_remove", "main"); d.Allowed {
		t.Fatalf("protected bucket_remove + auto_yes: %+v, want still denied", d)
	}

	deniedRemove := defaults
	deniedRemove.Policy.Deny = []string{"bucket_remove"}
	if d := decideMCPPolicy(deniedRemove, "bucket_remove", "demo"); d.Allowed || d.Code != "denied_by_policy" {
		t.Fatalf("deny bucket_remove: %+v, want denied_by_policy", d)
	}
}

// TestMCPInstallPending verifies the default flow never installs immediately:
// the tool returns a pending confirm token instead.
func TestMCPInstallPending(t *testing.T) {
	root := t.TempDir()
	session := newMCPTestSessionAt(t, root)

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "glue_install",
		Arguments: map[string]any{"package": "nodejs"},
	})
	if err != nil {
		t.Fatalf("CallTool(glue_install): %v", err)
	}
	if res.IsError {
		t.Fatalf("glue_install must not error before confirmation: %s", mcpContentText(res))
	}
	var payload struct {
		Status       string `json:"status"`
		ConfirmToken string `json:"confirm_token"`
		ExpiresIn    int    `json:"expires_in"`
		Action       string `json:"action"`
		Package      string `json:"package"`
	}
	if err := decodeMCPStructured(res, &payload); err != nil {
		t.Fatalf("pending payload: %v", err)
	}
	if payload.Status != "pending" || payload.ConfirmToken == "" || payload.Action != "install" {
		t.Fatalf("pending payload = %+v", payload)
	}
	if payload.ExpiresIn != int(mcpConfirmTTL.Seconds()) {
		t.Fatalf("expires_in = %d, want %d", payload.ExpiresIn, int(mcpConfirmTTL.Seconds()))
	}
}

// TestMCPConfirm_executesOnce drives the full two-step flow with an uninstall
// that fails fast (missing package): the token executes, then is single-use.
func TestMCPConfirm_executesOnce(t *testing.T) {
	root := t.TempDir()
	session := newMCPTestSessionAt(t, root)
	ctx := context.Background()

	pending, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "glue_uninstall",
		Arguments: map[string]any{"package": "definitely-not-installed"},
	})
	if err != nil {
		t.Fatalf("CallTool(glue_uninstall): %v", err)
	}
	var payload struct {
		ConfirmToken string `json:"confirm_token"`
	}
	if err := decodeMCPStructured(pending, &payload); err != nil {
		t.Fatalf("pending payload: %v", err)
	}
	if payload.ConfirmToken == "" {
		t.Fatalf("pending payload has no confirm_token: %s", mcpContentText(pending))
	}

	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "glue_confirm",
		Arguments: map[string]any{"token": payload.ConfirmToken},
	})
	if err != nil {
		t.Fatalf("CallTool(glue_confirm): %v", err)
	}
	if !res.IsError {
		t.Fatalf("confirming a missing package must fail: %s", mcpContentText(res))
	}

	again, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "glue_confirm",
		Arguments: map[string]any{"token": payload.ConfirmToken},
	})
	if err != nil {
		t.Fatalf("CallTool(glue_confirm again): %v", err)
	}
	if !again.IsError || !strings.Contains(mcpContentText(again), "invalid_confirm_token") {
		t.Fatalf("reused token must be rejected: %s", mcpContentText(again))
	}
}

// TestMCPPolicyDenyBlocksWrite verifies agent.policy.deny hard-blocks a write
// tool before any confirm token is issued.
func TestMCPPolicyDenyBlocksWrite(t *testing.T) {
	root := t.TempDir()
	writeMCPAgentConfig(t, root, `{"agent":{"policy":{"deny":["uninstall"]}}}`)
	session := newMCPTestSessionAt(t, root)

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "glue_uninstall",
		Arguments: map[string]any{"package": "nodejs"},
	})
	if err != nil {
		t.Fatalf("CallTool(glue_uninstall): %v", err)
	}
	if !res.IsError || !strings.Contains(mcpContentText(res), "denied_by_policy") {
		t.Fatalf("deny policy not enforced: %s", mcpContentText(res))
	}
}

// TestMCPPolicyProtectedBlocksUninstall verifies protected packages keep the
// agent from uninstalling while install stays available.
func TestMCPPolicyProtectedBlocksUninstall(t *testing.T) {
	root := t.TempDir()
	writeMCPAgentConfig(t, root, `{"agent":{"policy":{"protected":["nodejs"]}}}`)
	session := newMCPTestSessionAt(t, root)
	ctx := context.Background()

	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "glue_uninstall",
		Arguments: map[string]any{"package": "nodejs"},
	})
	if err != nil {
		t.Fatalf("CallTool(glue_uninstall): %v", err)
	}
	if !res.IsError || !strings.Contains(mcpContentText(res), "denied_by_policy") {
		t.Fatalf("protected package not enforced: %s", mcpContentText(res))
	}

	install, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "glue_install",
		Arguments: map[string]any{"package": "nodejs"},
	})
	if err != nil {
		t.Fatalf("CallTool(glue_install): %v", err)
	}
	if install.IsError {
		t.Fatalf("install must stay allowed for a protected package: %s", mcpContentText(install))
	}
}

// TestMCPAutoYesExecutesWrite verifies agent.auto_yes skips the confirm token
// and runs the operation (which then fails fast for a missing package).
func TestMCPAutoYesExecutesWrite(t *testing.T) {
	root := t.TempDir()
	writeMCPAgentConfig(t, root, `{"agent":{"auto_yes":true}}`)
	session := newMCPTestSessionAt(t, root)

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "glue_uninstall",
		Arguments: map[string]any{"package": "definitely-not-installed"},
	})
	if err != nil {
		t.Fatalf("CallTool(glue_uninstall): %v", err)
	}
	if !res.IsError {
		t.Fatalf("auto_yes must execute instead of returning pending: %s", mcpContentText(res))
	}
	if strings.Contains(mcpContentText(res), `"status":"pending"`) {
		t.Fatalf("auto_yes must not return a pending token: %s", mcpContentText(res))
	}
}

// TestMCPWriteTools_listed pins the write surface: install/uninstall plus the
// confirmation endpoint are registered.
func TestMCPWriteTools_listed(t *testing.T) {
	session := newMCPTestSession(t)
	res, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	want := map[string]bool{
		"glue_install":       false,
		"glue_uninstall":     false,
		"glue_update":        false,
		"glue_bucket_add":    false,
		"glue_bucket_update": false,
		"glue_bucket_remove": false,
		"glue_confirm":       false,
	}
	for _, tool := range res.Tools {
		if _, ok := want[tool.Name]; ok {
			want[tool.Name] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("write tool %s is not registered", name)
		}
	}
}

// TestMCPReadToolCallAudited verifies read-only calls are recorded as
// tool_call audit rows with source=mcp and the client actor.
func TestMCPReadToolCallAudited(t *testing.T) {
	root := t.TempDir()
	session := newMCPTestSessionAt(t, root)
	if _, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "glue_list"}); err != nil {
		t.Fatalf("CallTool(glue_list): %v", err)
	}

	eng, err := engine.NewEngine(&engine.EngineConfig{RootDir: root})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	defer eng.Close()
	entries, err := eng.QueryAuditLog("mcp", "", "", 10)
	if err != nil {
		t.Fatalf("QueryAuditLog: %v", err)
	}
	found := false
	for _, entry := range entries {
		if entry["operation"] != "tool_call" {
			continue
		}
		details, _ := entry["details"].(map[string]any)
		if details["tool"] != "glue_list" {
			continue
		}
		if entry["actor"] != "glue-test/0.0.0" {
			t.Fatalf("tool_call actor = %v, want glue-test/0.0.0", entry["actor"])
		}
		found = true
	}
	if !found {
		t.Fatalf("no tool_call audit row for glue_list: %+v", entries)
	}
}

// TestMCPConfirmSurvivesRestart proves tokens are persisted: a token issued by
// one server instance is confirmable by a fresh instance on the same data root.
func TestMCPConfirmSurvivesRestart(t *testing.T) {
	root := t.TempDir()
	session1 := newMCPTestSessionAt(t, root)
	pending, err := session1.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "glue_uninstall",
		Arguments: map[string]any{"package": "definitely-not-installed"},
	})
	if err != nil {
		t.Fatalf("CallTool(glue_uninstall): %v", err)
	}
	var payload struct {
		ConfirmToken string `json:"confirm_token"`
	}
	if err := decodeMCPStructured(pending, &payload); err != nil {
		t.Fatalf("pending payload: %v", err)
	}
	if payload.ConfirmToken == "" {
		t.Fatal("no confirm token issued")
	}
	_ = session1.Close() // simulate server restart

	session2 := newMCPTestSessionAt(t, root)
	res, err := session2.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "glue_confirm",
		Arguments: map[string]any{"token": payload.ConfirmToken},
	})
	if err != nil {
		t.Fatalf("CallTool(glue_confirm): %v", err)
	}
	if strings.Contains(mcpContentText(res), "invalid_confirm_token") {
		t.Fatalf("token did not survive restart: %s", mcpContentText(res))
	}
}

// TestMCPConsumeConfirm_expired rejects an expired persisted token.
func TestMCPConsumeConfirm_expired(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(mcpPendingDir(root), 0700); err != nil {
		t.Fatalf("mkdir pending: %v", err)
	}
	action := mcpPendingAction{Op: "install", Package: "nodejs", Expires: time.Now().Add(-time.Minute)}
	data, err := json.Marshal(action)
	if err != nil {
		t.Fatalf("marshal action: %v", err)
	}
	if err := os.WriteFile(mcpPendingPath(root, "deadbeefdeadbeef"), data, 0600); err != nil {
		t.Fatalf("write pending token: %v", err)
	}
	if _, err := mcpConsumeConfirm(root, "deadbeefdeadbeef"); err == nil {
		t.Fatal("expired token was accepted")
	}
}

// TestMCPNewWriteToolsPending verifies update/bucket tools also default to the
// confirm-token flow (no network before confirmation).
func TestMCPNewWriteToolsPending(t *testing.T) {
	session := newMCPTestSession(t)
	ctx := context.Background()
	cases := []struct {
		name string
		args map[string]any
	}{
		{"glue_update", map[string]any{"package": "nodejs"}},
		{"glue_bucket_add", map[string]any{"name": "main"}},
		{"glue_bucket_update", map[string]any{}},
		{"glue_bucket_remove", map[string]any{"name": "main"}},
	}
	for _, tc := range cases {
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: tc.name, Arguments: tc.args})
		if err != nil {
			t.Fatalf("CallTool(%s): %v", tc.name, err)
		}
		if res.IsError {
			t.Fatalf("%s must be pending, got error: %s", tc.name, mcpContentText(res))
		}
		var payload struct {
			Status       string `json:"status"`
			ConfirmToken string `json:"confirm_token"`
		}
		if err := decodeMCPStructured(res, &payload); err != nil {
			t.Fatalf("%s payload: %v", tc.name, err)
		}
		if payload.Status != "pending" || payload.ConfirmToken == "" {
			t.Fatalf("%s payload = %+v, want pending token", tc.name, payload)
		}
	}
}

// TestMCPAuditRecordsPendingDecision verifies the confirm_required decision is
// written to the audit trail with source=mcp and the client actor.
func TestMCPAuditRecordsPendingDecision(t *testing.T) {
	root := t.TempDir()
	session := newMCPTestSessionAt(t, root)
	if _, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "glue_install",
		Arguments: map[string]any{"package": "nodejs"},
	}); err != nil {
		t.Fatalf("CallTool(glue_install): %v", err)
	}

	eng, err := engine.NewEngine(&engine.EngineConfig{RootDir: root})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	defer eng.Close()
	entries, err := eng.QueryAuditLog("mcp", "nodejs", "", 10)
	if err != nil {
		t.Fatalf("QueryAuditLog: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("audit entries = %d, want 1 pending decision", len(entries))
	}
	if entries[0]["status"] != "pending" || entries[0]["actor"] != "glue-test/0.0.0" {
		t.Fatalf("audit entry = %+v, want pending + client actor", entries[0])
	}
}

// TestMCPHoldBlocksUninstall verifies the hold (version lock) gate: a held
// package cannot be uninstalled through MCP even when policy allows it.
func TestMCPHoldBlocksUninstall(t *testing.T) {
	oldHeld := mcpPackageHeld
	mcpPackageHeld = func(*engine.Engine, string) bool { return true }
	t.Cleanup(func() { mcpPackageHeld = oldHeld })

	_, err := runMCPUninstallCall(context.Background(), &engine.Engine{}, t.TempDir(), mcpUninstallInput{Package: "nodejs"})
	if err == nil || !strings.Contains(err.Error(), "held") {
		t.Fatalf("held package must be blocked, got err=%v", err)
	}
}

// TestMCPBucketRemove_confirmExecutes drives the destructive bucket flow end to
// end: the tool only issues a token, glue_confirm deletes the local checkout,
// and the audit trail carries source=mcp with the client actor.
func TestMCPBucketRemove_confirmExecutes(t *testing.T) {
	root := t.TempDir()
	bucketDir := filepath.Join(root, "buckets", "demo")
	if err := os.MkdirAll(filepath.Join(bucketDir, "bucket"), 0755); err != nil {
		t.Fatal(err)
	}
	session := newMCPTestSessionAt(t, root)
	ctx := context.Background()

	pending, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "glue_bucket_remove",
		Arguments: map[string]any{"name": "demo"},
	})
	if err != nil {
		t.Fatalf("CallTool(glue_bucket_remove): %v", err)
	}
	if pending.IsError {
		t.Fatalf("bucket_remove must be pending first: %s", mcpContentText(pending))
	}
	var payload struct {
		Status       string `json:"status"`
		ConfirmToken string `json:"confirm_token"`
		Action       string `json:"action"`
	}
	if err := decodeMCPStructured(pending, &payload); err != nil {
		t.Fatalf("pending payload: %v", err)
	}
	if payload.Status != "pending" || payload.ConfirmToken == "" || payload.Action != "bucket_remove" {
		t.Fatalf("pending payload = %+v", payload)
	}
	if _, err := os.Stat(bucketDir); err != nil {
		t.Fatalf("bucket must still exist before confirmation: %v", err)
	}

	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "glue_confirm",
		Arguments: map[string]any{"token": payload.ConfirmToken},
	})
	if err != nil {
		t.Fatalf("CallTool(glue_confirm): %v", err)
	}
	if res.IsError {
		t.Fatalf("confirm must remove the bucket: %s", mcpContentText(res))
	}
	var done struct {
		Command string `json:"command"`
		OK      bool   `json:"ok"`
		Results []struct {
			Ref string `json:"ref"`
		} `json:"results"`
	}
	if err := decodeMCPStructured(res, &done); err != nil {
		t.Fatalf("confirm payload: %v", err)
	}
	if done.Command != "bucket_remove" || !done.OK || len(done.Results) != 1 || done.Results[0].Ref != "demo" {
		t.Fatalf("confirm payload = %+v, want bucket_remove ok with results[0].ref=demo", done)
	}
	if _, err := os.Stat(bucketDir); !os.IsNotExist(err) {
		t.Fatalf("bucket dir still present after confirm (err=%v)", err)
	}

	eng, err := engine.NewEngine(&engine.EngineConfig{RootDir: root})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	defer eng.Close()
	entries, err := eng.QueryAuditLog("mcp", "demo", "", 10)
	if err != nil {
		t.Fatalf("QueryAuditLog: %v", err)
	}
	var completion map[string]any
	for _, entry := range entries {
		if entry["operation"] == "bucket_remove" && entry["status"] == "success" {
			completion = entry
			break
		}
	}
	if completion == nil {
		t.Fatalf("no bucket_remove success audit row: %+v", entries)
	}
	if completion["actor"] != "glue-test/0.0.0" {
		t.Fatalf("audit actor = %v, want glue-test/0.0.0", completion["actor"])
	}
}

// TestMCPBucketRemove_deniedByPolicy verifies agent.policy.deny hard-blocks the
// destructive tool even with auto_yes: no token is issued and the bucket stays.
func TestMCPBucketRemove_deniedByPolicy(t *testing.T) {
	root := t.TempDir()
	bucketDir := filepath.Join(root, "buckets", "demo")
	if err := os.MkdirAll(filepath.Join(bucketDir, "bucket"), 0755); err != nil {
		t.Fatal(err)
	}
	writeMCPAgentConfig(t, root, `{"agent":{"auto_yes":true,"policy":{"deny":["bucket_remove"]}}}`)
	session := newMCPTestSessionAt(t, root)

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "glue_bucket_remove",
		Arguments: map[string]any{"name": "demo"},
	})
	if err != nil {
		t.Fatalf("CallTool(glue_bucket_remove): %v", err)
	}
	if !res.IsError {
		t.Fatalf("deny must block bucket_remove: %s", mcpContentText(res))
	}
	if text := mcpContentText(res); !strings.Contains(text, "denied_by_policy") {
		t.Fatalf("error payload = %s, want denied_by_policy", text)
	}
	if _, err := os.Stat(bucketDir); err != nil {
		t.Fatalf("denied removal must leave the bucket on disk: %v", err)
	}
}
