package main

import (
	"os"
	"testing"

	"golang.org/x/term"
)

func TestResolveTerminalColor_NO_COLOR(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	t.Setenv("FORCE_COLOR", "")
	if resolveTerminalColor(nil) {
		t.Fatal("NO_COLOR should disable color")
	}
}

func TestResolveTerminalColor_FORCE_COLOR(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("FORCE_COLOR", "1")
	if !resolveTerminalColor(nil) {
		t.Fatal("FORCE_COLOR=1 should enable color")
	}
}

func TestResolveTerminalColor_nonTTY(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("FORCE_COLOR", "")
	if term.IsTerminal(int(os.Stderr.Fd())) {
		t.Skip("stderr is a TTY")
	}
	if resolveTerminalColor(nil) {
		t.Fatal("non-TTY stderr should disable color without FORCE_COLOR")
	}
}
