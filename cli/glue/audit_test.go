package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/gluestick-sh/core/engine"
)

// TestAuditList_jsonSourceFilter covers the CLI contract: glue audit list
// --json filters by source and returns source/actor per entry.
func TestAuditList_jsonSourceFilter(t *testing.T) {
	root := t.TempDir()
	setJSONTestFlags(t, root)

	eng, err := engine.NewEngine(&engine.EngineConfig{RootDir: root})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	_ = eng.RecordAudit(
		engine.WithAudit(context.Background(), engine.AuditInfo{Source: "mcp", Actor: "cline/3.7"}),
		"install", "nodejs", "", "pending", map[string]any{"phase": "confirm_required"})
	_ = eng.RecordAudit(context.Background(), "install", "git", "2.54.0", "success", nil)
	eng.Close()

	set := func(name, value string) {
		if err := auditListCmd.Flags().Set(name, value); err != nil {
			t.Fatalf("set --%s: %v", name, err)
		}
		t.Cleanup(func() { _ = auditListCmd.Flags().Set(name, "") })
	}
	set("source", "mcp")
	set("limit", "10")

	var runErr error
	out := captureStdoutToFile(t, func() {
		runErr = auditListCmd.RunE(auditListCmd, nil)
	})
	if runErr != nil {
		t.Fatalf("audit list: %v\n%s", runErr, out)
	}
	var payload struct {
		Entries []map[string]any `json:"entries"`
		Count   int              `json:"count"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if payload.Count != 1 || len(payload.Entries) != 1 {
		t.Fatalf("entries = %+v, want one mcp row", payload.Entries)
	}
	if payload.Entries[0]["source"] != "mcp" || payload.Entries[0]["actor"] != "cline/3.7" {
		t.Fatalf("entry = %+v, want source=mcp actor=cline/3.7", payload.Entries[0])
	}
}

// TestAuditVerify_json verifies the hash-chain command on a clean log.
func TestAuditVerify_json(t *testing.T) {
	root := t.TempDir()
	setJSONTestFlags(t, root)

	eng, err := engine.NewEngine(&engine.EngineConfig{RootDir: root})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	_ = eng.RecordAudit(context.Background(), "install", "git", "2.54.0", "success", nil)
	_ = eng.RecordAudit(context.Background(), "install", "nodejs", "1.0.0", "success", nil)
	eng.Close()

	var runErr error
	out := captureStdoutToFile(t, func() {
		runErr = auditVerifyCmd.RunE(auditVerifyCmd, nil)
	})
	if runErr != nil {
		t.Fatalf("audit verify: %v\n%s", runErr, out)
	}
	var payload engine.AuditVerifyResult
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if !payload.OK || payload.Entries != 2 {
		t.Fatalf("verify payload = %+v, want ok with 2 entries", payload)
	}
}

// TestAuditList_jsonlFlag reads the append-only JSONL instead of SQLite.
func TestAuditList_jsonlFlag(t *testing.T) {
	root := t.TempDir()
	setJSONTestFlags(t, root)

	eng, err := engine.NewEngine(&engine.EngineConfig{RootDir: root})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	_ = eng.RecordAudit(
		engine.WithAudit(context.Background(), engine.AuditInfo{Source: "mcp", Actor: "cline/3.7"}),
		"install", "nodejs", "", "pending", map[string]any{"phase": "confirm_required"})
	eng.Close()

	set := func(name, value string) {
		if err := auditListCmd.Flags().Set(name, value); err != nil {
			t.Fatalf("set --%s: %v", name, err)
		}
		t.Cleanup(func() { _ = auditListCmd.Flags().Set(name, "") })
	}
	set("source", "mcp")
	set("limit", "10")
	set("jsonl", "true")

	var runErr error
	out := captureStdoutToFile(t, func() {
		runErr = auditListCmd.RunE(auditListCmd, nil)
	})
	if runErr != nil {
		t.Fatalf("audit list --jsonl: %v\n%s", runErr, out)
	}
	var payload struct {
		Format  string           `json:"format"`
		Count   int              `json:"count"`
		Entries []map[string]any `json:"entries"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if payload.Format != "jsonl" || payload.Count != 1 {
		t.Fatalf("payload = %+v, want format=jsonl count=1", payload)
	}
	if payload.Entries[0]["actor"] != "cline/3.7" {
		t.Fatalf("entry = %+v, want actor cline/3.7", payload.Entries[0])
	}
}
