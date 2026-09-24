package engine

import (
	"context"
	"fmt"

	"github.com/gluestick-sh/core/engine/internal/catalog"
	"github.com/gluestick-sh/core/engine/internal/runtime"
)

// PackageHomepage returns the manifest homepage URL for an installed or catalog package.
func (e *Engine) PackageHomepage(ctx context.Context, pkgRef string) (string, error) {
	if e == nil {
		return "", fmt.Errorf("engine not configured")
	}
	pkgName := runtime.PackageBaseName(pkgRef)
	if detail, err := e.GetInstalledPackageDetail(pkgName); err == nil && detail.Homepage != "" {
		return detail.Homepage, nil
	}
	resolved, err := e.ResolveInstallRef(ctx, pkgRef)
	if err != nil {
		return "", err
	}
	lookupRef := runtime.ManifestLookupRef(resolved)
	if err := catalog.EnsureBucketForInstall(e.Engine, ctx, lookupRef, nil); err != nil {
		return "", err
	}
	_, m, err := e.BucketRegistry.GetManifestPath(lookupRef)
	if err != nil {
		return "", err
	}
	if m == nil || m.Homepage == "" {
		return "", fmt.Errorf("no homepage for %s", pkgRef)
	}
	return m.Homepage, nil
}
