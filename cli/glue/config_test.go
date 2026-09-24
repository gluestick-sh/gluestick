package main

import (
	"testing"

	"github.com/gluestick-sh/core/config"
)

func TestFormatVerbose(t *testing.T) {
	if formatVerbose(nil) != "false (default)" {
		t.Fatalf("nil = %q", formatVerbose(nil))
	}
	on := true
	if formatVerbose(&on) != "true" {
		t.Fatalf("true = %q", formatVerbose(&on))
	}
	off := false
	if formatVerbose(&off) != "false" {
		t.Fatalf("false = %q", formatVerbose(&off))
	}
}

func TestResolveVerbose_configUnset(t *testing.T) {
	t.Cleanup(func() { applyConfig(nil) })

	if resolveVerbose(&config.Basics{}) {
		t.Fatal("nil verbose pointer should default to false")
	}
	on := true
	if !resolveVerbose(&config.Basics{Verbose: &on}) {
		t.Fatal("explicit verbose=true")
	}
}
