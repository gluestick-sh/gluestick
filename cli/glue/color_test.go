package main

import (
	"strings"
	"testing"

	"github.com/gluestick-sh/core/config"
)

func TestSetColorEnabled_usesANSIWhenEnabled(t *testing.T) {
	t.Cleanup(func() { setColorEnabled(true) })

	setColorEnabled(true)
	cases := map[string]struct {
		got  string
		want string
	}{
		"markSuccess": {markSuccess, ansiGreen},
		"markFail":    {markFail, ansiRed},
		"colorBlue":   {colorBlue, ansiBlue},
	}
	for name, tc := range cases {
		if !strings.Contains(tc.got, tc.want) {
			t.Fatalf("%s = %q, want substring %q when color enabled", name, tc.got, tc.want)
		}
	}
}

func TestSetColorEnabled_plainWhenDisabled(t *testing.T) {
	t.Cleanup(func() { setColorEnabled(true) })

	setColorEnabled(false)
	if markSuccess != "✓" {
		t.Fatalf("markSuccess = %q, want plain checkmark", markSuccess)
	}
	if markFail != "✗" {
		t.Fatalf("markFail = %q, want plain cross", markFail)
	}
	for name, got := range map[string]string{
		"colorGreen": colorGreen,
		"colorRed":   colorRed,
		"colorBlue":  colorBlue,
	} {
		if got != "" {
			t.Fatalf("%s = %q, want empty when color disabled", name, got)
		}
	}
}

func TestColorEnabledFromConfig(t *testing.T) {
	if !colorEnabledFromConfig(nil) {
		t.Fatal("nil config should default color to enabled")
	}
	on := true
	if !colorEnabledFromConfig(&config.Basics{Color: &on}) {
		t.Fatal("explicit color=true")
	}
	off := false
	if colorEnabledFromConfig(&config.Basics{Color: &off}) {
		t.Fatal("explicit color=false")
	}
}

func TestApplyConfig_respectsColor(t *testing.T) {
	t.Cleanup(func() {
		applyConfig(nil)
		t.Setenv("FORCE_COLOR", "")
		t.Setenv("NO_COLOR", "")
	})

	t.Setenv("NO_COLOR", "1")
	off := false
	applyConfig(&config.Basics{Color: &off})
	if colorGreen != "" {
		t.Fatalf("colorGreen = %q after color=false config", colorGreen)
	}

	t.Setenv("NO_COLOR", "")
	t.Setenv("FORCE_COLOR", "1")
	on := true
	applyConfig(&config.Basics{Color: &on})
	if !strings.Contains(markSuccess, ansiGreen) {
		t.Fatalf("markSuccess = %q after color=true config", markSuccess)
	}
}
