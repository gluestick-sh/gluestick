package main

import (
	"encoding/json"
	"testing"

	"github.com/spf13/cobra"
)

// TestConfigAgentKeys_roundTrip covers the §4.6.2 agent keys through the CLI:
// set writes config.json, get/list read it back.
func TestConfigAgentKeys_roundTrip(t *testing.T) {
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

	run(configSetCmd, "agent.auto_yes", "true")
	run(configSetCmd, "agent.policy.mode", "auto")
	run(configSetCmd, "agent.policy.deny", "uninstall")
	run(configSetCmd, "agent.policy.protected", "git,nodejs")

	var got struct {
		Value any `json:"value"`
	}
	if err := json.Unmarshal([]byte(run(configGetCmd, "agent.auto_yes")), &got); err != nil {
		t.Fatalf("config get JSON: %v", err)
	}
	if got.Value != true {
		t.Fatalf("agent.auto_yes = %v, want true", got.Value)
	}

	var list struct {
		AutoYes   bool     `json:"agent_auto_yes"`
		Mode      string   `json:"agent_policy_mode"`
		Deny      []string `json:"agent_policy_deny"`
		Protected []string `json:"agent_policy_protected"`
	}
	if err := json.Unmarshal([]byte(run(configListCmd)), &list); err != nil {
		t.Fatalf("config list JSON: %v", err)
	}
	if !list.AutoYes || list.Mode != "auto" {
		t.Fatalf("config list = %+v, want auto_yes + auto mode", list)
	}
	if len(list.Deny) != 1 || list.Deny[0] != "uninstall" {
		t.Fatalf("deny = %v, want [uninstall]", list.Deny)
	}
	if len(list.Protected) != 2 {
		t.Fatalf("protected = %v, want 2 entries", list.Protected)
	}
}
