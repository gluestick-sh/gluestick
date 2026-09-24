package main

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/gluestick-sh/core/engine"
	"github.com/gluestick-sh/core/message"
)

// doctorCmd runs environment checks (data dir, git, 7z, shims, GitHub, etc.).
var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Check Glue environment and dependencies",
	RunE:  runDoctor,
}

func init() {
	rootCmd.AddCommand(doctorCmd)
}

func runDoctor(cmd *cobra.Command, _ []string) error {
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
