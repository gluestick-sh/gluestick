package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/gluestick-sh/core/engine"
	"github.com/gluestick-sh/core/humanize"
)

// infoCmd shows metadata for installed packages (path, cache, shims, manifest fields).
var infoCmd = &cobra.Command{
	Use:   "info <package>...",
	Short: "Show information about installed packages",
	Args:  cobra.MinimumNArgs(1),
	RunE:  runInfo,
}

func init() {
	rootCmd.AddCommand(infoCmd)
}

func runInfo(cmd *cobra.Command, args []string) error {
	root := glueRoot()

	eng, err := engine.NewEngine(&engine.EngineConfig{RootDir: root})
	if err != nil {
		return fmt.Errorf("initialize engine: %w", err)
	}
	defer eng.Close()

	var failed []string
	var details []*engine.InstalledPackageDetail
	for i, pkgRef := range args {
		if i > 0 && !jsonOutputEnabled() {
			fmt.Println()
		}
		pkgName := packageBaseName(pkgRef)
		detail, err := eng.GetInstalledPackageDetail(pkgName)
		if err != nil {
			if jsonOutputEnabled() {
				failed = append(failed, pkgName)
				continue
			}
			fmt.Printf("  %s %v\n", markFail, err)
			failed = append(failed, pkgName)
			continue
		}
		details = append(details, detail)
		if !jsonOutputEnabled() {
			printPackageInfo(detail)
		}
	}

	if jsonOutputEnabled() {
		payload := map[string]any{"packages": details, "count": len(details)}
		if len(failed) > 0 {
			payload["failed"] = failed
		}
		if err := emitJSON(payload); err != nil {
			return err
		}
		if len(failed) > 0 {
			return reportedFail()
		}
		return nil
	}

	if len(failed) > 0 {
		return reportedFail()
	}
	return nil
}

func printPackageInfo(d *engine.InstalledPackageDetail) {
	fmt.Printf("%s%s@%s%s\n", colorBlue, d.Name, d.Version, colorReset)

	printInfoField("Status", "Installed")
	printInfoField("Path", d.InstallPath)
	printInfoField("Current", d.CurrentPath)

	if d.InstalledAt != "" {
		printInfoField("Installed", humanize.FormatCacheDate(d.InstalledAt))
		printInfoField("Size", fmt.Sprintf("%s (%d indexed files)", humanize.FormatBytes(d.Size), d.FileCount))
	} else {
		printInfoField("Size", fmt.Sprintf("%s (%d files)", humanize.FormatBytes(d.Size), d.FileCount))
	}

	if len(d.Shims) > 0 {
		shims := append([]string(nil), d.Shims...)
		sort.Strings(shims)
		printInfoField("Shims", strings.Join(shims, ", "))
	}

	if d.Bucket != "" {
		printInfoField("Bucket", d.Bucket)
	}
	if d.Description != "" {
		printInfoField("Description", d.Description)
	}
	if d.Homepage != "" {
		printInfoField("Homepage", d.Homepage)
	}
	if d.License != "" {
		printInfoField("License", d.License)
	}
	if len(d.Depends) > 0 {
		printInfoField("Dependencies", strings.Join(d.Depends, ", "))
	}
	if len(d.Notes) > 0 {
		printInfoField("Notes", strings.Join(d.Notes, "; "))
	}
	if d.UpdateAvailable {
		printInfoField("Manifest", fmt.Sprintf("%s (update available)", d.ManifestVersion))
	}
}

func printInfoField(label, value string) {
	fmt.Printf("  %-14s %s\n", label+":", value)
}
