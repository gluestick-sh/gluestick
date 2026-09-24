package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/gluestick-sh/core/bucket"
	"github.com/gluestick-sh/core/git"
)

var bucketCmd = &cobra.Command{
	Use:   "bucket",
	Short: "Manage buckets (package repositories)",
}

var bucketAddCmd = &cobra.Command{
	Use:   "add <name> [repository]",
	Short: "Add a bucket",
	Long: `Add a bucket (package repository).

If repository URL is not provided, it will try to use a known bucket.`,
	Args: cobra.RangeArgs(1, 2),
	RunE: func(cmd *cobra.Command, args []string) error {
		root := glueRoot()

		br, err := bucket.NewRegistry(root)
		if err != nil {
			return fmt.Errorf("create bucket manager: %w", err)
		}

		// Check git first (before LoadExisting)
		if err := br.EnsureGit(); err != nil {
			return fmt.Errorf("git not available: %w\n\nPlease install git from https://git-scm.com/", err)
		}

		// Load existing buckets
		br.ReloadFromDisk()

		name := args[0]
		var repoURL string

		if len(args) == 2 {
			repoURL = args[1]
		} else {
			// Try known buckets
			var ok bool
			repoURL, ok = bucket.GetKnownBucketURL(name)
			if !ok {
				return fmt.Errorf("unknown bucket '%s', please provide repository URL\n\nKnown buckets: %s",
					name, strings.Join(getKnownBucketNames(), ", "))
			}
		}

		fmt.Printf("Adding bucket '%s'...\n", name)
		fmt.Printf("  Repository: %s\n", repoURL)

		// Check git availability
		if err := br.EnsureGit(); err != nil {
			return fmt.Errorf("git not available: %w\n\nPlease install git from https://git-scm.com/", err)
		}

		b, err := br.Add(name, repoURL)
		if err != nil {
			return err
		}

		fmt.Printf("  %s Bucket '%s' added\n", markSuccess, b.Name)
		if eng, engErr := openCLIEngine(); engErr == nil {
			syncEngineBucketsAfterAdd(eng, b.Name)
			eng.Close()
		}
		return nil
	},
}

var bucketRemoveCmd = &cobra.Command{
	Use:     "remove <name>...",
	Short:   "Remove buckets",
	Aliases: []string{"rm"},
	Args:    cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		root := glueRoot()

		br, err := bucket.NewRegistry(root)
		if err != nil {
			return fmt.Errorf("create bucket manager: %w", err)
		}

		// Initialize git (check/bootstrap) before loading existing buckets
		if err := br.EnsureGit(); err != nil {
			fmt.Printf("Warning: git not available: %v\n", err)
		}

		// Load existing buckets
		br.ReloadFromDisk()

		var failed []string
		var removed []string
		for _, name := range args {
			if err := br.Remove(name); err != nil {
				fmt.Printf("  %s Failed to remove '%s': %v\n", markFail, name, err)
				failed = append(failed, name)
			} else {
				fmt.Printf("  %s Bucket '%s' removed\n", markSuccess, name)
				removed = append(removed, name)
			}
		}

		if len(removed) > 0 {
			if eng, engErr := openCLIEngine(); engErr == nil {
				for _, name := range removed {
					syncEngineBucketsAfterRemove(eng, name)
				}
				eng.Close()
			}
		}

		if len(failed) > 0 {
			return reportedFail()
		}
		return nil
	},
}

var bucketListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all buckets",
	RunE: func(cmd *cobra.Command, args []string) error {
		root := glueRoot()

		br, err := bucket.NewRegistry(root)
		if err != nil {
			return fmt.Errorf("create bucket manager: %w", err)
		}

		// Initialize git (check/bootstrap) before loading existing buckets
		// This ensures LoadExisting can detect git repositories correctly
		if err := br.EnsureGit(); err != nil {
			fmt.Printf("Warning: git not available, some buckets may not be detected: %v\n", err)
		}

		// Load existing buckets
		br.ReloadFromDisk()

		buckets := br.List()
		if len(buckets) == 0 {
			fmt.Println("No buckets installed.")
			fmt.Println("\nAdd a bucket with:")
			fmt.Println("  glue bucket add <name>")
			return nil
		}

		fmt.Printf("%sInstalled buckets (%d):%s\n\n", colorBlue, len(buckets), colorReset)
		for _, b := range buckets {
			fmt.Printf("  %s\n", b.Name)
			if b.RepoURL != "" {
				fmt.Printf("    %s\n", b.RepoURL)
			}
		}

		return nil
	},
}

