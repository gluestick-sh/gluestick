package engine

import (
	"context"
	goruntime "runtime"

	"github.com/gluestick-sh/core/engine/internal/catalog"
	"github.com/gluestick-sh/core/engine/internal/install"
)

// ResolveInnounpPath returns the first usable innounp.exe on Windows.
func (e *Engine) ResolveInnounpPath() (string, error) {
	return install.ResolveInnounpPath(e.Engine)
}

// CatalogNeedsInnounp reports whether any indexed bucket manifest needs innounp.
func (e *Engine) CatalogNeedsInnounp() bool {
	return catalog.CatalogNeedsInnounp(e.Engine)
}

// EnsureInnounpBootstrap downloads and extracts innounp when it is missing.
func (e *Engine) EnsureInnounpBootstrap(ctx context.Context) (string, error) {
	if goruntime.GOOS != "windows" {
		return "", nil
	}
	return install.EnsureInnounpBootstrap(e.Engine, ctx)
}
