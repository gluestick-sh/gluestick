// Package apperr defines stable, inspectable errors for core operations.
package apperr

import (
	"errors"
	"fmt"
	"strings"
)

var (
	// ErrManifestNotFound is returned when no manifest matches a package reference.
	ErrManifestNotFound = errors.New("manifest not found")
	// ErrManifestAmbiguous is returned when multiple buckets provide the same package name.
	ErrManifestAmbiguous = errors.New("manifest ambiguous")
	// ErrBucketNotInstalled is returned when a required bucket is missing locally.
	ErrBucketNotInstalled = errors.New("bucket not installed")
	// ErrPackageNotInstalled is returned when an operation targets an uninstalled package.
	ErrPackageNotInstalled = errors.New("package not installed")
)

// ManifestNotFound describes a missing manifest.
type ManifestNotFound struct {
	Name     string
	Searched string
}

// Error implements the error interface.
func (e *ManifestNotFound) Error() string {
	if e.Searched != "" {
		return fmt.Sprintf("manifest not found: %s (searched in: %s)", e.Name, e.Searched)
	}
	return fmt.Sprintf("manifest not found: %s", e.Name)
}

// Is reports whether target is ErrManifestNotFound.
func (e *ManifestNotFound) Is(target error) bool {
	return target == ErrManifestNotFound
}

// ManifestAmbiguous describes duplicate package names across buckets.
type ManifestAmbiguous struct {
	Name    string
	Matches []string
}

// Error implements the error interface.
func (e *ManifestAmbiguous) Error() string {
	return fmt.Sprintf("manifest not found: %q is ambiguous (found in: %s)", e.Name, strings.Join(e.Matches, ", "))
}

// Is reports whether target is ErrManifestAmbiguous.
func (e *ManifestAmbiguous) Is(target error) bool {
	return target == ErrManifestAmbiguous
}

// ManifestSuggest augments a manifest-not-found error with install hints.
type ManifestSuggest struct {
	Cause error
	Hints []string
}

// Error implements the error interface.
func (e *ManifestSuggest) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "find manifest: %v", e.Cause)
	if len(e.Hints) == 0 {
		return b.String()
	}
	b.WriteString("\n\nDid you mean:\n")
	for _, h := range e.Hints {
		fmt.Fprintf(&b, "  %s\n", h)
	}
	return strings.TrimRight(b.String(), "\n")
}

// Unwrap returns the wrapped cause.
func (e *ManifestSuggest) Unwrap() error { return e.Cause }

// BucketNotInstalled describes a missing bucket checkout.
type BucketNotInstalled struct {
	Name string
}

// Error implements the error interface.
func (e *BucketNotInstalled) Error() string {
	return fmt.Sprintf("bucket %q is not installed; run: glue bucket add %s <repo-url>", e.Name, e.Name)
}

// Is reports whether target is ErrBucketNotInstalled.
func (e *BucketNotInstalled) Is(target error) bool {
	return target == ErrBucketNotInstalled
}

// PackageNotInstalled describes a missing installed package.
type PackageNotInstalled struct {
	Name    string
	Version string
}

// Error implements the error interface.
func (e *PackageNotInstalled) Error() string {
	if e.Version != "" {
		return fmt.Sprintf("%s@%s is not installed", e.Name, e.Version)
	}
	return fmt.Sprintf(`package %q is not installed (check with glue list)`, e.Name)
}

// Is reports whether target is ErrPackageNotInstalled.
func (e *PackageNotInstalled) Is(target error) bool {
	return target == ErrPackageNotInstalled
}

// IsResolveNotice reports user-facing resolve/manifest errors suitable for CLI notes.
func IsResolveNotice(err error) bool {
	var suggest *ManifestSuggest
	if errors.As(err, &suggest) {
		return true
	}
	return errors.Is(err, ErrManifestNotFound) || errors.Is(err, ErrManifestAmbiguous)
}
