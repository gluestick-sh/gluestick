package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gluestick-sh/core/config"
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

// TestAuditRotation_chainAcrossSegments covers the configurable thresholds in
// config.json ("audit.max_bytes"): rotation keeps the chain intact and
// QueryAuditJSONL reads every segment.
func TestAuditRotation_chainAcrossSegments(t *testing.T) {
	root := t.TempDir()
	eng, err := NewEngine(&EngineConfig{RootDir: root})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	defer eng.Close()
	writeAuditSettings(t, root, config.AuditSettings{MaxBytes: 200})

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
	if result.Anchor != "" {
		t.Fatalf("anchor = %q, want empty while no segment was pruned", result.Anchor)
	}
	entries, err := eng.QueryAuditJSONL("", "", "", 10)
	if err != nil || len(entries) != 3 {
		t.Fatalf("QueryAuditJSONL = %d entries (err=%v), want 3", len(entries), err)
	}
}

// TestAuditRotation_pruneKeepsVerifyGreen pins the retention contract: pruning
// keeps only audit.keep_segments segments and the anchor sidecar lets
// `glue audit verify` keep walking the surviving chain, while a hand-deleted
// segment is still reported as a break.
func TestAuditRotation_pruneKeepsVerifyGreen(t *testing.T) {
	root := t.TempDir()
	eng, err := NewEngine(&EngineConfig{RootDir: root})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	defer eng.Close()
	writeAuditSettings(t, root, config.AuditSettings{MaxBytes: 200, KeepSegments: 2})

	for i := 0; i < 12; i++ {
		if err := eng.RecordAudit(context.Background(), "install", fmt.Sprintf("pkg%d", i), "1.0.0", "success", nil); err != nil {
			t.Fatalf("RecordAudit(%d): %v", i, err)
		}
	}

	segments, err := filepath.Glob(filepath.Join(root, "logs", "audit-*.jsonl"))
	if err != nil {
		t.Fatalf("glob segments: %v", err)
	}
	if len(segments) != 2 {
		t.Fatalf("segments = %d, want 2 after pruning", len(segments))
	}
	if _, err := os.Stat(filepath.Join(root, "logs", "audit.anchor")); err != nil {
		t.Fatalf("stat audit.anchor: %v", err)
	}

	result, err := eng.VerifyAuditLog()
	if err != nil || !result.OK || result.Anchor == "" {
		t.Fatalf("VerifyAuditLog = %+v (err=%v), want OK anchored at the pruned chain head", result, err)
	}

	// Deleting a surviving segment by hand leaves a gap the anchor does not
	// cover, so verification must fail instead of silently passing.
	if err := os.Remove(segments[0]); err != nil {
		t.Fatalf("remove segment: %v", err)
	}
	result, err = eng.VerifyAuditLog()
	if err != nil {
		t.Fatalf("VerifyAuditLog after delete: %v", err)
	}
	if result.OK {
		t.Fatalf("VerifyAuditLog = %+v, want broken after a manual segment delete", result)
	}
}

// TestAuditRotation_disabledByConfig pins the negative sentinel: with
// audit.max_bytes < 0 the active file grows without rotating.
func TestAuditRotation_disabledByConfig(t *testing.T) {
	root := t.TempDir()
	eng, err := NewEngine(&EngineConfig{RootDir: root})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	defer eng.Close()
	writeAuditSettings(t, root, config.AuditSettings{MaxBytes: config.AuditRotationDisabled})

	for i := 0; i < 5; i++ {
		details := map[string]any{"filler": strings.Repeat("x", 500)}
		if err := eng.RecordAudit(context.Background(), "install", fmt.Sprintf("pkg%d", i), "1.0.0", "success", details); err != nil {
			t.Fatalf("RecordAudit(%d): %v", i, err)
		}
	}

	segments, err := filepath.Glob(filepath.Join(root, "logs", "audit-*.jsonl"))
	if err != nil {
		t.Fatalf("glob segments: %v", err)
	}
	if len(segments) != 0 {
		t.Fatalf("segments = %d, want none while rotation is disabled", len(segments))
	}
	if result, err := eng.VerifyAuditLog(); err != nil || !result.OK || result.Entries != 5 {
		t.Fatalf("VerifyAuditLog = %+v (err=%v), want OK with 5 entries", result, err)
	}
}

