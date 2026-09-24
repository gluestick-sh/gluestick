package main

import (
	"fmt"

	"github.com/gluestick-sh/core/engine"
	"github.com/gluestick-sh/core/message"
	"github.com/spf13/cobra"
)

// doctorCmd reports machine readiness: "is this machine agent-ready?" with a
// zero-interaction verdict (exit 0/1, --json for machines). The traditional
// environment checks live in `glue env`.
var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Check agent readiness",
	// Cobra errors are silenced so a not-ready verdict keeps stderr empty; the
	// argument hint therefore prints itself and still exits 2 (usage error).
	Args: func(cmd *cobra.Command, args []string) error {
		if len(args) > 0 {
			fmt.Fprintf(cmd.ErrOrStderr(), "glue doctor takes no arguments (got %q)\n", args[0])
			return wrapUsageError(fmt.Errorf("accepts 0 arg(s), received %d", len(args)))
		}
		return nil
	},
	RunE: runDoctor,
}

func init() {
	rootCmd.AddCommand(doctorCmd)
	doctorCmd.SilenceUsage = true
	doctorCmd.SilenceErrors = true
	doctorCmd.Flags().Bool("offline", false, "skip network probes (readiness mode: the network check reports skipped)")
	doctorCmd.Flags().Bool("probe-shim", false, "launch one installed shim to prove tools can run (readiness mode)")
	doctorCmd.Flags().Bool("fix", false, "apply safe fixes (PATH, buckets, UTF-8, PowerShell, git, profiles) and re-check (readiness mode)")
}

func runDoctor(cmd *cobra.Command, _ []string) error {
	offline, _ := cmd.Flags().GetBool("offline")
	probeShim, _ := cmd.Flags().GetBool("probe-shim")
	fix, _ := cmd.Flags().GetBool("fix")
	return runReadinessDoctor(cmd, offline, probeShim, fix)
}

// runEnvDoctor runs the traditional environment report behind `glue env`.
func runEnvDoctor(cmd *cobra.Command) error {
	config := &engine.EngineConfig{
		RootDir: glueRoot(),
		Workers: 1,
	}
	eng, err := engine.NewEngine(config)
	if err != nil {
		return fmt.Errorf("initialize engine: %w", err)
	}
	defer eng.Close()

	report := eng.RunDoctor(cmd.Context())
	if jsonOutputEnabled() {
		return emitJSON(report)
	}
	failures := 0
	for _, check := range report.Checks {
		mark := markSuccess
		if !check.OK {
			mark = markFail
			failures++
		}
		detail := formatDoctorDetail(check)
		fmt.Printf("%s %s: %s\n", mark, doctorCheckLabel(check.ID), detail)
		if check.Hint != "" {
			fmt.Printf("    → %s\n", check.Hint)
		}
	}
	fmt.Println()
	if failures == 0 {
		fmt.Println("Environment check passed.")
		return nil
	}
	fmt.Printf("Found %d issue(s); see hints above.\n", failures)
	return reportedFail()
}

// doctorCheckLabel maps engine check IDs to human-readable CLI labels.
func doctorCheckLabel(id string) string {
	switch id {
	case message.DoctorCheckGlueRoot:
		return "Glue data directory"
	case message.DoctorCheckGit:
		return "Git"
	case message.DoctorCheckSevenZip:
		return "7-Zip"
	case message.DoctorCheckDark:
		return "WiX dark"
	case message.DoctorCheckInnounp:
		return "innounp"
	case message.DoctorCheckShimDir:
		return "Shim directory"
	case message.DoctorCheckGitHub:
		return "GitHub connectivity"
	default:
		return id
	}
}

// formatDoctorDetail renders a check line from i18n keys and optional extra text.
func formatDoctorDetail(check engine.DoctorCheck) string {
	if check.DetailKey != "" {
		base := message.FormatEN(check.DetailKey, nil)
		if check.DetailText != "" {
			return base + " (" + check.DetailText + ")"
		}
		return base
	}
	return check.DetailText
}
