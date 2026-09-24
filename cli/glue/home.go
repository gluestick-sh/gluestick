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

var homeNoOpen bool

func init() {
	rootCmd.AddCommand(homeCmd)
	homeCmd.SilenceUsage = true
	homeCmd.SilenceErrors = true
	homeCmd.Flags().BoolVar(&homeNoOpen, "no-open", false, "print the homepage URL without opening a browser")
}

// jsonHomeItem is one package entry in the glue home --json output.
type jsonHomeItem struct {
	Ref    string `json:"ref"`
	URL    string `json:"url,omitempty"`
	Opened bool   `json:"opened"`
	Error  string `json:"error,omitempty"`
	Code   string `json:"code,omitempty"`
	Hint   string `json:"hint,omitempty"`
}

func runHome(cmd *cobra.Command, args []string) error {
	eng, err := openCLIEngine()
	if err != nil {
		return fmt.Errorf("initialize engine: %w", err)
	}
	defer eng.Close()

	ctx := cmd.Context()

	// JSON mode never opens a browser: an agent cannot see one, and popups
	// during automation are unacceptable. URLs are returned in the payload.
	if jsonOutputEnabled() {
		items := make([]jsonHomeItem, 0, len(args))
		var failed int
		for _, pkgRef := range args {
			url, err := eng.PackageHomepage(ctx, pkgRef)
			if err != nil {
				item := jsonHomeItem{Ref: pkgRef, Error: err.Error()}
				item.Code, item.Hint = jsonErrorInfo(err)
				items = append(items, item)
				failed++
				continue
			}
			items = append(items, jsonHomeItem{Ref: pkgRef, URL: url})
		}
		if err := emitJSON(map[string]any{
			"command": "home",
			"ok":      failed == 0,
			"results": items,
		}); err != nil {
			return err
		}
		if failed > 0 {
			return reportedFail()
		}
		return nil
	}

	var failed []string
	for _, pkgRef := range args {
		url, err := eng.PackageHomepage(ctx, pkgRef)
		if err != nil {
			fmt.Printf("  %s %s: %v\n", markFail, pkgRef, err)
			failed = append(failed, pkgRef)
			continue
		}
		if homeNoOpen {
			fmt.Printf("  %s %s\n", markSuccess, url)
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
