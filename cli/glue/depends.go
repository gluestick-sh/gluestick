package main

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/gluestick-sh/core/engine"
)

// dependsCmd prints install plans: required dependencies and optional suggestions.
var dependsCmd = &cobra.Command{
	Use:     "depends <package>...",
	Short:   "Show missing dependencies and optional suggestions for packages",
	Aliases: []string{"dep"},
	Args:    cobra.MinimumNArgs(1),
	RunE:    runDepends,
}

func init() {
	rootCmd.AddCommand(dependsCmd)
}

func runDepends(cmd *cobra.Command, args []string) error {
	// PlanInstall resolves manifests and checks what is already installed; no packages are modified.
	eng, err := engine.NewEngine(&engine.EngineConfig{
		RootDir: glueRoot(),
		Workers: 1,
	})
	if err != nil {
		return fmt.Errorf("initialize engine: %w", err)
	}
	defer eng.Close()

	var failed []string
	for i, pkgRef := range args {
		if i > 0 {
			fmt.Println()
		}
		plan, err := eng.PlanInstall(cmd.Context(), pkgRef)
		if err != nil {
			fmt.Printf("%s %s: %v\n", markFail, pkgRef, err)
			failed = append(failed, pkgRef)
			continue
		}
		fmt.Printf("%s%s%s\n", colorBlue, plan.Package, colorReset)
		printDependsSection(plan)
		printSuggestionsSection(plan)
	}

	if len(failed) > 0 {
		return reportedFail()
	}
	return nil
}

// printDependsSection lists manifest depends that would be installed first.
func printDependsSection(plan *engine.InstallPlan) {
	if len(plan.Depends) == 0 {
		fmt.Println("  Dependencies: (none missing)")
		return
	}
	fmt.Println("  Dependencies (will install first):")
	for _, d := range plan.Depends {
		fmt.Printf("    • %s\n", d.Ref)
	}
}

// printSuggestionsSection lists optional manifest suggestions and whether each is installed.
func printSuggestionsSection(plan *engine.InstallPlan) {
	if len(plan.Suggestions) == 0 {
		return
	}
	fmt.Println("  Suggestions (optional):")
	for _, s := range plan.Suggestions {
		mark := markFail
		status := "not installed"
		if s.Installed {
			mark = markSuccess
			status = "installed"
		}
		label := s.Ref
		if s.Label != "" {
			label = fmt.Sprintf("%s → %s", s.Label, s.Ref)
		}
		fmt.Printf("    %s %s (%s)\n", mark, label, status)
	}
}
