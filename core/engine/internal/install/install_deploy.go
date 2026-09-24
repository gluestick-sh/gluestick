package install

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/gluestick-sh/core/downloader"
	"github.com/gluestick-sh/core/engine/internal/runtime"
	etypes "github.com/gluestick-sh/core/engine/types"
	"github.com/gluestick-sh/core/message"
	"github.com/gluestick-sh/core/verbose"
)

// deployInstallPhase handles the third phase of installation:
// 1. Creates install directory
// 2. Routes to appropriate deployment strategy based on:
//   - multiArtifact vs single artifact
//   - fromCache vs fresh download
//
// 3. Calls finalizePackageInstall to complete installation
func deployInstallPhase(state *installState) error {
	// Helper to report progress
	report := func(phase etypes.Phase, status etypes.Status, pct float64, key string, args map[string]any, bytes, total int64) {
		runtime.ReportProgress(state.reporter, phase, state.pkgRef, status, pct, key, args, bytes, total)
	}

	root := state.engine.Config.RootDir

	// Validate manifest exists
	if state.manifest == nil {
		return fmt.Errorf("manifest is nil for %s (internal error)", state.pkgRef)
	}

	// Phase 3.1: Create install directory
	state.appsDir = filepath.Join(root, "apps")
	state.installDir = filepath.Join(state.appsDir, runtime.PackageBaseName(state.pkgRef), state.manifest.Version)

	if err := os.MkdirAll(state.installDir, 0755); err != nil {
		return permissionPhaseError("failed to create install directory", err)
	}

	// Phase 3.2: Clean install directory if needed
	if state.clearInstallDir {
		if err := cleanInstallDir(state.installDir); err != nil {
			return fmt.Errorf("clean install dir: %w", err)
		}
	}

	// Phase 3.3: Setup progress handlers for deployment
	attachExtractProgressHandlers(&state.prog, report)

	reportIndexProgress := func(processed, total int64) {
		pct := 0.0
		if total > 0 {
			pct = (float64(processed) / float64(total)) * 100
		}
		report(etypes.PhaseExtract, etypes.StatusRunning, pct, message.ProgressExtractIndexing, map[string]any{
			"current": processed,
			"total":   total,
		}, processed, total)
	}

	verbose.Progressf("  Installing to %s\n", state.installDir)

	// Phase 3.4: Route to appropriate deployment strategy
	actualFileExt := normalizeInstallFileExt(state.fileExt)

	if state.multiArtifact {
		return deployMultiArtifacts(state)
	}

	if state.fromCache {
		return deployFromCache(state, actualFileExt, reportIndexProgress)
	}

	return deployFromDownload(state, actualFileExt, reportIndexProgress)
}

// deployMultiArtifacts handles multi-artifact deployment
func deployMultiArtifacts(state *installState) error {
	report := func(phase etypes.Phase, status etypes.Status, pct float64, key string, args map[string]any, bytes, total int64) {
		runtime.ReportProgress(state.reporter, phase, state.pkgRef, status, pct, key, args, bytes, total)
	}

	if err := cleanInstallDir(state.installDir); err != nil {
		return fmt.Errorf("clean install dir: %w", err)
	}

	reportDeployStart(report, true)
	linkStart := time.Now()
	var linked int

	if state.fromCache {
		names, err := expectedMultiArtifactNames(state.urlHashPairs)
		if err != nil {
			return err
		}
		linked, err = linkMultiArtifactFromCache(state.engine.Store, state.installDir, state.cachedEntry, names, func(hash, name string) {
			recordFile(state, hash, name)
		})
		if err != nil {
			return err
		}
	} else {
		var err error
		linked, err = linkMultiArtifactFromResults(state.engine.Store, state.installDir, state.downloadResults, func(hash, name string) {
			recordFile(state, hash, name)
		})
		if err != nil {
			return err
		}
	}

	if state.prof != nil {
		state.prof.addLink(linkStart)
	}

	verbose.Progressf("  Installed %d file(s)\n", linked)
	return finalizePackageInstall(state.engine, state.ctx, state.pkgRef, state.pkgName, state.manifest,
		state.installDir, state.downloadName, state.fileExt, state.installedFiles, state.totalSize,
		state.req, state.reporter, state.prof)
}

