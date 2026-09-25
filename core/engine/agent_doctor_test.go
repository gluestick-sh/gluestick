package engine

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/gluestick-sh/core/config"
	"github.com/gluestick-sh/core/message"
)

// TestAgentCheckPolicy_reflectsConfig pins the readiness check for the MCP
// policy gates (roadmap section 4.6.2): a root without agent settings stays
// advisory with the current defaults spelled out, while any explicit setting
// (deny, protected, a non-confirm mode or auto_yes) passes.
func TestAgentCheckPolicy_reflectsConfig(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		c := agentCheckPolicy(t.TempDir())
		if c.OK || c.Status != "fail" || c.Level != AgentLevelAdvisory {
			t.Fatalf("empty root must stay an advisory failure: %+v", c)
		}
		if c.DetailKey != message.AgentPolicyDefaults {
			t.Fatalf("detailKey = %q, want %q", c.DetailKey, message.AgentPolicyDefaults)
		}
		data := c.Data
		if data["configured"] != false || data["mode"] != config.AgentPolicyModeConfirm {
			t.Fatalf("data = %+v, want configured=false mode=confirm", c.Data)
		}
		if strings.Contains(c.Hint, "arrive with the MCP phase") || c.Hint == "" {
			t.Fatalf("hint must describe the shipped gates, got %q", c.Hint)
		}
	})

	configured := []struct {
		name string
		cfg  string
	}{
		{"deny list", `{"agent":{"policy":{"deny":["uninstall","bucket_remove"]}}}`},
		{"protected list", `{"agent":{"policy":{"protected":["main"]}}}`},
		{"mode auto", `{"agent":{"policy":{"mode":"auto"}}}`},
		{"auto yes", `{"agent":{"auto_yes":true}}`},
	}
	for _, tc := range configured {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(config.Path(root), []byte(tc.cfg), 0644); err != nil {
				t.Fatalf("write config.json: %v", err)
			}
			c := agentCheckPolicy(root)
			if !c.OK || c.Status != "pass" {
				t.Fatalf("configured policy must pass: %+v", c)
			}
			if c.DetailKey != message.AgentPolicyConfigured {
				t.Fatalf("detailKey = %q, want %q", c.DetailKey, message.AgentPolicyConfigured)
			}
			data := c.Data
			if data["configured"] != true {
				t.Fatalf("data = %+v, want configured=true", c.Data)
			}
		})
	}
}

// agentDoctorCheckOrder is the stable check order of glue doctor (readiness
// view): grouped machine → shell → runtime → agents → workspace → glue.
var agentDoctorCheckOrder = []string{
	message.AgentCheckMachineOS,
	message.AgentCheckMachineArch,
	message.AgentCheckShellPWSH,
	message.AgentCheckShellUTF8,
	message.AgentCheckShellExecPolicy,
	message.AgentCheckShellGitBash,
	message.AgentCheckShellIntegration,
	message.AgentCheckToolchain,
	message.AgentCheckDuplicates,
	message.AgentCheckAgents,
	message.AgentCheckMCP,
	message.AgentCheckPolicy,
	message.AgentCheckWorkspaceFS,
	message.AgentCheckWorkspaceWSL,
	message.AgentCheckWorkspacePath,
	message.AgentCheckDataRoot,
	message.AgentCheckShimPath,
	message.AgentCheckShimRunner,
	message.AgentCheckInstallBackend,
	message.AgentCheckBuckets,
	message.AgentCheckGitConfig,
	message.AgentCheckNetwork,
	message.AgentCheckShimProbe,
}

func newAgentDoctorEngine(t *testing.T) (*Engine, string) {
	t.Helper()
	root := t.TempDir()
	eng, err := NewEngine(&EngineConfig{RootDir: root, Workers: 1})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	t.Cleanup(func() { eng.Close() })
	return eng, root
}

