package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gluestick-sh/core/engine"
	"github.com/gluestick-sh/core/message"
)

// stubDoctorInstallPackage replaces the package install seam for one test;
// tests must never hit the network or mutate the machine.
func stubDoctorInstallPackage(t *testing.T, fn func(context.Context, *engine.Engine, string, engine.ProgressReporter) (*engine.Result, error)) {
	t.Helper()
	old := doctorInstallPackage
	t.Cleanup(func() { doctorInstallPackage = old })
	doctorInstallPackage = fn
}

// TestDoctorInstallReporter_isSilent pins JSON stdout purity: the install
// reporter must never write to stdout while `doctor --fix --json` is running.
func TestDoctorInstallReporter_isSilent(t *testing.T) {
	if _, ok := doctorInstallReporter().(*engine.SilentReporter); !ok {
		t.Fatalf("doctorInstallReporter() = %T, want *engine.SilentReporter", doctorInstallReporter())
	}
}

// TestFixInstallPowerShell_installsMainPwsh verifies the fix uses Glue's own
// pipeline for main/pwsh (never winget) with a silent reporter.
func TestFixInstallPowerShell_installsMainPwsh(t *testing.T) {
	var gotRef string
	var gotReporter engine.ProgressReporter
	stubDoctorInstallPackage(t, func(_ context.Context, _ *engine.Engine, ref string, reporter engine.ProgressReporter) (*engine.Result, error) {
		gotRef, gotReporter = ref, reporter
		return &engine.Result{
			Name:    "pwsh",
			Version: "7.5.0",
			Status:  engine.StatusSuccess,
			Message: "Package installed successfully",
		}, nil
	})

	applied, detail, errText := fixInstallPowerShell(context.Background(), &engine.Engine{})
	if !applied || errText != "" {
		t.Fatalf("applied/detail/err = %v/%q/%q, want true with no error", applied, detail, errText)
	}
	if gotRef != doctorPwshRef {
		t.Fatalf("install ref = %q, want %q", gotRef, doctorPwshRef)
	}
	if _, ok := gotReporter.(*engine.SilentReporter); !ok {
		t.Fatalf("install reporter = %T, want *engine.SilentReporter", gotReporter)
	}
	for _, want := range []string{"main/pwsh", "7.5.0"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail %q missing %q", detail, want)
		}
	}
}

// TestFixInstallPowerShell_propagatesError maps a failed engine install into
// the fix item's error text instead of reporting success.
func TestFixInstallPowerShell_propagatesError(t *testing.T) {
	stubDoctorInstallPackage(t, func(context.Context, *engine.Engine, string, engine.ProgressReporter) (*engine.Result, error) {
		return &engine.Result{Name: "pwsh", Status: engine.StatusFailed, Error: errors.New("hash mismatch")},
			errors.New("install failed")
	})

	applied, detail, errText := fixInstallPowerShell(context.Background(), &engine.Engine{})
	if applied {
		t.Fatalf("applied = true with a failing install (detail: %s)", detail)
	}
	for _, want := range []string{"main/pwsh", "install failed"} {
		if !strings.Contains(errText, want) {
			t.Errorf("error %q missing %q", errText, want)
		}
	}
}

// TestFixInstallPowerShell_usesResultError covers engines that report failure
// through Result.Error with a nil error return.
func TestFixInstallPowerShell_usesResultError(t *testing.T) {
	stubDoctorInstallPackage(t, func(context.Context, *engine.Engine, string, engine.ProgressReporter) (*engine.Result, error) {
		return &engine.Result{Name: "pwsh", Status: engine.StatusFailed, Error: errors.New("manifest not found")}, nil
	})

	applied, _, errText := fixInstallPowerShell(context.Background(), &engine.Engine{})
	if applied || !strings.Contains(errText, "manifest not found") {
		t.Fatalf("applied/err = %v/%q, want false with the result error", applied, errText)
	}
}

// TestFixInstallPowerShell_requiresEngine guards the nil-engine path so a
// mis-wired caller gets an explicit failure instead of a panic.
func TestFixInstallPowerShell_requiresEngine(t *testing.T) {
	applied, _, errText := fixInstallPowerShell(context.Background(), nil)
	if applied || !strings.Contains(errText, "no engine") {
		t.Fatalf("applied/err = %v/%q, want false with a no-engine error", applied, errText)
	}
}

