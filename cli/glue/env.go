package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

// envCmd reports environment health: data directory, git, 7z,
// WiX dark, innounp, the shim directory, and GitHub connectivity.
var envCmd = &cobra.Command{
	Use:   "env",
	Short: "Check environment health (git, 7z, shims, GitHub)",
	Args: func(cmd *cobra.Command, args []string) error {
		if len(args) > 0 {
			fmt.Fprintf(cmd.ErrOrStderr(), "glue env takes no arguments (got %q)\n", args[0])
			return wrapUsageError(fmt.Errorf("accepts 0 arg(s), received %d", len(args)))
		}
		return nil
	},
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runEnvDoctor(cmd)
	},
}

func init() {
	rootCmd.AddCommand(envCmd)
	envCmd.SilenceUsage = true
	envCmd.SilenceErrors = true
}
