package install

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/gluestick-sh/core/apps"
	"github.com/gluestick-sh/core/cache"
	"github.com/gluestick-sh/core/engine/internal/runtime"
	etypes "github.com/gluestick-sh/core/engine/types"
	"github.com/gluestick-sh/core/manifest"
	"github.com/gluestick-sh/core/message"
	"github.com/gluestick-sh/core/verbose"
)

// DownloadNameFromManifest extracts the download filename from a manifest.
// This function parses the first URL in the manifest and returns the
// local filename that should be used for the download.
// Parameters:
//   - m: Package manifest
// Returns the download filename, or empty string if no URLs are defined.
func DownloadNameFromManifest(m *manifest.Manifest) string {
	urls := m.GetURLs()
	if len(urls) == 0 {
		return ""
	}
	return downloadFilename(urls[0])
}

// packageInCache checks if a package has a cache entry.
// This is used to determine if a package is already in the content cache,
// which allows skipping download on reinstall.
// Parameters:
//   - e: Runtime engine
//   - pkgName: Package name to check
// Returns true if the package exists in the cache.
func packageInCache(e *runtime.Engine, pkgName string) bool {
	_, ok := e.Cache.Get(pkgName)
	return ok
}

// repairOrphanInstall completes installation when files exist but cache is missing.
// This handles the case where a package was partially installed (files on disk)
// but the cache index was not updated. It rescans the install directory and
// completes the registration (hooks, shims, cache index).
// Parameters:
//   - e: Runtime engine
//   - ctx: Context for cancellation
//   - pkgRef: Package reference string
//   - pkgName: Package name
//   - version: Package version
//   - m: Package manifest
//   - req: Installation request
//   - reporter: Progress reporter
//   - prof: Optional profile for timing
// Returns error if repair fails or install directory is invalid.
func repairOrphanInstall(e *runtime.Engine,
	ctx context.Context,
	pkgRef, pkgName, version string,
	m *manifest.Manifest,
	req *etypes.InstallRequest,
	reporter etypes.ProgressReporter,
	prof *installPhaseProfile,
) error {
	root := e.Config.RootDir
	installDir := filepath.Join(apps.PkgRoot(root, pkgName), version)
	if !runtime.VersionDirInstalled(root, pkgName, version) {
		return fmt.Errorf("repair %s@%s: install directory missing", pkgName, version)
	}

	files, totalSize, err := cache.ScanInstallDir(installDir, e.Store)
	if err != nil {
		return fmt.Errorf("repair %s@%s: scan install dir: %w", pkgName, version, err)
	}
	if len(files) == 0 {
		return fmt.Errorf("repair %s@%s: install directory has no indexed files", pkgName, version)
	}

	verbose.Progressf("  Completing registration for %s@%s (files on disk, missing cache index)...\n", pkgName, version)
	downloadName := DownloadNameFromManifest(m)
	return finalizePackageInstall(e,
		ctx, pkgRef, pkgName, m, installDir, downloadName, getFileExtensionFromURL(m.GetURL()),
		files, totalSize, req, reporter, prof,
	)
}

