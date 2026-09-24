package main

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"github.com/gluestick-sh/core/downloader"
	"github.com/gluestick-sh/core/engine"
	"github.com/gluestick-sh/core/verbose"
)

// installCmd installs one or more packages via the engine (deps, download, cache store, shims).
var installCmd = &cobra.Command{
	Use:     "install <package>...",
	Short:   "Install packages",
	Aliases: []string{"add", "i"},
	Args:    cobra.MinimumNArgs(1),
	RunE:    runInstall,
}

var (
	installWorkers     int
	installForce       bool
	installInteractive bool
	installProfileFlag bool
	installNoParallel  bool
)

func init() {
	rootCmd.AddCommand(installCmd)
	installCmd.SilenceUsage = true
	installCmd.SilenceErrors = true
	installCmd.Flags().IntVarP(&installWorkers, "jobs", "j", downloader.DefaultWorkers, "parallel download connections and extract/cache store workers")
	installCmd.Flags().BoolVarP(&installForce, "force", "f", false, "force reinstall: skip cache and discard partial downloads")
	installCmd.Flags().BoolVar(&installInteractive, "interactive", false, "show installer UI for packages that use installer.script (default: unattended)")
	installCmd.Flags().BoolVar(&installProfileFlag, "profile", false, "print install phase timings (download/store/extract/link/shim)")
	installCmd.Flags().BoolVar(&installNoParallel, "no-parallel", false, "disable parallel range downloads (single connection; for speed tests)")
}

func runInstall(cmd *cobra.Command, args []string) error {
	// Each package is installed sequentially; flags map to engine InstallRequest options.
	root := glueRoot()

	cfg, err := loadConfig(root)
	if err != nil {
		verbose.Progressf("  Warning: failed to read config.json: %v (using defaults)\n", err)
	}
	parallelDL := parallelDownloadEnabled(cfg)
	if installNoParallel {
		parallelDL = false
	}

	eng, err := engine.NewEngine(&engine.EngineConfig{
		RootDir:  root,
		Verbose:  verbose.Enabled(),
		Workers:  installWorkers,
		Parallel: parallelDL,
	})
	if err != nil {
		return fmt.Errorf("initialize engine: %w", err)
	}
	defer eng.Close()

	reporter := installReporter()

	var failed []string
	var items []jsonResultItem
	start := time.Now()

	for _, pkgRef := range args {
		req := &engine.InstallRequest{
			Request: engine.Request{
				Name:    pkgRef,
				Force:   installForce,
				Options: map[string]string{},
			},
		}
		// --profile: engine emits a GLUE_PROFILE line per package (see core/engine/profile_install.go).
		if installProfileFlag {
			req.Options["profile"] = "true"
		}
		if installInteractive {
			req.Options["interactive"] = "true"
		}

		result, err := eng.Install(cmd.Context(), req, reporter)
		failErr := installFailureError(err, result)
		if failErr != nil && engine.IsInstallResolveNotice(failErr) {
			if !jsonOutputEnabled() {
				verbose.Progressf("%s\n", engine.FormatInstallResolveNotice(failErr))
			}
			items = append(items, jsonResultItem{Ref: pkgRef, Error: engine.FormatInstallResolveNotice(failErr)})
			failed = append(failed, pkgRef)
			continue
		}
		if err != nil {
			if !jsonOutputEnabled() {
				verbose.Progressf("  %s Failed: %v\n", markFail, err)
			}
			items = append(items, jsonResultItemFromInstall(pkgRef, result, err))
			failed = append(failed, pkgRef)
			continue
		}
		if failErr != nil {
			if !jsonOutputEnabled() {
				verbose.Progressf("  %s Failed: %v\n", markFail, failErr)
			}
			items = append(items, jsonResultItemFromInstall(pkgRef, result, failErr))
			failed = append(failed, pkgRef)
			continue
		}
		items = append(items, jsonResultItemFromInstall(pkgRef, result, nil))
		disableWindowsPythonAliases()
	}

	if jsonOutputEnabled() {
		if err := jsonOperationResult("install", items); err != nil {
			return err
		}
		if len(failed) > 0 {
			return reportedFail()
		}
		return nil
	}

	verbose.Progressf("\n")
	if len(failed) > 0 {
		return reportedFail()
	}
	verbose.Progressf("Done in %s\n", time.Since(start).Round(time.Millisecond))
	return nil
}

func installFailureError(err error, result *engine.Result) error {
	if err != nil {
		return err
	}
	if result != nil && result.Error != nil {
		return result.Error
	}
	return nil
}