// deployFromCache handles deployment from cache
func deployFromCache(state *installState, fileExt string, reportIndexProgress func(int64, int64)) error {
	report := func(phase etypes.Phase, status etypes.Status, pct float64, key string, args map[string]any, bytes, total int64) {
		runtime.ReportProgress(state.reporter, phase, state.pkgRef, status, pct, key, args, bytes, total)
	}

	m := state.manifest
	installArch := state.installArch
	downloadName := state.downloadName

	// Find archive hash from cache
	archiveHash := findCacheArchiveHash(state.cachedEntry, downloadName, state.firstHashValue)

	// Handle special portable exe installs
	if isPortableExeInstall(m, downloadName, installArch) && archiveHash != "" {
		return deployPortableExeFromCache(state, archiveHash, report)
	}

	// Handle pre-install 7z hook installs (SFX)
	if isPreInstall7zHookInstall(m, downloadName, installArch, state.pkgName) && archiveHash != "" {
		return deploySFXFromCache(state, archiveHash, report)
	}

	// Handle archive deployment
	return deployArchiveFromCache(state, fileExt, archiveHash, reportIndexProgress)
}

// deployFromDownload handles deployment from fresh downloads
func deployFromDownload(state *installState, fileExt string, reportIndexProgress func(int64, int64)) error {
	report := func(phase etypes.Phase, status etypes.Status, pct float64, key string, args map[string]any, bytes, total int64) {
		runtime.ReportProgress(state.reporter, phase, state.pkgRef, status, pct, key, args, bytes, total)
	}

	result := state.downloadResults[0]
	casPath := state.engine.Store.ObjectPath(result.Hash)

	// Handle different file types
	switch fileExt {
	case ".zip", ".nupkg":
		return deployZipArchive(state, casPath, result.Hash, reportIndexProgress, report)

	case ".tar", ".tgz":
		return deployTarArchive(state, casPath, result.Hash, reportIndexProgress, report)

	case ".msi_":
		return deployMSIAlias(state, result.Hash, report)

	case ".msi":
		return deployMSI(state, casPath, result.Hash, reportIndexProgress, report)

	case ".exe":
		return deployExe(state, casPath, result.Hash, reportIndexProgress, report)

	case ".7z", ".7z.exe":
		return deploy7zArchive(state, casPath, result.Hash, reportIndexProgress, report)

	default:
		return deployGenericFile(state, casPath, result.Hash, report)
	}
}

// deployPortableExeFromCache handles portable executable deployment from cache.
// Portable executables are single-file programs that don't require installation.
// They are linked directly from the CAS store to the install directory.
// Parameters:
//   - state: Current installation state
//   - archiveHash: Content hash of the portable executable
//   - report: Progress reporting callback
// Returns error if linking fails or installation directory is invalid.
func deployPortableExeFromCache(state *installState, archiveHash string, report func(etypes.Phase, etypes.Status, float64, string, map[string]interface{}, int64, int64)) error {
	if err := cleanInstallDir(state.installDir); err != nil {
		return fmt.Errorf("clean install dir: %w", err)
	}

	reportDeployStart(report, true)
	linkStart := time.Now()
	if err := linkPortableExe(state.engine.Store, state.installDir, state.downloadName, archiveHash, func(hash, name string) {
		recordFile(state, hash, name)
	}); err != nil {
		return err
	}

	if state.prof != nil {
		state.prof.addLink(linkStart)
	}

	verbose.Progressf("  Installed %s (portable exe)\n", state.downloadName)
	if err := validateInstallDir(state.installDir); err != nil {
		return err
	}

	if err := refreshInstalledFilesFromDir(state.engine.Store, state.installDir, state.installedFiles, &state.totalSize); err != nil {
		return fmt.Errorf("index installed files: %w", err)
	}

	return finalizePackageInstall(state.engine, state.ctx, state.pkgRef, state.pkgName, state.manifest,
		state.installDir, state.downloadName, state.fileExt, state.installedFiles, state.totalSize,
		state.req, state.reporter, state.prof)
}

