package engine

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gluestick-sh/core/apperr"
	"github.com/gluestick-sh/core/engine/internal/catalog"
	"github.com/gluestick-sh/core/engine/internal/install"
	"github.com/gluestick-sh/core/engine/internal/runtime"
	"github.com/gluestick-sh/core/engine/internal/uninstall"
	"github.com/gluestick-sh/core/message"
)

// NewEngine creates a new package engine instance.
func NewEngine(cfg *EngineConfig) (*Engine, error) {
	r, err := runtime.NewEngine(cfg)
	if err != nil {
		return nil, err
	}
	return &Engine{Engine: r}, nil
}

// Install installs a package using the same pipeline as the CLI.
func (e *Engine) Install(ctx context.Context, req *InstallRequest, reporter ProgressReporter) (*Result, error) {
	ctx = runtime.OperationContext(ctx, &req.Request)
	start := time.Now()
	pkgName, _ := runtime.ParsePkgRef(req.Name)

	prevVersion := ""
	if entry, ok := e.Cache.Get(pkgName); ok {
		prevVersion = entry.Version
	}

	runtime.ReportProgress(reporter, PhaseResolve, req.Name, StatusRunning, 0, message.ProgressInstallStarting, nil, 0, 0)

	if err := catalog.ValidateInstallTarget(e.Engine, ctx, req); err != nil {
		_ = e.Cache.RecordActivity("install", pkgName, "", "failed", map[string]interface{}{
			"error": err.Error(),
		})
		reportInstallFailure(reporter, req.Name, err)
		return createErrorResult(e, &req.Request, req.Name, err, installFailureMessage(err), start)
	}

	if err := e.installDependsFirst(ctx, req, reporter); err != nil {
		_ = e.Cache.RecordActivity("install", pkgName, "", "failed", map[string]interface{}{
			"error": err.Error(),
		})
		reportInstallFailure(reporter, req.Name, err)
		return createErrorResult(e, &req.Request, req.Name, err, installFailureMessage(err), start)
	}

	if err := install.PackageFull(e.Engine, ctx, req.Name, req, reporter); err != nil {
		_ = e.Cache.RecordActivity("install", pkgName, "", "failed", map[string]interface{}{
			"error": err.Error(),
		})
		reportInstallFailure(reporter, req.Name, err)
		return createErrorResult(e, &req.Request, req.Name, err, installFailureMessage(err), start)
	}

	if err := e.installDependsPost(ctx, req, reporter); err != nil {
		_ = e.Cache.RecordActivity("install", pkgName, "", "failed", map[string]interface{}{
			"error": err.Error(),
		})
		reportInstallFailure(reporter, req.Name, err)
		return createErrorResult(e, &req.Request, req.Name, err, installFailureMessage(err), start)
	}

	version := ""
	if entry, ok := e.Cache.Get(pkgName); ok {
		version = entry.Version
	}

	runtime.ReportProgress(reporter, PhaseComplete, req.Name, StatusSuccess, 100, message.ProgressInstallComplete, nil, 0, 0)
	e.RecordSuccessfulInstall()

	if prevVersion != "" && prevVersion != version {
		_ = e.Cache.RecordActivity("upgrade", pkgName, version, "success", map[string]interface{}{
			"from": prevVersion,
			"to":   version,
		})
	} else {
		_ = e.Cache.RecordActivity("install", pkgName, version, "success", nil)
	}

	var suggestions []PackageSuggestion
	if resolvedRef, err := catalog.ResolveInstallRef(e.Engine, ctx, req.Name); err == nil {
		if s, err := e.missingSuggestions(ctx, resolvedRef); err == nil {
			suggestions = s
		}
	}

	return &Result{
		Name:        pkgName,
		Version:     version,
		Status:      StatusSuccess,
		Message:     "Package installed successfully",
		Duration:    time.Since(start),
		Suggestions: suggestions,
	}, nil
}

// Uninstall uninstalls a package using the same pipeline as the CLI.
func (e *Engine) Uninstall(ctx context.Context, req *UninstallRequest, reporter ProgressReporter) (*Result, error) {
	ctx = runtime.OperationContext(ctx, &req.Request)
	start := time.Now()
	pkgName, targetVer := runtime.ParsePkgRef(req.Name)

	runtime.ReportProgress(reporter, PhaseResolve, req.Name, StatusRunning, 0, message.ProgressUninstallStarting, nil, 0, 0)

	uninstalledVer, err := uninstall.PackageFull(e.Engine, ctx, req.Name, req)
	if err != nil {
		_ = e.Cache.RecordActivity("uninstall", pkgName, targetVer, "failed", map[string]interface{}{
			"error": err.Error(),
		})
		return createErrorResult(e, &req.Request, req.Name, err, "Uninstall failed", start)
	}

	runtime.ReportProgress(reporter, PhaseComplete, req.Name, StatusSuccess, 100, message.ProgressUninstallComplete, nil, 0, 0)
	e.RecordSuccessfulOp()
	_ = e.Cache.RecordActivity("uninstall", pkgName, uninstalledVer, "success", nil)

	return &Result{
		Name:     pkgName,
		Version:  uninstalledVer,
		Status:   StatusSuccess,
		Message:  "Package uninstalled successfully",
		Duration: time.Since(start),
	}, nil
}

