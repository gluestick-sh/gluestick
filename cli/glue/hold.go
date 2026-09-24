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
	for _, c := range []*cobra.Command{holdCmd, unholdCmd} {
		c.SilenceUsage = true
		c.SilenceErrors = true
	}
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
	command := "hold"
	if !locked {
		verb = "unheld"
		command = "unhold"
	}

	items := make([]jsonResultItem, 0, len(args))
	var failed []string
	for _, pkgRef := range args {
		name := packageBaseName(pkgRef)
		if err := eng.SetPackageVersionLock(name, locked); err != nil {
			item := jsonResultItem{Ref: pkgRef, Error: err.Error()}
			item.Code, item.Hint = jsonErrorInfo(err)
			items = append(items, item)
			failed = append(failed, name)
			continue
		}
		items = append(items, jsonResultItem{Ref: pkgRef})
	}

	if jsonOutputEnabled() {
		if err := jsonOperationResult(command, items); err != nil {
			return err
		}
		if len(failed) > 0 {
			return reportedFail()
		}
		return nil
	}

	for _, item := range items {
		if item.Error != "" {
			fmt.Printf("  %s %s: %s\n", markFail, item.Ref, item.Error)
			continue
		}
		fmt.Printf("  %s %s %s\n", markSuccess, packageBaseName(item.Ref), verb)
	}
	if len(failed) > 0 {
		fmt.Printf("\nFailed: %s\n", strings.Join(failed, ", "))
		return reportedFail()
	}
	return nil
}
