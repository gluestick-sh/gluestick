package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/gluestick-sh/core/engine"
)

// resetCmd switches the active version symlink and rebuilds shims without re-downloading.
var resetCmd = &cobra.Command{
	Use:   "reset <package[@version]>...",
	Short: "Switch active version and rebuild shims",
	Args:  cobra.MinimumNArgs(1),
	RunE:  runReset,
}

func init() {
	rootCmd.AddCommand(resetCmd)
	resetCmd.SilenceUsage = true
	resetCmd.SilenceErrors = true
}

func runReset(cmd *cobra.Command, args []string) error {
	root := glueRoot()

	eng, err := engine.NewEngine(&engine.EngineConfig{RootDir: root})
	if err != nil {
		return fmt.Errorf("initialize engine: %w", err)
	}
	defer eng.Close()

	var failed []string
	for _, pkgRef := range args {
		pkgName := packageBaseName(pkgRef)
		fmt.Printf("Resetting %s...\n", pkgRef)
		if err := eng.ResetPackage(pkgRef); err != nil {
			fmt.Printf("  %s Failed: %v\n", markFail, err)
			failed = append(failed, pkgName)
			continue
		}
		fmt.Printf("  %s %s reset\n", markSuccess, pkgRef)
	}

	if len(failed) > 0 {
		fmt.Printf("\nFailed to reset: %s\n", strings.Join(failed, ", "))
		return reportedFail()
	}
	return nil
}
