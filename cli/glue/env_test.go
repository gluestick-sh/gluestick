package main

import (
	"strings"
	"testing"
)

// TestEnvCommand_registered pins the CLI surface: the environment report
// is a first-class command, not only a doctor flag.
func TestEnvCommand_registered(t *testing.T) {
	cmd, _, err := rootCmd.Find([]string{"env"})
	if err != nil {
		t.Fatalf("rootCmd.Find(env): %v", err)
	}
	if cmd != envCmd {
		t.Fatalf("Find(env) = %q, want envCmd", cmd.Name())
	}
}

// TestEnvCommand_rejectsPositional verifies glue env takes no arguments and
// keeps the exit-2 usage contract.
func TestEnvCommand_rejectsPositional(t *testing.T) {
	var runErr error
	errOut := captureStderrToFile(t, func() {
		runErr = envCmd.Args(envCmd, []string{"extra"})
	})
	if code := exitCode(runErr); code != 2 {
		t.Fatalf("exitCode = %d, want 2 (usage error)", code)
	}
	if !strings.Contains(errOut, "glue env takes no arguments") {
		t.Fatalf("stderr missing argument hint:\n%s", errOut)
	}
}
