//go:build windows

package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/gluestick-sh/core/shim"
)

var pathCmd = &cobra.Command{
	Use:   "path",
	Short: "Manage PATH integration",
}

var pathShowCmd = &cobra.Command{
	Use:   "show",
	Short: "Show the PATH entry for glue",
	RunE: func(cmd *cobra.Command, args []string) error {
		root := glueRoot()

		shimMgr, err := shim.NewManager(root)
		if err != nil {
			return fmt.Errorf("initialize shim manager: %w", err)
		}

		fmt.Println(shimMgr.BinDir())
		return nil
	},
}

var pathCheckCmd = &cobra.Command{
	Use:   "check",
	Short: "Check if Glue is in PATH",
	RunE: func(cmd *cobra.Command, args []string) error {
		root := glueRoot()

		shimMgr, err := shim.NewManager(root)
		if err != nil {
			return fmt.Errorf("initialize shim manager: %w", err)
		}

		if !shimMgr.InPath() {
			fmt.Println(markFail + " Glue is NOT in PATH")
			fmt.Println("\nAdd the following to your PATH:")
			fmt.Println(shimMgr.BinDir())
			fmt.Println("\nOr run: glue path setup")
			return reportedFail()
		}

		fmt.Println(markSuccess + " Glue is in PATH")
		apps := windowsAppsDir()
		if apps != "" && pathDirPrecedes(os.Getenv("PATH"), apps, shimMgr.BinDir()) {
			fmt.Println(markFail + " Microsoft Store python aliases may shadow Glue shims")
			fmt.Println("    → Run: glue path setup")
			return reportedFail()
		}
		return nil
	},
}

var pathSetupCmd = &cobra.Command{
	Use:   "setup",
	Short: "Add glue shims to the front of PATH (no admin required on Windows)",
	RunE: func(cmd *cobra.Command, args []string) error {
		root := glueRoot()

		shimMgr, err := shim.NewManager(root)
		if err != nil {
			return fmt.Errorf("initialize shim manager: %w", err)
		}

		return addToUserPath(shimMgr.BinDir())
	},
}

// addToUserPath puts dir first on the user PATH so Store aliases cannot shadow shims.
func addToUserPath(dir string) error {
	fmt.Printf("Putting %s first on user PATH...\n", dir)
	changed, err := ensureUserPathFront(dir)
	if err != nil {
		return err
	}
	prependDirToProcessPath(dir)
	disableWindowsPythonAliases()
	if !changed {
		fmt.Println(markSuccess + " glue shims already first on user PATH")
		return nil
	}
	fmt.Println(markSuccess + " Added to user PATH")
	fmt.Println("\n⚠ Restart your terminal for changes to take effect")
	return nil
}

func init() {
	rootCmd.AddCommand(pathCmd)
	pathCmd.AddCommand(pathShowCmd)
	pathCmd.AddCommand(pathCheckCmd)
	pathCmd.AddCommand(pathSetupCmd)
}
