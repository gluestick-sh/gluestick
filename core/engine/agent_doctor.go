// Agent readiness checks for glue doctor (roadmap sections 1 and 3.3).
//
// A machine is agent-ready when an AI agent can complete the whole
// install -> PATH -> doctor -> run chain with zero human interaction. This file
// answers that question with a machine-readable verdict while reusing the probes
// from doctor.go instead of duplicating them.
package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/gluestick-sh/core/config"
	"github.com/gluestick-sh/core/engine/internal/runtime"
	"github.com/gluestick-sh/core/message"
	"github.com/gluestick-sh/core/shim"
)

// AgentDoctorSchemaVersion is the stable schema version of the agent doctor report.
const AgentDoctorSchemaVersion = 1

// Check severity and status values used by agent readiness output.
const (
	AgentLevelBlocking = "blocking"
	AgentLevelAdvisory = "advisory"

	AgentStatusPass    = "pass"
	AgentStatusFail    = "fail"
	AgentStatusSkipped = "skipped"
)

const (
	// agentIndexWait bounds how long readiness waits for the background bucket
	// manifest index build before reporting it as not ready.
	agentIndexWait = 5 * time.Second
	// agentShimProbeTimeout bounds a --probe-shim child process.
	agentShimProbeTimeout = 10 * time.Second
)

// AgentDoctorOptions configures one readiness run.
type AgentDoctorOptions struct {
	// Offline skips network probes; the network check then reports "skipped".
	Offline bool
	// ProbeShim launches one installed shim to prove tools can actually run.
	ProbeShim bool
	// CLIVersion is echoed into the report contract block.
	CLIVersion string
	// DataRootIsolated reports whether this build uses an isolated dev data root.
	DataRootIsolated bool
	// MCPAvailable reports whether the glue mcp entry point exists.
	MCPAvailable bool
}

// AgentDoctorSummary aggregates check outcomes.
type AgentDoctorSummary struct {
	Total          int      `json:"total"`
	Passed         int      `json:"passed"`
	Failed         int      `json:"failed"`
	Skipped        int      `json:"skipped"`
	BlockingFailed []string `json:"blockingFailed,omitempty"`
	AdvisoryFailed []string `json:"advisoryFailed,omitempty"`
}

// AgentDoctorReport is the machine-readable agent readiness verdict.
//
// Contract: agentReady (plus the process exit code: 0 ready, 1 not ready) is the
// answer; checks[].id, checks[].level, and checks[].status are stable; data is a
// free-form extension area where unknown keys must be tolerated.
type AgentDoctorReport struct {
	// Command is the CLI entry point that produced this report (clients set it).
	Command          string             `json:"command,omitempty"`
	Checks           []DoctorCheck      `json:"checks"`
	OK               bool               `json:"ok"`
	AgentReady       bool               `json:"agentReady"`
	SchemaVersion    int                `json:"schemaVersion"`
	DataRoot         string             `json:"dataRoot,omitempty"`
	DataRootIsolated bool               `json:"dataRootIsolated"`
	CLIVersion       string             `json:"cliVersion,omitempty"`
	Summary          AgentDoctorSummary `json:"summary"`
	// Score is a human-facing readiness percentage (0-100): blocking checks weigh
	// 2, advisory checks weigh 1, and skipped checks leave the denominator. The
	// process exit code is decided by agentReady alone, never by the score.
	Score       int      `json:"score"`
	NextActions []string `json:"nextActions,omitempty"`
	// Fixes lists the --fix attempts made before this report was re-run. It is
	// absent unless glue doctor --fix was used (additive field).
	Fixes []AgentFixResult `json:"fixes,omitempty"`
	Error *string          `json:"error"`
}

