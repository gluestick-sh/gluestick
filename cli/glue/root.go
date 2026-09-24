// Command glue is the Scoop-compatible CLI for installing and managing packages via github.com/gluestick-sh/core/engine.
package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gluestick-sh/cli/version"
	"github.com/gluestick-sh/core/engine"
	"github.com/spf13/cobra"
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

// glueRoot returns the glue data directory.
//
// The directory is derived from the executable name so development builds stay
// fully isolated from a real installation: glue.exe uses ~/.glue, while
// glue-alpha.exe (any glue-<suffix>) automatically uses ~/.glue-<suffix>.
// The hidden --root flag always wins.
func glueRoot() string {
	if root, _ := rootCmd.PersistentFlags().GetString("root"); root != "" {
		return root
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), defaultDataDir())
	}
	return filepath.Join(home, defaultDataDir())
}

// defaultDataDir derives the data directory name from the running executable:
// ".glue" for glue.exe, ".glue-<suffix>" for glue-<suffix>.exe (e.g. glue-alpha.exe),
// and ".glue" for anything else (including test binaries like glue.test).
func defaultDataDir() string {
	exePath, err := os.Executable()
	if err != nil {
		return ".glue"
	}
	if suffix, ok := executableDataDirSuffix(strings.TrimSuffix(filepath.Base(exePath), ".exe")); ok {
		return ".glue-" + suffix
	}
	return ".glue"
}

// executableDataDirSuffix reports the isolation suffix of an executable base name:
// "glue-alpha" → ("alpha", true); "glue" and anything else → ("", false).
func executableDataDirSuffix(name string) (string, bool) {
	if suffix, ok := strings.CutPrefix(name, "glue-"); ok && validDataDirSuffix(suffix) {
		return suffix, true
	}
	return "", false
}

// validDataDirSuffix allows only letters, digits, and dashes in a data-dir suffix.
func validDataDirSuffix(suffix string) bool {
	if suffix == "" {
		return false
	}
	for _, r := range suffix {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-':
		default:
			return false
		}
	}
	return true
}

func initConfig() {
	if cfg, err := loadConfig(glueRoot()); err == nil {
		applyConfig(cfg)
	} else {
		applyConfig(nil)
	}

	// Quiet notes for git/7z when missing; skip when the command's own report covers them.
	if !suppressesStartupToolNotes(os.Args[1:]) && !jsonOutputEnabled() {
		engine.WriteStartupToolNotes(os.Stderr, glueRoot())
	}
}

// suppressesStartupToolNotes reports whether the invocation is glue doctor or
// glue env: both report git/7z health themselves, so startup notes would be
// duplicate noise.
func suppressesStartupToolNotes(args []string) bool {
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			continue
		}
		return arg == "doctor" || arg == "env" || arg == "mcp"
	}
	return false
}

func main() {
	Execute()
}