var bucketUpdateCmd = &cobra.Command{
	Use:   "update [name...]",
	Short: "Update buckets (fetch latest manifests)",
	RunE: func(cmd *cobra.Command, args []string) error {
		root := glueRoot()

		br, err := bucket.NewRegistry(root)
		if err != nil {
			return fmt.Errorf("create bucket manager: %w", err)
		}

		// Check git first (before LoadExisting)
		if err := br.EnsureGit(); err != nil {
			return fmt.Errorf("git not available: %w", err)
		}

		// Load existing buckets
		br.ReloadFromDisk()

		if len(args) == 0 {
			fmt.Println("Updating all buckets...")
		}

		updated := append([]string(nil), args...)
		if err := br.Update(args); err != nil {
			return err
		}

		if eng, engErr := openCLIEngine(); engErr == nil {
			syncEngineBucketsAfterUpdate(eng, updated...)
			eng.Close()
		}
		return nil
	},
}

var bucketCheckCmd = &cobra.Command{
	Use:   "check [name...]",
	Short: "Check whether buckets have upstream updates",
	Long: `Fetch upstream for each bucket and report which have updates available.

Does not modify local buckets; use 'glue bucket update' to sync.`,
	Args: cobra.ArbitraryArgs,
	RunE: runBucketCheck,
}

var bucketKnownCmd = &cobra.Command{
	Use:   "known",
	Short: "List known buckets",
	RunE: func(cmd *cobra.Command, args []string) error {
		buckets := bucket.KnownBuckets()

		fmt.Println("Known buckets:")
		for name, url := range buckets {
			fmt.Printf("  %-15s %s\n", name, url)
		}

		fmt.Println("\nAdd a bucket with:")
		fmt.Println("  glue bucket add <name>")

		return nil
	},
}

func init() {
	rootCmd.AddCommand(bucketCmd)
	bucketCmd.AddCommand(bucketAddCmd)
	bucketCmd.AddCommand(bucketRemoveCmd)
	bucketCmd.AddCommand(bucketListCmd)
	bucketCheckCmd.SilenceUsage = true
	bucketCheckCmd.SilenceErrors = true
	bucketCmd.AddCommand(bucketCheckCmd)
	bucketCmd.AddCommand(bucketUpdateCmd)
	bucketCmd.AddCommand(bucketKnownCmd)
}

func getKnownBucketNames() []string {
	buckets := bucket.KnownBuckets()
	names := make([]string, 0, len(buckets))
	for name := range buckets {
		names = append(names, name)
	}
	return names
}

func runBucketCheck(_ *cobra.Command, args []string) error {
	root := glueRoot()

	br, err := bucket.NewRegistry(root)
	if err != nil {
		return fmt.Errorf("create bucket manager: %w", err)
	}
	if err := br.EnsureGit(); err != nil {
		return fmt.Errorf("git not available: %w", err)
	}
	if err := br.ReloadFromDisk(); err != nil {
		return fmt.Errorf("load buckets: %w", err)
	}

	buckets := br.List()
	if len(buckets) == 0 {
		fmt.Println("No buckets installed.")
		return nil
	}

	statuses, err := bucketCheckStatuses(br, args)
	if err != nil {
		return err
	}

	names := make([]string, 0, len(statuses))
	for name := range statuses {
		names = append(names, name)
	}
	sort.Strings(names)

	if len(args) == 0 {
		fmt.Printf("Checking %d bucket(s)...\n\n", len(names))
	}

	checkFailed := 0
	withUpdates := 0
	for _, name := range names {
		status := statuses[name]
		printBucketCheckResult(name, status)
		if !status.OK {
			checkFailed++
			continue
		}
		if status.HasUpdates {
			withUpdates++
		}
	}

	fmt.Println()
	if checkFailed > 0 {
		fmt.Printf("%d bucket(s) failed to check.\n", checkFailed)
		return reportedFail()
	}
	if withUpdates > 0 {
		fmt.Printf("%d bucket(s) have updates. Run 'glue bucket update' to sync.\n", withUpdates)
	} else {
		fmt.Println("All buckets are up to date.")
	}
	return nil
}

func bucketCheckStatuses(br *bucket.Registry, names []string) (map[string]git.UpdateStatus, error) {
	if len(names) == 0 {
		return br.CheckUpdates()
	}

	out := make(map[string]git.UpdateStatus, len(names))
	for _, name := range names {
		status, err := br.CheckUpdate(name)
		if err != nil && status.LocalCommit == "" && status.ErrMsg == "" {
			status.ErrMsg = err.Error()
		}
		out[name] = status
	}
	return out, nil
}

func printBucketCheckResult(name string, status git.UpdateStatus) {
	if !status.OK {
		fmt.Printf("  %s %s: %s\n", markFail, name, bucket.FormatGitError(status.ErrMsg))
		return
	}
	if status.HasUpdates {
		fmt.Printf("  %s %s  %s → %s\n", markSuccess, name, shortCommit(status.LocalCommit), shortCommit(status.RemoteCommit))
		return
	}
	if local := shortCommit(status.LocalCommit); local != "" {
		fmt.Printf("  %s %s  %s (up to date)\n", markSuccess, name, local)
		return
	}
	fmt.Printf("  %s %s  up to date\n", markSuccess, name)
}

func shortCommit(sha string) string {
	sha = strings.TrimSpace(sha)
	if len(sha) <= 7 {
		return sha
	}
	return sha[:7]
}
