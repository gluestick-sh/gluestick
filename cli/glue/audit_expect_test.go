package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/gluestick-sh/core/engine"
)

// TestAuditVerify_expectPinnedHead covers the external anchor: a matching pinned
// head passes, a stale pin fails with reason "head mismatch".
func TestAuditVerify_expectPinnedHead(t *testing.T) {
	root := t.TempDir()
	setJSONTestFlags(t, root)

	eng, err := engine.NewEngine(&engine.EngineConfig{RootDir: root})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	_ = eng.RecordAudit(context.Background(), "install", "git", "2.54.0", "success", nil)
	eng.Close()

	run := func() (engine.AuditVerifyResult, error) {
		var runErr error
		out := captureStdoutToFile(t, func() { runErr = auditVerifyCmd.RunE(auditVerifyCmd, nil) })
		var payload engine.AuditVerifyResult
		if err := json.Unmarshal([]byte(out), &payload); err != nil {
			t.Fatalf("invalid JSON: %v\n%s", err, out)
		}
		return payload, runErr
	}
	setExpect := func(value string) {
		if err := auditVerifyCmd.Flags().Set("expect", value); err != nil {
			t.Fatalf("set --expect: %v", err)
		}
		t.Cleanup(func() { _ = auditVerifyCmd.Flags().Set("expect", "") })
	}

	payload, runErr := run()
	if runErr != nil {
		t.Fatalf("audit verify: %v", runErr)
	}
	if payload.Head == "" {
		t.Fatal("verify payload carries no head to pin")
	}

	setExpect(payload.Head)
	pinned, runErr := run()
	if runErr != nil {
		t.Fatalf("audit verify --expect <head>: %v", runErr)
	}
	if !pinned.OK || pinned.Expected != payload.Head {
		t.Fatalf("pinned verify = %+v, want ok with the expected head echoed", pinned)
	}

	setExpect("0000000000000000000000000000000000000000000000000000000000000000")
	stale, runErr := run()
	if runErr == nil {
		t.Fatal("audit verify --expect <stale>: want a failure")
	}
	if stale.OK || stale.Reason != "head mismatch" || stale.Expected == "" {
		t.Fatalf("stale verify = %+v, want ok=false reason=head mismatch", stale)
	}
}
