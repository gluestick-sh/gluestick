package install

import (
	"fmt"
	"path/filepath"
	"time"

	etypes "github.com/gluestick-sh/core/engine/types"
	"github.com/gluestick-sh/core/verbose"
)

// deployMSIAlias handles MSI dl.msi_ alias deployment.
// This is a Scoop-specific alias where MSI files are linked to the install directory
// and extracted later by pre_install hooks.
// Parameters:
//   - state: Current installation state
//   - hash: Content hash of the MSI file in CAS
//   - report: Progress reporting callback
// Returns error if the MSI file cannot be linked or installation fails.
func deployMSIAlias(state *installState, hash string,
	report func(etypes.Phase, etypes.Status, float64, string, map[string]any, int64, int64),
) error {
	reportDeployStart(report, true)
	linkStart := time.Now()
	targetPath := filepath.Join(state.installDir, state.downloadName)
	if err := state.engine.Store.Link(hash, targetPath); err != nil {
		return fmt.Errorf("link %s: %w", state.downloadName, err)
	}
	recordFile(state, hash, state.downloadName)
	if state.prof != nil {
		state.prof.addLink(linkStart)
	}
	verbose.Progressf("  Installed %s (MSI, pre_install extract)\n", state.downloadName)

	return finalizePackageInstall(state.engine, state.ctx, state.pkgRef, state.pkgName, state.manifest,
		state.installDir, state.downloadName, ".msi_", state.installedFiles, state.totalSize,
		state.req, state.reporter, state.prof)
}

// deployExe handles EXE deployment, routing to appropriate strategy.
// Supports multiple EXE types:
// - Portable executables (linked directly)
// - Self-extracting archives (SFX) with pre_install hooks
// - InnoSetup installers (extracted with innounp)
// - Custom installer scripts
// - Standard archives (extracted with 7z)
// Parameters:
//   - state: Current installation state
//   - casPath: Path to the EXE file in CAS store
//   - hash: Content hash of the EXE file
//   - reportIndexProgress: Progress callback for file indexing
//   - report: Progress reporting callback
// Returns error if deployment fails.
func deployExe(state *installState, casPath, hash string,
	reportIndexProgress func(int64, int64),
	report func(etypes.Phase, etypes.Status, float64, string, map[string]any, int64, int64),
) error {
	m := state.manifest
	installArch := state.installArch
	actualFileExt := normalizeInstallFileExt(state.fileExt)
	downloadName := state.downloadName

	// Handle portable exe installs
	if actualFileExt == ".exe" && isPortableExeInstall(m, downloadName, installArch) {
		linkStart := time.Now()
		if err := linkPortableExe(state.engine.Store, state.installDir, downloadName, hash, func(h, n string) {
			recordFile(state, h, n)
		}); err != nil {
			return err
		}
		if state.prof != nil {
			state.prof.addLink(linkStart)
		}
		verbose.Progressf("  Installed %s (portable exe)\n", downloadName)
		if err := validateInstallDir(state.installDir); err != nil {
			return err
		}
		if err := refreshInstalledFilesFromDir(state.engine.Store, state.installDir, state.installedFiles, &state.totalSize); err != nil {
			return fmt.Errorf("index installed files: %w", err)
		}
		return finalizePackageInstall(state.engine, state.ctx, state.pkgRef, state.pkgName, state.manifest,
			state.installDir, state.downloadName, ".exe", state.installedFiles, state.totalSize,
			state.req, state.reporter, state.prof)
	}

	// Handle pre-install 7z hook installs (SFX)
	if actualFileExt == ".exe" && isPreInstall7zHookInstall(m, downloadName, installArch, state.pkgName) {
		linkStart := time.Now()
		if err := linkPortableExe(state.engine.Store, state.installDir, downloadName, hash, func(h, n string) {
			recordFile(state, h, n)
		}); err != nil {
			return err
		}
		if state.prof != nil {
			state.prof.addLink(linkStart)
		}
		verbose.Progressf("  Installed %s (SFX, pre_install extract)\n", downloadName)
		if err := validateInstallDir(state.installDir); err != nil {
			return err
		}
		if err := refreshInstalledFilesFromDir(state.engine.Store, state.installDir, state.installedFiles, &state.totalSize); err != nil {
			return fmt.Errorf("index installed files: %w", err)
		}
		return finalizePackageInstall(state.engine, state.ctx, state.pkgRef, state.pkgName, state.manifest,
			state.installDir, state.downloadName, ".exe", state.installedFiles, state.totalSize,
			state.req, state.reporter, state.prof)
	}

	// Handle InnoSetup installers
	if actualFileExt == ".exe" && m.InnoSetup {
		return deployInnoSetup(state, casPath, report)
	}

	// Handle installer scripts
	if actualFileExt == ".exe" && m.HasInstallerScriptForInstall(installArch) {
		return deployInstallerScript(state, casPath, hash, reportIndexProgress, report)
	}

	// Handle as archive (7z, etc.)
	return deployExeAsArchive(state, casPath, hash, reportIndexProgress, report)
}