// AgentFixResult records one glue doctor --fix attempt. Report-only actions
// (duplicate detection) set applied=true with an empty Error when the report
// was delivered; Detail carries human summaries for every action.
type AgentFixResult struct {
	Check   string `json:"check"`
	Action  string `json:"action"`
	Applied bool   `json:"applied"`
	Error   string `json:"error,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

// RunAgentDoctor probes this machine and reports whether it is agent-ready.
func (e *Engine) RunAgentDoctor(ctx context.Context, opts AgentDoctorOptions) AgentDoctorReport {
	if ctx == nil {
		ctx = context.Background()
	}
	root := e.Config.RootDir

	checks := []DoctorCheck{
		// machine
		agentCheckMachineOS(),
		agentCheckMachineArch(),
		// shell
		agentCheckShellPWSH(),
		agentCheckShellUTF8(),
		agentCheckShellExecPolicy(),
		agentCheckShellGitBash(root),
		agentCheckShellProfile(),
		// runtime
		e.agentCheckToolchain(),
		agentCheckDuplicates(root),
		// agent compatibility
		agentCheckAgents(),
		agentCheckMCP(opts.MCPAvailable),
		agentCheckPolicy(root),
		// workspace
		agentCheckWorkspaceFS(root),
		agentCheckWorkspaceWSL(),
		agentCheckWorkspacePath(root),
		// glue (its own plumbing: data root, PATH, buckets, network)
		agentCheckDataRoot(root),
		agentCheckShimPath(root),
		agentCheckShimRunner(root),
		agentCheckInstallBackend(root),
		e.agentCheckBuckets(ctx),
		e.agentCheckGitConfig(ctx),
		agentCheckNetwork(ctx, root, opts.Offline),
		agentCheckShimProbe(root, opts.ProbeShim),
	}
	for i := range checks {
		checks[i].Group = agentGroupFor(checks[i].ID)
	}

	report := AgentDoctorReport{
		Checks:           checks,
		SchemaVersion:    AgentDoctorSchemaVersion,
		DataRoot:         root,
		DataRootIsolated: opts.DataRootIsolated,
		CLIVersion:       opts.CLIVersion,
		Summary:          AgentDoctorSummary{Total: len(checks)},
	}
	for _, c := range checks {
		switch {
		case c.Status == AgentStatusSkipped:
			report.Summary.Skipped++
		case c.OK:
			report.Summary.Passed++
		default:
			report.Summary.Failed++
			if c.Level != AgentLevelBlocking {
				report.Summary.AdvisoryFailed = append(report.Summary.AdvisoryFailed, c.ID)
				continue
			}
			report.Summary.BlockingFailed = append(report.Summary.BlockingFailed, c.ID)
			if action := agentActionForCheck(c); action != "" {
				report.NextActions = append(report.NextActions, action)
			}
		}
	}
	report.Score = computeAgentScore(checks)
	report.AgentReady = len(report.Summary.BlockingFailed) == 0
	report.OK = report.AgentReady
	return report
}

// Report sections of the grouped readiness view; mirrored as checks[].group.
const (
	AgentGroupMachine   = "machine"
	AgentGroupShell     = "shell"
	AgentGroupRuntime   = "runtime"
	AgentGroupAgents    = "agents"
	AgentGroupWorkspace = "workspace"
	AgentGroupGlue      = "glue"
)

// agentCheckGroups maps every readiness check to its report section. A missing
// entry is a programming error; the human view renders such checks under "Other".
var agentCheckGroups = map[string]string{
	message.AgentCheckMachineOS:        AgentGroupMachine,
	message.AgentCheckMachineArch:      AgentGroupMachine,
	message.AgentCheckShellPWSH:        AgentGroupShell,
	message.AgentCheckShellUTF8:        AgentGroupShell,
	message.AgentCheckShellExecPolicy:  AgentGroupShell,
	message.AgentCheckShellGitBash:     AgentGroupShell,
	message.AgentCheckShellIntegration: AgentGroupShell,
	message.AgentCheckToolchain:        AgentGroupRuntime,
	message.AgentCheckDuplicates:       AgentGroupRuntime,
	message.AgentCheckAgents:           AgentGroupAgents,
	message.AgentCheckMCP:              AgentGroupAgents,
	message.AgentCheckPolicy:           AgentGroupAgents,
	message.AgentCheckWorkspaceFS:      AgentGroupWorkspace,
	message.AgentCheckWorkspaceWSL:     AgentGroupWorkspace,
	message.AgentCheckWorkspacePath:    AgentGroupWorkspace,
	message.AgentCheckDataRoot:         AgentGroupGlue,
	message.AgentCheckShimPath:         AgentGroupGlue,
	message.AgentCheckShimRunner:       AgentGroupGlue,
	message.AgentCheckInstallBackend:   AgentGroupGlue,
	message.AgentCheckBuckets:          AgentGroupGlue,
	message.AgentCheckGitConfig:        AgentGroupGlue,
	message.AgentCheckNetwork:          AgentGroupGlue,
	message.AgentCheckShimProbe:        AgentGroupGlue,
}

// agentGroupFor returns the report section of one check ("" when unmapped).
func agentGroupFor(id string) string {
	return agentCheckGroups[id]
}

// computeAgentScore quantifies readiness for humans: passed checks earn weight
// 2 when blocking and 1 when advisory; skipped checks (for example network with
// --offline) never enter the denominator, so flags alone cannot change the
// score. The verdict stays agentReady — the score is display color only.
func computeAgentScore(checks []DoctorCheck) int {
	numerator, denominator := 0, 0
	for _, c := range checks {
		if c.Status == AgentStatusSkipped {
			continue
		}
		weight := 1
		if c.Level == AgentLevelBlocking {
			weight = 2
		}
		denominator += weight
		if c.OK && c.Status == AgentStatusPass {
			numerator += weight
		}
	}
	if denominator == 0 {
		return 100
	}
	// Integer rounding half-up: (100*num + den/2) / den.
	return (100*numerator + denominator/2) / denominator
}

// agentNextActions maps a failed blocking check to the command an agent should
// run next, so the human view always ends with a copy-pasteable fix.
var agentNextActions = map[string]string{
	message.AgentCheckDataRoot:       "glue doctor",
	message.AgentCheckShimPath:       "glue path setup",
	message.AgentCheckShimRunner:     "glue doctor",
	message.AgentCheckInstallBackend: "glue doctor",
	message.AgentCheckBuckets:        "glue bucket add main",
	message.AgentCheckNetwork:        "glue config set github_proxy <mirror-url>",
}

// agentActionForCheck resolves the next action for a failed check. Buckets are
// reason-aware: "add main" only helps when no bucket is installed, while an
// installed-but-unindexed bucket needs the background index build to finish and
// a bucket with no manifests needs a re-pull.
func agentActionForCheck(c DoctorCheck) string {
	if c.ID == message.AgentCheckBuckets {
		switch c.DetailKey {
		case message.AgentBucketsIndexNotReady:
			return "glue doctor"
		case message.AgentBucketsNoManifests:
			return "glue bucket update"
		}
	}
	return agentNextActions[c.ID]
}

// statusFromOK derives the agent check status from its boolean result.
func statusFromOK(ok bool) string {
	if ok {
		return AgentStatusPass
	}
	return AgentStatusFail
}

// withAgentLevel stamps severity and derives status for a check.
func (c DoctorCheck) withAgentLevel(level string) DoctorCheck {
	c.Level = level
	c.Status = statusFromOK(c.OK)
	return c
}

// agentCheckDataRoot verifies the data root exists and accepts writes.
func agentCheckDataRoot(root string) DoctorCheck {
	c := checkGlueRootWritable(root)
	c.ID = message.AgentCheckDataRoot
	c.Data = map[string]any{"root": root}
	return c.withAgentLevel(AgentLevelBlocking)
}

// agentCheckShimPath verifies the shims directory is usable on PATH and is not
// shadowed by Microsoft Store app-execution aliases.
func agentCheckShimPath(root string) DoctorCheck {
	c := DoctorCheck{ID: message.AgentCheckShimPath, Level: AgentLevelBlocking}
	mgr, err := shim.NewManager(root)
	if err != nil {
		c.DetailText = err.Error()
		c.HintKey = message.DoctorHintShimPath
		c.Hint = doctorHint(c.HintKey)
		c.Status = AgentStatusFail
		return c
	}

	binDir := mgr.BinDir()
	inPath := mgr.InPath()
	shadowed := StoreAliasShadowsShims(binDir)
	c.Data = map[string]any{
		"bin_dir":               binDir,
		"in_path":               inPath,
		"store_alias_shadowing": shadowed,
	}

	switch {
	case inPath && !shadowed:
		c.OK = true
		c.DetailKey = message.DoctorShimInPath
		c.DetailText = binDir
	case !inPath:
		c.DetailKey = message.DoctorShimNotInPath
		c.DetailText = binDir
		c.HintKey = message.DoctorHintShimPath
	default:
		c.DetailKey = message.AgentShimAliasShadowing
		c.DetailText = binDir
		c.HintKey = message.AgentHintShimAlias
	}
	if !c.OK {
		c.Hint = doctorHint(c.HintKey)
	}
	c.Status = statusFromOK(c.OK)
	return c
}

// agentCheckShimRunner verifies that shim metadata exists and every shim still
// points at an existing target, so installed tools can actually be launched.
func agentCheckShimRunner(root string) DoctorCheck {
	c := DoctorCheck{ID: message.AgentCheckShimRunner, Level: AgentLevelBlocking}
	mgr, err := shim.NewManager(root)
	if err != nil {
		return agentShimRunnerFailure(c, err)
	}
	configs, err := mgr.List()
	if err != nil {
		return agentShimRunnerFailure(c, err)
	}

	broken := []string{}
	for _, cfg := range configs {
		target := strings.TrimSpace(cfg.Path)
		if target == "" {
			target = strings.TrimSpace(cfg.Command)
		}
		if target == "" {
			broken = append(broken, cfg.Name)
			continue
		}
		if _, statErr := os.Stat(target); statErr != nil {
			broken = append(broken, cfg.Name)
		}
	}
	sort.Strings(broken)

	c.Data = map[string]any{"shim_count": len(configs), "broken_shims": broken}
	if len(broken) == 0 {
		c.OK = true
		c.DetailKey = message.AgentShimRunnerOK
		c.DetailText = fmt.Sprintf("%d shims", len(configs))
	} else {
		c.DetailKey = message.AgentShimRunnerMissing
		c.DetailText = fmt.Sprintf("%d of %d shims point at missing targets", len(broken), len(configs))
		c.HintKey = message.AgentHintShimRunner
		c.Hint = doctorHint(c.HintKey)
	}
	c.Status = statusFromOK(c.OK)
	return c
}

func agentShimRunnerFailure(c DoctorCheck, err error) DoctorCheck {
	c.DetailText = err.Error()
	c.HintKey = message.AgentHintShimRunner
	c.Hint = doctorHint(c.HintKey)
	c.Status = AgentStatusFail
	return c
}

// agentCheckInstallBackend verifies the tools an unattended install needs.
// git and 7-Zip are downloaded on demand, so "not installed yet" still counts as
// ready on Windows; the network check covers the download requirement separately.
func agentCheckInstallBackend(root string) DoctorCheck {
	c := DoctorCheck{ID: message.AgentCheckInstallBackend, Level: AgentLevelBlocking}

	gitProbe := ProbeGit(root)
	sevenZipProbe := ProbeSevenZip(root)
	bootstrapPending := []string{}
	if !gitProbe.OK {
		bootstrapPending = append(bootstrapPending, "git")
	}
	if !sevenZipProbe.OK {
		bootstrapPending = append(bootstrapPending, "7-Zip")
	}

	c.Data = map[string]any{
		"git":               gitProbe.Path,
		"git_ok":            gitProbe.OK,
		"seven_zip":         sevenZipProbe.Path,
		"seven_zip_ok":      sevenZipProbe.OK,
		"bootstrap_pending": bootstrapPending,
	}

	c.OK = true
	c.DetailKey = message.AgentInstallBackendOK
	switch {
	case gitProbe.OK && sevenZipProbe.OK:
		c.DetailText = "git and 7-Zip available"
	case len(bootstrapPending) > 0:
		c.DetailText = "bootstrap pending: " + strings.Join(bootstrapPending, ", ")
	default:
		c.DetailText = "available"
	}
	if !gitProbe.OK && !sevenZipProbe.OK && len(bootstrapPending) > 0 && !bootstrapPossible() {
		c.OK = false
		c.DetailKey = message.AgentInstallBackendMissing
		c.DetailText = "missing: " + strings.Join(bootstrapPending, ", ")
		c.HintKey = message.AgentHintInstallBackend
		c.Hint = doctorHint(c.HintKey)
	}
	c.Status = statusFromOK(c.OK)
	return c
}

// agentCheckBuckets verifies that at least one bucket contributes searchable
// manifests, which is what makes unattended search and install possible.
func (e *Engine) agentCheckBuckets(ctx context.Context) DoctorCheck {
	c := DoctorCheck{ID: message.AgentCheckBuckets, Level: AgentLevelBlocking}

	indexReady := e.SearchIndexReady()
	if !indexReady {
		waitCtx, cancel := context.WithTimeout(ctx, agentIndexWait)
		_ = runtime.WaitSearchIndexReady(e.Engine, waitCtx)
		cancel()
		indexReady = e.SearchIndexReady()
	}

	counts := e.PackageCountsByBucket()
	buckets := e.BucketRegistry.List()
	bucketData := make([]map[string]any, 0, len(buckets))
	total := 0
	indexed := 0
	for _, b := range buckets {
		count := counts[b.Name]
		total += count
		if count > 0 {
			indexed++
		}
		bucketData = append(bucketData, map[string]any{"name": b.Name, "packages": count})
	}

	c.Data = map[string]any{
		"buckets":         bucketData,
		"bucket_count":    len(buckets),
		"buckets_indexed": indexed,
		"manifest_count":  total,
		"index_ready":     indexReady,
	}

	verdict := bucketsCheckVerdict(len(buckets), indexed, total, indexReady)
	c.OK = verdict.ok
	c.DetailKey = verdict.detailKey
	c.DetailText = verdict.detail
	if !c.OK && verdict.hintKey != "" {
		c.HintKey = verdict.hintKey
		c.Hint = doctorHint(c.HintKey)
	}
	c.Status = statusFromOK(c.OK)
	return c
}

// bucketsVerdict is the decision table behind agentCheckBuckets, kept pure so
// every reason (and its message keys) can be unit-tested without an engine.
// The failure hints matter: buckets can be installed and still have an empty
// index, so "add main" would be wrong advice there — rescanning is the fix.
type bucketsVerdict struct {
	ok        bool
	detailKey string
	detail    string
	hintKey   string
}

func bucketsCheckVerdict(bucketCount, indexed, total int, indexReady bool) bucketsVerdict {
	switch {
	case bucketCount == 0:
		return bucketsVerdict{
			detailKey: message.AgentBucketsEmpty,
			detail:    "no buckets installed",
			hintKey:   message.AgentHintBuckets,
		}
	case !indexReady:
		return bucketsVerdict{
			detailKey: message.AgentBucketsIndexNotReady,
			detail:    fmt.Sprintf("%d of %d bucket(s) indexed, %d package(s)", indexed, bucketCount, total),
			hintKey:   message.AgentHintBucketsIndexBusy,
		}
	case total == 0:
		return bucketsVerdict{
			detailKey: message.AgentBucketsNoManifests,
			detail:    fmt.Sprintf("no manifests indexed from %d bucket(s)", bucketCount),
			hintKey:   message.AgentHintBucketsNoManifests,
		}
	default:
		return bucketsVerdict{
			ok:        true,
			detailKey: message.AgentBucketsOK,
			detail:    fmt.Sprintf("%d buckets, %d packages", bucketCount, total),
		}
	}
}

// agentCheckNetwork verifies that buckets and downloads can be reached.
// With --offline the check reports "skipped" and never blocks readiness.
func agentCheckNetwork(ctx context.Context, root string, offline bool) DoctorCheck {
	c := DoctorCheck{ID: message.AgentCheckNetwork, Level: AgentLevelBlocking}
	proxySet := len(config.LoadProxies(root)) > 0

	if offline {
		c.Status = AgentStatusSkipped
		c.DetailKey = message.AgentNetworkSkipped
		c.DetailText = "skipped (--offline)"
		c.Data = map[string]any{"offline": true, "proxy_set": proxySet}
		return c
	}

	base := checkGitHubReachable(ctx, root)
	via := "none"
	switch base.DetailKey {
	case message.DoctorGitHubGitOK:
		via = "git"
	case message.DoctorGitHubDirectOK:
		via = "direct"
	case message.DoctorGitHubMirrorOK:
		via = "mirror"
	}

	c.OK = base.OK
	c.DetailKey = base.DetailKey
	c.DetailText = base.DetailText
	c.HintKey = base.HintKey
	c.Hint = base.Hint
	c.Data = map[string]any{
		"offline":   false,
		"reachable": base.OK,
		"via":       via,
		"proxy_set": proxySet,
	}
	c.Status = statusFromOK(c.OK)
	return c
}

// agentCheckToolchain reports which common agent tools resolve on PATH.
// This is advisory: readiness only requires that tools can be installed and run.
func (e *Engine) agentCheckToolchain() DoctorCheck {
	c := DoctorCheck{ID: message.AgentCheckToolchain, Level: AgentLevelAdvisory}

	tools := ProbeCommonToolsDetailed()
	present := make([]string, 0, len(tools))
	missing := []string{}
	for _, tool := range tools {
		if tool.Found {
			present = append(present, tool.Name)
			continue
		}
		missing = append(missing, tool.Name)
	}
	sort.Strings(present)

	installed := 0
	if list, err := e.ListInstalledAllVersions(nil); err == nil {
		installed = len(list)
	}

	c.Data = map[string]any{
		"present":            present,
		"missing":            missing,
		"installed_packages": installed,
		"tools":              tools,
	}
	if len(missing) == 0 {
		c.OK = true
		c.DetailKey = message.AgentToolchainOK
		c.DetailText = strings.Join(present, ", ")
	} else {
		c.DetailKey = message.AgentToolchainMissing
		c.DetailText = "missing: " + strings.Join(missing, ", ")
		c.HintKey = message.AgentHintToolchain
		c.Hint = doctorHint(c.HintKey)
	}
	c.Status = statusFromOK(c.OK)
	return c
}

// agentCheckShimProbe optionally launches one installed shim to prove the whole
// shim -> target chain works. Any exit code is accepted: the goal is to observe a
// successful launch, not to interpret the tool's output.
func agentCheckShimProbe(root string, enabled bool) DoctorCheck {
	c := DoctorCheck{ID: message.AgentCheckShimProbe, Level: AgentLevelAdvisory}
	if !enabled {
		c.Status = AgentStatusSkipped
		c.DetailKey = message.AgentShimProbeSkipped
		c.DetailText = "pass --probe-shim to launch one installed shim"
		return c
	}

	mgr, err := shim.NewManager(root)
	if err != nil {
		return agentShimProbeSkipped(c, err.Error())
	}
	configs, err := mgr.List()
	if err != nil {
		return agentShimProbeSkipped(c, err.Error())
	}

	sort.Slice(configs, func(i, j int) bool { return configs[i].Name < configs[j].Name })
	name, target := "", ""
	for _, cfg := range configs {
		if _, statErr := os.Stat(cfg.Path); statErr == nil {
			name, target = cfg.Name, cfg.Path
			break
		}
	}
	if name == "" {
		return agentShimProbeSkipped(c, "no installed shim to probe")
	}

	probeCtx, cancel := context.WithTimeout(context.Background(), agentShimProbeTimeout)
	defer cancel()
	shimExe := filepath.Join(mgr.BinDir(), name+".exe")
	cmd := exec.CommandContext(probeCtx, shimExe, "--version")
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard

	start := time.Now()
	runErr := cmd.Run()
	elapsed := time.Since(start)

	exitCode := 0
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		exitCode = exitErr.ExitCode()
	} else if runErr != nil {
		exitCode = -1
	}
	timedOut := probeCtx.Err() != nil

	c.Data = map[string]any{
		"shim":        name,
		"target":      target,
		"exit_code":   exitCode,
		"duration_ms": elapsed.Milliseconds(),
		"timed_out":   timedOut,
	}
	if exitCode >= 0 && !timedOut {
		c.OK = true
		c.DetailKey = message.AgentShimProbeOK
		c.DetailText = fmt.Sprintf("%s launched (exit %d, %dms)", name, exitCode, elapsed.Milliseconds())
	} else {
		c.DetailKey = message.AgentShimProbeFailed
		c.DetailText = fmt.Sprintf("%s failed to launch", name)
		c.HintKey = message.AgentHintShimProbe
		c.Hint = doctorHint(c.HintKey)
	}
	c.Status = statusFromOK(c.OK)
	return c
}

// agentShimProbeSkipped records that a shim probe could not be attempted.
func agentShimProbeSkipped(c DoctorCheck, detail string) DoctorCheck {
	c.Status = AgentStatusSkipped
	c.DetailKey = message.AgentShimProbeSkipped
	c.DetailText = detail
	return c
}

// agentCheckMCP reports whether the agent-native MCP entry point exists.
func agentCheckMCP(available bool) DoctorCheck {
	c := DoctorCheck{ID: message.AgentCheckMCP, Level: AgentLevelAdvisory}
	c.Data = map[string]any{"available": available, "command": "glue mcp"}
	if available {
		c.OK = true
		c.DetailKey = message.AgentMCPAvailable
		c.DetailText = "glue mcp (stdio)"
	} else {
		c.DetailKey = message.AgentMCPUnavailable
		c.HintKey = message.AgentHintMCP
		c.Hint = doctorHint(c.HintKey)
	}
	c.Status = statusFromOK(c.OK)
	return c
}

// agentCheckPolicy reports the agent policy gate state (roadmap section 4.6.2).
// The gates shipped with the MCP phase, so the check reads config.json and
// reports the effective policy instead of assuming the keys do not exist: an
// explicit setting (auto_yes, a non-confirm mode, deny or protected entries)
// passes; pure defaults stay advisory with those defaults spelled out.
func agentCheckPolicy(root string) DoctorCheck {
	c := DoctorCheck{ID: message.AgentCheckPolicy, Level: AgentLevelAdvisory}
	settings, err := config.ReadAgent(root)
	if err != nil {
		c.DetailKey = message.AgentPolicyNotConfigured
		c.DetailText = fmt.Sprintf("cannot read agent policy: %v", err)
		c.HintKey = message.AgentHintPolicy
		c.Hint = doctorHint(c.HintKey)
		c.Status = statusFromOK(c.OK)
		return c
	}

	deny := settings.Policy.Deny
	if deny == nil {
		deny = []string{}
	}
	protected := settings.Policy.Protected
	if protected == nil {
		protected = []string{}
	}

	configured := settings.AutoYes ||
		settings.Policy.Mode != config.AgentPolicyModeConfirm ||
		len(deny) > 0 || len(protected) > 0
	c.OK = configured
	c.Data = map[string]any{
		"configured": configured,
		"mode":       settings.Policy.Mode,
		"auto_yes":   settings.AutoYes,
		"deny":       deny,
		"protected":  protected,
	}
	if configured {
		c.DetailKey = message.AgentPolicyConfigured
		c.DetailText = fmt.Sprintf("mode=%s auto_yes=%t deny=%d protected=%d",
			settings.Policy.Mode, settings.AutoYes, len(deny), len(protected))
	} else {
		c.DetailKey = message.AgentPolicyDefaults
		c.DetailText = fmt.Sprintf("defaults in effect: mode=%s, no deny/protected entries", settings.Policy.Mode)
		c.HintKey = message.AgentHintPolicy
		c.Hint = doctorHint(c.HintKey)
	}
	c.Status = statusFromOK(c.OK)
	return c
}

// CommonTool is one toolchain entry probed by glue doctor (readiness view).
type CommonTool struct {
	Name   string
	Lookup []string
	// VersionArgs are run against the resolved path to print a version;
	// empty means presence-only (PowerShell startup and 7z output are
	// not worth a child process on every doctor run).
	VersionArgs []string
}

// CommonToolProbe is one data.tools row behind the toolchain and agent checks.
type CommonToolProbe struct {
	Name    string `json:"name"`
	Found   bool   `json:"found"`
	Path    string `json:"path,omitempty"`
	Version string `json:"version,omitempty"`
}

// CommonTools returns the toolchain probed on every readiness run: git and 7z
// matter to Glue itself, the rest cover typical agent workloads.
func CommonTools() []CommonTool {
	return []CommonTool{
		{Name: "git", Lookup: []string{"git"}, VersionArgs: []string{"--version"}},
		{Name: "node", Lookup: []string{"node"}, VersionArgs: []string{"-v"}},
		{Name: "npm", Lookup: []string{"npm"}, VersionArgs: []string{"--version"}},
		{Name: "python", Lookup: []string{"python", "python3"}, VersionArgs: []string{"--version"}},
		{Name: "powershell", Lookup: []string{"pwsh", "powershell"}},
		{Name: "7z", Lookup: []string{"7z"}},
	}
}

// agentVersionPattern matches dotted versions like 2.49.0 or 22.18 in tool output.
var agentVersionPattern = regexp.MustCompile(`\d+(?:\.\d+)+`)

// probeToolVersion runs path with args (best effort, 3s bound) and extracts the
// first dotted version from combined output; failures yield "".
func probeToolVersion(path string, args []string) string {
	if path == "" || len(args) == 0 {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, path, args...).CombinedOutput()
	return agentVersionPattern.FindString(string(out))
}

// ProbeCommonToolsDetailed reports every common tool with resolved path and
// best-effort version; drives the per-tool sub-rows of the human report.
func ProbeCommonToolsDetailed() []CommonToolProbe {
	probes := make([]CommonToolProbe, 0, len(CommonTools()))
	for _, tool := range CommonTools() {
		p := CommonToolProbe{Name: tool.Name}
		for _, name := range tool.Lookup {
			if resolved, err := exec.LookPath(name); err == nil {
				p.Path = resolved
				p.Found = true
				break
			}
		}
		if p.Found {
			p.Version = probeToolVersion(p.Path, tool.VersionArgs)
		}
		probes = append(probes, p)
	}
	return probes
}

// ProbeCommonTools reports which common tools resolve on PATH.
// found maps tool name to resolved path; missing is the sorted absent list.
func ProbeCommonTools() (found map[string]string, missing []string) {
	found = map[string]string{}
	for _, p := range ProbeCommonToolsDetailed() {
		if p.Found {
			found[p.Name] = p.Path
			continue
		}
		missing = append(missing, p.Name)
	}
	sort.Strings(missing)
	return found, missing
}

// PathDirPrecedes reports whether earlier appears before later in a PATH list.
// Readiness uses it to detect Microsoft Store aliases shadowing Glue shims.
func PathDirPrecedes(pathList, earlier, later string) bool {
	earlier = strings.TrimRight(earlier, `\`)
	later = strings.TrimRight(later, `\`)
	sawEarlier := false
	for _, p := range filepath.SplitList(pathList) {
		p = strings.TrimRight(strings.TrimSpace(p), `\`)
		if strings.EqualFold(p, later) {
			return sawEarlier
		}
		if strings.EqualFold(p, earlier) {
			sawEarlier = true
		}
	}
	return false
}
