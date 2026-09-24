package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gluestick-sh/core/config"
	"github.com/gluestick-sh/core/engine"
)

// TestDoctorAndEnv_recordAuditRows covers the CLI wiring: both check-up commands
// leave an audit row (glue doctor as "doctor", glue env as "env").
func TestDoctorAndEnv_recordAuditRows(t *testing.T) {
	root := t.TempDir()
	setJSONTestFlags(t, root)

	// Point the GitHub probe at a local server so `glue env` stays offline.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	if err := config.WriteConfigGitHubProxy(root, server.URL+"/"); err != nil {
		t.Fatalf("WriteConfigGitHubProxy: %v", err)
	}

	// The exit code is decided by the machine, not by this test: only the audit
	// row matters here.
	_ = captureStdoutToFile(t, func() { _ = runReadinessDoctor(doctorCmd, true, false, false) })
	_ = captureStdoutToFile(t, func() { _ = runEnvDoctor(doctorCmd) })

	eng, err := engine.NewEngine(&engine.EngineConfig{RootDir: root})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	defer eng.Close()

	rows, err := eng.QueryActivityLog("", 20, 0)
	if err != nil {
		t.Fatalf("QueryActivityLog: %v", err)
	}
	ops := map[string]bool{}
	for _, row := range rows {
		if op, _ := row["operation"].(string); op != "" {
			ops[op] = true
		}
	}
	if !ops["doctor"] || !ops["env"] {
		t.Fatalf("activity operations = %v, want both doctor and env rows", ops)
	}
}