// Search searches for packages across buckets.
func (e *Engine) Search(ctx context.Context, req *SearchRequest, reporter ProgressReporter) ([]*Package, error) {
	if err := runtime.WaitSearchIndexReady(e.Engine, ctx); err != nil {
		return nil, err
	}

	if reporter != nil {
		reporter.ReportProgress(ProgressEvent{
			Phase:     PhaseResolve,
			Package:   "search",
			Status:    StatusRunning,
			Message:   "Searching for packages",
			Timestamp: time.Now(),
		})
	}

	bucketsToSearch := req.Buckets
	if len(bucketsToSearch) == 0 {
		for _, bucket := range e.BucketRegistry.List() {
			bucketsToSearch = append(bucketsToSearch, bucket.Name)
		}
	}

	return catalog.SearchPackages(e.Engine, req.Query, bucketsToSearch, req.Limit), nil
}

// ResolveInstallRef resolves an unqualified package name to bucket/pkg when needed.
func (e *Engine) ResolveInstallRef(ctx context.Context, pkgRef string) (string, error) {
	return catalog.ResolveInstallRef(e.Engine, ctx, pkgRef)
}

// IsInstallResolveNotice reports user-facing resolve/manifest errors.
func IsInstallResolveNotice(err error) bool {
	return apperr.IsResolveNotice(err)
}

// FormatInstallResolveNotice formats a resolve/manifest error as a CLI Note block.
func FormatInstallResolveNotice(err error) string {
	return catalog.FormatInstallResolveNotice(err)
}

// SearchIndexReady reports whether the initial bucket manifest index build has finished.
func (e *Engine) SearchIndexReady() bool {
	return runtime.SearchIndexReady(e.Engine)
}

// PackageCountsByBucket returns indexed package counts per installed bucket.
func (e *Engine) PackageCountsByBucket() map[string]int {
	return catalog.PackageCountsByBucket(e.Engine)
}

// CountAvailablePackages returns indexed packages across all buckets.
func (e *Engine) CountAvailablePackages(hideDeprecated bool) int {
	return catalog.CountAvailablePackages(e.Engine, hideDeprecated)
}

// CatalogBuckets returns installed buckets with indexed package counts.
func (e *Engine) CatalogBuckets(q CatalogBucketsQuery) []CatalogBucketSummary {
	return catalog.CatalogBuckets(e.Engine, q)
}

// ListCatalogPackages returns paginated packages from the search index.
func (e *Engine) ListCatalogPackages(q CatalogPackageQuery) (*CatalogPackagePage, error) {
	return catalog.ListCatalogPackages(e.Engine, q)
}

// ResolveCatalogPackages resolves package names for recipe cards.
func (e *Engine) ResolveCatalogPackages(requests []CatalogResolveRequest) []*Package {
	return catalog.ResolveCatalogPackages(e.Engine, requests)
}

// HideCatalogPackage hides a package from catalog browse/search results.
func (e *Engine) HideCatalogPackage(pkgRef string) error {
	return catalog.HideCatalogPackage(e.Engine, pkgRef)
}

// LoadSearchIndexBucket immediately indexes a single bucket.
func (e *Engine) LoadSearchIndexBucket(name string) {
	runtime.LoadSearchIndexBucket(e.Engine, name)
}

// RemoveSearchIndexBucket drops all indexed packages for a bucket.
func (e *Engine) RemoveSearchIndexBucket(name string) {
	runtime.RemoveSearchIndexBucket(e.Engine, name)
}

// BucketManifestPaths lists package manifest JSON paths under a Scoop bucket repo.
func BucketManifestPaths(bucketRoot, bucketName string) []string {
	return runtime.BucketManifestPaths(bucketRoot, bucketName)
}

func installFailureMessage(err error) string {
	if IsInstallResolveNotice(err) {
		return "Package not found"
	}
	if err != nil && strings.Contains(err.Error(), "dependency ") {
		return "Dependency installation failed"
	}
	return "Installation failed"
}

func reportInstallFailure(reporter ProgressReporter, pkgRef string, err error) {
	if reporter == nil || err == nil {
		return
	}
	reporter.ReportProgress(ProgressEvent{
		Phase:     PhaseError,
		Package:   pkgRef,
		Status:    StatusFailed,
		Message:   err.Error(),
		Timestamp: time.Now(),
	})
}

func createErrorResult(e *Engine, req *Request, name string, err error, message string, start time.Time) (*Result, error) {
	if err == nil {
		err = fmt.Errorf("%s", message)
	}
	result := &Result{
		Name:     name,
		Status:   StatusFailed,
		Message:  message,
		Error:    err,
		Duration: time.Since(start),
	}
	e.RecordFailedOp(result.Duration)
	return result, err
}
