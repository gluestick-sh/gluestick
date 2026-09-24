package engine

import "github.com/gluestick-sh/core/apperr"

// Stable sentinel errors for programmatic handling (errors.Is).
var (
	ErrManifestNotFound    = apperr.ErrManifestNotFound
	ErrManifestAmbiguous   = apperr.ErrManifestAmbiguous
	ErrBucketNotInstalled  = apperr.ErrBucketNotInstalled
	ErrPackageNotInstalled = apperr.ErrPackageNotInstalled
)

// ManifestNotFoundError is the typed manifest-not-found error.
type ManifestNotFoundError = apperr.ManifestNotFound

// ManifestAmbiguousError is the typed ambiguous-manifest error.
type ManifestAmbiguousError = apperr.ManifestAmbiguous

// BucketNotInstalledError is the typed missing-bucket error.
type BucketNotInstalledError = apperr.BucketNotInstalled

// PackageNotInstalledError is the typed not-installed error.
type PackageNotInstalledError = apperr.PackageNotInstalled
