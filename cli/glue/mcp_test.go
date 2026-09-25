package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/gluestick-sh/core/engine"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestMCPCommand_registered pins the CLI surface: glue mcp is a first-class
// command (roadmap §4.1).
func TestMCPCommand_registered(t *testing.T) {
	cmd, _, err := rootCmd.Find([]string{"mcp"})
	if err != nil {
		t.Fatalf("rootCmd.Find(mcp): %v", err)
	}
	if cmd != mcpCmd {
		t.Fatalf("Find(mcp) = %q, want mcpCmd", cmd.Name())
	}
}

// TestIsMCPShutdown pins the normal-shutdown classification: a client closing
// stdin (which the SDK reports as "server is closing: EOF") must exit 0, while a
// real startup/operation failure must still fail.
func TestIsMCPShutdown(t *testing.T) {
	shutdown := []error{
		nil,
		io.EOF,
		context.Canceled,
		context.DeadlineExceeded,
		mcp.ErrConnectionClosed,
		fmt.Errorf("connect: %w", io.EOF),
		errors.New("server is closing: EOF"),
		errors.New("client is closing"),
		errors.New("connection closed"),
	}
	for _, err := range shutdown {
		if !isMCPShutdown(err) {
			t.Errorf("isMCPShutdown(%v) = false, want true", err)
		}
	}

	failures := []error{
		errors.New("initialize engine: open C:\\x: access denied"),
		errors.New("tool glue_install: download failed"),
		errors.New("server connect failed"),
	}
	for _, err := range failures {
		if isMCPShutdown(err) {
			t.Errorf("isMCPShutdown(%v) = true, want false", err)
		}
	}
}

// TestMCPTools_listed verifies the read tools are registered with descriptions
// and inferred input schemas.
func TestMCPTools_listed(t *testing.T) {
	session := newMCPTestSession(t)
	res, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	want := map[string]bool{
		"glue_search":      false,
		"glue_list":        false,
		"glue_info":        false,
		"glue_depends":     false,
		"glue_path_check":  false,
		"glue_bucket_list": false,
		"glue_doctor":      false,
	}
	for _, tool := range res.Tools {
		if _, ok := want[tool.Name]; !ok {
			continue
		}
		want[tool.Name] = true
		if tool.Description == "" {
			t.Errorf("tool %s has no description", tool.Name)
		}
		if tool.InputSchema == nil {
			t.Errorf("tool %s has no input schema", tool.Name)
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("tool %s is not registered", name)
		}
	}
}

// TestMCPCall_doctor runs the readiness tool end to end over an in-memory
// transport: the payload must use the same report schema as the CLI --json.
func TestMCPCall_doctor(t *testing.T) {
	session := newMCPTestSession(t)
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "glue_doctor",
		Arguments: map[string]any{"offline": true},
	})
	if err != nil {
		t.Fatalf("CallTool(glue_doctor): %v", err)
	}
	if res.IsError {
		t.Fatalf("glue_doctor returned a tool error: %s", mcpContentText(res))
	}
	var payload struct {
		AgentReady    bool `json:"agentReady"`
		SchemaVersion int  `json:"schemaVersion"`
	}
	if err := decodeMCPStructured(res, &payload); err != nil {
		t.Fatalf("structuredContent is not the doctor report: %v", err)
	}
	if payload.SchemaVersion == 0 {
		t.Fatalf("doctor report missing schemaVersion: %s", res.StructuredContent)
	}
}