// TestAuditRotation_keepAllSegments pins audit.keep_segments < 0: rotation still
// happens, but nothing is pruned.
func TestAuditRotation_keepAllSegments(t *testing.T) {
	root := t.TempDir()
	eng, err := NewEngine(&EngineConfig{RootDir: root})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	defer eng.Close()
	writeAuditSettings(t, root, config.AuditSettings{MaxBytes: 200, KeepSegments: config.AuditKeepAllSegments})

	for i := 0; i < 6; i++ {
		if err := eng.RecordAudit(context.Background(), "install", fmt.Sprintf("pkg%d", i), "1.0.0", "success", nil); err != nil {
			t.Fatalf("RecordAudit(%d): %v", i, err)
		}
	}

	segments, err := filepath.Glob(filepath.Join(root, "logs", "audit-*.jsonl"))
	if err != nil {
		t.Fatalf("glob segments: %v", err)
	}
	if len(segments) != 5 {
		t.Fatalf("segments = %d, want all 5 rotated segments retained", len(segments))
	}
	if _, err := os.Stat(filepath.Join(root, "logs", "audit.anchor")); !os.IsNotExist(err) {
		t.Fatalf("audit.anchor err = %v, want no anchor while nothing is pruned", err)
	}
	if result, err := eng.VerifyAuditLog(); err != nil || !result.OK || result.Entries != 6 {
		t.Fatalf("VerifyAuditLog = %+v (err=%v), want OK with 6 entries", result, err)
	}
}

// TestAuditVerifyHead_tracksNewestEntry pins the external-anchor contract: the
// verified head is the hash of the newest entry and moves when the log grows.
func TestAuditVerifyHead_tracksNewestEntry(t *testing.T) {
	root := t.TempDir()
	eng, err := NewEngine(&EngineConfig{RootDir: root})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	defer eng.Close()

	_ = eng.RecordAudit(context.Background(), "install", "git", "2.54.0", "success", nil)
	result, err := eng.VerifyAuditLog()
	if err != nil {
		t.Fatalf("VerifyAuditLog: %v", err)
	}
	want, err := lastAuditHash(auditLogPath(root))
	if err != nil {
		t.Fatalf("lastAuditHash: %v", err)
	}
	if result.Head == "" || result.Head != want {
		t.Fatalf("head = %q, want the newest entry hash %q", result.Head, want)
	}

	_ = eng.RecordAudit(context.Background(), "install", "nodejs", "1.0.0", "success", nil)
	next, err := eng.VerifyAuditLog()
	if err != nil {
		t.Fatalf("VerifyAuditLog: %v", err)
	}
	if next.Head == result.Head {
		t.Fatalf("head did not move after an append (%q)", next.Head)
	}
}

// TestScheduledAuditVerify_quietWhenHealthy pins the tripwire contract: a
// healthy chain leaves no audit row behind (the stamp is the local evidence).
func TestScheduledAuditVerify_quietWhenHealthy(t *testing.T) {
	root := t.TempDir()
	eng, err := NewEngine(&EngineConfig{RootDir: root})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	defer eng.Close()
	writeAuditSettings(t, root, config.AuditSettings{VerifyIntervalHours: 1})

	for i := 0; i < 3; i++ {
		_ = eng.RecordAudit(context.Background(), "install", fmt.Sprintf("pkg%d", i), "1.0.0", "success", nil)
	}

	if rows := auditVerifyRows(t, eng); len(rows) != 0 {
		t.Fatalf("audit_verify rows = %d, want none for a healthy chain", len(rows))
	}
	if _, err := os.Stat(auditVerifyStampPath(filepath.Join(root, "logs"))); err != nil {
		t.Fatalf("stat verification stamp: %v", err)
	}
	if result, err := eng.VerifyAuditLog(); err != nil || !result.OK || result.Entries != 3 {
		t.Fatalf("VerifyAuditLog = %+v (err=%v), want OK with 3 entries", result, err)
	}
}

// TestScheduledAuditVerify_runsWhenDue ages the stamp past the configured
// interval and checks the next audited operation re-verifies (stamp refreshed).
func TestScheduledAuditVerify_runsWhenDue(t *testing.T) {
	root := t.TempDir()
	eng, err := NewEngine(&EngineConfig{RootDir: root})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	defer eng.Close()
	writeAuditSettings(t, root, config.AuditSettings{VerifyIntervalHours: 1})

	_ = eng.RecordAudit(context.Background(), "install", "git", "2.54.0", "success", nil)
	stampPath := auditVerifyStampPath(filepath.Join(root, "logs"))
	ageAuditVerifyStamp(t, root, 48*time.Hour)

	_ = eng.RecordAudit(context.Background(), "install", "nodejs", "1.0.0", "success", nil)

	info, err := os.Stat(stampPath)
	if err != nil {
		t.Fatalf("stat verification stamp: %v", err)
	}
	if time.Since(info.ModTime()) > time.Minute {
		t.Fatalf("stamp mtime = %v, want a fresh stamp after the due verification", info.ModTime())
	}
	if rows := auditVerifyRows(t, eng); len(rows) != 0 {
		t.Fatalf("audit_verify rows = %d, want none for a healthy chain", len(rows))
	}
}

