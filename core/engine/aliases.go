package engine

import (
	"github.com/gluestick-sh/core/engine/internal/catalog"
	"github.com/gluestick-sh/core/engine/internal/runtime"
	etypes "github.com/gluestick-sh/core/engine/types"
)

// Engine is the public facade over the runtime engine implementation.
type Engine struct {
	*runtime.Engine
}

// These aliases re-export the engine's public request, result, and catalog
// types from the internal types and catalog packages.
type (
	ProgressEvent     = etypes.ProgressEvent
	Phase             = etypes.Phase
	Status            = etypes.Status
	Request           = etypes.Request
	InstallRequest    = etypes.InstallRequest
	UninstallRequest  = etypes.UninstallRequest
	SearchRequest     = etypes.SearchRequest
	ListRequest       = etypes.ListRequest
	Result            = etypes.Result
	Package           = etypes.Package
	ManifestInfo      = etypes.ManifestInfo
	BinaryInfo        = etypes.BinaryInfo
	ProgressReporter  = etypes.ProgressReporter
	EngineConfig      = etypes.EngineConfig
	EngineStats       = etypes.EngineStats
	PackageSuggestion = etypes.PackageSuggestion

	CatalogBucketsQuery   = catalog.CatalogBucketsQuery
	CatalogPackageQuery   = catalog.CatalogPackageQuery
	CatalogResolveRequest = catalog.CatalogResolveRequest
	CatalogBucketSummary  = catalog.CatalogBucketSummary
	CatalogPackagePage    = catalog.CatalogPackagePage
)

// These constants re-export the progress phase and status values so API
// consumers can reference them without importing the internal types package.
const (
	PhaseResolve   = etypes.PhaseResolve
	PhaseDownload  = etypes.PhaseDownload
	PhaseExtract   = etypes.PhaseExtract
	PhaseLink      = etypes.PhaseLink
	PhaseShim      = etypes.PhaseShim
	PhaseIndex     = etypes.PhaseIndex
	PhaseBootstrap = etypes.PhaseBootstrap
	PhaseComplete  = etypes.PhaseComplete
	PhaseError     = etypes.PhaseError

	StatusRunning = etypes.StatusRunning
	StatusSuccess = etypes.StatusSuccess
	StatusFailed  = etypes.StatusFailed
	StatusSkipped = etypes.StatusSkipped
	StatusWaiting = etypes.StatusWaiting
	StatusInfo    = etypes.StatusInfo
)