// TestRunDoctorFix_installPwshOfflineSkips preserves --offline semantics: no
// install is attempted and the fix records a skip reason.
func TestRunDoctorFix_installPwshOfflineSkips(t *testing.T) {
	called := false
	stubDoctorInstallPackage(t, func(context.Context, *engine.Engine, string, engine.ProgressReporter) (*engine.Result, error) {
		called = true
		return nil, nil
	})

	step := doctorFixStep{check: message.AgentCheckShellPWSH, action: "install_pwsh", title: "Configure PowerShell 7"}
	report := engine.AgentDoctorReport{DataRoot: t.TempDir()}
	check := engine.DoctorCheck{ID: message.AgentCheckShellPWSH, Status: engine.AgentStatusFail}

	applied, detail, errText := runDoctorFix(context.Background(), step, report, check, nil, true)
	if applied {
		t.Fatalf("applied = true while --offline (detail: %s)", detail)
	}
	if called {
		t.Fatal("install seam was called while --offline")
	}
	if !strings.Contains(errText, "offline") || !strings.Contains(errText, doctorPwshRef) {
		t.Fatalf("error = %q, want an offline skip naming %s", errText, doctorPwshRef)
	}
}

// TestApplyDoctorFixes_recordsPwshInstallDetail drives the fix-plan plumbing:
// a failing shell_pwsh check produces one fix item carrying the install detail.
func TestApplyDoctorFixes_recordsPwshInstallDetail(t *testing.T) {
	t.Setenv("GLUE_DOCTOR_FIX_SKIP", "")
	stubDoctorInstallPackage(t, func(context.Context, *engine.Engine, string, engine.ProgressReporter) (*engine.Result, error) {
		return &engine.Result{Name: "pwsh", Version: "7.5.0", Status: engine.StatusSuccess}, nil
	})

	report := engine.AgentDoctorReport{
		DataRoot: t.TempDir(),
		Checks: []engine.DoctorCheck{
			{ID: message.AgentCheckShellPWSH, Status: engine.AgentStatusFail, Level: engine.AgentLevelAdvisory},
		},
	}
	fixes := applyDoctorFixes(context.Background(), &engine.Engine{}, report, false)
	if len(fixes) != 1 {
		t.Fatalf("fixes = %+v, want exactly the shell_pwsh install", fixes)
	}
	fix := fixes[0]
	if fix.Action != "install_pwsh" || !fix.Applied || fix.Error != "" {
		t.Fatalf("fix = %+v, want an applied install_pwsh with no error", fix)
	}
	if !strings.Contains(fix.Detail, doctorPwshRef) || !strings.Contains(fix.Detail, "7.5.0") {
		t.Fatalf("detail = %q, want %s and the installed version", fix.Detail, doctorPwshRef)
	}
}

// TestDoctorFixableChecks_excludesReportOnly pins the footer count: report-only
// duplicate detection must not be advertised as a safe fix.
func TestDoctorFixableChecks_excludesReportOnly(t *testing.T) {
	t.Setenv("GLUE_DOCTOR_FIX_SKIP", "")
	report := engine.AgentDoctorReport{Checks: []engine.DoctorCheck{
		{ID: message.AgentCheckShellUTF8, Status: engine.AgentStatusFail, Level: engine.AgentLevelAdvisory},
		{ID: message.AgentCheckDuplicates, Status: engine.AgentStatusFail, Level: engine.AgentLevelAdvisory},
	}}
	got := doctorFixableChecks(report)
	if len(got) != 1 || got[0] != message.AgentCheckShellUTF8 {
		t.Fatalf("doctorFixableChecks = %v, want only %s", got, message.AgentCheckShellUTF8)
	}
}

// TestWriteDoctorFixLines_reportsReportOnly pins the human rendering: the
// duplicate step shows the skip mark and an explicit "report only" label
// instead of a check mark.
func TestWriteDoctorFixLines_reportsReportOnly(t *testing.T) {
	out := captureStdoutToFile(t, func() {
		writeDoctorFixLines([]engine.AgentFixResult{
			{Check: message.AgentCheckDuplicates, Action: "report_duplicates", Applied: true, Detail: "reported: git (4)"},
		})
	})
	for _, want := range []string{"Detect duplicate runtimes", "report only", "reported: git (4)"} {
		if !strings.Contains(out, want) {
			t.Errorf("fix line missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "✓") {
		t.Fatalf("report-only line must not use the success mark:\n%s", out)
	}
}
