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

func TestCode(t *testing.T) {
	tests := []struct {
		name string
		err  error
		code string
		hint string
	}{
		{"nil", nil, "", ""},
		{"manifest not found sentinel", ErrManifestNotFound, "manifest_not_found", ""},
		{"manifest not found typed", &ManifestNotFound{Name: "foo"}, "manifest_not_found", ""},
		{"manifest suggest with hints", &ManifestSuggest{Cause: &ManifestNotFound{Name: "foo"}, Hints: []string{"glue install extras/foo", "glue install main/foo"}}, "manifest_not_found", "glue install extras/foo\nglue install main/foo"},
		{"manifest ambiguous", ErrManifestAmbiguous, "manifest_ambiguous", ""},
		{"bucket not installed sentinel", ErrBucketNotInstalled, "bucket_not_installed", ""},
		{"bucket not installed typed", &BucketNotInstalled{Name: "extras"}, "bucket_not_installed", ""},
		{"bucket not found sentinel", ErrBucketNotFound, "bucket_not_found", "glue bucket list shows installed buckets"},
		{"bucket not found typed", &BucketNotFound{Name: "extras"}, "bucket_not_found", "glue bucket list shows installed buckets"},
		{"package not installed", ErrPackageNotInstalled, "package_not_installed", ""},
		{"wrapped", fmt.Errorf("remove bucket: %w", &BucketNotFound{Name: "extras"}), "bucket_not_found", "glue bucket list shows installed buckets"},
		{"unknown", errors.New("download failed"), "unknown", ""},
	}
	for _, tt := range tests {
		code, hint := Code(tt.err)
		if code != tt.code || hint != tt.hint {
			t.Errorf("%s: Code() = (%q, %q), want (%q, %q)", tt.name, code, hint, tt.code, tt.hint)
		}
	}
}
