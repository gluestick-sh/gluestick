package main

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/gluestick-sh/core/engine"
	"github.com/gluestick-sh/core/verbose"
)

var updateAll bool

// updateCmd lists or applies package upgrades (Scoop-compatible).
var updateCmd = &cobra.Command{
	Use:     "update [package]...",
	Short:   "List or install available package updates",
	Aliases: []string{"up"},
	RunE:    runUpdate,
}

func init() {
	rootCmd.AddCommand(updateCmd)
	updateCmd.SilenceUsage = true
	updateCmd.SilenceErrors = true
	updateCmd.Flags().BoolVarP(&updateAll, "all", "a", false, "upgrade all outdated packages")
}

func runUpdate(cmd *cobra.Command, args []string) error {
	eng, err := openCLIEngine()
	if err != nil {
		return fmt.Errorf("initialize engine: %w", err)
	}
	defer eng.Close()

	updates, err := eng.CheckPackageUpdates()
	if err != nil {
		return fmt.Errorf("check updates: %w", err)
	}
	if len(updates) == 0 {
		if jsonOutputEnabled() {
			return emitJSON(map[string]any{"updates": updates, "count": 0, "upToDate": true})
		}
		fmt.Println("All packages are up to date.")
		_ = eng.RecordCheckUpdatesActivity(0, "all up to date")
		return nil
	}

	targets := selectUpdateTargets(updates, args, updateAll)
	if len(targets) == 0 && len(args) == 0 && !updateAll {
		if jsonOutputEnabled() {
			return emitJSON(map[string]any{"updates": updates, "count": len(updates)})
		}
		printAvailableUpdates(updates)
		_ = eng.RecordCheckUpdatesActivity(len(updates), fmt.Sprintf("%d update(s) available", len(updates)))
		return nil
	}
	if len(targets) == 0 {
		if jsonOutputEnabled() {
			return emitJSON(map[string]any{"updates": []engine.PackageUpdate{}, "count": 0, "matched": false})
		}
		fmt.Println("No matching packages to update.")
		return nil
	}

	reporter := installReporter()
	var failed []string
	var items []jsonResultItem
	for _, u := range targets {
		ref := updateInstallRef(u)
		if !jsonOutputEnabled() {
			verbose.Progressf("Updating %s (%s -> %s)...\n", ref, u.InstalledVersion, u.LatestVersion)
		}
		req := &engine.InstallRequest{Request: engine.Request{Name: ref, Force: true}}
		result, err := eng.Install(cmd.Context(), req, reporter)
		if err != nil {
			if !jsonOutputEnabled() {
				fmt.Printf("  %s Failed: %v\n", markFail, err)
			}
			failed = append(failed, ref)
			items = append(items, jsonResultItemFromInstall(ref, result, err))
			continue
		}
		if !jsonOutputEnabled() {
			fmt.Printf("  %s %s updated to %s\n", markSuccess, ref, u.LatestVersion)
		}
		items = append(items, jsonResultItemFromInstall(ref, result, nil))
		disableWindowsPythonAliases()
	}
	if jsonOutputEnabled() {
		if err := jsonOperationResult("update", items); err != nil {
			return err
		}
		if len(failed) > 0 {
			return reportedFail()
		}
		_ = eng.RecordCheckUpdatesActivity(len(targets), fmt.Sprintf("updated %d package(s)", len(targets)))
		return nil
	}
	if len(failed) > 0 {
		return reportedFail()
	}
	_ = eng.RecordCheckUpdatesActivity(len(targets), fmt.Sprintf("updated %d package(s)", len(targets)))
	return nil
}

func updateInstallRef(u engine.PackageUpdate) string {
	if u.Bucket != "" && u.Bucket != "main" {
		return u.Bucket + "/" + u.Name
	}
	return u.Name
}

func printAvailableUpdates(updates []engine.PackageUpdate) {
	fmt.Printf("%s%d package(s) have updates:%s\n\n", colorBlue, len(updates), colorReset)
	for _, u := range updates {
		fmt.Printf("  %s: %s -> %s\n", updateInstallRef(u), u.InstalledVersion, u.LatestVersion)
	}
	fmt.Println("\nRun 'glue update --all' or 'glue update <package>' to upgrade.")
}

func selectUpdateTargets(updates []engine.PackageUpdate, args []string, all bool) []engine.PackageUpdate {
	if !all && len(args) == 0 {
		return nil
	}
	byName := make(map[string]engine.PackageUpdate, len(updates))
	for _, u := range updates {
		byName[u.Name] = u
	}
	if all {
		return updates
	}
	var out []engine.PackageUpdate
	for _, arg := range args {
		if arg == "*" {
			return updates
		}
		name := packageBaseName(arg)
		if u, ok := byName[name]; ok {
			out = append(out, u)
			continue
		}
		if !jsonOutputEnabled() {
			fmt.Printf("  %s %s: no update available\n", markFail, arg)
		}
	}
	return out
}