// deploySFXFromCache handles SFX (self-extracting archive) deployment from cache.
// SFX executables are archives that extract themselves when run.
// This function links the SFX file to the install directory; pre_install hooks
// will later extract its contents.
// Parameters:
//   - state: Current installation state
//   - archiveHash: Content hash of the SFX file
//   - report: Progress reporting callback
// Returns error if linking or validation fails.
func deploySFXFromCache(state *installState, archiveHash string, report func(etypes.Phase, etypes.Status, float64, string, map[string]interface{}, int64, int64)) error {
	if err := cleanInstallDir(state.installDir); err != nil {
		return fmt.Errorf("clean install dir: %w", err)
	}

	reportDeployStart(report, true)
	linkStart := time.Now()
	if err := linkPortableExe(state.engine.Store, state.installDir, state.downloadName, archiveHash, func(hash, name string) {
		recordFile(state, hash, name)
	}); err != nil {
		return err
	}

	if state.prof != nil {
		state.prof.addLink(linkStart)
	}

	verbose.Progressf("  Installed %s (SFX, pre_install extract)\n", state.downloadName)
	if err := validateInstallDir(state.installDir); err != nil {
		return err
	}

	if err := refreshInstalledFilesFromDir(state.engine.Store, state.installDir, state.installedFiles, &state.totalSize); err != nil {
		return fmt.Errorf("index installed files: %w", err)
	}

	return finalizePackageInstall(state.engine, state.ctx, state.pkgRef, state.pkgName, state.manifest,
		state.installDir, state.downloadName, state.fileExt, state.installedFiles, state.totalSize,
		state.req, state.reporter, state.prof)
}

// deployArchiveFromCache handles archive deployment from cache.
// This function deploys archives (zip, 7z, tar, etc.) that were previously
// downloaded and cached. It verifies the archive and links extracted files
// from the CAS store to the install directory.
// Parameters:
//   - state: Current installation state
//   - fileExt: File extension of the archive
//   - archiveHash: Content hash of the archive in CAS
//   - reportIndexProgress: Progress callback for file indexing
// Returns error if verification, linking, or finalization fails.
func deployArchiveFromCache(state *installState, fileExt string, archiveHash string, reportIndexProgress func(int64, int64)) error {
	_ = reportIndexProgress // progress callback not supported by LinkFromCache
	report := func(phase etypes.Phase, status etypes.Status, pct float64, key string, args map[string]any, bytes, total int64) {
		runtime.ReportProgress(state.reporter, phase, state.pkgRef, status, pct, key, args, bytes, total)
	}

	downloadName := state.downloadName

	// Verify archive if needed
	if state.firstHashValue != "" && archiveHash != "" {
		if err := downloader.VerifyArchiveObject(state.engine.Store, downloader.Task{
			HashAlgo:  state.firstHashAlgo,
			HashValue: state.firstHashValue,
		}, archiveHash); err != nil {
			return phaseError("file verification failed", err, "Reinstall the package or try a different mirror")
		}
	}

	// Link from cache
	reportDeployStart(report, true)
	linkStart := time.Now()
	installArch := state.installArch
	m := state.manifest
	extractTo := m.GetExtractToForInstall(installArch)
	extractDir := m.GetExtractDirForInstall(installArch)

	linked, err := LinkFromCache(state.engine.Store, state.installDir, state.cachedEntry, downloadName, fileExt, archiveHash, extractTo, extractDir, m, installArch, state.pkgName)
	if state.prof != nil {
		state.prof.addLink(linkStart)
	}
	if err != nil {
		return err
	}

	verbose.Progressf("  Linked %d file(s) from cache\n", linked)

	// If no files were linked, the cache might only contain the archive without extracted files
	// We need to extract the archive and link the extracted files
	if linked == 0 && archiveHash != "" {
		verbose.Progressf("  No files linked from cache, extracting archive...\n")
		return deployArchiveFromCacheExtract(state, fileExt, archiveHash, extractTo, extractDir, reportIndexProgress)
	}

	if err := validateInstallDir(state.installDir); err != nil {
		return err
	}

	if err := refreshInstalledFilesFromDir(state.engine.Store, state.installDir, state.installedFiles, &state.totalSize); err != nil {
		return fmt.Errorf("index installed files: %w", err)
	}

	return finalizePackageInstall(state.engine, state.ctx, state.pkgRef, state.pkgName, state.manifest,
		state.installDir, state.downloadName, fileExt, state.installedFiles, state.totalSize,
		state.req, state.reporter, state.prof)
}

