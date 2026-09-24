package main

import (
	"errors"
	"fmt"
	"testing"

	"github.com/gluestick-sh/core/engine"
)

func TestInstallFailureError_prefersTopLevelErr(t *testing.T) {
	want := fmt.Errorf("download failed")
	got := installFailureError(want, &engine.Result{Error: fmt.Errorf("ignored")})
	if !errors.Is(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestInstallFailureError_fallsBackToResultError(t *testing.T) {
	want := fmt.Errorf("manifest missing")
	got := installFailureError(nil, &engine.Result{Error: want})
	if !errors.Is(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestInstallFailureError_success(t *testing.T) {
	if got := installFailureError(nil, &engine.Result{Status: engine.StatusSuccess}); got != nil {
		t.Fatalf("want nil, got %v", got)
	}
}
