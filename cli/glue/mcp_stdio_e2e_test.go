package main

// End-to-end test of the MCP stdio server through the exact path an IDE client
// uses: spawn the built binary as a subprocess, talk newline-delimited JSON-RPC
// over stdin/stdout (mcp.CommandTransport), and assert the tool surface, the
// payload contract and the destructive confirm flow.
//
// It needs a built binary and is therefore opt-in:
//
//	go build -o glue-alpha.exe ./cli/glue
//	$env:GLUE_MCP_BIN = "$PWD\glue-alpha.exe"
//	go test ./cli/glue -run TestMCPStdio -count=1 -v
//
// The server runs against the dev data root (%USERPROFILE%\.glue-alpha for
// glue-alpha.exe) — the same root the IDE client configs use — and refuses to
// run against a real %USERPROFILE%\.glue. Set GLUE_MCP_ROOT to point it at
// another directory. Without GLUE_MCP_BIN the test skips, so the normal suite
// stays hermetic.

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// mcpE2EDevRoot is the data root the test drives: %USERPROFILE%\.glue-alpha,
// overridable with GLUE_MCP_ROOT.
func mcpE2EDevRoot(t *testing.T) string {
	t.Helper()
	if root := os.Getenv("GLUE_MCP_ROOT"); root != "" {
		return root
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("cannot resolve the user profile: %v", err)
	}
	return filepath.Join(home, ".glue-alpha")
}

func TestMCPStdio_clientFlow(t *testing.T) {
	bin := os.Getenv("GLUE_MCP_BIN")
	if bin == "" {
		t.Skip("set GLUE_MCP_BIN to a built glue binary to run the MCP stdio E2E")
	}
	if _, err := os.Stat(bin); err != nil {
		t.Fatalf("GLUE_MCP_BIN=%s is not usable: %v", bin, err)
	}
	abs, err := filepath.Abs(bin)
	if err != nil {
		t.Fatalf("resolve GLUE_MCP_BIN: %v", err)
	}
	root := mcpE2EDevRoot(t)
	if home, homeErr := os.UserHomeDir(); homeErr == nil {
		realRoot := filepath.Join(home, ".glue")
		if strings.EqualFold(filepath.Clean(root), filepath.Clean(realRoot)) {
			t.Fatalf("refusing to run the destructive flow against the real install root %s (use the dev build/root)", realRoot)
		}
	}
	if err := os.MkdirAll(root, 0755); err != nil {
		t.Fatalf("create data root %s: %v", root, err)
	}

	// A local (non-git) bucket the destructive tool can delete.
	bucketDir := filepath.Join(root, "buckets", "demo")
	if err := os.MkdirAll(filepath.Join(bucketDir, "bucket"), 0755); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := exec.Command(abs, "--root", root, "mcp")
	client := mcp.NewClient(&mcp.Implementation{Name: "glue-stdio-e2e", Version: "0.0.1"}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("connect to `%s --root <temp> mcp`: %v", bin, err)
	}

	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	names := make(map[string]bool, len(tools.Tools))
	for _, tool := range tools.Tools {
		names[tool.Name] = true
	}
	for _, want := range []string{
		"glue_search", "glue_list", "glue_info", "glue_depends", "glue_path_check",
		"glue_bucket_list", "glue_doctor",
		"glue_install", "glue_uninstall", "glue_update",
		"glue_bucket_add", "glue_bucket_update", "glue_bucket_remove", "glue_confirm",
	} {
		if !names[want] {
			t.Errorf("tool %s missing from tools/list (%d tools)", want, len(tools.Tools))
		}
	}

	// Read tool: payload must be the CLI schema (empty install → []).
	listRes, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "glue_list"})
	if err != nil {
		t.Fatalf("tools/call glue_list: %v", err)
	}
	if listRes.IsError {
		t.Fatalf("glue_list returned a tool error: %s", mcpContentText(listRes))
	}
	var listPayload struct {
		Packages []json.RawMessage `json:"packages"`
		Count    int               `json:"count"`
	}
	if err := decodeMCPStructured(listRes, &listPayload); err != nil {
		t.Fatalf("glue_list payload: %v", err)
	}
	if listPayload.Packages == nil {
		t.Errorf("glue_list packages must be [] (not null) over stdio: %s", mcpContentText(listRes))
	}

	// --root isolation: the shim dir must live inside the temp root.
	pathRes, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "glue_path_check"})
	if err != nil {
		t.Fatalf("tools/call glue_path_check: %v", err)
	}
	var pathPayload struct {
		BinDir string `json:"bin_dir"`
	}
	if err := decodeMCPStructured(pathRes, &pathPayload); err != nil {
		t.Fatalf("glue_path_check payload: %v", err)
	}
	if want := filepath.Join(root, "shims"); pathPayload.BinDir != want {
		t.Errorf("bin_dir = %q, want %q (--root must win over the exe-name data root)", pathPayload.BinDir, want)
	}

	// Destructive tool: pending token first, then glue_confirm executes.
	pending, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "glue_bucket_remove",
		Arguments: map[string]any{"name": "demo"},
	})
	if err != nil {
		t.Fatalf("tools/call glue_bucket_remove: %v", err)
	}
	if pending.IsError {
		t.Fatalf("glue_bucket_remove must be pending before confirmation: %s", mcpContentText(pending))
	}
	var pendingPayload struct {
		Status       string `json:"status"`
		ConfirmToken string `json:"confirm_token"`
		Action       string `json:"action"`
	}
	if err := decodeMCPStructured(pending, &pendingPayload); err != nil {
		t.Fatalf("pending payload: %v", err)
	}
	if pendingPayload.Status != "pending" || pendingPayload.ConfirmToken == "" || pendingPayload.Action != "bucket_remove" {
		t.Fatalf("pending payload = %+v", pendingPayload)
	}
	if _, err := os.Stat(bucketDir); err != nil {
		t.Fatalf("bucket must still exist before confirmation: %v", err)
	}

	done, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "glue_confirm",
		Arguments: map[string]any{"token": pendingPayload.ConfirmToken},
	})
	if err != nil {
		t.Fatalf("tools/call glue_confirm: %v", err)
	}
	if done.IsError {
		t.Fatalf("glue_confirm must remove the bucket: %s", mcpContentText(done))
	}
	var donePayload struct {
		Command string `json:"command"`
		OK      bool   `json:"ok"`
		Name    string `json:"name"`
	}
	if err := decodeMCPStructured(done, &donePayload); err != nil {
		t.Fatalf("confirm payload: %v", err)
	}
	if donePayload.Command != "bucket_remove" || !donePayload.OK || donePayload.Name != "demo" {
		t.Fatalf("confirm payload = %+v", donePayload)
	}
	if _, err := os.Stat(bucketDir); !os.IsNotExist(err) {
		t.Fatalf("bucket dir still present after confirm (err=%v)", err)
	}

	// Closing the session closes stdin: the server must exit 0, not 1.
	if err := session.Close(); err != nil {
		t.Fatalf("session close: %v", err)
	}
	if cmd.ProcessState == nil {
		t.Fatal("no process state after close (was the process waited on?)")
	}
	if !cmd.ProcessState.Success() {
		t.Fatalf("glue mcp exited %v, want 0 after the client closed stdin", cmd.ProcessState)
	}
}