// deployInnoSetup handles InnoSetup installer deployment.
// InnoSetup installers are extracted using the innounp tool.
// Parameters:
//   - state: Current installation state
//   - casPath: Path to the InnoSetup installer in CAS store
// Returns error if extraction fails.
func deployInnoSetup(state *installState, casPath string,
	_ /*report*/ func(etypes.Phase, etypes.Status, float64, string, map[string]any, int64, int64),
) error {
	if err := extractInnoInstaller(state.engine, state.ctx, state.prof, state.engine.Config.RootDir, casPath, state.installDir, state.pkgName, state.manifest, state.installedFiles, &state.totalSize); err != nil {
		return err
	}
	return finalizePackageInstall(state.engine, state.ctx, state.pkgRef, state.pkgName, state.manifest,
		state.installDir, state.downloadName, ".exe", state.installedFiles, state.totalSize,
		state.req, state.reporter, state.prof)
}

// deployInstallerScript handles deployment via custom installer scripts.
// Installer scripts (installer.ps1, installer.sh) are executed to perform
// custom installation logic defined in the manifest.
// Parameters:
//   - state: Current installation state
//   - hash: Content hash of the downloaded file
//   - reportIndexProgress: Progress callback for file indexing
// Returns error if script execution fails.
func deployInstallerScript(state *installState, _ /* casPath */, hash string,
	reportIndexProgress func(int64, int64),
	_ /*report*/ func(etypes.Phase, etypes.Status, float64, string, map[string]any, int64, int64),
) error {
	m := state.manifest
	installArch := state.installArch
	if _, err := extractViaInstallerScript(state.engine, state.ctx, state.prof, state.pkgRef, state.installDir, state.downloadName, hash, installArch, m, installInteractive(state.req), state.installedFiles, &state.totalSize, reportIndexProgress); err != nil {
		return err
	}
	return finalizePackageInstall(state.engine, state.ctx, state.pkgRef, state.pkgName, state.manifest,
		state.installDir, state.downloadName, ".exe", state.installedFiles, state.totalSize,
		state.req, state.reporter, state.prof)
}

// deployExeAsArchive handles EXE deployment as a standard archive.
// Some EXE files are actually self-extracting archives that can be
// extracted with 7-Zip. This function handles those cases.
// Parameters:
//   - state: Current installation state
//   - casPath: Path to the EXE file in CAS store
//   - hash: Content hash of the EXE file
//   - reportIndexProgress: Progress callback for file indexing
//   - report: Progress reporting callback
// Returns error if extraction fails or no files are extracted.
func deployExeAsArchive(state *installState, casPath, hash string,
	reportIndexProgress func(int64, int64),
	report func(etypes.Phase, etypes.Status, float64, string, map[string]any, int64, int64),
) error {
	if err := ensureExtractor7zWithProf(state.engine, state.ctx, state.prof, state.pkgName); err != nil {
		return fmt.Errorf("ensure 7z: %w", err)
	}

	m := state.manifest
	installArch := state.installArch
	extractTo := m.GetExtractToForInstall(installArch)
	extractDir := m.GetExtractDirForInstall(installArch)
	downloadName := state.downloadName

	reportDeployStart(report, archiveMemberIndexReady(state.engine.Downloader, hash))
	count, err := installArchiveFromMemberIndex(state.engine, state.prof, casPath, state.installDir, downloadName, hash, extractTo, extractDir, state.installedFiles, &state.totalSize, reportIndexProgress)
	if err != nil {
		return extractPhaseError(err)
	}
	if count == 0 {
		return fmt.Errorf("no files were extracted from %s", state.fileExt)
	}
	verbose.Progressf("  Installed %d file(s)\n", count)

	return finalizePackageInstall(state.engine, state.ctx, state.pkgRef, state.pkgName, state.manifest,
		state.installDir, state.downloadName, ".exe", state.installedFiles, state.totalSize,
		state.req, state.reporter, state.prof)
}

// deployGenericFile handles single-file package deployment.
// Used for simple packages consisting of a single file (e.g., .bat, .cmd, .ps1, .jar, .sh).
// These files are linked directly without extraction.
// Parameters:
//   - state: Current installation state
//   - hash: Content hash of the file in CAS store
// Returns error if the file cannot be linked.
func deployGenericFile(state *installState, _ /* casPath */, hash string,
	_ /*report*/ func(etypes.Phase, etypes.Status, float64, string, map[string]any, int64, int64),
) error {
	linkStart := time.Now()
	targetPath := filepath.Join(state.installDir, state.downloadName)
	if err := state.engine.Store.Link(hash, targetPath); err != nil {
		return fmt.Errorf("link %s: %w", state.downloadName, err)
	}
	recordFile(state, hash, state.downloadName)
	if state.prof != nil {
		state.prof.addLink(linkStart)
	}
	verbose.Progressf("  Installed %s\n", state.downloadName)

	return finalizePackageInstall(state.engine, state.ctx, state.pkgRef, state.pkgName, state.manifest,
		state.installDir, state.downloadName, state.fileExt, state.installedFiles, state.totalSize,
		state.req, state.reporter, state.prof)
}
