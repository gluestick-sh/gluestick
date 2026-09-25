package main

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/gluestick-sh/core/engine"
	"github.com/gluestick-sh/core/message"
)

// captureStdoutToFile captures stdout into a temp file. The shared captureStdout
// helper reads its pipe only after fn returns, and Windows anonymous pipes buffer
// roughly 4 KiB, which deadlocks for reports larger than that (the readiness
// report is a few kilobytes of JSON).
func captureStdoutToFile(t *testing.T, fn func()) string {
	t.Helper()
	return captureStreamToFile(t, &os.Stdout, fn)
}

// captureStderrToFile captures stderr the same way (cobra's ErrOrStderr falls
// back to os.Stderr when the command has no dedicated writer).
func captureStderrToFile(t *testing.T, fn func()) string {
	t.Helper()
	return captureStreamToFile(t, &os.Stderr, fn)
}

// TestWriteAgentCheckRow_rendersClients pins the human view of data.clients: the
// agents verdict stays CLI-only, so the client sub-rows explain an
// "agent CLI not found" result on a machine driven from inside an IDE.
func TestWriteAgentCheckRow_rendersClients(t *testing.T) {
	check := engine.DoctorCheck{
		ID:         message.AgentCheckAgents,
		Level:      engine.AgentLevelAdvisory,
		Status:     engine.AgentStatusFail,
		DetailText: "claude, codex, opencode not found",
		Hint:       "Install an agent CLI",
		Data: map[string]any{
			"tools": []engine.CommonToolProbe{{Name: "Claude Code"}, {Name: "Codex"}, {Name: "OpenCode"}},
			"clients": []engine.AgentClientProbe{
				{Name: "Cline", Detected: false},
				{Name: "GitHub Copilot Chat", Detected: true, Wired: true, Config: `C:\Users\me\AppData\Roaming\Code\User\mcp.json`},
				{Name: "Cursor agent", Detected: true, Config: `C:\Users\me\.cursor\mcp.json`},
				{Name: "Claude Desktop", Detected: true},
			},
		},
	}

	out := captureStdoutToFile(t, func() { writeAgentCheckRow(check) })
	for _, want := range []string{
		"GitHub Copilot Chat",
		"installed, glue MCP registered",
		"installed, glue not registered",
		"installed, no MCP config yet",
		"not installed",
		"Install an agent CLI",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}

func captureStreamToFile(t *testing.T, target **os.File, fn func()) string {
	t.Helper()
	old := *target
	f, err := os.CreateTemp(t.TempDir(), "stream-*.txt")
	if err != nil {
		t.Fatal(err)
	}
	*target = f
	fn()
	*target = old
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// setDoctorReadinessFlags sets the readiness flags for one test and restores them.
func setDoctorReadinessFlags(t *testing.T, offline, probeShim bool) {
	t.Helper()
	set := func(name string, v bool) {
		if err := doctorCmd.Flags().Set(name, strconv.FormatBool(v)); err != nil {
			t.Fatal(err)
		}
	}
	set("offline", offline)
	set("probe-shim", probeShim)
	t.Cleanup(func() {
		_ = doctorCmd.Flags().Set("offline", "false")
		_ = doctorCmd.Flags().Set("probe-shim", "false")
	})
}

// agentDoctorJSON is the documented machine-readable contract of glue doctor
// (default readiness view).
type agentDoctorJSON struct {
	Command       string                  `json:"command"`
	OK            bool                    `json:"ok"`
	AgentReady    bool                    `json:"agentReady"`
	SchemaVersion int                     `json:"schemaVersion"`
	DataRoot      string                  `json:"dataRoot"`
	Score         int                     `json:"score"`
	Error         *string                 `json:"error"`
	NextActions   []string                `json:"nextActions"`
	Fixes         []engine.AgentFixResult `json:"fixes,omitempty"`
	Summary       struct {
		Total          int      `json:"total"`
		Passed         int      `json:"passed"`
		Failed         int      `json:"failed"`
		Skipped        int      `json:"skipped"`
		BlockingFailed []string `json:"blockingFailed"`
	} `json:"summary"`
	Checks []struct {
		ID     string `json:"id"`
		Level  string `json:"level"`
		Status string `json:"status"`
		Group  string `json:"group"`
	} `json:"checks"`
}

func runAgentDoctorJSON(t *testing.T) (agentDoctorJSON, string, error) {
	t.Helper()
	var runErr error
	out := captureStdoutToFile(t, func() {
		runErr = doctorCmd.RunE(doctorCmd, nil)
	})
	var res agentDoctorJSON
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	return res, out, runErr
}

func agentDoctorStatus(res agentDoctorJSON, id string) string {
	for _, c := range res.Checks {
		if c.ID == id {
			return c.Status
		}
	}
	return ""
}

func containsString(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

func TestAgentDoctorJSON_notReadyOnEmptyRoot(t *testing.T) {
	root := t.TempDir()
	setJSONTestFlags(t, root)
	setDoctorReadinessFlags(t, true, false)

	res, out, runErr := runAgentDoctorJSON(t)

	if code := exitCode(runErr); code != 1 {
		t.Fatalf("exitCode = %d, want 1 (not agent-ready)\n%s", code, out)
	}
	if res.Command != "doctor" {
		t.Fatalf("command = %q, want doctor", res.Command)
	}
	if res.AgentReady || res.OK {
		t.Fatalf("agentReady/ok = %v/%v, want false/false on an empty root", res.AgentReady, res.OK)
	}
	if res.SchemaVersion != engine.AgentDoctorSchemaVersion {
		t.Fatalf("schemaVersion = %d, want %d", res.SchemaVersion, engine.AgentDoctorSchemaVersion)
	}
	if res.DataRoot != root {
		t.Fatalf("dataRoot = %q, want %q", res.DataRoot, root)
	}
	if res.Error != nil {
		t.Fatalf("error = %q, want null", *res.Error)
	}
	if !strings.Contains(out, `"error": null`) {
		t.Fatalf("error key must be emitted as null:\n%s", out)
	}
	// 23 checks: 6 blocking (glue plumbing) + 17 advisory (env sections);
	// roadmap section 1 readiness definition.
	// Keep in sync with the engine's agentDoctorCheckOrder (grouped report).
	if len(res.Checks) != 23 {
		t.Fatalf("checks = %d, want 23\n%s", len(res.Checks), out)
	}
	for _, check := range res.Checks {
		if check.Level != engine.AgentLevelBlocking && check.Level != engine.AgentLevelAdvisory {
			t.Fatalf("check %s level = %q", check.ID, check.Level)
		}
	}
	if !containsString(res.Summary.BlockingFailed, "buckets") {
		t.Fatalf("blockingFailed = %v, want it to contain buckets", res.Summary.BlockingFailed)
	}
	if got := agentDoctorStatus(res, "network"); got != "skipped" {
		t.Fatalf("network status = %q, want skipped with --offline", got)
	}
	if got := agentDoctorStatus(res, "shim_probe"); got != "skipped" {
		t.Fatalf("shim_probe status = %q, want skipped by default", got)
	}
	if len(res.NextActions) == 0 {
		t.Fatal("nextActions is empty, want copy-pasteable fixes")
	}
	if res.Summary.Passed+res.Summary.Failed+res.Summary.Skipped != res.Summary.Total {
		t.Fatalf("summary = %+v, want passed+failed+skipped == total", res.Summary)
	}
	if res.Score < 0 || res.Score > 100 {
		t.Fatalf("score = %d, want 0..100", res.Score)
	}
	for _, c := range res.Checks {
		if c.Group == "" {
			t.Fatalf("check %s missing group", c.ID)
		}
	}
}

func TestAgentDoctorJSON_probeShimWithoutShimsIsSkipped(t *testing.T) {
	root := t.TempDir()
	setJSONTestFlags(t, root)
	setDoctorReadinessFlags(t, true, true)

	res, out, runErr := runAgentDoctorJSON(t)

	if code := exitCode(runErr); code != 1 {
		t.Fatalf("exitCode = %d, want 1\n%s", code, out)
	}
	if got := agentDoctorStatus(res, "shim_probe"); got != "skipped" {
		t.Fatalf("shim_probe status = %q, want skipped when nothing is installed", got)
	}
}

func TestAgentDoctorHuman_notReady(t *testing.T) {
	root := t.TempDir()
	if err := rootCmd.PersistentFlags().Set("root", root); err != nil {
		t.Fatal(err)
	}
	if err := rootCmd.PersistentFlags().Set("json", "false"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = rootCmd.PersistentFlags().Set("root", "")
		_ = rootCmd.PersistentFlags().Set("json", "false")
	})
	setDoctorReadinessFlags(t, true, false)

	var runErr error
	out := captureStdoutToFile(t, func() {
		runErr = doctorCmd.RunE(doctorCmd, nil)
	})

	if code := exitCode(runErr); code != 1 {
		t.Fatalf("exitCode = %d, want 1\n%s", code, out)
	}
	for _, want := range []string{
		"Glue agent readiness",
		"Machine",
		"Shell",
		"Runtime",
		"Agent compatibility",
		"Workspace",
		"Glue",
		"Result",
		"Agent Ready: no",
		"score",
		"Next:",
		"buckets",
		"glue doctor --fix",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("human output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, `"agentReady"`) {
		t.Fatalf("human output must not contain JSON:\n%s", out)
	}
}

// TestDoctorEnvFlag_removed pins that the traditional environment report only
// lives behind `glue env`: the old doctor flag is gone, not hidden.
func TestDoctorEnvFlag_removed(t *testing.T) {
	if f := doctorCmd.Flags().Lookup("env"); f != nil {
		t.Fatalf("doctor --env must be removed (use glue env): %+v", f)
	}
}

// TestDoctorArgs_rejectsPositional verifies glue doctor rejects extra arguments
// with a self-printed hint (SilenceErrors suppresses cobra's own message).
func TestDoctorArgs_rejectsPositional(t *testing.T) {
	var runErr error
	errOut := captureStderrToFile(t, func() {
		runErr = doctorCmd.Args(doctorCmd, []string{"extra"})
	})
	if code := exitCode(runErr); code != 2 {
		t.Fatalf("exitCode = %d, want 2 (usage error)", code)
	}
	if !strings.Contains(errOut, "glue doctor takes no arguments") {
		t.Fatalf("stderr missing argument hint:\n%s", errOut)
	}
}

// setDoctorFixFlag sets the readiness --fix flag for one test and restores it.
func setDoctorFixFlag(t *testing.T, v bool) {
	t.Helper()
	if err := doctorCmd.Flags().Set("fix", strconv.FormatBool(v)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = doctorCmd.Flags().Set("fix", "false")
	})
}

// TestDoctorFix_JSONReportsFixAttempts runs --fix offline on an empty root:
// the bucket fix must record its offline skip, machine-touching fixes (PATH,
// profiles, console/registry, package installs) stay gated by
// GLUE_DOCTOR_FIX_SKIP (tests never mutate the host), and the exit code still
// reflects the post-fix verdict.
func TestDoctorFix_JSONReportsFixAttempts(t *testing.T) {
	root := t.TempDir()
	setJSONTestFlags(t, root)
	setDoctorReadinessFlags(t, true, false)
	setDoctorFixFlag(t, true)
	t.Setenv("GLUE_DOCTOR_FIX_SKIP",
		"shim_path,shell_git_bash,shell_utf8,shell_pwsh,shell_exec_policy,shell_integration")

	res, out, runErr := runAgentDoctorJSON(t)

	if code := exitCode(runErr); code != 1 {
		t.Fatalf("exitCode = %d, want 1 (buckets still failing offline)\n%s", code, out)
	}
	if len(res.Fixes) == 0 {
		t.Fatalf("fixes = empty, want the bucket fix attempt\n%s", out)
	}
	byAction := map[string]engine.AgentFixResult{}
	for _, fix := range res.Fixes {
		byAction[fix.Action] = fix
	}
	bucketFix, ok := byAction["bucket_add"]
	if !ok {
		t.Fatalf("no bucket_add attempt in fixes: %+v\n%s", res.Fixes, out)
	}
	if bucketFix.Applied {
		t.Fatal("bucket_add must not apply while --offline")
	}
	if !strings.Contains(bucketFix.Error, "offline") {
		t.Fatalf("bucket_add error = %q, want an offline skip", bucketFix.Error)
	}
	if _, ok := byAction["path_setup"]; ok {
		t.Fatalf("path_setup must be skipped via GLUE_DOCTOR_FIX_SKIP: %+v", res.Fixes)
	}
}

// TestDoctorFix_humanShowsFixAttempts verifies the human view lists each fix
// by its human title, item by item, before Result (offline: bucket fix records
// its skip).
func TestDoctorFix_humanShowsFixAttempts(t *testing.T) {
	root := t.TempDir()
	if err := rootCmd.PersistentFlags().Set("root", root); err != nil {
		t.Fatal(err)
	}
	if err := rootCmd.PersistentFlags().Set("json", "false"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = rootCmd.PersistentFlags().Set("root", "")
		_ = rootCmd.PersistentFlags().Set("json", "false")
	})
	setDoctorReadinessFlags(t, true, false)
	setDoctorFixFlag(t, true)
	t.Setenv("GLUE_DOCTOR_FIX_SKIP",
		"shim_path,shell_git_bash,shell_utf8,shell_pwsh,shell_exec_policy,shell_integration")

	var runErr error
	out := captureStdoutToFile(t, func() {
		runErr = doctorCmd.RunE(doctorCmd, nil)
	})

	if code := exitCode(runErr); code != 1 {
		t.Fatalf("exitCode = %d, want 1\n%s", code, out)
	}
	for _, want := range []string{
		"Add main bucket",
		"offline",
		"Agent Ready: no",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("human --fix output missing %q:\n%s", want, out)
		}
	}
	iFix, iResult := strings.Index(out, "Add main bucket"), strings.Index(out, "Result")
	if iFix < 0 || iResult < 0 || iFix > iResult {
		t.Fatalf("fix list must appear before Result:\n%s", out)
	}
}

