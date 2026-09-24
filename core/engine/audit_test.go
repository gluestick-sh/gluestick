package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestRecordAudit_sqliteAndJSONL pins the §4.6.1 contract: every audited
// operation lands in activity_log with source/actor and in logs/audit.jsonl.
func TestRecordAudit_sqliteAndJSONL(t *testing.T) {
	root := t.TempDir()
	eng, err := NewEngine(&EngineConfig{RootDir: root})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	defer eng.Close()

	cliCtx := WithAudit(context.Background(), AuditInfo{Source: "cli"})
	mcpCtx := WithAudit(context.Background(), AuditInfo{Source: "mcp", Actor: "cline/3.7"})
	if err := eng.RecordAudit(cliCtx, "install", "git", "2.54.0", "success", map[string]any{"via": "cli"}); err != nil {
		t.Fatalf("RecordAudit(cli): %v", err)
	}
	if err := eng.RecordAudit(mcpCtx, "install", "nodejs", "", "pending", map[string]any{"phase": "confirm_required"}); err != nil {
		t.Fatalf("RecordAudit(mcp): %v", err)
	}

	entries, err := eng.QueryAuditLog("mcp", "", "", 10)
	if err != nil {
		t.Fatalf("QueryAuditLog: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("audit entries = %d, want 1 mcp row", len(entries))
	}
	if entries[0]["source"] != "mcp" || entries[0]["actor"] != "cline/3.7" {
		t.Fatalf("entry = %+v, want source=mcp actor=cline/3.7", entries[0])
	}

	data, err := os.ReadFile(filepath.Join(root, "logs", "audit.jsonl"))
	if err != nil {
		t.Fatalf("read audit.jsonl: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("audit.jsonl lines = %d, want 2:\n%s", len(lines), data)
	}
	if !strings.Contains(lines[1], `"source":"mcp"`) || !strings.Contains(lines[1], `"cline/3.7"`) {
		t.Fatalf("audit.jsonl mcp line = %s", lines[1])
	}
	if result, err := eng.VerifyAuditLog(); err != nil || !result.OK || result.Entries != 2 {
		t.Fatalf("VerifyAuditLog = %+v (err=%v), want OK with 2 entries", result, err)
	}
}

// TestVerifyAuditLog_detectsTamper verifies the hash chain catches an edited
// audit entry (the JSONL is the tamper-evident copy of activity_log).
func TestVerifyAuditLog_detectsTamper(t *testing.T) {
	root := t.TempDir()
	eng, err := NewEngine(&EngineConfig{RootDir: root})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	defer eng.Close()
	_ = eng.RecordAudit(context.Background(), "install", "git", "2.54.0", "success", nil)
	_ = eng.RecordAudit(context.Background(), "install", "nodejs", "1.0.0", "success", nil)

	path := filepath.Join(root, "logs", "audit.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read audit.jsonl: %v", err)
	}
	lines := strings.Split(string(data), "\n")
	lines[0] = strings.Replace(lines[0], `"package":"git"`, `"package":"got"`, 1)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0600); err != nil {
		t.Fatalf("write tampered audit.jsonl: %v", err)
	}

	result, err := eng.VerifyAuditLog()
	if err != nil {
		t.Fatalf("VerifyAuditLog: %v", err)
	}
	if result.OK || result.BrokenAt != 1 {
		t.Fatalf("VerifyAuditLog = %+v, want broken at entry 1", result)
	}
}

// TestAuditConcurrentAppends_chainIntact verifies the file lock serializes
// concurrent appends and keeps the hash chain valid.
func TestAuditConcurrentAppends_chainIntact(t *testing.T) {
	root := t.TempDir()
	eng, err := NewEngine(&EngineConfig{RootDir: root})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	defer eng.Close()

	var wg sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < 5; i++ {
				_ = eng.RecordAudit(context.Background(), "install",
					fmt.Sprintf("pkg-%d-%d", worker, i), "1.0.0", "success", nil)
			}
		}(worker)
	}
	wg.Wait()

	result, err := eng.VerifyAuditLog()
	if err != nil || !result.OK || result.Entries != 20 {
		t.Fatalf("VerifyAuditLog = %+v (err=%v), want OK with 20 entries", result, err)
	}
}

// continues across segments and QueryAuditJSONL reads them all.
func TestAuditRotation_chainAcrossSegments(t *testing.T) {
	oldMax := auditLogMaxBytes
	auditLogMaxBytes = 200
	t.Cleanup(func() { auditLogMaxBytes = oldMax })

	root := t.TempDir()
	eng, err := NewEngine(&EngineConfig{RootDir: root})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	defer eng.Close()
	for i := 0; i < 3; i++ {
		if err := eng.RecordAudit(context.Background(), "install", fmt.Sprintf("pkg%d", i), "1.0.0", "success", nil); err != nil {
			t.Fatalf("RecordAudit(%d): %v", i, err)
		}
	}

	segments, err := filepath.Glob(filepath.Join(root, "logs", "audit-*.jsonl"))
	if err != nil {
		t.Fatalf("glob segments: %v", err)
	}
	if len(segments) == 0 {
		t.Fatal("expected at least one rotated audit segment")
	}

	result, err := eng.VerifyAuditLog()
	if err != nil || !result.OK || result.Entries != 3 {
		t.Fatalf("VerifyAuditLog = %+v (err=%v), want OK with 3 entries", result, err)
	}
	entries, err := eng.QueryAuditJSONL("", "", "", 10)
	if err != nil || len(entries) != 3 {
		t.Fatalf("QueryAuditJSONL = %d entries (err=%v), want 3", len(entries), err)
	}
}