// deployZipArchive handles ZIP archive deployment from download.
// ZIP archives can be deployed in two ways:
// 1. If a member index exists, files are linked directly from CAS
// 2. If no index exists, the ZIP is extracted and files are adopted into CAS
// Parameters:
//   - state: Current installation state
//   - casPath: Path to the ZIP file in CAS store
//   - archiveHash: Content hash of the ZIP archive
//   - reportIndexProgress: Progress callback for file indexing
//   - report: Progress reporting callback
// Returns error if deployment fails.
func deployZipArchive(state *installState, casPath string, archiveHash string, reportIndexProgress func(int64, int64), report func(etypes.Phase, etypes.Status, float64, string, map[string]interface{}, int64, int64)) error {
	m := state.manifest
	installArch := state.installArch
	extractTo := m.GetExtractToForInstall(installArch)
	extractDir := m.GetExtractDirForInstall(installArch)

	zipFiles, zipTotalBytes, _ := state.engine.Downloader.ResolveZipMemberIndex(archiveHash)

	if len(zipFiles) == 0 {
		// No index exists, need to extract and adopt files
		return deployZipExtractAndAdopt(state, casPath, extractTo, extractDir, archiveHash, reportIndexProgress, report)
	}

	// Index exists, link files directly
	return deployZipFromIndex(state, archiveHash, zipFiles, zipTotalBytes, extractTo, extractDir, report)
}

// deployZipExtractAndAdopt handles ZIP extraction and adoption into CAS store.
// Used when no member index exists for the ZIP. The archive is extracted to
// the install directory, then files are adopted into the CAS store for future reuse.
// Parameters:
//   - state: Current installation state
//   - casPath: Path to the ZIP file in CAS store
//   - extractTo: Target subdirectory for extraction (from manifest)
//   - extractDir: Directory layout to apply after extraction (from manifest)
//   - archiveHash: Content hash of the ZIP archive
//   - reportIndexProgress: Progress callback for file indexing
//   - report: Progress reporting callback
// Returns error if extraction or adoption fails.
func deployZipExtractAndAdopt(state *installState, casPath, extractTo, extractDir, archiveHash string, reportIndexProgress func(int64, int64), report func(etypes.Phase, etypes.Status, float64, string, map[string]interface{}, int64, int64)) error {
	if err := ensureExtractor7zWithProf(state.engine, state.ctx, state.prof, state.pkgName); err != nil {
		return fmt.Errorf("ensure 7z: %w", err)
	}
	if err := cleanInstallDir(state.installDir); err != nil {
		return fmt.Errorf("clean install dir: %w", err)
	}

	reportDeployStart(report, false)
	var zipFiles map[string]string
	var zipTotalBytes int64

	runExtractAdopt := func() error {
		verbose.Progressf("  Extracting zip to install dir...\n")
		dest, err := installExtractDest(state.installDir, extractTo)
		if err != nil {
			return fmt.Errorf("extract destination: %w", err)
		}
		if err := state.engine.Extractor.ExtractToDir(casPath, dest, state.downloadName); err != nil {
			return err
		}
		if extractDir != "" {
			if _, err := applyExtractDirLayout(dest, extractDir); err != nil {
				return fmt.Errorf("apply extract_dir: %w", err)
			}
		}
		verbose.Progressf("  Adopting installed files into store...\n")
		zipFiles, zipTotalBytes, err = adoptInstallDirToStore(state.engine.Store, state.installDir, reportIndexProgress)
		return err
	}

	var extractErr error
	if state.prof != nil {
		extractErr = state.prof.runExtract(runExtractAdopt)
	} else {
		extractErr = runExtractAdopt()
	}
	if extractErr != nil {
		return extractPhaseError(extractErr)
	}

	if archiveInfo, err := os.Stat(casPath); err == nil {
		zipTotalBytes += archiveInfo.Size()
	}
	if err := state.engine.Downloader.SaveZipMemberIndex(archiveHash, zipFiles, zipTotalBytes); err != nil {
		return fmt.Errorf("save zip member index: %w", err)
	}

	zipIndexed, zipSize, err := indexZipMemberLinks(state.engine.Store, zipFiles, zipTotalBytes, extractTo, extractDir, state.downloadName, archiveHash)
	if err != nil {
		return fmt.Errorf("index adopted files: %w", err)
	}
	for hash, rel := range zipIndexed {
		state.installedFiles[hash] = rel
	}
	state.totalSize = zipSize
	verbose.Progressf("  Installed %d file(s)\n", len(zipFiles))

	return finalizePackageInstall(state.engine, state.ctx, state.pkgRef, state.pkgName, state.manifest,
		state.installDir, state.downloadName, state.fileExt, state.installedFiles, state.totalSize,
		state.req, state.reporter, state.prof)
}

