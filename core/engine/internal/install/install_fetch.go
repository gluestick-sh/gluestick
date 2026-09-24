package install

import (
	"fmt"
	"os"
	"time"

	"github.com/gluestick-sh/core/downloader"
	"github.com/gluestick-sh/core/engine/internal/runtime"
	etypes "github.com/gluestick-sh/core/engine/types"
	"github.com/gluestick-sh/core/humanize"
	"github.com/gluestick-sh/core/manifest"
	"github.com/gluestick-sh/core/message"
	"github.com/gluestick-sh/core/verbose"
)

// fetchInstallPhase handles the second phase of installation:
// 1. Validates manifest URLs and hashes
// 2. Checks cache for reusable entries
// 3. Ensures required extractors (7z) are available
// 4. Downloads artifacts (multi-artifact or single with mirror fallback)
// 5. Records download results and progress
func fetchInstallPhase(state *installState) error {
	// Helper to report progress
	report := func(phase etypes.Phase, status etypes.Status, pct float64, key string, args map[string]any, bytes, total int64) {
		runtime.ReportProgress(state.reporter, phase, state.pkgRef, status, pct, key, args, bytes, total)
	}

	m := state.manifest

	// Validate manifest exists (should never happen, but prevents panics)
	if m == nil {
		return fmt.Errorf("manifest is nil for %s (internal error)", state.pkgRef)
	}

	tried := map[string]bool{}
	var lastErr error
	for {
		archKey := state.installArch
		if archKey == "" {
			archKey = "_"
		}
		if tried[archKey] {
			if lastErr != nil {
				return lastErr
			}
			return fmt.Errorf("manifest has no download URLs")
		}
		tried[archKey] = true

		lastErr = fetchInstallAttempt(state, report)
		if lastErr == nil {
			return nil
		}
		err := lastErr
		if architectureExplicit(state.req) || !isHTTPNotFound(err) {
			return err
		}
		next := m.FallbackArchitecture(state.installArch)
		if next == "" {
			return err
		}
		verbose.Progressf("  %s download not found; falling back to %s\n", state.installArch, next)
		resetFetchAttempt(state)
		state.installArch = next
	}
}

func resetFetchAttempt(state *installState) {
	state.urlHashPairs = nil
	state.downloadResults = nil
	state.fromCache = false
	state.cachedEntry = nil
	state.fileExt = ""
	state.downloadName = ""
	state.multiArtifact = false
	state.firstHashAlgo = ""
	state.firstHashValue = ""
	state.sawExtractDuringFetch = false
	state.zipIngestProgress = nil
	state.fetchDownloadProgress = nil
}

// fetchInstallAttempt handles the second phase of installation:
// 1. Validates manifest URLs and hashes
// 2. Checks cache for reusable entries
// 3. Ensures required extractors (7z) are available
// 4. Downloads artifacts (multi-artifact or single with mirror fallback)
// 5. Records download results and progress
func fetchInstallAttempt(state *installState, report func(phase etypes.Phase, status etypes.Status, pct float64, key string, args map[string]any, bytes, total int64)) error {
	m := state.manifest
	installArch := state.installArch

	// Phase 2.1: Validate manifest has download URLs
	urls := m.GetURLsForInstall(installArch)
	hashes := m.GetHashesForInstall(installArch)

	if len(urls) == 0 {
		return fmt.Errorf("manifest has no download URLs")
	}

	// Phase 2.2: Build URL-hash pairs and determine download type
	state.urlHashPairs = buildURLHashPairs(urls, hashes)
	state.multiArtifact = manifestUsesMultiArtifactURLs(urls, hashes)

	// Phase 2.3: Parse first URL to determine file type
	downloadURL := state.urlHashPairs[0].url
	parsedURL, err := manifest.ParseURL(downloadURL)
	if err != nil {
		return fmt.Errorf("parse download URL: %w", err)
	}
	state.fileExt = parsedURL.Extension
	state.downloadName = parsedURL.LocalName

	// Store hash info for later use
	state.firstHashAlgo = state.urlHashPairs[0].hashAlgo
	state.firstHashValue = state.urlHashPairs[0].hashValue

	// Phase 2.4: Check cache for reusable entry
	if !state.req.Force {
		if entry, ok := state.engine.Cache.Get(state.pkgName); ok && cacheReusableForInstall(state.engine.Store, entry, m.Version, state.downloadName, state.firstHashValue) {
			state.fromCache = true
			state.cachedEntry = entry
			state.totalSize = entry.Size
			verbose.Progressf("  Using cache for %s@%s (%s, %d files)\n",
				state.pkgName, m.Version, humanize.FormatBytes(entry.Size), len(entry.Files))
			return nil // Cache hit, skip download
		}
	}

	// Phase 2.5: Ensure 7z extractor if needed
	if installNeedsSevenZip(m, state.downloadName, normalizeInstallFileExt(state.fileExt), installArch) {
		if err := ensureExtractor7zWithProf(state.engine, state.ctx, state.prof, state.pkgName); err != nil {
			return fmt.Errorf("ensure 7z: %w", err)
		}
	}

	// Phase 2.6: Setup progress handlers
	report(etypes.PhaseDownload, etypes.StatusRunning, 0, message.ProgressPreparingDownload, downloadFileArgs(state.downloadName), 0, 0)
	activeDownloadFile := state.downloadName

	dlProgress := downloader.NewThrottledProgress(func(downloaded, total int64, _ string) {
		pct := 0.0
		if total > 0 {
			pct = (float64(downloaded) / float64(total)) * 100
		}
		report(etypes.PhaseDownload, etypes.StatusRunning, pct, message.ProgressDownloading, downloadFileArgs(activeDownloadFile), downloaded, total)
	})
	state.fetchDownloadProgress = dlProgress

	zipIngestProgress := downloader.NewThrottledProgress(func(processed, total int64, _ string) {
		state.sawExtractDuringFetch = true
		pct := 0.0
		if total > 0 {
			pct = (float64(processed) / float64(total)) * 100
		}
		report(etypes.PhaseExtract, etypes.StatusRunning, pct, message.ProgressExtractProcessing, map[string]any{
			"current": processed,
			"total":   total,
		}, processed, total)
	})
	state.zipIngestProgress = zipIngestProgress

	state.prog.Bytes = dlProgress.Report
	state.prog.Files = func(processed, total int64) { zipIngestProgress.Report(processed, total, "") }

	// Phase 2.7: Execute download (multi-artifact or single with mirrors)
	if state.multiArtifact {
		if err := downloadMultiArtifacts(state); err != nil {
			return err
		}
	} else {
		if err := downloadSingleArtifactWithMirrors(state, activeDownloadFile); err != nil {
			return err
		}
	}

	return nil
}