// TestDoctorFix_listsEveryModificationBeforeResult pins the product principle:
// every change --fix applied is listed individually by title, before the
// verdict — Glue never modifies the machine silently.
func TestDoctorFix_listsEveryModificationBeforeResult(t *testing.T) {
	report := engine.AgentDoctorReport{
		DataRoot:   `C:\dev\.glue`,
		AgentReady: true,
		OK:         true,
		Score:      100,
		Fixes: []engine.AgentFixResult{
			{Check: "shell_utf8", Action: "set_utf8", Applied: true},
			{Check: "git_config", Action: "git_config_fix", Applied: false, Error: "boom"},
			{Check: "shell_integration", Action: "shell_profile", Applied: true, Detail: "1 written"},
		},
	}
	out := captureStdoutToFile(t, func() {
		writeAgentDoctorReport(report, true)
	})
	for _, want := range []string{
		"Configure UTF-8",
		"Configure Git — boom",
		"Fix shell integration",
		"Agent Ready: yes",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("human --fix output missing %q:\n%s", want, out)
		}
	}
	iFix, iResult := strings.Index(out, "Configure UTF-8"), strings.Index(out, "Result")
	if iFix < 0 || iResult < 0 || iFix > iResult {
		t.Fatalf("fix list must appear before Result:\n%s", out)
	}
}

