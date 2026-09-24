package main

import (
	"context"
	"fmt"

	"github.com/gluestick-sh/core/engine"
	"github.com/spf13/cobra"
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
	dependsCmd.SilenceUsage = true
	dependsCmd.SilenceErrors = true
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

	if jsonOutputEnabled() {
		return runDependsJSON(eng, cmd.Context(), args)
	}

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

// runDependsJSON emits the --json form of glue depends: one plan entry per package ref.
// Per-ref errors are reported in the plan entry; the command still exits 1 when any ref failed.
func runDependsJSON(eng *engine.Engine, ctx context.Context, args []string) error {
	plans := make([]jsonDependsPlan, 0, len(args))
	ok := true
	for _, pkgRef := range args {
		item := jsonDependsPlan{Ref: pkgRef}
		plan, err := eng.PlanInstall(ctx, pkgRef)
		if err != nil {
			item.Error = err.Error()
			item.Code, item.Hint = jsonErrorInfo(err)
			ok = false
		} else {
			item.Package = plan.Package
			item.Depends = plan.Depends
			item.Suggestions = plan.Suggestions
		}
		if item.Depends == nil {
			item.Depends = []engine.InstallPlanItem{}
		}
		if item.Suggestions == nil {
			item.Suggestions = []engine.InstallPlanItem{}
		}
		plans = append(plans, item)
	}
	if err := emitJSON(jsonDependsResult{Command: "depends", OK: ok, Plans: plans}); err != nil {
		return err
	}
	if !ok {
		return reportedFail()
	}
	return nil
}

// jsonDependsPlan is one package entry in the glue depends --json output.
type jsonDependsPlan struct {
	Ref         string                   `json:"ref"`
	Package     string                   `json:"package,omitempty"`
	Depends     []engine.InstallPlanItem `json:"depends"`
	Suggestions []engine.InstallPlanItem `json:"suggestions"`
	Error       string                   `json:"error,omitempty"`
	Code        string                   `json:"code,omitempty"` // stable machine-readable error code
	Hint        string                   `json:"hint,omitempty"`
}

// jsonDependsResult is the envelope of the glue depends --json output.
type jsonDependsResult struct {
	Command string            `json:"command"`
	OK      bool              `json:"ok"`
	Plans   []jsonDependsPlan `json:"plans"`
}
