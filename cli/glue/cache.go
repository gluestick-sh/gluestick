package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gluestick-sh/core/cache"
	"github.com/gluestick-sh/core/engine"
	"github.com/gluestick-sh/core/humanize"
	"github.com/gluestick-sh/core/verbose"
	"github.com/spf13/cobra"
)

// cacheCmd manages the SQLite cache index and content-store blobs.
var cacheCmd = &cobra.Command{
	Use:   "cache",
	Short: "Manage download cache (index and store blobs)",
	Long: `Manage the download cache under ~/.glue/cache and ~/.glue/store.

  list    — show indexed packages and sizes
  clear   — remove SQLite index entries (blobs on disk are kept)
  rebuild — rescan store and rebuild the index
  gc      — delete store blobs not referenced by the index or installed apps`,
}

var cacheListCmd = &cobra.Command{
	Use:   "list",
	Short: "List cached packages",
	RunE: func(cmd *cobra.Command, args []string) error {
		eng, err := newCacheEngine()
		if err != nil {
			return err
		}
		defer eng.Close()

		if jsonOutputEnabled() {
			packages := eng.ListCachePackages()
			if packages == nil {
				packages = []engine.CachePackageInfo{}
			}
			return emitJSON(map[string]any{"packages": packages, "total": eng.CacheSummary()})
		}

		listCacheByPackage(eng)
		return nil
	},
}

var cacheClearCmd = &cobra.Command{
	Use:   "clear [package...]",
	Short: "Clear cache index entries (store blobs on disk are kept)",
	Long:  "Removes package rows from the SQLite cache index. Does not delete files under ~/.glue/store; use cache gc for that.",
	RunE: func(cmd *cobra.Command, args []string) error {
		clearAll, _ := cmd.Flags().GetBool("all")

		eng, err := newCacheEngine()
		if err != nil {
			return err
		}
		defer eng.Close()

		if clearAll {
			return clearAllCacheIndex(eng)
		}
		if len(args) == 0 {
			return fmt.Errorf("specify package name(s) or use --all")
		}
		return clearCacheIndexByName(eng, args)
	},
}

var (
	cacheClearAll bool
)

func init() {
	rootCmd.AddCommand(cacheCmd)
	cacheCmd.AddCommand(cacheListCmd)
	cacheCmd.AddCommand(cacheClearCmd)
	cacheCmd.AddCommand(cacheRebuildCmd)
	cacheCmd.AddCommand(cacheGCCmd)

	for _, c := range []*cobra.Command{cacheListCmd, cacheClearCmd, cacheRebuildCmd, cacheGCCmd} {
		c.SilenceUsage = true
		c.SilenceErrors = true
	}

	cacheClearCmd.Flags().BoolVarP(&cacheClearAll, "all", "a", false, "clear all cache index entries")
}

var cacheGCCmd = &cobra.Command{
	Use:   "gc",
	Short: "Remove unreferenced store blobs (orphan GC)",
	Long:  "Deletes content-store files that are no longer referenced by the cache index or installed apps. Does not clear index rows; use cache clear for that.",
	RunE:  runCacheGC,
}

func runCacheGC(cmd *cobra.Command, args []string) error {
	eng, err := newCacheEngine()
	if err != nil {
		return err
	}
	defer eng.Close()

	var reporter cache.GCProgressReporter
	if jsonOutputEnabled() {
		reporter = func(cache.GCProgressEvent) {} // no-op: keep stdout pure JSON
	} else {
		reporter = newCLICacheGCReporter()
		verbose.Progressf("glue cache gc\n")
	}

	res, err := eng.RunCacheGCWithProgress(reporter)
	if err != nil {
		if jsonOutputEnabled() {
			code, hint := jsonErrorInfo(err)
			if emitErr := emitJSON(map[string]any{
				"command": "cache_gc",
				"ok":      false,
				"error":   err.Error(),
				"code":    code,
				"hint":    hint,
			}); emitErr != nil {
				return emitErr
			}
			return reportedFail()
		}
		return fmt.Errorf("cache gc: %w", err)
	}

	if jsonOutputEnabled() {
		return emitJSON(map[string]any{
			"command":       "cache_gc",
			"ok":            true,
			"removed_blobs": res.RemovedBlobs,
			"freed_bytes":   res.FreedBytes,
		})
	}

	verbose.Progressf("Done.\n")
	return nil
}

var cacheRebuildCmd = &cobra.Command{
	Use:   "rebuild",
	Short: "Rebuild cache index from installed apps (scans hardlinks)",
	RunE: func(cmd *cobra.Command, args []string) error {
		eng, err := newCacheEngine()
		if err != nil {
			return err
		}
		defer eng.Close()

		root := glueRoot()
		appsDir := filepath.Join(root, "apps")

		if jsonOutputEnabled() {
			count, err := eng.RebuildCacheIndex(func(string, string, int) {})
			if err != nil {
				code, hint := jsonErrorInfo(err)
				if emitErr := emitJSON(map[string]any{
					"command": "cache_rebuild",
					"ok":      false,
					"error":   err.Error(),
					"code":    code,
					"hint":    hint,
				}); emitErr != nil {
					return emitErr
				}
				return reportedFail()
			}
			return emitJSON(map[string]any{"command": "cache_rebuild", "ok": true, "indexed": count})
		}

		verbose.Progressf("glue cache rebuild\n")
		verbose.Progressf("  Source: %s\n", appsDir)
		if _, err := os.Stat(appsDir); os.IsNotExist(err) {
			verbose.Progressf("  Apps directory not found (nothing to index)\n")
			verbose.Progressf("Done.\n")
			return nil
		}

		verbose.Progressf("  Scanning installed apps...\n")
		count, err := eng.RebuildCacheIndex(func(pkg, version string, fileCount int) {
			verbose.Progressf("    %s %s@%s (%d file(s))\n", markSuccess, pkg, version, fileCount)
		})
		if err != nil {
			return fmt.Errorf("rebuild cache index: %w", err)
		}
		if count == 0 {
			verbose.Progressf("  No installed packages found to index\n")
		} else {
			verbose.Progressf("  Indexed %d package(s)\n", count)
		}
		verbose.Progressf("Done.\n")
		return nil
	},
}

