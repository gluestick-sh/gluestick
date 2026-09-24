package engine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gluestick-sh/core/config"
	"github.com/gluestick-sh/core/message"
)

func TestRecordDoctorActivity(t *testing.T) {
	root := t.TempDir()
	eng, err := NewEngine(&EngineConfig{RootDir: root})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	defer eng.Close()

	if err := eng.RecordDoctorActivity(DoctorReport{
		OK: true,
		Checks: []DoctorCheck{
			{ID: message.DoctorCheckGlueRoot, OK: true},
			{ID: message.DoctorCheckGit, OK: true},
		},
	}); err != nil {
		t.Fatalf("RecordDoctorActivity success: %v", err)
	}

	if err := eng.RecordDoctorActivity(DoctorReport{
		OK: false,
		Checks: []DoctorCheck{
			{ID: message.DoctorCheckGlueRoot, OK: true},
			{ID: message.DoctorCheckGitHub, OK: false},
		},
	}); err != nil {
		t.Fatalf("RecordDoctorActivity failed: %v", err)
	}

	// glue env shares the DoctorReport shape but is its own audit operation.
	if err := eng.RecordEnvironmentActivity(DoctorReport{
		OK: true,
		Checks: []DoctorCheck{
			{ID: message.DoctorCheckGlueRoot, OK: true},
			{ID: message.DoctorCheckGit, OK: true},
		},
	}); err != nil {
		t.Fatalf("RecordEnvironmentActivity: %v", err)
	}

	// glue doctor's readiness report keeps the verdict and the failing checks.
	if err := eng.RecordAgentDoctorActivity(AgentDoctorReport{
		OK:         false,
		AgentReady: false,
		Score:      64,
		Summary: AgentDoctorSummary{
			Total: 23, Passed: 20, Failed: 2, Skipped: 1,
			BlockingFailed: []string{"shim_path"},
		},
		Fixes: []AgentFixResult{{Check: "buckets", Action: "Add main bucket", Applied: true}},
	}); err != nil {
		t.Fatalf("RecordAgentDoctorActivity: %v", err)
	}

	rows, err := eng.QueryActivityLog("", 10, 0)
	if err != nil {
		t.Fatalf("QueryActivityLog: %v", err)
	}
	if len(rows) < 4 {
		t.Fatalf("expected at least 4 activity rows, got %d", len(rows))
	}
	// Rows are newest first: keep the newest row per operation.
	latest := map[string]map[string]any{}
	for _, row := range rows {
		op, _ := row["operation"].(string)
		if _, seen := latest[op]; !seen {
			latest[op] = row
		}
	}
	if op, _ := rows[0]["operation"].(string); op != "doctor" {
		t.Fatalf("latest operation = %q, want doctor", op)
	}
	doctorRow, ok := latest["doctor"]
	if !ok {
		t.Fatalf("missing doctor row in %+v", rows)
	}
	if status, _ := doctorRow["status"].(string); status != "failed" {
		t.Fatalf("doctor status = %q, want failed while agentReady is false", status)
	}
	doctorDetails, _ := doctorRow["details"].(map[string]any)
	if doctorDetails["agentReady"] != false || doctorDetails["score"] != float64(64) {
		t.Fatalf("doctor details = %+v, want agentReady=false score=64", doctorDetails)
	}
	if doctorDetails["fixesApplied"] != float64(1) {
		t.Fatalf("doctor details = %+v, want fixesApplied=1", doctorDetails)
	}

	envRow, ok := latest["env"]
	if !ok {
		t.Fatalf("missing env row in %+v", rows)
	}
	if status, _ := envRow["status"].(string); status != "success" {
		t.Fatalf("env status = %q, want success", status)
	}
	envDetails, _ := envRow["details"].(map[string]any)
	if envDetails["ok"] != true || envDetails["passed"] != float64(2) {
		t.Fatalf("env details = %+v, want ok=true passed=2", envDetails)
	}
}

func TestRunDoctorWritableRoot(t *testing.T) {
	root := t.TempDir()
	eng, err := NewEngine(&EngineConfig{RootDir: root})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	defer eng.Close()

	report := eng.RunDoctor(context.Background())
	found := false
	for _, c := range report.Checks {
		if c.ID == message.DoctorCheckGlueRoot {
			found = true
			if !c.OK {
				t.Fatalf("data dir check failed: %+v", c)
			}
			if c.DetailText != root {
				t.Fatalf("detail = %q, want %q", c.DetailText, root)
			}
		}
	}
	if !found {
		t.Fatal("missing glue_root check")
	}
}

func TestCheckGlueRootWritableEmptyRoot(t *testing.T) {
	check := checkGlueRootWritable("")
	if check.OK {
		t.Fatal("expected failure for empty root")
	}
	if check.ID != message.DoctorCheckGlueRoot {
		t.Fatalf("id = %q, want %q", check.ID, message.DoctorCheckGlueRoot)
	}
}

func TestProbeURLViaGitHubMirror(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	mirrorURL := config.MirrorURLs(doctorGitHubProbeURL, []string{server.URL + "/"})[0]
	code, err := probeURL(context.Background(), mirrorURL)
	if err != nil {
		t.Fatalf("probeURL mirror: %v", err)
	}
	if !httpStatusOK(code) {
		t.Fatalf("code = %d", code)
	}
}

func TestCheckGitHubReachableWithConfiguredProxy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	root := t.TempDir()
	if err := config.WriteConfigGitHubProxy(root, server.URL+"/"); err != nil {
		t.Fatalf("WriteConfigGitHubProxy: %v", err)
	}

	check := checkGitHubReachable(context.Background(), root)
	if !check.OK {
		t.Fatalf("expected ok with working proxy, got %+v", check)
	}
}
