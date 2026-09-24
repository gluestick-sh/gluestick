// Package uninstall removes installed package versions, running manifest
// uninstall hooks and cleaning up shortcuts, shims, links, and cache entries.
package uninstall

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/gluestick-sh/core/apps"
	"github.com/gluestick-sh/core/cache"
	"github.com/gluestick-sh/core/engine/internal/install"
	"github.com/gluestick-sh/core/engine/internal/runtime"
	etypes "github.com/gluestick-sh/core/engine/types"
	"github.com/gluestick-sh/core/humanize"
	"github.com/gluestick-sh/core/procutil"
	"github.com/gluestick-sh/core/store"
	"github.com/gluestick-sh/core/verbose"
)

// PackageFull implements the complete uninstallation logic.
// This function handles all aspects of package removal including:
// - Running uninstall hooks
// - Removing shortcuts
// - Saving persist data
// - Removing version directory
// - Cleaning up shims
// - Updating cache index
// - Switching to remaining versions (if any)
// Parameters:
//   - e: Runtime engine
//   - ctx: Context for cancellation
//   - pkgRef: Package reference (e.g., "package" or "package@version")
//   - req: Uninstallation request with options
// Returns:
//   - The version that was uninstalled
//   - Error if uninstallation fails
func PackageFull(e *runtime.Engine,
	ctx context.Context,
	pkgRef string,
	req *etypes.UninstallRequest,
) (string, error) {
	if err := runtime.ContextCanceled(ctx); err != nil {
		return "", err
	}
	root := e.Config.RootDir
	appsDir := filepath.Join(root, "apps")
	shimsMetaDir := filepath.Join(root, "shims-meta")

	pkgName, targetVer := runtime.ParsePkgRef(pkgRef)
	pkgRoot := apps.PkgRoot(root, pkgName)

	versions, err := apps.ListVersions(pkgRoot)
	if err != nil {
		return "", err
	}
	if len(versions) == 0 {
		entry, cacheOk := e.Cache.Get(pkgName)
		if cacheOk || install.HasShimsForPackage(shimsMetaDir, pkgName) {
			version := ""
			if cacheOk {
				version = entry.Version
			}
			verbose.Progressf("Cleaning up %s (no install files on disk)...\n", pkgName)
			if err := finishPackageRemoved(e, appsDir, shimsMetaDir, pkgName, pkgRoot, version, req.Purge); err != nil {
				return "", err
			}
			return version, nil
		}
		return "", fmt.Errorf("%s", runtime.FormatNotInstalled(pkgName))
	}

	if targetVer == "" {
		targetVer, _ = apps.ReadCurrent(pkgRoot)
		if targetVer == "" {
			targetVer, _ = apps.EnsureCurrent(pkgRoot)
		}
		if targetVer == "" {
			targetVer, _ = apps.PickDefaultVersion(pkgRoot)
		}
	}

	if !runtime.VersionDirInstalled(root, pkgName, targetVer) {
		return "", fmt.Errorf("%s@%s is not installed", pkgName, targetVer)
	}

	currentVer, _ := apps.ReadCurrent(pkgRoot)
	wasActive := currentVer == targetVer

	verbose.Progressf("Uninstalling %s@%s...\n", pkgName, targetVer)

	verDir := filepath.Join(pkgRoot, targetVer)
	fileCount, dirSize := CountInstallDir(verDir)
	verbose.Progressf("  Removing %s", verDir)
	if fileCount > 0 {
		verbose.Progressf(" (%d file(s), %s)", fileCount, humanize.FormatBytes(dirSize))
	}
	verbose.Progressf("\n")

	m, bucketName, installArch := loadUninstallManifestContext(e, verDir, pkgName)
	if m != nil {
		if err := runManifestUninstallHooks(e, ctx, bucketName, pkgName, verDir, targetVer, installArch, m); err != nil {
			return "", err
		}
		if err := install.RemovePackageShortcuts(m, installArch); err != nil {
			verbose.Progressf("  Warning: failed to remove shortcuts: %v\n", err)
		}
	}

	if err := runtime.ContextCanceled(ctx); err != nil {
		return "", err
	}
	releaseInstallDirFileLocks(verDir)

	if err := checkProcessesBlockingUninstall(pkgRoot, verDir, targetVer, pkgName); err != nil {
		return "", err
	}

	if m != nil {
		persistEntries := m.PersistEntriesForInstall(installArch)
		if len(persistEntries) > 0 {
			persistDir := filepath.Join(root, "persist", pkgName)
			if err := install.SavePersistOnUninstall(verDir, persistDir, persistEntries); err != nil {
				return "", fmt.Errorf("save persist data: %w", err)
			}
		}
	}

	removedCurrent := false
	if wasActive {
		if err := apps.RemoveCurrent(pkgRoot); err != nil {
			return "", fmt.Errorf("remove current link: %w", err)
		}
		removedCurrent = true
	}
	if err := runtime.ContextCanceled(ctx); err != nil {
		return "", err
	}
	if err := install.RemoveAll(verDir); err != nil {
		if removedCurrent {
			if linkErr := apps.LinkCurrent(pkgRoot, targetVer); linkErr != nil {
				verbose.Progressf("  Warning: failed to restore current link after uninstall error: %v\n", linkErr)
			}
		}
		blockDirs := []string{verDir}
		if cur, readErr := apps.ReadCurrent(pkgRoot); readErr == nil && cur == targetVer {
			blockDirs = append(blockDirs, filepath.Join(pkgRoot, apps.CurrentLinkName))
		}
		if procs, _ := procutil.ProcessesBlockingUninstall(blockDirs...); len(procs) > 0 {
			return "", processesBlockingUninstallError(pkgName, targetVer, procs)
		}
		if isAccessDeniedRemoveErr(err) {
			return "", fmt.Errorf("remove version dir: %w\n%s", err, accessDeniedUninstallHint(pkgName))
		}
		return "", fmt.Errorf("remove version dir: %w", err)
	}

	if m != nil {
		if err := runManifestPostUninstallHooks(e, ctx, bucketName, pkgName, verDir, targetVer, installArch, m); err != nil {
			verbose.Progressf("  Warning: post_uninstall failed: %v\n", err)
		}
	}

	remaining, err := apps.ListVersions(pkgRoot)
	if err != nil {
		return "", err
	}

	if len(remaining) == 0 {
		if m != nil {
			if err := install.RemovePackageEnvSet(m, installArch); err != nil {
				verbose.Progressf("  Warning: failed to remove env_set: %v\n", err)
			}
		}
		if err := finishPackageRemoved(e, appsDir, shimsMetaDir, pkgName, pkgRoot, targetVer, req.Purge); err != nil {
			return "", err
		}
		return targetVer, nil
	}

	if wasActive {
		newVer, ok := apps.PickDefaultVersion(pkgRoot)
		if !ok {
			return "", fmt.Errorf("no version left to activate for %s", pkgName)
		}
		verbose.Progressf("  Switching current => %s\n", newVer)
		if err := apps.LinkCurrent(pkgRoot, newVer); err != nil {
			return "", fmt.Errorf("link current: %w", err)
		}
		if err := install.ResetPackage(e, appsDir, shimsMetaDir, pkgName+"@"+newVer); err != nil {
			return "", fmt.Errorf("reset shims: %w", err)
		}
	} else {
		verbose.Progressf("  %s is not the active version; shims unchanged\n", targetVer)
	}

	if req.Purge {
		verbose.Progressf("  Cache kept (other versions installed; purge after removing all versions)\n")
	}

	verbose.Progressf("  %s %s@%s uninstalled\n", install.SuccessMark(), pkgName, targetVer)
	return targetVer, nil
}