func agentCheckByID(t *testing.T, report AgentDoctorReport, id string) DoctorCheck {
	t.Helper()
	for _, c := range report.Checks {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("check %q missing from report", id)
	return DoctorCheck{}
}

func TestRunAgentDoctor_checkOrderAndLevels(t *testing.T) {
	eng, root := newAgentDoctorEngine(t)

	report := eng.RunAgentDoctor(context.Background(), AgentDoctorOptions{Offline: true, CLIVersion: "test"})

	if len(report.Checks) != len(agentDoctorCheckOrder) {
		t.Fatalf("checks = %d, want %d", len(report.Checks), len(agentDoctorCheckOrder))
	}
	for i, want := range agentDoctorCheckOrder {
		got := report.Checks[i]
		if got.ID != want {
			t.Fatalf("check[%d].ID = %q, want %q", i, got.ID, want)
		}
		if got.Level != AgentLevelBlocking && got.Level != AgentLevelAdvisory {
			t.Fatalf("check %s level = %q", got.ID, got.Level)
		}
		if got.Status == "" {
			t.Fatalf("check %s has an empty status", got.ID)
		}
	}
	if report.DataRoot != root {
		t.Fatalf("dataRoot = %q, want %q", report.DataRoot, root)
	}
	if report.CLIVersion != "test" {
		t.Fatalf("cliVersion = %q, want test", report.CLIVersion)
	}
	if report.SchemaVersion != AgentDoctorSchemaVersion {
		t.Fatalf("schemaVersion = %d, want %d", report.SchemaVersion, AgentDoctorSchemaVersion)
	}
	if report.Error != nil {
		t.Fatal("error must stay nil: readiness is a result, not an error")
	}
}

func TestRunAgentDoctor_emptyRootIsNotReady(t *testing.T) {
	eng, _ := newAgentDoctorEngine(t)

	report := eng.RunAgentDoctor(context.Background(), AgentDoctorOptions{Offline: true})

	if report.AgentReady || report.OK {
		t.Fatalf("agentReady/ok = %v/%v, want false/false on an empty data root", report.AgentReady, report.OK)
	}
	if !slices.Contains(report.Summary.BlockingFailed, message.AgentCheckBuckets) {
		t.Fatalf("blockingFailed = %v, want it to contain %q", report.Summary.BlockingFailed, message.AgentCheckBuckets)
	}
	if !slices.Contains(report.Summary.BlockingFailed, message.AgentCheckShimPath) {
		t.Fatalf("blockingFailed = %v, want it to contain %q (temp root is not on PATH)",
			report.Summary.BlockingFailed, message.AgentCheckShimPath)
	}
	if got := report.Summary.Passed + report.Summary.Failed + report.Summary.Skipped; got != report.Summary.Total {
		t.Fatalf("summary = %+v, want passed+failed+skipped == total", report.Summary)
	}
	if report.Summary.Total != len(report.Checks) {
		t.Fatalf("total = %d, want %d", report.Summary.Total, len(report.Checks))
	}
	if len(report.NextActions) == 0 {
		t.Fatal("nextActions is empty for a machine that is not ready")
	}

	buckets := agentCheckByID(t, report, message.AgentCheckBuckets)
	if buckets.Level != AgentLevelBlocking || buckets.Status != AgentStatusFail {
		t.Fatalf("buckets = %s/%s, want fail/blocking", buckets.Level, buckets.Status)
	}
	count, ok := buckets.Data["bucket_count"].(int)
	if !ok || count != 0 {
		t.Fatalf("buckets data = %v, want bucket_count 0", buckets.Data)
	}
}

func TestRunAgentDoctor_skipsOfflineAndShimProbe(t *testing.T) {
	eng, _ := newAgentDoctorEngine(t)

	report := eng.RunAgentDoctor(context.Background(), AgentDoctorOptions{Offline: true, ProbeShim: false})

	network := agentCheckByID(t, report, message.AgentCheckNetwork)
	if network.Status != AgentStatusSkipped {
		t.Fatalf("network status = %q, want skipped when Offline is set", network.Status)
	}
	if slices.Contains(report.Summary.BlockingFailed, message.AgentCheckNetwork) {
		t.Fatal("a skipped network check must never block readiness")
	}
	probe := agentCheckByID(t, report, message.AgentCheckShimProbe)
	if probe.Status != AgentStatusSkipped {
		t.Fatalf("shim_probe status = %q, want skipped without ProbeShim", probe.Status)
	}
	if report.Summary.Skipped < 2 {
		t.Fatalf("skipped = %d, want at least network and shim_probe", report.Summary.Skipped)
	}
}

func TestRunAgentDoctor_jsonschemaContract(t *testing.T) {
	eng, root := newAgentDoctorEngine(t)

	report := eng.RunAgentDoctor(context.Background(), AgentDoctorOptions{Offline: true})
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}

	var decoded struct {
		OK            bool    `json:"ok"`
		AgentReady    bool    `json:"agentReady"`
		SchemaVersion int     `json:"schemaVersion"`
		DataRoot      string  `json:"dataRoot"`
		Score         int     `json:"score"`
		Error         *string `json:"error"`
		Checks        []struct {
			ID     string         `json:"id"`
			Level  string         `json:"level"`
			Status string         `json:"status"`
			Group  string         `json:"group"`
			Data   map[string]any `json:"data"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal report: %v\n%s", err, raw)
	}
	if decoded.DataRoot != root {
		t.Fatalf("dataRoot = %q, want %q", decoded.DataRoot, root)
	}
	if decoded.SchemaVersion != AgentDoctorSchemaVersion {
		t.Fatalf("schemaVersion = %d, want %d", decoded.SchemaVersion, AgentDoctorSchemaVersion)
	}
	if decoded.Score < 0 || decoded.Score > 100 {
		t.Fatalf("score = %d, want 0..100", decoded.Score)
	}
	if strings.Contains(string(raw), `"fixes"`) {
		t.Fatalf(`"fixes" must be absent without --fix:\n%s`, raw)
	}
	if decoded.Error != nil {
		t.Fatal("error must stay null")
	}
	if !strings.Contains(string(raw), `"error":null`) {
		t.Fatalf("error key must be present and null:\n%s", raw)
	}
	if decoded.AgentReady != decoded.OK {
		t.Fatalf("agentReady/ok = %v/%v, want them in sync", decoded.AgentReady, decoded.OK)
	}
	if len(decoded.Checks) != len(agentDoctorCheckOrder) {
		t.Fatalf("checks = %d, want %d", len(decoded.Checks), len(agentDoctorCheckOrder))
	}
	for _, c := range decoded.Checks {
		if c.Group == "" {
			t.Fatalf("check %s missing group", c.ID)
		}
	}
	foundMCP := false
	for _, c := range decoded.Checks {
		if c.ID != message.AgentCheckMCP {
			continue
		}
		foundMCP = true
		if c.Data["command"] != "glue mcp" {
			t.Fatalf("mcp data = %v, want command glue mcp", c.Data)
		}
	}
	if !foundMCP {
		t.Fatal("mcp check missing from JSON output")
	}
}

func TestProbeCommonTools_partitionsToolchain(t *testing.T) {
	found, missing := ProbeCommonTools()
	if len(found)+len(missing) != len(CommonTools()) {
		t.Fatalf("found %d + missing %d != %d probed tools", len(found), len(missing), len(CommonTools()))
	}
	for name := range found {
		if slices.Contains(missing, name) {
			t.Fatalf("tool %q reported as both present and missing", name)
		}
	}
	if !slices.IsSorted(missing) {
		t.Fatalf("missing = %v, want sorted", missing)
	}
}

func TestPathDirPrecedes(t *testing.T) {
	shims := `C:\Users\xuc\.glue\shims`
	apps := `C:\Users\xuc\AppData\Local\Microsoft\WindowsApps`
	pathList := apps + ";" + shims

	if !PathDirPrecedes(pathList, apps, shims) {
		t.Fatal("WindowsApps should precede shims")
	}
	if PathDirPrecedes(pathList, shims, apps) {
		t.Fatal("shims should not precede WindowsApps")
	}
}

// TestGitBashShadowsShims pins the shadow rule behind shell_git_bash: only a
// non-WSL bash whose directory precedes the glue shims counts as shadowing, so
// `glue doctor --fix` (path_setup) can resolve the warning.
func TestGitBashShadowsShims(t *testing.T) {
	shims := `C:\Users\xuc\.glue\shims`
	gitBash := `C:\Program Files\Git\usr\bin\bash.exe`
	gitDir := `C:\Program Files\Git\usr\bin`

	if !gitBashShadowsShims(gitBash, shims, gitDir+";"+shims) {
		t.Fatal("Git Bash before shims should shadow")
	}
	if gitBashShadowsShims(gitBash, shims, shims+";"+gitDir) {
		t.Fatal("Git Bash after shims must not shadow")
	}
	if gitBashShadowsShims(`C:\Windows\System32\bash.exe`, shims, `C:\Windows\System32;`+shims) {
		t.Fatal("WSL launcher must never count as Git Bash shadowing")
	}
	if gitBashShadowsShims(gitBash, "", gitDir+";"+shims) {
		t.Fatal("unknown shim dir must not shadow")
	}
	if gitBashShadowsShims(gitBash, shims, gitDir) {
		t.Fatal("missing shim dir in PATH must not shadow")
	}
}

func TestRunAgentDoctor_groupsAndScore(t *testing.T) {
	eng, _ := newAgentDoctorEngine(t)

	report := eng.RunAgentDoctor(context.Background(), AgentDoctorOptions{Offline: true})

	valid := map[string]bool{
		AgentGroupMachine:   true,
		AgentGroupShell:     true,
		AgentGroupRuntime:   true,
		AgentGroupAgents:    true,
		AgentGroupWorkspace: true,
		AgentGroupGlue:      true,
	}
	for _, c := range report.Checks {
		if !valid[c.Group] {
			t.Errorf("check %s group = %q, want a known group", c.ID, c.Group)
		}
		switch c.Status {
		case AgentStatusPass, AgentStatusFail, AgentStatusSkipped:
		default:
			t.Errorf("check %s status = %q, want a valid status", c.ID, c.Status)
		}
	}
	if report.Score < 0 || report.Score > 100 {
		t.Fatalf("score = %d, want 0..100", report.Score)
	}
}

func TestComputeAgentScore_weightsAndSkips(t *testing.T) {
	passBlocking := DoctorCheck{Level: AgentLevelBlocking, Status: AgentStatusPass, OK: true}
	failBlocking := DoctorCheck{Level: AgentLevelBlocking, Status: AgentStatusFail}
	passAdvisory := DoctorCheck{Level: AgentLevelAdvisory, Status: AgentStatusPass, OK: true}
	failAdvisory := DoctorCheck{Level: AgentLevelAdvisory, Status: AgentStatusFail}
	skipAdvisory := DoctorCheck{Level: AgentLevelAdvisory, Status: AgentStatusSkipped}

	cases := []struct {
		name   string
		checks []DoctorCheck
		want   int
	}{
		{"empty means nothing failed", nil, 100},
		{"all pass", []DoctorCheck{passBlocking, passAdvisory}, 100},
		{"blocking weighs double: (100*1 + 1) / 3", []DoctorCheck{passAdvisory, failBlocking}, 33},
		{"advisory fail: (100*2 + 1) / 3", []DoctorCheck{passBlocking, failAdvisory}, 67},
		{"skipped leaves the denominator", []DoctorCheck{passBlocking, skipAdvisory}, 100},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := computeAgentScore(tc.checks); got != tc.want {
				t.Fatalf("computeAgentScore = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestAgentWorkspacePathIssue(t *testing.T) {
	cases := map[string]string{
		`C:\Users\xuc\.glue`:                "",
		`D:\work\project`:                   "",
		`\\server\share\.glue`:              "unc",
		`\\wsl$\Ubuntu\home\xuc\.glue`:      "wsl",
		`\\WSL.localhost\Ubuntu\home\.glue`: "wsl",
		`  \\wsl$\Ubuntu\home`:              "wsl",
		`relative\path`:                     "",
	}
	for path, want := range cases {
		if got := agentWorkspacePathIssue(path); got != want {
			t.Errorf("agentWorkspacePathIssue(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestProbeCommonToolsDetailed_shapeAndVersions(t *testing.T) {
	tools := CommonTools()
	probes := ProbeCommonToolsDetailed()
	if len(probes) != len(tools) {
		t.Fatalf("probes = %d, want %d", len(probes), len(tools))
	}
	for i, p := range probes {
		if p.Name != tools[i].Name {
			t.Fatalf("probes[%d].Name = %q, want %q", i, p.Name, tools[i].Name)
		}
		if p.Version != "" && !agentVersionPattern.MatchString(p.Version) {
			t.Fatalf("tool %s version %q is not a dotted version", p.Name, p.Version)
		}
		if p.Found && p.Path == "" {
			t.Fatalf("tool %s found without a path", p.Name)
		}
	}
}

// TestAgentFindDuplicateTools filters glue's own shims/store (inside the data
// root) out of duplicate detection and case-folds PATH entries.
func TestAgentFindDuplicateTools(t *testing.T) {
	root := t.TempDir()
	shims := filepath.Join(root, "shims")
	dirA, dirB := t.TempDir(), t.TempDir()
	for _, dir := range []string{shims, dirA, dirB} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "node.exe"), []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", strings.Join([]string{shims, dirA, dirB}, ";"))

	dups := agentFindDuplicateTools(root)
	if len(dups) != 1 || dups[0].Name != "node" || len(dups[0].Paths) != 2 {
		t.Fatalf("duplicates = %+v, want node with exactly the two external dirs", dups)
	}

	// One external install only → no duplicate.
	t.Setenv("PATH", dirA)
	if d := agentFindDuplicateTools(root); len(d) != 0 {
		t.Fatalf("duplicates = %+v, want none for a single install", d)
	}
}

// TestShellProfileRoundTrip pins the shell_integration contract in an isolated
// home: no profile → check fails; writing the glue block → check passes.
func TestShellProfileRoundTrip(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("PowerShell profiles are Windows-only")
	}
	t.Setenv("USERPROFILE", t.TempDir())
	t.Setenv("OneDrive", "")

	targets := GluePowerShellProfiles()
	if len(targets) == 0 {
		t.Fatal("no profile targets resolved under the isolated home")
	}
	c := agentCheckShellProfile()
	if c.Status != AgentStatusFail {
		t.Fatalf("status = %q, want fail before the block exists (%s %s)", c.Status, c.DetailKey, c.DetailText)
	}

	for _, target := range targets {
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte(GlueProfileBlock(`C:\glue\shims`)), 0644); err != nil {
			t.Fatal(err)
		}
	}
	c = agentCheckShellProfile()
	if c.Status != AgentStatusPass {
		t.Fatalf("status = %q, want pass after writing the block (%s %s)", c.Status, c.DetailKey, c.DetailText)
	}
	if !GlueProfileHasBlock(targets[0]) {
		t.Fatalf("GlueProfileHasBlock(%q) = false after write", targets[0])
	}
	// The block must contain the PATH guard, chcp, and UTF-8 stdin fix.
	block := GlueProfileBlock(`C:\glue\shims`)
	for _, part := range []string{GlueProfileMarker, "chcp 65001", "UTF8Encoding", `$env:Path`} {
		if !strings.Contains(block, part) {
			t.Errorf("profile block missing %q:\n%s", part, block)
		}
	}
}

// TestProbeGitBucket classifies a fresh repo (local autocrlf unpinned) and a
// pinned one (healthy). Ownership errors need a foreign owner and stay covered
// by the smoke runs.
func TestProbeGitBucket(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	if out, errOut, err := agentGit(context.Background(), dir, "init"); err != nil {
		t.Fatalf("git init: %v\n%s\n%s", err, out, errOut)
	}
	if kind, detail := ProbeGitBucket(context.Background(), dir); kind != "unpinned" {
		t.Fatalf("kind = %q (%s), want unpinned", kind, detail)
	}
	if _, errOut, err := agentGit(context.Background(), dir, "config", "--local", "core.autocrlf", "false"); err != nil {
		t.Fatalf("pin: %v\n%s", err, errOut)
	}
	if kind, detail := ProbeGitBucket(context.Background(), dir); kind != "" {
		t.Fatalf("kind = %q (%s), want healthy after pin", kind, detail)
	}
}
