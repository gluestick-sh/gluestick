package install

import (
	"github.com/gluestick-sh/core/engine/internal/runtime"
)

const metadataVersionLocked = "versionLocked"

// versionLockedFromMetadata extracts the version lock state from package metadata.
// This reads the versionLocked flag to determine if a package should be
// protected from automatic upgrades.
// Parameters:
//   - metadata: Package metadata map (may be nil)
// Returns true if the package is version-locked.
func versionLockedFromMetadata(metadata map[string]any) bool {
	if metadata == nil {
		return false
	}
	v, ok := metadata[metadataVersionLocked]
	if !ok {
		return false
	}
	locked, ok := v.(bool)
	return ok && locked
}

// IsPackageVersionLocked reports whether upgrades are blocked for the package.
func IsPackageVersionLocked(e *runtime.Engine, pkgName string) bool {
	if e == nil || e.Cache == nil {
		return false
	}
	inst, ok := e.Cache.GetInstalled(pkgName)
	if !ok {
		return false
	}
	return versionLockedFromMetadata(inst.Metadata)
}