func newCacheEngine() (*engine.Engine, error) {
	root := glueRoot()
	eng, err := engine.NewEngine(&engine.EngineConfig{RootDir: root, Verbose: verbose.Enabled()})
	if err != nil {
		return nil, fmt.Errorf("initialize engine: %w", err)
	}
	return eng, nil
}

// listCacheByPackage lists cached files grouped by package
func listCacheByPackage(eng *engine.Engine) {
	packages := eng.ListCachePackages()

	if len(packages) == 0 {
		fmt.Println("No packages in cache index")
		return
	}

	summary := eng.CacheSummary()

	const filesColWidth = 14 // width of "%8d" + " files"

	fmt.Printf("%-20s %-18s %-22s %12s %s\n", "Package", "Version", "Updated", "Size", cacheListPadLeft(filesColWidth, "Files"))
	fmt.Println(strings.Repeat("-", 90))

	for _, entry := range packages {
		files := fmt.Sprintf("%d files", entry.FileCount)
		fmt.Printf("%-20s %-18s %-22s %12s %s\n",
			entry.Name, entry.Version, humanize.FormatCacheDate(entry.Installed), humanize.FormatBytes(entry.Size), cacheListPadLeft(filesColWidth, files))
	}

	fmt.Println(strings.Repeat("-", 90))
	fmt.Printf("Total: %d packages, %d files, %s\n",
		summary.PackageCount, summary.TotalFiles, humanize.FormatBytes(summary.TotalSize))
}

func cacheListPadLeft(width int, s string) string {
	if pad := width - len(s); pad > 0 {
		return strings.Repeat(" ", pad) + s
	}
	return s
}

func clearCacheIndexByName(eng *engine.Engine, names []string) error {
	byName := make(map[string]engine.CachePackageInfo, len(names))
	for _, p := range eng.ListCachePackages() {
		byName[p.Name] = p
	}

	if jsonOutputEnabled() {
		return clearCacheIndexByNameJSON(eng, byName, names)
	}

	var cleared int
	for _, name := range names {
		entry, ok := byName[name]
		if !ok {
			fmt.Printf("Not in cache index: %s\n", name)
			continue
		}
		n, err := eng.ClearCacheIndex([]string{name})
		if err != nil {
			return fmt.Errorf("clear index for %s: %w", name, err)
		}
		if n == 0 {
			continue
		}
		cleared += n
		fmt.Printf("Cleared index for %s (%s, %d files, %s)\n",
			name, entry.Version, entry.FileCount, humanize.FormatBytes(entry.Size))
	}

	if cleared == 0 {
		return reportedFail()
	}
	fmt.Printf("\nCleared %d package(s) from cache index (store files kept on disk)\n", cleared)
	return nil
}

// clearCacheIndexByNameJSON emits the --json form of cache clear <names>.
// Matches text semantics: nothing cleared (all names missing from the index) exits 1.
func clearCacheIndexByNameJSON(eng *engine.Engine, byName map[string]engine.CachePackageInfo, names []string) error {
	clearedFiles := 0
	cleared, notIndexed := []string{}, []string{}
	for _, name := range names {
		entry, ok := byName[name]
		if !ok {
			notIndexed = append(notIndexed, name)
			continue
		}
		n, err := eng.ClearCacheIndex([]string{name})
		if err != nil {
			code, hint := jsonErrorInfo(err)
			if emitErr := emitJSON(map[string]any{
				"command": "cache_clear",
				"ok":      false,
				"error":   err.Error(),
				"code":    code,
				"hint":    hint,
			}); emitErr != nil {
				return emitErr
			}
			return reportedFail()
		}
		if n == 0 {
			notIndexed = append(notIndexed, name)
			continue
		}
		cleared = append(cleared, name)
		clearedFiles += entry.FileCount
	}
	if cleared == nil {
		cleared = []string{}
	}
	if notIndexed == nil {
		notIndexed = []string{}
	}
	ok := len(cleared) > 0
	if err := emitJSON(map[string]any{
		"command":       "cache_clear",
		"ok":            ok,
		"cleared":       cleared,
		"cleared_files": clearedFiles,
		"not_in_index":  notIndexed,
	}); err != nil {
		return err
	}
	if !ok {
		return reportedFail()
	}
	return nil
}

func clearAllCacheIndex(eng *engine.Engine) error {
	summary, err := eng.ClearAllCacheIndex()
	if err != nil {
		return fmt.Errorf("clear cache index: %w", err)
	}
	if jsonOutputEnabled() {
		return emitJSON(map[string]any{
			"command":    "cache_clear",
			"ok":         true,
			"all":        true,
			"packages":   summary.PackageCount,
			"files":      summary.TotalFiles,
			"size_bytes": summary.TotalSize,
		})
	}
	if summary.PackageCount == 0 {
		fmt.Println("Cache index is empty")
		return nil
	}

	fmt.Printf("Cleared index for %d package(s), %d file(s), %s\n",
		summary.PackageCount, summary.TotalFiles, humanize.FormatBytes(summary.TotalSize))
	fmt.Println("Cache store blobs kept on disk (use glue cache gc to remove unreferenced blobs)")
	return nil
}