// TestMCPCall_listEmpty covers a non-doctor tool on an empty data root: the
// payload mirrors `glue list --json` and stays structured.
func TestMCPCall_listEmpty(t *testing.T) {
	session := newMCPTestSession(t)
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "glue_list"})
	if err != nil {
		t.Fatalf("CallTool(glue_list): %v", err)
	}
	if res.IsError {
		t.Fatalf("glue_list returned a tool error: %s", mcpContentText(res))
	}
	var payload struct {
		Packages []json.RawMessage `json:"packages"`
		Count    int               `json:"count"`
	}
	if err := decodeMCPStructured(res, &payload); err != nil {
		t.Fatalf("structuredContent is not the list payload: %v", err)
	}
	if payload.Count != 0 {
		t.Fatalf("count = %d, want 0 on a fresh data root", payload.Count)
	}
	if payload.Packages == nil {
		t.Fatalf("packages must be an empty array, not null: %s", mcpContentText(res))
	}
}

// TestMCPCall_pathCheckAndBuckets covers the local read tools on an empty root.
func TestMCPCall_pathCheckAndBuckets(t *testing.T) {
	session := newMCPTestSession(t)
	ctx := context.Background()

	pathRes, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "glue_path_check"})
	if err != nil {
		t.Fatalf("CallTool(glue_path_check): %v", err)
	}
	if pathRes.IsError {
		t.Fatalf("glue_path_check returned a tool error: %s", mcpContentText(pathRes))
	}
	var pathPayload struct {
		BinDir string `json:"bin_dir"`
		OK     bool   `json:"ok"`
	}
	if err := decodeMCPStructured(pathRes, &pathPayload); err != nil {
		t.Fatalf("path check payload: %v", err)
	}
	if pathPayload.BinDir == "" {
		t.Fatal("path check payload missing bin_dir")
	}

	bucketRes, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "glue_bucket_list"})
	if err != nil {
		t.Fatalf("CallTool(glue_bucket_list): %v", err)
	}
	if bucketRes.IsError {
		t.Fatalf("glue_bucket_list returned a tool error: %s", mcpContentText(bucketRes))
	}
	var bucketPayload struct {
		Count int `json:"count"`
	}
	if err := decodeMCPStructured(bucketRes, &bucketPayload); err != nil {
		t.Fatalf("bucket list payload: %v", err)
	}
	if bucketPayload.Count != 0 {
		t.Fatalf("bucket count = %d, want 0 on a fresh data root", bucketPayload.Count)
	}
}

// TestMCPCall_infoMissing surfaces a missing package as a tool error.
func TestMCPCall_infoMissing(t *testing.T) {
	session := newMCPTestSession(t)
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "glue_info",
		Arguments: map[string]any{"package": "definitely-not-installed"},
	})
	if err != nil {
		t.Fatalf("CallTool(glue_info): %v", err)
	}
	if !res.IsError {
		t.Fatalf("glue_info on a missing package must be a tool error: %s", mcpContentText(res))
	}
}

// newMCPTestSession connects a client to a server backed by a temp-root engine.
func newMCPTestSession(t *testing.T) *mcp.ClientSession {
	t.Helper()
	return newMCPTestSessionAt(t, t.TempDir())
}

// newMCPTestSessionAt is newMCPTestSession with an explicit data root, so tests
// can pre-write config.json (agent policy / auto_yes) before connecting.
func newMCPTestSessionAt(t *testing.T, root string) *mcp.ClientSession {
	t.Helper()
	eng, err := engine.NewEngine(&engine.EngineConfig{RootDir: root})
	if err != nil {
		t.Fatalf("initialize engine: %v", err)
	}
	t.Cleanup(func() { eng.Close() })

	ctx := context.Background()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	if _, err := newGlueMCPServer(eng, root).Connect(ctx, serverTransport, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "glue-test", Version: "0.0.0"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

// decodeMCPStructured re-marshals the SDK's structuredContent value and decodes
// it into v (the SDK exposes it as any, not json.RawMessage).
func decodeMCPStructured(res *mcp.CallToolResult, v any) error {
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, v)
}

// mcpContentText returns the first text block of a tool result ("" when none).
func mcpContentText(res *mcp.CallToolResult) string {
	for _, content := range res.Content {
		if text, ok := content.(*mcp.TextContent); ok {
			return text.Text
		}
	}
	return ""
}
