package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestReadAuditDefaultsAndNormalize pins the §4.6.1 rotation contract: a missing
// section means 8 MiB / 5 segments, negative values are sentinels (rotation off
// / keep every segment) and an oversized keep count is capped.
func TestReadAuditDefaultsAndNormalize(t *testing.T) {
	root := t.TempDir()

	settings, err := ReadAudit(root)
	if err != nil {
		t.Fatalf("ReadAudit(empty root): %v", err)
	}
	if settings.MaxBytes != DefaultAuditMaxBytes || settings.KeepSegments != DefaultAuditKeepSegments {
		t.Fatalf("defaults = %+v, want %d bytes / %d segments", settings, DefaultAuditMaxBytes, DefaultAuditKeepSegments)
	}
	if !settings.RotationEnabled() || !settings.PruneEnabled() {
		t.Fatalf("defaults = %+v, want rotation and pruning enabled", settings)
	}

	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, "config.json"), []byte(body), 0644); err != nil {
			t.Fatalf("write config.json: %v", err)
		}
	}

	write(`{"audit":{"max_bytes":1024,"keep_segments":2}}`)
	settings, err = ReadAudit(root)
	if err != nil {
		t.Fatalf("ReadAudit: %v", err)
	}
	if settings.MaxBytes != 1024 || settings.KeepSegments != 2 {
		t.Fatalf("parsed = %+v, want 1024 bytes / 2 segments", settings)
	}

	write(`{"audit":{"max_bytes":-1,"keep_segments":-1}}`)
	settings, err = ReadAudit(root)
	if err != nil {
		t.Fatalf("ReadAudit: %v", err)
	}
	if settings.RotationEnabled() || settings.PruneEnabled() {
		t.Fatalf("parsed = %+v, want rotation and pruning disabled", settings)
	}

	write(`{"audit":{"keep_segments":5000}}`)
	settings, err = ReadAudit(root)
	if err != nil {
		t.Fatalf("ReadAudit: %v", err)
	}
	if settings.KeepSegments != MaxAuditKeepSegments || settings.MaxBytes != DefaultAuditMaxBytes {
		t.Fatalf("parsed = %+v, want keep count capped at %d", settings, MaxAuditKeepSegments)
	}
}

// TestWriteAudit_preservesOtherKeys verifies `glue config set audit.*` keeps the
// rest of config.json (agent section) intact.
func TestWriteAudit_preservesOtherKeys(t *testing.T) {
	root := t.TempDir()

	if err := WriteAgent(root, AgentSettings{AutoYes: true, Policy: AgentPolicy{Mode: AgentPolicyModeAuto}}); err != nil {
		t.Fatalf("WriteAgent: %v", err)
	}
	if err := WriteAudit(root, AuditSettings{MaxBytes: 4096, KeepSegments: 3}); err != nil {
		t.Fatalf("WriteAudit: %v", err)
	}

	agent, err := ReadAgent(root)
	if err != nil {
		t.Fatalf("ReadAgent: %v", err)
	}
	if !agent.AutoYes || agent.Policy.Mode != AgentPolicyModeAuto {
		t.Fatalf("agent = %+v, want the previously written auto_yes/auto settings", agent)
	}
	audit, err := ReadAudit(root)
	if err != nil {
		t.Fatalf("ReadAudit: %v", err)
	}
	if audit.MaxBytes != 4096 || audit.KeepSegments != 3 {
		t.Fatalf("audit = %+v, want 4096 bytes / 3 segments", audit)
	}

	data, err := os.ReadFile(filepath.Join(root, "config.json"))
	if err != nil {
		t.Fatalf("read config.json: %v", err)
	}
	if !strings.Contains(string(data), `"agent"`) || !strings.Contains(string(data), `"audit"`) {
		t.Fatalf("config.json = %s, want both agent and audit sections", data)
	}
}
