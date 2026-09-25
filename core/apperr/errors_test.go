package apperr

import (
	"errors"
	"fmt"
	"testing"
)

func TestManifestNotFoundIs(t *testing.T) {
	err := &ManifestNotFound{Name: "foo"}
	if !errors.Is(err, ErrManifestNotFound) {
		t.Fatal("expected ErrManifestNotFound")
	}
}

func TestIsResolveNotice(t *testing.T) {
	if !IsResolveNotice(fmt.Errorf("find manifest: %w", &ManifestNotFound{Name: "foo"})) {
		t.Fatal("wrapped manifest not found")
	}
	if !IsResolveNotice(&ManifestSuggest{Cause: &ManifestNotFound{Name: "foo"}, Hints: []string{"glue install extras/foo"}}) {
		t.Fatal("suggest error")
	}
	if IsResolveNotice(fmt.Errorf("download failed")) {
		t.Fatal("unexpected resolve notice")
	}
}

func TestPackageNotInstalledIs(t *testing.T) {
	err := &PackageNotInstalled{Name: "git"}
	if !errors.Is(err, ErrPackageNotInstalled) {
		t.Fatal("expected ErrPackageNotInstalled")
	}
}

func TestBucketNotFoundIs(t *testing.T) {
	err := &BucketNotFound{Name: "extras"}
	if !errors.Is(err, ErrBucketNotFound) {
		t.Fatal("expected ErrBucketNotFound")
	}
	if err.Error() != "bucket not found: extras" {
		t.Fatalf("Error() = %q, want the historic text", err.Error())
	}
	wrapped := fmt.Errorf("remove bucket: %w", err)
	if !errors.Is(wrapped, ErrBucketNotFound) {
		t.Fatal("expected wrapped ErrBucketNotFound")
	}
	if errors.Is(err, ErrBucketNotInstalled) {
		t.Fatal("BucketNotFound must not be confused with BucketNotInstalled")
	}
}
