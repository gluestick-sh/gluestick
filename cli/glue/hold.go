package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// holdCmd prevents a package from being upgraded (Scoop-compatible).
var holdCmd = &cobra.Command{
	Use:   "hold <package>...",
	Short: "Prevent package from being upgraded",
	Args:  cobra.MinimumNArgs(1),
	RunE:  runHold,
}

// unholdCmd re-enables upgrades for a held package.
var unholdCmd = &cobra.Command{
	Use:   "unhold <package>...",
	Short: "Allow package upgrades again",
	Args:  cobra.MinimumNArgs(1),
	RunE:  runUnhold,
}

func init() {
	rootCmd.AddCommand(holdCmd)
	rootCmd.AddCommand(unholdCmd)
}

func runHold(cmd *cobra.Command, args []string) error {
	return setVersionLock(cmd, args, true)
}

func runUnhold(cmd *cobra.Command, args []string) error {
	return setVersionLock(cmd, args, false)
}

func setVersionLock(_ *cobra.Command, args []string, locked bool) error {
	eng, err := openCLIEngine()
	if err != nil {
		return fmt.Errorf("initialize engine: %w", err)
	}
	defer eng.Close()

	verb := "held"
	if !locked {
		verb = "unheld"
	}

	var failed []string
	for _, pkgRef := range args {
		name := packageBaseName(pkgRef)
		if err := eng.SetPackageVersionLock(name, locked); err != nil {
			fmt.Printf("  %s %s: %v\n", markFail, pkgRef, err)
			failed = append(failed, name)
			continue
		}
		fmt.Printf("  %s %s %s\n", markSuccess, name, verb)
	}
	if len(failed) > 0 {
		fmt.Printf("\nFailed: %s\n", strings.Join(failed, ", "))
		return reportedFail()
	}
	return nil
}