// downloadMultiArtifacts handles downloading multiple artifacts.
// Used when a manifest specifies multiple download URLs (e.g., separate
// binaries for different architectures or components).
// Parameters:
//   - state: Current installation state
// Returns error if any artifact download fails.
func downloadMultiArtifacts(state *installState) error {
	state.downloadResults = make([]downloader.Result, 0, len(state.urlHashPairs))
	var lastErr error

	for i, pair := range state.urlHashPairs {
		result, _, err := downloadManifestArtifact(state.engine, state.ctx, pair, state.req.Force, true)
		if err != nil {
			lastErr = err
			verbose.Fprintf("Failed to download artifact %d: %v\n", i+1, err)
			break
		}
		state.downloadResults = append(state.downloadResults, result)
	}

	if lastErr != nil {
		return downloadPhaseError(len(state.urlHashPairs), lastErr)
	}
	if len(state.downloadResults) != len(state.urlHashPairs) {
		return downloadPhaseError(len(state.urlHashPairs), fmt.Errorf("incomplete multi-artifact download"))
	}

	var totalDownload int64
	for _, r := range state.downloadResults {
		totalDownload += r.Size
		recordFile(state, r.Hash, r.Task.Filename)
	}
	verbose.Progressf("  Downloaded %d artifact(s) (%s)\n", len(state.downloadResults), humanize.FormatBytes(totalDownload))
	if state.fetchDownloadProgress != nil {
		state.fetchDownloadProgress.Flush()
	}
	if state.zipIngestProgress != nil {
		state.zipIngestProgress.Flush()
	}
	if !state.sawExtractDuringFetch {
		runtime.ReportProgress(state.reporter, etypes.PhaseDownload, state.pkgRef, etypes.StatusSuccess, 100, message.ProgressDownloadComplete, map[string]interface{}{
			"size": humanize.FormatBytes(totalDownload),
		}, totalDownload, totalDownload)
	}
	state.prof.absorbDownloadResults(state.downloadResults)

	return nil
}

