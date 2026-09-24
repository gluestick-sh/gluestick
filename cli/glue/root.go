// Command glue is the Scoop-compatible CLI for installing and managing packages via github.com/gluestick-sh/core/engine.
package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/gluestick-sh/core/engine"
	"github.com/gluestick-sh/cli/version"
)

// errReported indicates failure was already printed; suppress duplicate summaries in Execute.
var errReported = errors.New("operation failed")

func reportedFail() error { return errReported }

var rootCmd = &cobra.Command{
	Use:     "glue",
	Version: version.CLIVersion(),
}

// Execute runs the root command
func Execute() {

	exePath, err := os.Executable()
	if err == nil {
		exeName := filepath.Base(exePath)
		exeName = strings.TrimSuffix(exeName, ".exe")

		rootCmd.Use = exeName + " [command]"
	}

	rootCmd.SetFlagErrorFunc(func(c *cobra.Command, err error) error {
		c.SilenceUsage = false
		return wrapUsageError(err)
	})

	if err := rootCmd.Execute(); err != nil {
		code := exitCode(err)
		if code == 1 && !errors.Is(err, errReported) {
			fmt.Fprintln(os.Stderr, err)
		}
		os.Exit(code)
	}
}

func init() {
	cobra.OnInitialize(initConfig)

	initJSONOutput()

	// Hidden: used by benchmark scripts only; normal installs use ~/.glue.
	rootCmd.PersistentFlags().String("root", "", "")
	_ = rootCmd.PersistentFlags().MarkHidden("root")
	rootCmd.PersistentFlags().Bool("verbose", false, "print detailed progress (mirrors, retries, failed URLs)")
}

// glueRoot returns the glue data directory (~/.glue).
func glueRoot() string {
	if root, _ := rootCmd.PersistentFlags().GetString("root"); root != "" {
		return root
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), ".glue")
	}
	return filepath.Join(home, ".glue")
}

func initConfig() {
	if cfg, err := loadConfig(glueRoot()); err == nil {
		applyConfig(cfg)
	} else {
		applyConfig(nil)
	}

	// Quiet notes for git/7z when missing; skip on glue doctor (reported there instead).
	if !isDoctorCommand() && !jsonOutputEnabled() {
		engine.WriteStartupToolNotes(os.Stderr, glueRoot())
	}
}

// isDoctorCommand reports whether the CLI was invoked as glue doctor.
func isDoctorCommand() bool {
	for _, arg := range os.Args[1:] {
		if strings.HasPrefix(arg, "-") {
			continue
		}
		return arg == "doctor"
	}
	return false
}

func main() {
	Execute()
}