// deployZipFromIndex handles ZIP deployment using existing member index.
// When a ZIP member index exists, files can be linked directly from the CAS store
// without re-extracting the archive. This is much faster than extraction.
// Parameters:
//   - state: Current installation state
//   - archiveHash: Content hash of the ZIP archive
//   - zipFiles: Map of content hashes to relative paths within the ZIP
//   - zipTotalBytes: Total size of all files in the ZIP
//   - extractTo: Target subdirectory for extraction (from manifest)
//   - extractDir: Directory layout to apply after extraction (from manifest)
//   - report: Progress reporting callback
// Returns error if linking or finalization fails.
func deployZipFromIndex(state *installState, archiveHash string, zipFiles map[string]string, zipTotalBytes int64, extractTo, extractDir string, report func(etypes.Phase, etypes.Status, float64, string, map[string]interface{}, int64, int64)) error {
	if err := cleanInstallDir(state.installDir); err != nil {
		return fmt.Errorf("clean install dir: %w", err)
	}

	reportDeployStart(report, true)
	verbose.Progressf("  Linking files from store...\n")
	linkStart := time.Now()

	linked, err := LinkExtractedFiles(state.engine.Store, state.installDir, extractTo, extractDir, zipFiles, func(hash, name string) {
		recordFile(state, hash, name)
	})
	if state.prof != nil {
		state.prof.addLink(linkStart)
	}
	if err != nil {
		return err
	}

	if linked == 0 {
		return fmt.Errorf("no files were linked from archive")
	}

	verbose.Progressf("  Linked %d file(s)\n", linked)

	zipIndexed, zipSize, err := indexZipMemberLinks(state.engine.Store, zipFiles, zipTotalBytes, extractTo, extractDir, state.downloadName, archiveHash)
	if err != nil {
		return fmt.Errorf("index linked files: %w", err)
	}
	for hash, rel := range zipIndexed {
		state.installedFiles[hash] = rel
	}
	state.totalSize = zipSize

	return finalizePackageInstall(state.engine, state.ctx, state.pkgRef, state.pkgName, state.manifest,
		state.installDir, state.downloadName, state.fileExt, state.installedFiles, state.totalSize,
		state.req, state.reporter, state.prof)
}

// deployMSI handles MSI (Microsoft Installer) deployment.
// MSI files can be deployed in two ways:
// 1. Administrative installation (msiexec /a): Extracts all files to a directory
// 2. Standard extraction: Uses 7-Zip to extract MSI contents
// The method used depends on the manifest configuration.
// Parameters:
//   - state: Current installation state
//   - casPath: Path to the MSI file in CAS store
//   - hash: Content hash of the MSI file
//   - reportIndexProgress: Progress callback for file indexing
//   - report: Progress reporting callback
// Returns error if deployment fails.
func deployMSI(state *installState, casPath, hash string, reportIndexProgress func(int64, int64), report func(etypes.Phase, etypes.Status, float64, string, map[string]interface{}, int64, int64)) error {
	m := state.manifest
	installArch := state.installArch
	extractDir := m.GetExtractDirForInstall(installArch)

	if msiNeedsAdministrativeExtract(m, installArch) {
		return deployMSIAdmin(state, casPath, hash, extractDir, report)
	}

	// MSI without extract_dir: extract with 7z
	return deployMSIStandard(state, casPath, hash, extractDir, reportIndexProgress, report)
}