// downloadSingleArtifactWithMirrors handles single artifact downloads with mirror fallback.
// Attempts to download from each URL in order until one succeeds.
// This provides resilience against failed mirrors or regional restrictions.
// Parameters:
//   - state: Current installation state
//   - activeDownloadFile: Name of the file being downloaded (for progress reporting)
// Returns error if all download attempts fail.
func downloadSingleArtifactWithMirrors(state *installState, activeDownloadFile string) error {
	var lastErr error

	// Helper to report progress
	report := func(phase etypes.Phase, status etypes.Status, pct float64, key string, args map[string]any, bytes, total int64) {
		runtime.ReportProgress(state.reporter, phase, state.pkgRef, status, pct, key, args, bytes, total)
	}

	for i, pair := range state.urlHashPairs {
		pairParsed, err := manifest.ParseURL(pair.url)
		if err != nil {
			lastErr = fmt.Errorf("parse download URL: %w", err)
			continue
		}

		downloadTask := downloader.Task{
			URL:       pairParsed.FetchURL,
			Filename:  pairParsed.LocalName,
			HashAlgo:  pair.hashAlgo,
			HashValue: pair.hashValue,
		}
		activeDownloadFile = downloadTask.Filename

		if state.req.Force {
			state.engine.Downloader.ClearPartial(downloadTask)
		}

		fetchURLs := state.engine.Downloader.ResolveDownloadURLs(pairParsed.FetchURL)
		if i == 0 || verbose.Enabled() {
			verbose.Progressf("  Downloading %s\n", downloadTask.Filename)
			if len(fetchURLs) > 1 {
				verbose.Progressf("  (%d URLs to try, including fallbacks)\n", len(fetchURLs))
			}
		}

		// Download with cache
		result := state.engine.Downloader.DownloadWithCache(state.ctx, downloadTask, pair.hashValue, state.req.Force)
		state.downloadResults = []downloader.Result{result}

		if result.Error != nil {
			lastErr = result.Error
			verbose.Fprintf("Failed to download from URL %d: %v\n", i+1, result.Error)
			continue // Try next URL/mirror
		}

		// Verify fresh downloads; store hits were verified when first ingested
		if !result.FromStore {
			if err := downloader.VerifyDownloadResult(state.engine.Store, downloadTask, result); err != nil {
				lastErr = err
				verbose.Fprintf("Verification failed for URL %d: %v\n", i+1, err)
				continue // Try next URL/mirror
			}
		}

		// Success! Break out of the retry loop
		lastErr = nil
		break
	}

	if lastErr != nil {
		dlErr := downloadPhaseError(len(state.urlHashPairs), lastErr)
		if state.reporter != nil {
			state.reporter.ReportProgress(etypes.ProgressEvent{
				Phase:     etypes.PhaseDownload,
				Package:   state.pkgRef,
				Status:    etypes.StatusFailed,
				Message:   dlErr.Error(),
				Timestamp: time.Now(),
			})
		}
		return dlErr
	}

	if len(state.downloadResults) == 0 || state.downloadResults[0].Error != nil {
		var dlErr error
		if len(state.downloadResults) > 0 {
			dlErr = state.downloadResults[0].Error
		} else {
			dlErr = fmt.Errorf("no download results")
		}
		return downloadPhaseError(1, dlErr)
	}

	// Report download success (verification already done in download loop)
	r := state.downloadResults[0]
	if state.fetchDownloadProgress != nil {
		state.fetchDownloadProgress.Flush()
	}
	if state.zipIngestProgress != nil {
		state.zipIngestProgress.Flush()
	}
	if r.FromStore {
		verbose.Progressf("  Using existing store blob %s (skipped download)\n", humanize.FormatBytes(r.Size))
		if !state.sawExtractDuringFetch {
			report(etypes.PhaseDownload, etypes.StatusSuccess, 100, message.ProgressDownloadCached, nil, r.Size, r.Size)
		}
	} else {
		net := r.Timing.Network
		casIngest := r.Timing.StoreIngest
		total := net + casIngest
		if total > 0 {
			verbose.Progressf("  Downloaded %s in %s (network %s, store %s)\n",
				humanize.FormatBytes(r.Size), humanize.FormatDuration(total), humanize.FormatDuration(net), humanize.FormatDuration(casIngest))
		} else {
			verbose.Progressf("  Downloaded %s\n", humanize.FormatBytes(r.Size))
		}
		if !state.sawExtractDuringFetch {
			report(etypes.PhaseDownload, etypes.StatusSuccess, 100, message.ProgressDownloadComplete, map[string]any{
				"size": humanize.FormatBytes(r.Size),
			}, r.Size, r.Size)
		}
	}
	recordFile(state, state.downloadResults[0].Hash, state.downloadName)
	state.prof.absorbDownloadResults(state.downloadResults)

	return nil
}

// recordFile records a file in the installed files map and tracks total size.
// This helper function updates the installation state with each file that
// will be part of the final installation. It skips hidden files and
// tracks the total size of all unique files.
// Parameters:
//   - state: Current installation state
//   - hash: Content hash of the file in CAS store
//   - name: Relative path/filename of the file
func recordFile(state *installState, hash, name string) {
	if hash == "" || runtime.IsHiddenInstallPath(name) {
		return
	}
	if _, exists := state.installedFiles[hash]; !exists {
		if info, err := os.Stat(state.engine.Store.ObjectPath(hash)); err == nil {
			state.totalSize += info.Size()
		}
	}
	state.installedFiles[hash] = name
}
