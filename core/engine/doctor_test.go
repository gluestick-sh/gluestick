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

	rows, err := eng.QueryActivityLog("", 10, 0)
	if err != nil {
		t.Fatalf("QueryActivityLog: %v", err)
	}
	if len(rows) < 2 {
		t.Fatalf("expected at least 2 activity rows, got %d", len(rows))
	}
	if op, _ := rows[0]["operation"].(string); op != "doctor" {
		t.Fatalf("latest operation = %q, want doctor", op)
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
