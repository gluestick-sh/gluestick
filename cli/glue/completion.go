package main

import (
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/gluestick-sh/core/bucket"
)

var completionCmd = &cobra.Command{
	Use:   "completion [bash|zsh|fish|powershell]",
	Short: "Generate shell completion script",
	Long: `Generate a shell completion script for glue.

Load once per session, or install into your shell profile:

  # PowerShell
  glue completion powershell | Out-String | Invoke-Expression

  # Bash
  source <(glue completion bash)

  # Zsh
  source <(glue completion zsh)

  # Fish
  glue completion fish | source
`,
	Args:              cobra.ExactArgs(1),
	ValidArgs:         []string{"bash", "zsh", "fish", "powershell"},
	RunE:              runCompletion,
	ValidArgsFunction: completeShellNames,
}

func init() {
	rootCmd.CompletionOptions.DisableDefaultCmd = true
	rootCmd.AddCommand(completionCmd)
	registerCommandCompletions()
}

func runCompletion(cmd *cobra.Command, args []string) error {
	switch args[0] {
	case "bash":
		return rootCmd.GenBashCompletionV2(os.Stdout, true)
	case "zsh":
		return rootCmd.GenZshCompletion(os.Stdout)
	case "fish":
		return rootCmd.GenFishCompletion(os.Stdout, true)
	case "powershell":
		return rootCmd.GenPowerShellCompletionWithDesc(os.Stdout)
	default:
		return wrapUsageError(cmd.Help())
	}
}

func completeShellNames(_ *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	names := []string{"bash", "zsh", "fish", "powershell"}
	return filterPrefix(names, toComplete), cobra.ShellCompDirectiveNoFileComp
}

func registerCommandCompletions() {
	installCmd.ValidArgsFunction = completeCatalogPackages
	uninstallCmd.ValidArgsFunction = completeInstalledPackages
	infoCmd.ValidArgsFunction = completeInstalledPackages
	resetCmd.ValidArgsFunction = completeInstalledPackages
	dependsCmd.ValidArgsFunction = completeCatalogPackages
	updateCmd.ValidArgsFunction = completeInstalledPackages
	holdCmd.ValidArgsFunction = completeInstalledPackages
	unholdCmd.ValidArgsFunction = completeInstalledPackages
	homeCmd.ValidArgsFunction = completeCatalogPackages
	searchCmd.ValidArgsFunction = func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) > 0 {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		return completeCatalogPackages(cmd, args, toComplete)
	}

	bucketAddCmd.ValidArgsFunction = completeBucketAddArgs
	bucketRemoveCmd.ValidArgsFunction = completeInstalledBucketNames
	bucketUpdateCmd.ValidArgsFunction = completeInstalledBucketNames
	bucketCheckCmd.ValidArgsFunction = completeInstalledBucketNames

	configGetCmd.ValidArgsFunction = completeConfigKeys
	configSetCmd.ValidArgsFunction = completeConfigSetArgs
	configUnsetCmd.ValidArgsFunction = completeConfigKeys
}

func completeCatalogPackages(_ *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	eng, err := openEngineForCompletion()
	if err != nil {
		return nil, cobra.ShellCompDirectiveError
	}
	defer eng.Close()

	refs, err := catalogPackageRefs(eng, toComplete, 50)
	if err != nil {
		return nil, cobra.ShellCompDirectiveError
	}
	return refs, cobra.ShellCompDirectiveNoFileComp
}

func completeInstalledPackages(_ *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	eng, err := openEngineForCompletion()
	if err != nil {
		return nil, cobra.ShellCompDirectiveError
	}
	defer eng.Close()

	names, err := installedPackageNames(eng, toComplete)
	if err != nil {
		return nil, cobra.ShellCompDirectiveError
	}
	return names, cobra.ShellCompDirectiveNoFileComp
}

func completeInstalledBucketNames(_ *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	root := glueRoot()
	br, err := loadBucketRegistry(root)
	if err != nil {
		return nil, cobra.ShellCompDirectiveError
	}
	var names []string
	for _, b := range br.List() {
		names = append(names, b.Name)
	}
	sort.Strings(names)
	return filterPrefix(names, toComplete), cobra.ShellCompDirectiveNoFileComp
}

func completeBucketAddArgs(_ *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) == 0 {
		names := knownBucketNames()
		return filterPrefix(names, toComplete), cobra.ShellCompDirectiveNoFileComp
	}
	return nil, cobra.ShellCompDirectiveFilterDirs
}

func completeConfigKeys(_ *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	keys := []string{"github_proxy", "parallel_download", "color", "verbose"}
	return filterPrefix(keys, toComplete), cobra.ShellCompDirectiveNoFileComp
}

func completeConfigSetArgs(_ *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) == 0 {
		return completeConfigKeys(nil, nil, toComplete)
	}
	if len(args) == 1 {
		switch args[0] {
		case "color", "verbose", "parallel_download":
			return filterPrefix([]string{"true", "false"}, toComplete), cobra.ShellCompDirectiveNoFileComp
		}
	}
	return nil, cobra.ShellCompDirectiveNoFileComp
}

func knownBucketNames() []string {
	known := bucket.KnownBuckets()
	names := make([]string, 0, len(known))
	for name := range known {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func filterPrefix(items []string, prefix string) []string {
	if prefix == "" {
		return items
	}
	prefix = strings.ToLower(prefix)
	var out []string
	for _, item := range items {
		if strings.HasPrefix(strings.ToLower(item), prefix) {
			out = append(out, item)
		}
	}
	return out
}
