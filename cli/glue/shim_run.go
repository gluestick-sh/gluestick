package main

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/gluestick-sh/core/shim"
)

// shim-run is an internal compatibility command, intentionally hidden from normal help output.
//
// Design intent:
//   - Preferred shim path on Windows is shim.exe stubs created by core/shim.Manager.
//   - If shim.exe is unavailable, core/shim falls back to generating .bat shims that invoke:
//       glue shim-run <name> [args...]
//   - This command loads shims-meta/<name>.json and delegates execution to shim.Manager.Run.
//
// In short: this command is a fallback execution bridge for generated batch shims, not a user-facing API.
var shimRunCmd = &cobra.Command{
	Use:    "shim-run <name> [args...]",
	Short:  "Internal command to run a shim (used by generated shims)",
	Hidden: true,
	Args:   cobra.MinimumNArgs(1),
	RunE:   runShimRun,
}

func init() {
	rootCmd.AddCommand(shimRunCmd)
}

// runShimRun executes one shim by name with passthrough args.
// The shim metadata and target resolution are handled inside shim.Manager.
func runShimRun(cmd *cobra.Command, args []string) error {
	root := glueRoot()

	shimMgr, err := shim.NewManager(root)
	if err != nil {
		return fmt.Errorf("initialize shim manager: %w", err)
	}

	name := args[0]
	shimArgs := args[1:]

	if err := shimMgr.Run(name, shimArgs...); err != nil {
		return fmt.Errorf("run shim: %w", err)
	}

	return nil
}
