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
