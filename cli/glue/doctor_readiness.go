package main

import (
	"fmt"
	"strings"

	"github.com/gluestick-sh/cli/version"
	"github.com/gluestick-sh/core/engine"
	"github.com/gluestick-sh/core/message"
	"github.com/spf13/cobra"
)

// runReadinessDoctor implements the default view of glue doctor: a
// machine-readable "is this machine agent-ready?" verdict — --json output plus
// exit code 0 (ready) / 1 (not ready). --fix applies the safe remediations
// first and reports the re-run state.
func runReadinessDoctor(cmd *cobra.Command, offline, probeShim, fix bool) error {
	eng, err := openCLIEngine()
	if err != nil {
		return fmt.Errorf("initialize engine: %w", err)
	}
	defer eng.Close()

	opts := engine.AgentDoctorOptions{
		Offline:          offline,
		ProbeShim:        probeShim,
		CLIVersion:       version.CLIVersion(),
		DataRootIsolated: dataRootIsolated(),
		// glue mcp exists now (cli/glue/mcp.go), so the readiness check passes.
		MCPAvailable: true,
	}
	report := eng.RunAgentDoctor(cmd.Context(), opts)
	report.Command = "doctor"

	if fix {
		// Apply first, then re-run: the report below is the post-fix state, and
		// the human view lists every applied fix between sections and Result.
		fixes := applyDoctorFixes(cmd.Context(), eng, report, offline)
		report = eng.RunAgentDoctor(cmd.Context(), opts)
		report.Command = "doctor"
		report.Fixes = fixes
	}

	// Best-effort audit row so `glue audit list` shows every readiness run and
	// why it failed; an audit failure must never fail the check itself.
	_ = eng.RecordAgentDoctorActivity(report)

	if jsonOutputEnabled() {
		if err := emitJSON(report); err != nil {
			return err
		}
	} else {
		writeAgentDoctorReport(report, fix)
	}
	if !report.AgentReady {
		return reportedFail()
	}
	return nil
}

// dataRootIsolated reports whether this build uses an isolated data root:
// a dev build named glue-alpha.exe resolves to ~/.glue-alpha instead of ~/.glue.
func dataRootIsolated() bool {
	return strings.HasPrefix(defaultDataDir(), ".glue-")
}

// agentCheckLabel maps agent readiness check IDs to human-readable labels.
func agentCheckLabel(id string) string {
	switch id {
	case message.AgentCheckDataRoot:
		return "data root"
	case message.AgentCheckShimPath:
		return "shims on PATH"
	case message.AgentCheckShimRunner:
		return "shim runner"
	case message.AgentCheckInstallBackend:
		return "install backend"
	case message.AgentCheckBuckets:
		return "buckets"
	case message.AgentCheckNetwork:
		return "network"
	case message.AgentCheckToolchain:
		return "toolchain"
	case message.AgentCheckShimProbe:
		return "shim probe"
	case message.AgentCheckMCP:
		return "MCP server"
	case message.AgentCheckPolicy:
		return "agent policy"
	case message.AgentCheckMachineOS:
		return "OS"
	case message.AgentCheckMachineArch:
		return "architecture"
	case message.AgentCheckShellPWSH:
		return "PowerShell"
	case message.AgentCheckShellUTF8:
		return "output encoding"
	case message.AgentCheckShellExecPolicy:
		return "exec policy"
	case message.AgentCheckShellGitBash:
		return "Git Bash"
	case message.AgentCheckShellIntegration:
		return "shell integration"
	case message.AgentCheckDuplicates:
		return "duplicate runtimes"
	case message.AgentCheckGitConfig:
		return "git config"
	case message.AgentCheckAgents:
		return "agent CLIs"
	case message.AgentCheckWorkspaceFS:
		return "filesystem"
	case message.AgentCheckWorkspaceWSL:
		return "WSL"
	case message.AgentCheckWorkspacePath:
		return "data path"
	default:
		return id
	}
}

// agentGroupTitle maps checks[].group to the section header of the human view.
func agentGroupTitle(group string) string {
	switch group {
	case engine.AgentGroupMachine:
		return "Machine"
	case engine.AgentGroupShell:
		return "Shell"
	case engine.AgentGroupRuntime:
		return "Runtime"
	case engine.AgentGroupAgents:
		return "Agent compatibility"
	case engine.AgentGroupWorkspace:
		return "Workspace"
	case engine.AgentGroupGlue:
		return "Glue"
	default:
		return "Other"
	}
}