// deployMSIAdmin handles MSI administrative installation.
// Administrative installation (msiexec /a) extracts all files from an MSI
// to a directory without installing the product. This is useful for
// portable installations.
// Parameters:
//   - state: Current installation state
//   - hash: Content hash of the MSI file
//   - extractDir: Directory layout to apply after extraction (from manifest)
//   - report: Progress reporting callback
// Returns error if administrative installation fails.
func deployMSIAdmin(state *installState, _ /*casPath*/, hash string, extractDir string, report func(etypes.Phase, etypes.Status, float64, string, map[string]interface{}, int64, int64)) error {
	reportDeployStart(report, false)
	linkStart := time.Now()
	msiTarget := filepath.Join(state.installDir, state.downloadName)
	if err := state.engine.Store.Link(hash, msiTarget); err != nil {
		return fmt.Errorf("link %s: %w", state.downloadName, err)
	}
	recordFile(state, hash, state.downloadName)
	if state.prof != nil {
		state.prof.addLink(linkStart)
	}
	verbose.Progressf("  Extracting MSI via administrative install...\n")
	if err := state.prof.runExtract(func() error {
		return extractMSIAdministrative(state.installDir, msiTarget, extractDir)
	}); err != nil {
		return err
	}
	if err := refreshInstalledFilesFromDir(state.engine.Store, state.installDir, state.installedFiles, &state.totalSize); err != nil {
		return fmt.Errorf("index installed files: %w", err)
	}
	verbose.Progressf("  Installed MSI files\n")

	return finalizePackageInstall(state.engine, state.ctx, state.pkgRef, state.pkgName, state.manifest,
		state.installDir, state.downloadName, ".msi", state.installedFiles, state.totalSize,
		state.req, state.reporter, state.prof)
}

// deployMSIStandard handles standard MSI extraction using 7-Zip.
// This method extracts MSI contents using 7-Zip instead of administrative
// installation. It's faster but may not work for all MSI files.
// Parameters:
//   - state: Current installation state
//   - casPath: Path to the MSI file in CAS store
//   - extractDir: Directory layout to apply after extraction (from manifest)
//   - report: Progress reporting callback
// Returns error if extraction fails or no files are extracted.
func deployMSIStandard(state *installState, casPath, _ /*hash*/ string, extractDir string, _ /* reportIndexProgress */ func(int64, int64), report func(etypes.Phase, etypes.Status, float64, string, map[string]interface{}, int64, int64)) error {
	//_ = reportIndexProgress // progress callback not supported by underlying MSI functions
	reportDeployStart(report, false)
	if err := ensureExtractor7zWithProf(state.engine, state.ctx, state.prof, state.pkgName); err != nil {
		return fmt.Errorf("ensure 7z: %w", err)
	}

	verbose.Progressf("  Extracting MSI (this may take a while)...\n")
	var files map[string]string
	if err := state.prof.runExtract(func() error {
		var err error
		_, files, err = state.engine.Extractor.ExtractMSI(casPath)
		if err != nil {
			return fmt.Errorf("extract MSI: %w", err)
		}
		return nil
	}); err != nil {
		return err
	}

	m := state.manifest
	installArch := state.installArch
	extractTo := m.GetExtractToForInstall(installArch)

	linkStart := time.Now()
	linked, err := LinkExtractedFiles(state.engine.Store, state.installDir, extractTo, extractDir, files, func(hash, name string) {
		recordFile(state, hash, name)
	})
	if err != nil {
		return err
	}
	if state.prof != nil {
		state.prof.addLink(linkStart)
	}
	if linked == 0 {
		return fmt.Errorf("no files were linked from msi")
	}
	verbose.Progressf("  Linked %d files\n", linked)
	if extractDir != "" {
		if _, err := applyExtractDirLayout(state.installDir, extractDir); err != nil {
			return fmt.Errorf("apply extract_dir: %w", err)
		}
	}

	return finalizePackageInstall(state.engine, state.ctx, state.pkgRef, state.pkgName, state.manifest,
		state.installDir, state.downloadName, ".msi", state.installedFiles, state.totalSize,
		state.req, state.reporter, state.prof)
}

// deployArchiveFromCacheExtract extracts archive from cache when no files were linked.
// This handles the case where the cache entry only contains the archive without extracted files.
// The archive is extracted, files are adopted into CAS, and installation is finalized.
func deployArchiveFromCacheExtract(state *installState, fileExt, archiveHash, extractTo, extractDir string, reportIndexProgress func(int64, int64)) error {
	report := func(phase etypes.Phase, status etypes.Status, pct float64, key string, args map[string]interface{}, bytes, total int64) {
		runtime.ReportProgress(state.reporter, phase, state.pkgRef, status, pct, key, args, bytes, total)
	}

	// Get CAS path for the archive
	casPath := state.engine.Store.ObjectPath(archiveHash)
	if _, err := os.Stat(casPath); err != nil {
		return fmt.Errorf("archive not found in CAS: %w", err)
	}

	// Handle different archive types based on file extension
	switch fileExt {
	case ".zip", ".nupkg":
		return deployZipExtractAndAdopt(state, casPath, extractTo, extractDir, archiveHash, reportIndexProgress, report)

	case ".tar", ".tgz":
		// Handle tar archives (including .tar.gz which is normalized to .tar)
		return deployTarExtractAndAdopt(state, casPath, extractTo, extractDir, archiveHash, reportIndexProgress, report)

	case ".7z":
		// Handle 7z archives
		return deploy7zExtractAndAdopt(state, casPath, extractTo, extractDir, archiveHash, reportIndexProgress, report)

	default:
		return fmt.Errorf("unsupported archive format for extraction from cache: %s", fileExt)
	}
}

