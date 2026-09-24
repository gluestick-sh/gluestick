package install

import (
	"context"

	"github.com/gluestick-sh/core/cache"
	"github.com/gluestick-sh/core/downloader"
	"github.com/gluestick-sh/core/engine/internal/runtime"
	etypes "github.com/gluestick-sh/core/engine/types"
	"github.com/gluestick-sh/core/manifest"
	"github.com/gluestick-sh/core/progress"
)

// installState holds all mutable state for the installation process.
// It is passed through each phase of the installation pipeline (resolve, fetch, deploy, finalize).
// This structure allows phases to communicate results and share resources.
//
// Fields are grouped by which phase populates them:
// - Shared context: engine, ctx, req, reporter, pkgRef (initialized at creation)
// - Phase 1 (Resolve): pkgName, pinVersion, targetVersion, installArch, manifest, etc.
// - Phase 2 (Fetch): urlHashPairs, downloadResults, fromCache, cachedEntry, etc.
// - Phase 3 (Deploy): installDir, appsDir, installedFiles, totalSize
// - Shared infrastructure: prof (profiling), prog (progress handler)
type installState struct {
	// Shared context
	engine   *runtime.Engine
	ctx      context.Context
	req      *etypes.InstallRequest
	reporter etypes.ProgressReporter
	pkgRef   string

	// Phase 1: Resolve output
	pkgName         string
	pinVersion      string
	targetVersion   string
	installArch     string
	manifest        *manifest.Manifest
	manifestPath    string
	lookupRef       string
	overrideRef     string
	clearInstallDir bool

	// Phase 2: Fetch output
	urlHashPairs    []urlHashPair
	downloadResults []downloader.Result
	fromCache       bool
	cachedEntry     *cache.PackageEntry
	fileExt         string
	downloadName    string
	multiArtifact   bool
	firstHashAlgo   string
	firstHashValue  string
	sawExtractDuringFetch bool // zip member ingest during download reports extract phase
	zipIngestProgress     *downloader.ThrottledProgress
	fetchDownloadProgress *downloader.ThrottledProgress

	// Phase 3: Deploy output
	installDir     string
	appsDir        string
	installedFiles map[string]string
	totalSize      int64

	// Shared infrastructure
	prof *installPhaseProfile
	prog progress.Handler

	// Outcome flags (set by resolve/deploy phases)
	done             bool // resolve finished without needing fetch/deploy
	installSucceeded bool // deploy+finalize completed successfully
}

// newInstallState creates a new installState and initializes shared infrastructure.
// This function allocates a new installation state structure and sets up:
// - Progress handler for extraction/file operations
// - Context cancellation support for subprocesses
// - Optional profiling for performance analysis
//
// Parameters:
//   - e: Runtime engine with access to cache, store, downloader, etc.
//   - ctx: Context for cancellation (may be nil)
//   - pkgRef: Package reference (e.g., "package" or "package@version")
//   - req: Installation request with options (force, purge, etc.)
//   - reporter: Progress reporter for UI updates (may be nil)
//
// Returns an initialized installState ready for the resolve phase.
func newInstallState(e *runtime.Engine,
	ctx context.Context,
	pkgRef string, req *etypes.InstallRequest,
	reporter etypes.ProgressReporter,
) *installState {
	state := &installState{
		engine:   e,
		ctx:      ctx,
		req:      req,
		reporter: reporter,
		pkgRef:   pkgRef,

		installedFiles: make(map[string]string),
		lookupRef:      runtime.ManifestLookupRef(pkgRef),
		overrideRef:    runtime.ManifestLookupRef(pkgRef),
	}

	// Setup profiling
	if profileEnabled(req) {
		state.prof = &installPhaseProfile{enabled: true}
	}

	// Setup progress handler
	state.prog = progress.Handler{}
	ctx = progress.WithHandler(ctx, &state.prog)
	state.ctx = ctx
	e.Extractor.SetContext(ctx)

	return state
}

// cleanup releases resources held by the installState.
// This function should be called via defer when the installation completes.
// It ensures that:
// - Extractor context is reset to prevent goroutine leaks
// - Profile data is emitted if profiling was enabled
//
// This is safe to call multiple times and on nil states.
func (s *installState) cleanup() {
	if s != nil && s.engine != nil && !s.installSucceeded && !s.done &&
		s.pkgName != "" && s.targetVersion != "" {
		cleanupIncompleteInstall(s.engine, s.engine.Config.RootDir, s.pkgName, s.targetVersion)
	}
	if s.engine != nil && s.engine.Extractor != nil {
		s.engine.Extractor.SetContext(context.Background())
	}
	if s.prof != nil {
		s.prof.emit()
	}
}
