package main

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/gluestick-sh/core/engine"
	"github.com/gluestick-sh/core/verbose"
)

// searchCmd queries bucket manifests by name/description.
var searchCmd = &cobra.Command{
	Use:   "search <query>",
	Short: "Search for packages in buckets",
	Args:  cobra.ExactArgs(1),
	RunE:  runSearch,
}

func init() {
	rootCmd.AddCommand(searchCmd)
}

func runSearch(cmd *cobra.Command, args []string) error {
	root := glueRoot()

	eng, err := engine.NewEngine(&engine.EngineConfig{RootDir: root, Verbose: verbose.Enabled()})
	if err != nil {
		return fmt.Errorf("initialize engine: %w", err)
	}
	defer eng.Close()

	packages, err := eng.Search(cmd.Context(), &engine.SearchRequest{
		Query: args[0],
		Limit: 0,
	}, engine.NewSilentReporter())
	if err != nil {
		return fmt.Errorf("search: %w", err)
	}

	if len(packages) == 0 {
		if jsonOutputEnabled() {
			return emitJSON(map[string]any{"query": args[0], "results": packages, "count": 0})
		}
		fmt.Printf("No results found for '%s'\n", args[0])
		return nil
	}

	if jsonOutputEnabled() {
		return emitJSON(map[string]any{"query": args[0], "results": packages, "count": len(packages)})
	}

	fmt.Printf("Found %d result(s):\n\n", len(packages))
	for _, pkg := range packages {
		fmt.Printf("%s/%s %s\n", pkg.Bucket, pkg.Name, pkg.Version)
		if pkg.Description != "" {
			fmt.Printf("  %s\n", pkg.Description)
		}
		fmt.Println()
	}
	return nil
}
