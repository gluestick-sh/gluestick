package main

import (
	"encoding/json"
	"testing"

	"github.com/gluestick-sh/core/config"
	"github.com/spf13/cobra"
)

// TestConfigAuditKeys_roundTrip covers the §4.6.1 rotation keys through the CLI:
// set writes config.json, get/list read it back, unset restores the defaults.
func TestConfigAuditKeys_roundTrip(t *testing.T) {
	root := t.TempDir()
	setJSONTestFlags(t, root)

	run := func(cmd *cobra.Command, args ...string) string {
		var runErr error
		out := captureStdoutToFile(t, func() { runErr = cmd.RunE(cmd, args) })
		if runErr != nil {
			t.Fatalf("%s %v: %v\n%s", cmd.Name(), args, runErr, out)
		}
		return out
	}

	run(configSetCmd, "audit.max_bytes", "1048576")
	run(configSetCmd, "audit.keep_segments", "3")

	var got struct {
		Value any `json:"value"`
	}
	if err := json.Unmarshal([]byte(run(configGetCmd, "audit.max_bytes")), &got); err != nil {
		t.Fatalf("config get JSON: %v", err)
	}
	if got.Value != float64(1048576) {
		t.Fatalf("audit.max_bytes = %v, want 1048576", got.Value)
	}

	settings, err := config.ReadAudit(root)
	if err != nil {
		t.Fatalf("ReadAudit: %v", err)
	}
	if settings.MaxBytes != 1048576 || settings.KeepSegments != 3 {
		t.Fatalf("config.json audit = %+v, want 1048576 bytes / 3 segments", settings)
	}

	var list struct {
		MaxBytes     int64 `json:"audit_max_bytes"`
		KeepSegments int   `json:"audit_keep_segments"`
	}
	if err := json.Unmarshal([]byte(run(configListCmd)), &list); err != nil {
		t.Fatalf("config list JSON: %v", err)
	}
	if list.MaxBytes != 1048576 || list.KeepSegments != 3 {
		t.Fatalf("config list = %+v, want the audit settings just written", list)
	}

	run(configUnsetCmd, "audit.keep_segments")
	settings, err = config.ReadAudit(root)
	if err != nil {
		t.Fatalf("ReadAudit: %v", err)
	}
	if settings.KeepSegments != config.DefaultAuditKeepSegments {
		t.Fatalf("keep_segments = %d, want default %d after unset", settings.KeepSegments, config.DefaultAuditKeepSegments)
	}

	// The negative sentinels are reachable as keywords (a bare "-1" would be
	// parsed as a flag by cobra).
	run(configSetCmd, "audit.max_bytes", "off")
	run(configSetCmd, "audit.keep_segments", "all")
	settings, err = config.ReadAudit(root)
	if err != nil {
		t.Fatalf("ReadAudit: %v", err)
	}
	if settings.RotationEnabled() || settings.PruneEnabled() {
		t.Fatalf("audit = %+v, want rotation and pruning disabled by the keywords", settings)
	}
}

// TestConfigAuditKeys_rejectInvalid pins the argument errors: non-numeric input
// and an absurd keep count are refused with code invalid_argument.
func TestConfigAuditKeys_rejectInvalid(t *testing.T) {
	root := t.TempDir()
	setJSONTestFlags(t, root)

	var runErr error
	_ = captureStdoutToFile(t, func() { runErr = configSetCmd.RunE(configSetCmd, []string{"audit.max_bytes", "big"}) })
	if runErr == nil {
		t.Fatal("config set audit.max_bytes big: want an error")
	}
	_ = captureStdoutToFile(t, func() { runErr = configSetCmd.RunE(configSetCmd, []string{"audit.keep_segments", "1000"}) })
	if runErr == nil {
		t.Fatal("config set audit.keep_segments 1000: want an error")
	}
}
