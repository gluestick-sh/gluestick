package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

// homeCmd opens a package homepage in the default browser (Scoop-compatible).
var homeCmd = &cobra.Command{
	Use:   "home <package>...",
	Short: "Open package homepage in the default browser",
	Args:  cobra.MinimumNArgs(1),
	RunE:  runHome,
}

func init() {
	rootCmd.AddCommand(homeCmd)
}

func runHome(cmd *cobra.Command, args []string) error {
	eng, err := openCLIEngine()
	if err != nil {
		return fmt.Errorf("initialize engine: %w", err)
	}
	defer eng.Close()

	ctx := cmd.Context()
	var failed []string
	for _, pkgRef := range args {
		url, err := eng.PackageHomepage(ctx, pkgRef)
		if err != nil {
			fmt.Printf("  %s %s: %v\n", markFail, pkgRef, err)
			failed = append(failed, pkgRef)
			continue
		}
		if err := openBrowser(url); err != nil {
			fmt.Printf("  %s %s: %v\n", markFail, pkgRef, err)
			failed = append(failed, pkgRef)
			continue
		}
		fmt.Printf("  %s Opened %s\n", markSuccess, url)
	}
	if len(failed) > 0 {
		return reportedFail()
	}
	return nil
}