// finishPackageRemoved performs final cleanup after the last version is removed.
// This handles shim removal, cache purging (if requested), and status updates.
// Parameters:
//   - e: Runtime engine
//   - appsDir: Applications directory
//   - shimsMetaDir: Shims metadata directory
//   - pkgName: Package name
//   - pkgRoot: Package root directory
//   - version: Version being removed
//   - purge: If true, remove cached blobs
// Returns error if cleanup fails.
func finishPackageRemoved(e *runtime.Engine,
	appsDir, shimsMetaDir, pkgName, pkgRoot, version string,
	purge bool,
) error {
	removedShims, err := install.RemoveShimsForPackage(e.ShimMgr, shimsMetaDir, appsDir, pkgName)
	if err != nil {
		verbose.Progressf("    Warning: failed to remove shims: %v\n", err)
	} else if len(removedShims) > 0 {
		verbose.Progressf("  Removing shims...\n")
		for _, shimName := range removedShims {
			verbose.Progressf("    %s %s\n", install.SuccessMark(), shimName)
		}
	}

	if err := install.RemoveAll(pkgRoot); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove install directory: %w", err)
	}

	if purge {
		entry, _ := e.Cache.Get(pkgName)
		if entry != nil {
			verbose.Progressf("  Purging cache index...\n")
			if err := purgePackageCache(e.Cache, e.Store, appsDir, pkgName, entry); err != nil {
				verbose.Progressf("    Warning: failed to purge cache: %v\n", err)
			}
		} else {
			verbose.Progressf("  No cache index entry to purge\n")
		}
		verbose.Progressf("  %s %s@%s uninstalled and cache purged\n", install.SuccessMark(), pkgName, version)
	} else {
		if _, ok := e.Cache.GetInstalled(pkgName); ok {
			if err := e.Cache.RemoveInstalled(pkgName); err != nil {
				return fmt.Errorf("update install registry: %w", err)
			}
		}
		if _, ok := e.Cache.Get(pkgName); ok {
			// verbose.Progressf("  Install removed; content cache kept for %s@%s (%s, %d files — reinstall skips download)\n",
			// 	pkgName, entry.Version, humanize.FormatBytes(entry.Size), len(entry.Files))
			verbose.Progressf("  Install removed\n")
			// verbose.Progressf("  Use uninstall --purge to delete cached blobs\n")
		} else {
			verbose.Progressf("  Install removed (no content cache entry)\n")
		}
		verbose.Progressf("  %s %s@%s uninstalled\n", install.SuccessMark(), pkgName, version)
	}
	return nil
}

