package engine

import (
	"context"
	"time"

	"github.com/gluestick-sh/core/engine/internal/install"
)

// ResolveBootstrappedGitPath returns the bootstrapped git path when present.
func (e *Engine) ResolveBootstrappedGitPath() (string, error) {
	return install.ResolveBootstrappedGitPath(e.Engine)
}

// ResolveBootstrappedSevenZipPath returns the bootstrapped 7-Zip path when present.
func (e *Engine) ResolveBootstrappedSevenZipPath() (string, error) {
	return install.ResolveBootstrappedSevenZipPath(e.Engine)
}

// ResolveBootstrappedDarkPath returns the bootstrapped WiX dark path when present.
func (e *Engine) ResolveBootstrappedDarkPath() (string, error) {
	return install.ResolveBootstrappedDarkPath(e.Engine)
}

// ResolveBootstrappedInnounpPath returns the bootstrapped innounp path when present.
func (e *Engine) ResolveBootstrappedInnounpPath() (string, error) {
	return install.ResolveBootstrappedInnounpPath(e.Engine)
}

// ResolveGitPath returns the first usable git executable.
func (e *Engine) ResolveGitPath() (string, error) {
	return install.ResolveGitPath(e.Engine)
}

// ResolveSevenZipPath returns the first usable 7-Zip executable.
func (e *Engine) ResolveSevenZipPath() (string, error) {
	return install.ResolveSevenZipPath(e.Engine)
}

// ResolveDarkPath returns the first usable WiX dark executable.
func (e *Engine) ResolveDarkPath() (string, error) {
	return install.ResolveDarkPath(e.Engine)
}

// CatalogNeedsDark reports whether any bucket manifest needs WiX dark.
func (e *Engine) CatalogNeedsDark() bool {
	return install.CatalogNeedsDark(e.Engine)
}

// EnsureGitBootstrap downloads MinGit when git is missing.
func (e *Engine) EnsureGitBootstrap(ctx context.Context) (string, error) {
	return install.EnsureGitBootstrap(e.Engine, ctx)
}

// EnsureSevenZipBootstrap downloads 7-Zip when it is missing.
func (e *Engine) EnsureSevenZipBootstrap(ctx context.Context) (string, error) {
	return install.EnsureSevenZipBootstrap(e.Engine, ctx)
}

// EnsureDarkBootstrap downloads WiX dark when it is missing.
func (e *Engine) EnsureDarkBootstrap(ctx context.Context) (string, error) {
	return install.EnsureDarkBootstrap(e.Engine, ctx)
}

// Stats returns engine statistics.
func (e *Engine) Stats() *EngineStats {
	return e.GetStats()
}

// List lists installed packages.
func (e *Engine) List(ctx context.Context, req *ListRequest, reporter ProgressReporter) ([]*Package, error) {
	if reporter != nil {
		reporter.ReportProgress(ProgressEvent{
			Phase:     PhaseResolve,
			Package:   "list",
			Status:    StatusRunning,
			Message:   "Listing installed packages",
			Timestamp: time.Now(),
		})
	}
	return e.listInstalledPackages(ctx, ListOptions{Detailed: req.Details}, reporter)
}