// finalizePackageInstall completes the installation process.
// This is the final phase that:
// 1. Restores persisted data (if any)
// 2. Runs pre-install hooks (if any)
// 3. Runs installer scripts (if any)
// 4. Flattens directory structure (if needed)
// 5. Validates manifest binaries
// 6. Runs post-install hooks
// 7. Creates the "current" symlink
// 8. Saves install record
// 9. Creates shims for binaries
// 10. Creates shortcuts (if any)
// 11. Updates cache index
//
// Parameters:
//   - e: Runtime engine
//   - ctx: Context for cancellation
//   - pkgRef: Package reference string
//   - pkgName: Package name
//   - m: Package manifest
//   - installDir: Directory where package is installed
//   - downloadName: Original download filename
//   - fileExt: File extension of the downloaded file
//   - installedFiles: Map of content hash -> relative path for installed files
//   - totalSize: Total size of all installed files
//   - req: Installation request
//   - reporter: Progress reporter
//   - prof: Optional profile for timing
//
// Returns error if any finalization step fails.
func finalizePackageInstall(e *runtime.Engine,
	ctx context.Context,
	pkgRef, pkgName string,
	m *manifest.Manifest,
	installDir, downloadName, fileExt string,
	installedFiles map[string]string,
	totalSize int64,
	req *etypes.InstallRequest,
	reporter etypes.ProgressReporter,
	prof *installPhaseProfile,
) error {
	report := func(phase etypes.Phase, status etypes.Status, pct float64, key string, args map[string]any, bytes, total int64) {
		runtime.ReportProgress(reporter, phase, pkgRef, status, pct, key, args, bytes, total)
	}

	root := e.Config.RootDir
	installArch := installArchitecture(req, m)
	persistDir := filepath.Join(root, "persist", pkgName)
	persistEntries := m.PersistEntriesForInstall(installArch)

	if len(persistEntries) > 0 && !isExeInstallerScriptInstall(fileExt, m, installArch) {
		if err := restorePersistOnInstall(installDir, persistDir, persistEntries); err != nil {
			return err
		}
	}

	if !isExeInstallerScriptInstall(fileExt, m, installArch) {
		hooks := m.PreInstallHooksForInstall(installArch)
		if isScoopMoveFlattenPreInstall(hooks) {
			if err := ensureScoopMoveFlattenInstallDir(installDir, installedFiles); err != nil {
				return err
			}
			hooks = nil
		}
		if isSevenZipArm64SFXPreInstall(pkgName, installArch, hooks) {
			if err := applySevenZipArm64SFXInstall(e, ctx, installDir, downloadName, archiveHashForDownload(installedFiles, downloadName), m); err != nil {
				return err
			}
		} else if len(hooks) > 0 {
			if err := prepareInstallDirForPreInstallHooks(installDir, persistDir, persistEntries); err != nil {
				return err
			}
			sevenZip, dark, err := ResolveHookHelpers(e, ctx, hooks, prof, pkgName)
			if err != nil {
				return err
			}
			env := NewHookScriptEnv(e, installDir, downloadName, m.Version, pkgRef, pkgName, installArch, hooks)
			env.SevenZip = sevenZip
			env.Dark = dark
			if err := runPreInstallHooks(env); err != nil {
				return err
			}
			if err := refreshInstalledFilesFromDir(e.Store, installDir, installedFiles, &totalSize); err != nil {
				return fmt.Errorf("index installed files after pre_install: %w", err)
			}
		}
	}
	if m.HasInstallerScriptForInstall(installArch) && !isExeInstallerScriptInstall(fileExt, m, installArch) {
		if err := runManifestInstallerHook(e, ctx, installDir, downloadName, pkgName, pkgRef, m, installArch, installInteractive(req)); err != nil {
			return err
		}
		if err := refreshInstalledFilesFromDir(e.Store, installDir, installedFiles, &totalSize); err != nil {
			return fmt.Errorf("index installed files after installer script: %w", err)
		}
	}
	flattened, err := ensureExtractDirFlattened(installDir, m, installArch)
	if err != nil {
		return err
	}
	if flattened {
		if err := refreshInstalledFilesFromDir(e.Store, installDir, installedFiles, &totalSize); err != nil {
			return fmt.Errorf("index installed files after flatten: %w", err)
		}
	}
	if err := validateManifestBins(installDir, m); err != nil {
		return err
	}

	if !isExeInstallerScriptInstall(fileExt, m, installArch) {
		preHooks := m.PreInstallHooksForInstall(installArch)
		if isScoopMoveFlattenPreInstall(preHooks) || installNeedsMoveFlatten(installDir) {
			if err := ensureScoopMoveFlattenInstallDir(installDir, installedFiles); err != nil {
				return err
			}
		}
		if err := runManifestPostInstall(e, ctx, pkgRef, pkgName, installDir, downloadName, m.Version, installArch, m, prof); err != nil {
			return err
		}
		if postInstallNeedsFileIndexRefresh(m.PostInstallHooksForInstall(installArch)) {
			if err := refreshInstalledFilesFromDir(e.Store, installDir, installedFiles, &totalSize); err != nil {
				return fmt.Errorf("index installed files after post_install: %w", err)
			}
		}
		if err := repairSevenZipArm64Layout(installDir); err != nil {
			return fmt.Errorf("repair 7zip arm64 layout: %w", err)
		}
		applyInstallContextRegistry(installDir)
	}

	report(PhaseLink, StatusRunning, 0, message.ProgressLinkingFiles, nil, 0, 0)
	pkgRoot := apps.PkgRoot(root, pkgName)
	if err := apps.LinkCurrent(pkgRoot, m.Version); err != nil {
		return fmt.Errorf("link current: %w", err)
	}
	verbose.Progressf("  Linking current => %s\n", m.Version)

	if err := apps.SaveInstallRecord(installDir, runtime.PackageBucketName(pkgRef), m); err != nil {
		return fmt.Errorf("save install record: %w", err)
	}

	report(PhaseShim, StatusRunning, 0, message.ProgressCreatingShims, nil, 0, 0)
	appsDir := filepath.Join(root, "apps")
	currentDir := filepath.Join(pkgRoot, apps.CurrentLinkName)
	shimsMetaDir := filepath.Join(root, "shims-meta")
	if _, err := removeShimsForPackage(e.ShimMgr, shimsMetaDir, appsDir, pkgName); err != nil {
		return fmt.Errorf("remove old shims: %w", err)
	}

	runShim := func(fn func() error) error { return fn() }
	if prof != nil {
		runShim = prof.runShim
	}
	if err := runShim(func() error {
		return createPackageShims(e.ShimMgr, shimsMetaDir, pkgName, installDir, currentDir, m)
	}); err != nil {
		return fmt.Errorf("create shims: %w", err)
	}

	if err := ApplyPackageEnvSet(m, installDir, installArch); err != nil {
		return fmt.Errorf("apply env_set: %w", err)
	}

	if err := createPackageShortcuts(installDir, m, installArch); err != nil {
		return fmt.Errorf("create shortcuts: %w", err)
	}

	report(PhaseIndex, StatusRunning, 0, message.ProgressUpdatingCache, nil, 0, 0)
	runCache := func(fn func() error) error { return fn() }
	if prof != nil {
		runCache = prof.runCache
	}
	if err := runCache(func() error {
		if err := e.Cache.Add(pkgName, m.Version, installedFiles, totalSize); err != nil {
			return err
		}
		metadata := preserveInstalledMetadata(e.Cache, pkgName)
		return e.Cache.AddInstalled(pkgName, m.Version, installDir, totalSize, metadata)
	}); err != nil {
		return fmt.Errorf("update cache index: %w", err)
	}

	verbose.Progressf("  %s %s@%s installed\n", successMark(), pkgName, m.Version)
	report(PhaseComplete, StatusSuccess, 100, message.ProgressPackageInstallComplete, map[string]any{
		"package": pkgName,
	}, 0, 0)
	return nil
}