// TestScheduledAuditVerify_reportsBrokenChain tampers with the log, ages the
// stamp and asserts the next audited operation records one broken audit_verify
// row — and only one, until the interval elapses again.
func TestScheduledAuditVerify_reportsBrokenChain(t *testing.T) {
	root := t.TempDir()
	eng, err := NewEngine(&EngineConfig{RootDir: root})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	defer eng.Close()
	writeAuditSettings(t, root, config.AuditSettings{VerifyIntervalHours: 1})

	_ = eng.RecordAudit(context.Background(), "install", "git", "2.54.0", "success", nil)

	path := auditLogPath(root)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read audit.jsonl: %v", err)
	}
	tampered := strings.Replace(string(data), `"package":"git"`, `"package":"got"`, 1)
	if err := os.WriteFile(path, []byte(tampered), 0600); err != nil {
		t.Fatalf("write tampered audit.jsonl: %v", err)
	}
	ageAuditVerifyStamp(t, root, 48*time.Hour)

	_ = eng.RecordAudit(context.Background(), "install", "nodejs", "1.0.0", "success", nil)

	rows := auditVerifyRows(t, eng)
	if len(rows) != 1 {
		t.Fatalf("audit_verify rows = %d, want exactly 1", len(rows))
	}
	if status, _ := rows[0]["status"].(string); status != "broken" {
		t.Fatalf("audit_verify status = %q, want broken", status)
	}
	details, _ := rows[0]["details"].(map[string]any)
	if details["brokenAt"] != float64(1) || details["scheduled"] != true {
		t.Fatalf("audit_verify details = %+v, want brokenAt=1 scheduled=true", details)
	}

	// The stamp was refreshed before the row was written, so the following
	// appends stay quiet inside the interval.
	_ = eng.RecordAudit(context.Background(), "install", "python", "3.13.0", "success", nil)
	if rows := auditVerifyRows(t, eng); len(rows) != 1 {
		t.Fatalf("audit_verify rows = %d, want still 1 inside the interval", len(rows))
	}
}

// TestScheduledAuditVerify_disabledByConfig pins audit.verify_interval_hours < 0:
// nothing is verified and no stamp is written.
func TestScheduledAuditVerify_disabledByConfig(t *testing.T) {
	root := t.TempDir()
	eng, err := NewEngine(&EngineConfig{RootDir: root})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	defer eng.Close()
	writeAuditSettings(t, root, config.AuditSettings{VerifyIntervalHours: config.AuditVerifyDisabled})

	for i := 0; i < 3; i++ {
		_ = eng.RecordAudit(context.Background(), "install", fmt.Sprintf("pkg%d", i), "1.0.0", "success", nil)
	}

	if rows := auditVerifyRows(t, eng); len(rows) != 0 {
		t.Fatalf("audit_verify rows = %d, want none while the schedule is off", len(rows))
	}
	if _, err := os.Stat(auditVerifyStampPath(filepath.Join(root, "logs"))); !os.IsNotExist(err) {
		t.Fatalf("stamp err = %v, want no stamp while the schedule is off", err)
	}
}

// auditVerifyRows returns the audit_verify activity rows (newest first).
func auditVerifyRows(t *testing.T, eng *Engine) []map[string]any {
	t.Helper()
	rows, err := eng.QueryActivityLog("", 50, 0)
	if err != nil {
		t.Fatalf("QueryActivityLog: %v", err)
	}
	out := []map[string]any{}
	for _, row := range rows {
		if op, _ := row["operation"].(string); op == "audit_verify" {
			out = append(out, row)
		}
	}
	return out
}

// ageAuditVerifyStamp backdates the scheduled-verification stamp so the next
// audited operation sees a due verification.
func ageAuditVerifyStamp(t *testing.T, root string, age time.Duration) {
	t.Helper()
	path := auditVerifyStampPath(filepath.Join(root, "logs"))
	if _, err := os.Stat(path); err != nil {
		// No stamp yet: the verification is due anyway.
		return
	}
	when := time.Now().Add(-age)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatalf("Chtimes(%s): %v", path, err)
	}
}

// writeAuditSettings pins config.json's audit section for a test root.
func writeAuditSettings(t *testing.T, root string, settings config.AuditSettings) {
	t.Helper()
	if err := config.WriteAudit(root, settings); err != nil {
		t.Fatalf("WriteAudit: %v", err)
	}
}