// deployTarExtractAndAdopt handles tar archive extraction and adoption into CAS store.
// Used when tar archives need to be extracted from cache but no member index exists.
func deployTarExtractAndAdopt(state *installState, casPath, extractTo, extractDir, archiveHash string, reportIndexProgress func(int64, int64), report func(etypes.Phase, etypes.Status, float64, string, map[string]interface{}, int64, int64)) error {
	if err := cleanInstallDir(state.installDir); err != nil {
		return fmt.Errorf("clean install dir: %w", err)
	}

	reportDeployStart(report, false)
	var tarFiles map[string]string
	var tarTotalBytes int64

	runExtractAdopt := func() error {
		verbose.Progressf("  Extracting tar to install dir...\n")
		dest, err := installExtractDest(state.installDir, extractTo)
		if err != nil {
			return fmt.Errorf("extract destination: %w", err)
		}
		if err := state.engine.Extractor.ExtractToDir(casPath, dest, state.downloadName); err != nil {
			return err
		}
		if extractDir != "" {
			if _, err := applyExtractDirLayout(dest, extractDir); err != nil {
				return fmt.Errorf("apply extract_dir: %w", err)
			}
		}
		verbose.Progressf("  Adopting installed files into store...\n")
		tarFiles, tarTotalBytes, err = adoptInstallDirToStore(state.engine.Store, state.installDir, reportIndexProgress)
		return err
	}

	var extractErr error
	if state.prof != nil {
		extractErr = state.prof.runExtract(runExtractAdopt)
	} else {
		extractErr = runExtractAdopt()
	}
	if extractErr != nil {
		return extractPhaseError(extractErr)
	}

	// Update total size
	if archiveInfo, err := os.Stat(casPath); err == nil {
		tarTotalBytes += archiveInfo.Size()
	}

	// Save tar member index for future reuse
	if err := state.engine.Downloader.SaveZipMemberIndex(archiveHash, tarFiles, tarTotalBytes); err != nil {
		verbose.Progressf("  Warning: failed to save tar index: %v\n", err)
	}

	if err := refreshInstalledFilesFromDir(state.engine.Store, state.installDir, state.installedFiles, &state.totalSize); err != nil {
		return fmt.Errorf("index installed files: %w", err)
	}

	return finalizePackageInstall(state.engine, state.ctx, state.pkgRef, state.pkgName, state.manifest,
		state.installDir, state.downloadName, ".tar", state.installedFiles, state.totalSize,
		state.req, state.reporter, state.prof)
}

// deploy7zExtractAndAdopt handles 7z archive extraction and adoption into CAS store.
// Used when 7z archives need to be extracted from cache.
func deploy7zExtractAndAdopt(state *installState, casPath, extractTo, extractDir, archiveHash string, reportIndexProgress func(int64, int64), report func(etypes.Phase, etypes.Status, float64, string, map[string]interface{}, int64, int64)) error {
	// For now, treat 7z similar to tar - extract, adopt, finalize
	return deployTarExtractAndAdopt(state, casPath, extractTo, extractDir, archiveHash, reportIndexProgress, report)
}

