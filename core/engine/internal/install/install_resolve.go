package install

import (
	"errors"
	"fmt"

	"github.com/gluestick-sh/core/engine/internal/catalog"
	"github.com/gluestick-sh/core/engine/internal/override"
	"github.com/gluestick-sh/core/engine/internal/runtime"
	etypes "github.com/gluestick-sh/core/engine/types"
	"github.com/gluestick-sh/core/manifest"
	"github.com/gluestick-sh/core/message"
	"github.com/gluestick-sh/core/verbose"
)

// resolveInstallPhase handles the first phase of installation.
// This phase prepares the installation by:
// 1. Ensuring the required bucket is available
// 2. Finding and parsing the package manifest
// 3. Handling version pinning (e.g., package@1.2.3)
// 4. Checking for existing installations and version locks
// 5. Applying manifest overrides (local customizations)
// 6. Determining the installation architecture (arm64, amd64, etc.)
//
// Parameters:
//   - state: Current installation state (updated with resolution results)
//
// Returns error if:
// - Bucket cannot be found or accessed
// - Manifest cannot be parsed
// - Version pinning is requested but unsupported
// - Package is version-locked at a different version
// - Installation should be skipped (already installed)
//
// Side effects:
// - Updates state.pkgName, state.targetVersion, state.manifest
// - May return nil error if package is already installed (success case)
func resolveInstallPhase(state *installState) error {
	// Helper to report progress
	report := func(phase etypes.Phase, status etypes.Status, pct float64, key string, args map[string]any, bytes, total int64) {
		runtime.ReportProgress(state.reporter, phase, state.pkgRef, status, pct, key, args, bytes, total)
	}

	// Parse package reference
	state.pkgName, state.pinVersion = runtime.ParsePkgRef(state.pkgRef)
	root := state.engine.Config.RootDir

	// Resolve install ref if possible
	if resolved, resolveErr := catalog.ResolveInstallRef(state.engine, state.ctx, state.pkgRef); resolveErr == nil && resolved != "" {
		state.overrideRef = resolved
	}

	// Phase 1.1: Ensure bucket + find manifest
	report(etypes.PhaseResolve, etypes.StatusRunning, 0, message.ProgressResolvingManifest, nil, 0, 0)
	if err := catalog.EnsureBucketForInstall(state.engine, state.ctx, state.lookupRef, state.req.Buckets); err != nil {
		return err
	}

	manifestPath, m, err := state.engine.BucketRegistry.GetManifestPath(state.lookupRef)
	if err != nil {
		return catalog.WrapManifestNotFound(state.engine, state.pkgRef, err)
	}
	state.manifestPath = manifestPath

	// Phase 1.2: Handle version pinning
	if state.pinVersion != "" {
		if m.Version != state.pinVersion {
			verbose.Progressf("  Generating manifest for %s@%s (bucket has %s)...\n", state.pkgName, state.pinVersion, m.Version)
			m, err = m.ForVersion(state.pkgName, state.pinVersion)
			if err != nil {
				if errors.Is(err, manifest.ErrNoAutoupdate) {
					return fmt.Errorf("%s: use a manifest with autoupdate or install without @version", err)
				}
				return err
			}
		}
	}

	state.targetVersion = m.Version
	if state.pinVersion != "" {
		state.targetVersion = state.pinVersion
	}

	// Phase 1.3: Check for existing installations
	if !state.req.Force {
		if state.pinVersion == "" {
			if installedVersion, ok := runtime.InstalledPackage(root, state.pkgName); ok {
				if IsPackageVersionLocked(state.engine, state.pkgName) && installedVersion != state.targetVersion {
					return fmt.Errorf("%s is version-locked at %s; remove the lock or install with @version", state.pkgName, installedVersion)
				}
			}
		}

		activeVer := ActiveInstallVersion(root, state.pkgName)
		if runtime.VersionDirInstalled(root, state.pkgName, state.targetVersion) {
			if activeVer == state.targetVersion {
				if ver, needs := packageNeedsCurrentRepair(root, state.pkgName); needs && ver == state.targetVersion {
					if err := repairBrokenCurrentInstall(state.engine, state.pkgRef, state.pkgName, ver, m); err != nil {
						return err
					}
					state.done = true
					return nil
				}
				if packageInCache(state.engine, state.pkgName) {
					verbose.Progressf("  %s\n", runtime.FormatAlreadyInstalled(state.pkgName, state.targetVersion))
					state.done = true
					return nil
				}
				if orphanInstallRepairable(root, state.pkgName, state.targetVersion, m) {
					if err := repairOrphanInstall(state.engine, state.ctx, state.pkgRef, state.pkgName, state.targetVersion, m, state.req, state.reporter, state.prof); err != nil {
						return err
					}
					state.done = true
					return nil
				}
				verbose.Progressf("  Previous install incomplete; reinstalling...\n")
				state.clearInstallDir = true
			} else if LocalVersionReadyToActivate(root, state.pkgName, state.targetVersion, m) {
				if err := activateLocalVersion(state.engine, state.ctx, state.pkgRef, state.pkgName, state.targetVersion, m, state.req, state.reporter, state.prof); err != nil {
					return err
				}
				state.done = true
				return nil
			} else {
				verbose.Progressf("  Previous install incomplete; reinstalling...\n")
				state.clearInstallDir = true
			}
		}
	}

	// Phase 1.4: Display package info
	if m.Description != "" {
		verbose.Progressf("  %s\n", m.Description)
	}
	verbose.Progressf("  Version: %s\n", m.Version)

	// Phase 1.5: Determine architecture
	state.installArch = installArchitecture(state.req, m)
	if state.installArch != "" {
		verbose.Progressf("  Architecture: %s\n", state.installArch)
	}

	// Phase 1.6: Apply manifest overrides
	m, err = override.ApplyManifestOverrides(state.engine, state.overrideRef, manifestPath, m, state.installArch, state.req)
	if err != nil {
		return err
	}

	// Defensive check: ensure manifest is not nil after overrides
	if m == nil {
		return fmt.Errorf("manifest is nil after overrides for %s (internal error)", state.pkgRef)
	}

	state.manifest = m
	return nil
}