// TestDuplicatesFixDetail covers the report-only fix step's JSON summary.
func TestDuplicatesFixDetail(t *testing.T) {
	check := engine.DoctorCheck{Data: map[string]any{
		"duplicates": []engine.DuplicateTool{
			{Name: "node", Paths: []string{`C:\a\node.exe`, `C:\b\node.exe`}},
		},
	}}
	if got := duplicatesFixDetail(check); !strings.Contains(got, "node (2)") {
		t.Fatalf("detail = %q, want node (2)", got)
	}
	if got := duplicatesFixDetail(engine.DoctorCheck{Data: map[string]any{}}); got != "no duplicates" {
		t.Fatalf("detail = %q, want no duplicates", got)
	}
}

// TestWriteAgentCheckRow_labelsDuplicateRows pins the fix for the confusing
// double "git" line: the duplicate row must carry its copy count ("git (2)")
// so it reads as a different check from the toolchain row ("✓ git 2.54.0").
func TestWriteAgentCheckRow_labelsDuplicateRows(t *testing.T) {
	check := engine.DoctorCheck{
		ID:     message.AgentCheckDuplicates,
		Level:  engine.AgentLevelAdvisory,
		Status: engine.AgentStatusFail,
		Data: map[string]any{
			"duplicates": []engine.DuplicateTool{
				{Name: "git", Paths: []string{`C:\a\git.exe`, `C:\b\git.exe`}},
			},
		},
	}
	out := captureStdoutToFile(t, func() { writeAgentCheckRow(check) })
	if !strings.Contains(out, "git (2)") {
		t.Fatalf("duplicate row missing the copy count:\n%s", out)
	}
	if strings.Contains(out, "git  ") { // two spaces = old bare %-24s padding
		t.Fatalf("duplicate row still uses the bare tool name:\n%s", out)
	}
}