// deployTarArchive handles tar archive deployment from download.
// Tar archives (including .tar.gz, .tar.bz2, .tar.xz) are deployed similarly to ZIP archives:
// 1. If a member index exists, files are linked directly from CAS
// 2. If no index exists, the tar is extracted and files are adopted into CAS
// Parameters:
//   - state: Current installation state
//   - casPath: Path to the tar file in CAS store
//   - archiveHash: Content hash of the tar archive
//   - reportIndexProgress: Progress callback for file indexing
//   - report: Progress reporting callback
// Returns error if deployment fails.
func deployTarArchive(state *installState, casPath string, archiveHash string, reportIndexProgress func(int64, int64), report func(etypes.Phase, etypes.Status, float64, string, map[string]interface{}, int64, int64)) error {
	m := state.manifest
	installArch := state.installArch
	extractTo := m.GetExtractToForInstall(installArch)
	extractDir := m.GetExtractDirForInstall(installArch)

	// Check for existing member index (reuse the same index as ZIP for simplicity)
	tarFiles, tarTotalBytes, _ := state.engine.Downloader.ResolveZipMemberIndex(archiveHash)

	if len(tarFiles) == 0 {
		// No index exists, need to extract and adopt files
		return deployTarExtractAndAdopt(state, casPath, extractTo, extractDir, archiveHash, reportIndexProgress, report)
	}

	// Index exists, link files directly
	return deployTarFromIndex(state, archiveHash, tarFiles, tarTotalBytes, extractTo, extractDir, report)
}

// deploy7zArchive handles 7z archive deployment from download (e.g. zoom's dl.7z SFX).
func deploy7zArchive(state *installState, casPath, archiveHash string, reportIndexProgress func(int64, int64), report func(etypes.Phase, etypes.Status, float64, string, map[string]interface{}, int64, int64)) error {
	if err := ensureExtractor7zWithProf(state.engine, state.ctx, state.prof, state.pkgName); err != nil {
		return fmt.Errorf("ensure 7z: %w", err)
	}

	m := state.manifest
	installArch := state.installArch
	extractTo := m.GetExtractToForInstall(installArch)
	extractDir := m.GetExtractDirForInstall(installArch)

	reportDeployStart(report, archiveMemberIndexReady(state.engine.Downloader, archiveHash))
	count, err := installArchiveFromMemberIndex(state.engine, state.prof, casPath, state.installDir, state.downloadName, archiveHash, extractTo, extractDir, state.installedFiles, &state.totalSize, reportIndexProgress)
	if err != nil {
		return extractPhaseError(err)
	}
	if count == 0 {
		return fmt.Errorf("no files were extracted from %s", state.fileExt)
	}
	verbose.Progressf("  Installed %d file(s)\n", count)

	return finalizePackageInstall(state.engine, state.ctx, state.pkgRef, state.pkgName, state.manifest,
		state.installDir, state.downloadName, state.fileExt, state.installedFiles, state.totalSize,
		state.req, state.reporter, state.prof)
}

// deployTarFromIndex deploys tar archive by linking files from existing member index.
// Parameters:
//   - state: Current installation state
//   - archiveHash: Content hash of the tar archive
//   - tarFiles: Map of file hashes to relative paths from member index
//   - tarTotalBytes: Total size of all files in bytes
//   - extractTo: Target subdirectory for extraction (from manifest)
//   - extractDir: Directory layout to apply after extraction (from manifest)
//   - report: Progress reporting callback
// Returns error if deployment fails.
func deployTarFromIndex(state *installState, archiveHash string, tarFiles map[string]string, tarTotalBytes int64, extractTo, extractDir string, report func(etypes.Phase, etypes.Status, float64, string, map[string]interface{}, int64, int64)) error {
	if err := cleanInstallDir(state.installDir); err != nil {
		return fmt.Errorf("clean install dir: %w", err)
	}

	reportDeployStart(report, true)
	linkStart := time.Now()

	linked, err := LinkExtractedFiles(state.engine.Store, state.installDir, extractTo, extractDir, tarFiles, func(hash, name string) {
		recordFile(state, hash, name)
	})
	if state.prof != nil {
		state.prof.addLink(linkStart)
	}
	if err != nil {
		return err
	}

	verbose.Progressf("  Installed %d file(s)\n", linked)
	if err := refreshInstalledFilesFromDir(state.engine.Store, state.installDir, state.installedFiles, &state.totalSize); err != nil {
		return fmt.Errorf("index installed files: %w", err)
	}

	return finalizePackageInstall(state.engine, state.ctx, state.pkgRef, state.pkgName, state.manifest,
		state.installDir, state.downloadName, ".tar", state.installedFiles, state.totalSize,
		state.req, state.reporter, state.prof)
}