// agentMark picks the status mark: ✓ pass, ✗ blocking fail, ⚠ advisory fail,
// — skipped. Colors follow the NO_COLOR-aware global state (plain when piped).
func agentMark(check engine.DoctorCheck) string {
	switch {
	case check.Status == engine.AgentStatusSkipped:
		return markSkip
	case check.Status == engine.AgentStatusPass:
		return markSuccess
	case check.Level == engine.AgentLevelAdvisory:
		return markWarn
	default:
		return markFail
	}
}

// writeAgentDoctorReport renders the grouped human view: one section per
// report group, then Result (verdict + score + next command) and the --fix
// footer when safe fixes are pending.
func writeAgentDoctorReport(report engine.AgentDoctorReport, fixRan bool) {
	fmt.Printf("Glue agent readiness — %s", report.DataRoot)
	if report.CLIVersion != "" {
		// CLIVersion carries commit and build date; the header only needs the number.
		version := report.CLIVersion
		if short, _, found := strings.Cut(version, " ("); found {
			version = short
		}
		fmt.Printf(" (glue %s)", version)
	}
	fmt.Println()
	fmt.Println()

	lastGroup := ""
	for _, check := range report.Checks {
		if check.Group != lastGroup {
			if lastGroup != "" {
				fmt.Println()
			}
			fmt.Println(agentGroupTitle(check.Group))
			lastGroup = check.Group
		}
		writeAgentCheckRow(check)
	}
	fmt.Println()

	// After the sections and before Result: the per-item fix list, so every
	// modification --fix made is visible next to the verdict it improved.
	if fixRan {
		writeDoctorFixLines(report.Fixes)
	}

	fmt.Println("Result")
	if report.AgentReady {
		fmt.Printf("  Agent Ready: yes — score %d%%\n", report.Score)
	} else {
		fmt.Printf("  Agent Ready: no — %d required check(s) failed (%s), score %d%%\n",
			len(report.Summary.BlockingFailed), strings.Join(report.Summary.BlockingFailed, ", "), report.Score)
		if len(report.NextActions) > 0 {
			fmt.Printf("  Next: %s\n", strings.Join(report.NextActions, "; "))
		}
	}
	if !fixRan {
		if n := len(doctorFixableChecks(report)); n > 0 {
			fmt.Printf("\nRun `glue doctor --fix` to apply %d safe fix(es).\n", n)
		}
	}
}

// writeAgentCheckRow prints one check row, expanding data.tools into per-tool
// sub-rows (toolchain versions, agent CLI presence).
func writeAgentCheckRow(check engine.DoctorCheck) {
	if tools, ok := check.Data["tools"].([]engine.CommonToolProbe); ok && len(tools) > 0 {
		for _, tool := range tools {
			mark, detail := markWarn, "not on PATH"
			if tool.Found {
				mark = markSuccess
				detail = tool.Version
				if detail == "" {
					detail = tool.Path
				}
			}
			fmt.Printf("  %s %-24s %s\n", mark, tool.Name, detail)
		}
		if check.Status == engine.AgentStatusFail && check.Hint != "" {
			fmt.Printf("       → %s\n", check.Hint)
		}
		return
	}
	// Duplicate runtimes: one sub-row per tool listing every PATH hit. The
	// label carries the copy count ("git (4)") so it cannot be mistaken for the
	// toolchain row above it ("✓ git 2.54.0"), which is a different check.
	if dups, ok := check.Data["duplicates"].([]engine.DuplicateTool); ok && len(dups) > 0 {
		for _, d := range dups {
			fmt.Printf("  %s %-24s %s\n", markWarn, fmt.Sprintf("%s (%d)", d.Name, len(d.Paths)), strings.Join(d.Paths, " ; "))
		}
		if check.Status == engine.AgentStatusFail && check.Hint != "" {
			fmt.Printf("       → %s\n", check.Hint)
		}
		return
	}
	fmt.Printf("  %s %-24s %s\n", agentMark(check), agentCheckLabel(check.ID), formatDoctorDetail(check))
	if check.Status == engine.AgentStatusFail && check.Hint != "" {
		fmt.Printf("       → %s\n", check.Hint)
	}
}