// purgePackageCache removes cached blobs for a package.
// This deletes both the cache index entry and the CAS blobs,
// freeing disk space. This is only called when purging the last version.
// Parameters:
//   - cacheIdx: Cache index
//   - store: CAS store
//   - appsDir: Applications directory (for reference checking)
//   - pkgName: Package name
//   - entry: Cache entry for the package
// Returns error if cache removal fails.
func purgePackageCache(cacheIdx *cache.Index,
	store *store.Store,
	appsDir, pkgName string,
	entry *cache.PackageEntry,
) error {
	if entry != nil {
		verbose.Progressf("    Index entry: %d file(s), %s\n", len(entry.Files), humanize.FormatBytes(entry.Size))
	}
	removed, freed, err := cache.PurgePackage(cacheIdx, store, appsDir, pkgName)
	if err != nil {
		return err
	}
	verbose.Progressf("    Removed %d cached file(s), %s\n", removed, humanize.FormatBytes(freed))
	return nil
}

// CountInstallDir returns file count and total size for a package version directory.
// This walks the directory tree recursively to count files and sum their sizes.
// Used for reporting how much will be removed during uninstallation.
// Parameters:
//   - pkgDir: Package version directory to scan
// Returns:
//   - fileCount: Number of files (excluding directories)
//   - totalSize: Total size of all files in bytes
func CountInstallDir(pkgDir string) (fileCount int, totalSize int64) {
	return apps.CountInstallDir(pkgDir)
}
