package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestReadAgentDefaultsAndNormalize pins the §4.6.2 config contract: missing
// agent section = confirm, and an unknown mode clamps back to confirm.
func TestReadAgentDefaultsAndNormalize(t *testing.T) {
	root := t.TempDir()

	settings, err := ReadAgent(root)
	if err != nil {
		t.Fatalf("ReadAgent(empty root): %v", err)
	}
	if settings.AutoYes || settings.Policy.Mode != AgentPolicyModeConfirm {
		t.Fatalf("defaults = %+v, want confirm without auto_yes", settings)
	}

	body := `{"agent":{"auto_yes":true,"policy":{"mode":"AUTO","deny":["uninstall"],"protected":["git"]}}}`
	if err := os.WriteFile(filepath.Join(root, "config.json"), []byte(body), 0644); err != nil {
		t.Fatalf("write config.json: %v", err)
	}
	settings, err = ReadAgent(root)
	if err != nil {
		t.Fatalf("ReadAgent: %v", err)
	}
	if !settings.AutoYes || settings.Policy.Mode != AgentPolicyModeAuto {
		t.Fatalf("parsed = %+v, want auto_yes + auto mode", settings)
	}
	if len(settings.Policy.Deny) != 1 || len(settings.Policy.Protected) != 1 {
		t.Fatalf("lists = %+v, want one deny and one protected", settings.Policy)
	}

	body = `{"agent":{"policy":{"mode":"nonsense"}}}`
	if err := os.WriteFile(filepath.Join(root, "config.json"), []byte(body), 0644); err != nil {
		t.Fatalf("write config.json: %v", err)
	}
	settings, err = ReadAgent(root)
	if err != nil {
		t.Fatalf("ReadAgent: %v", err)
	}
	if settings.Policy.Mode != AgentPolicyModeConfirm {
		t.Fatalf("mode = %q, want confirm for an unknown value", settings.Policy.Mode)
	}
}
