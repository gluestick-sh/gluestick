package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/gluestick-sh/core/engine"
	"github.com/gluestick-sh/core/verbose"
)

// listCmd lists installed packages (current version by default, or all versions with --all).
var listCmd = &cobra.Command{
	Use:     "list",
	Short:   "List installed packages",
	Aliases: []string{"ls"},
	RunE:    runList,
}

var listDetailed bool
var listAll bool

func init() {
	rootCmd.AddCommand(listCmd)
	listCmd.Flags().BoolVarP(&listDetailed, "detailed", "d", false, "show detailed information")
	listCmd.Flags().BoolVar(&listAll, "all", false, "show all installed versions")
}

func runList(cmd *cobra.Command, args []string) error {
	root := glueRoot()
	if listAll {
		// --all reads version dirs on disk instead of the engine installed index.
		return runListAllVersions(root, args)
	}

	eng, err := engine.NewEngine(&engine.EngineConfig{RootDir: root, Verbose: verbose.Enabled()})
	if err != nil {
		return fmt.Errorf("initialize engine: %w", err)
	}
	defer eng.Close()

	packages, err := eng.List(cmd.Context(), &engine.ListRequest{Details: listDetailed}, engine.NewSilentReporter())
	if err != nil {
		return fmt.Errorf("list: %w", err)
	}

	if len(args) > 0 {
		selected := make(map[string]struct{}, len(args))
		for _, arg := range args {
			selected[packageBaseName(arg)] = struct{}{}
		}
		var filtered []*engine.Package
		for _, pkg := range packages {
			if _, ok := selected[pkg.Name]; ok {
				filtered = append(filtered, pkg)
			}
		}
		packages = filtered
	}

	sort.Slice(packages, func(i, j int) bool {
		return packages[i].Name < packages[j].Name
	})

	if jsonOutputEnabled() {
		return emitJSON(map[string]any{"packages": packages, "count": len(packages)})
	}

	if len(packages) == 0 {
		fmt.Printf("%sNo packages installed%s\n", colorBlue, colorReset)
		fmt.Println("\nInstall a package with:")
		fmt.Println("  glue install <name>")
		return nil
	}
	fmt.Printf("%s%d packages installed:%s\n\n", colorBlue, len(packages), colorReset)
	for _, pkg := range packages {
		if listDetailed {
			fmt.Printf("%s@%s\n", pkg.Name, pkg.Version)
			if pkg.Manifest != nil && len(pkg.Manifest.Binaries) > 0 {
				names := make([]string, 0, len(pkg.Manifest.Binaries))
				for _, b := range pkg.Manifest.Binaries {
					names = append(names, b.Name)
				}
				fmt.Printf("  Binaries: %s\n", strings.Join(names, ", "))
			}
		} else {
			fmt.Printf("%s@%s\n", pkg.Name, pkg.Version)
		}
	}

	return nil
}

// runListAllVersions walks apps/<pkg>/<version>; * marks the current symlink target.
func runListAllVersions(root string, args []string) error {
	eng, err := engine.NewEngine(&engine.EngineConfig{RootDir: root, Verbose: verbose.Enabled()})
	if err != nil {
		return fmt.Errorf("initialize engine: %w", err)
	}
	defer eng.Close()

	var filter []string
	if len(args) > 0 {
		for _, arg := range args {
			filter = append(filter, packageBaseName(arg))
		}
	}

	packages, err := eng.ListInstalledAllVersions(filter)
	if err != nil {
		return err
	}

	if jsonOutputEnabled() {
		return emitJSON(map[string]any{"packages": packages, "count": len(packages), "allVersions": true})
	}

	if len(packages) == 0 {
		fmt.Printf("%sNo packages installed%s\n", colorBlue, colorReset)
		fmt.Println("\nInstall a package with:")
		fmt.Println("  glue install <name>")
		return nil
	}

	fmt.Printf("%s%d packages installed:%s\n\n", colorBlue, len(packages), colorReset)
	for _, pkg := range packages {
		fmt.Printf("%s\n", pkg.Name)
		for i := len(pkg.Versions) - 1; i >= 0; i-- {
			ver := pkg.Versions[i]
			marker := " "
			if ver == pkg.Current {
				marker = "*"
			}
			fmt.Printf("  %s %s\n", marker, ver)
		}
	}

	return nil
}
