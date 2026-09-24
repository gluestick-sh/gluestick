package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/gluestick-sh/core/engine"
	"github.com/gluestick-sh/core/verbose"
)

// uninstallCmd removes installed packages via the engine; --purge drops cache index entries too.
var uninstallCmd = &cobra.Command{
	Use:     "uninstall <package[@version]>...",
	Short:   "Uninstall packages",
	Aliases: []string{"remove", "rm"},
	Args:    cobra.MinimumNArgs(1),
	RunE:    runUninstall,
}

var uninstallPurge bool

func init() {
	rootCmd.AddCommand(uninstallCmd)
	uninstallCmd.SilenceUsage = true
	uninstallCmd.SilenceErrors = true
	uninstallCmd.Flags().BoolVarP(&uninstallPurge, "purge", "p", false, "also remove cache index and store files")
}

func runUninstall(cmd *cobra.Command, args []string) error {
	root := glueRoot()

	eng, err := engine.NewEngine(&engine.EngineConfig{RootDir: root, Verbose: verbose.Enabled()})
	if err != nil {
		return fmt.Errorf("initialize engine: %w", err)
	}
	defer eng.Close()

	reporter := installReporter()

	var failed []string
	var items []jsonResultItem
	start := time.Now()

	for _, pkgRef := range args {
		req := &engine.UninstallRequest{
			Request: engine.Request{Name: pkgRef},
			Purge:   uninstallPurge,
		}
		result, err := eng.Uninstall(cmd.Context(), req, reporter)
		if err != nil {
			if !jsonOutputEnabled() {
				verbose.Progressf("  %s Failed: %v\n", markFail, err)
			}
			items = append(items, jsonResultItemFromInstall(pkgRef, result, err))
			failed = append(failed, packageBaseName(pkgRef))
			continue
		}
		items = append(items, jsonResultItemFromInstall(pkgRef, result, nil))
	}

	if jsonOutputEnabled() {
		if err := jsonOperationResult("uninstall", items); err != nil {
			return err
		}
		if len(failed) > 0 {
			return reportedFail()
		}
		return nil
	}

	verbose.Progressf("\n")
	if len(failed) > 0 {
		verbose.Progressf("Failed to uninstall: %s\n", strings.Join(failed, ", "))
		return reportedFail()
	}
	verbose.Progressf("Done in %s\n", time.Since(start).Round(time.Millisecond))
	return nil
}
