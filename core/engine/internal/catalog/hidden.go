package catalog

import (
	"github.com/gluestick-sh/core/config"
	"github.com/gluestick-sh/core/engine/internal/runtime"
)

func hiddenCatalogPackages(e *runtime.Engine) map[string]struct{} {
	if e == nil || e.Config == nil || e.Config.RootDir == "" {
		return nil
	}
	hidden, err := config.ReadConfigHiddenCatalogPackages(e.Config.RootDir)
	if err != nil || len(hidden) == 0 {
		return nil
	}
	return hidden
}

// HideCatalogPackage hides a package from catalog browse/search results.
func HideCatalogPackage(e *runtime.Engine, pkgRef string) error {
	if e == nil || e.Config == nil {
		return nil
	}
	return config.AddConfigHiddenCatalogPackage(e.Config.RootDir, pkgRef)
}
