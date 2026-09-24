package main

import (
	"os"
	"strings"

	"golang.org/x/term"
	"github.com/gluestick-sh/core/config"
)

// colorEnabled reports whether ANSI styling is active for this process.
func colorEnabled() bool {
	return terminalColorEnabled
}

var terminalColorEnabled = true

// resolveTerminalColor applies NO_COLOR, FORCE_COLOR, TTY detection, then config.json.
func resolveTerminalColor(cfg *config.Basics) bool {
	if strings.TrimSpace(os.Getenv("NO_COLOR")) != "" {
		return false
	}
	if v := strings.TrimSpace(os.Getenv("FORCE_COLOR")); v != "" {
		if v == "0" || strings.EqualFold(v, "false") || strings.EqualFold(v, "no") {
			return false
		}
		return true
	}
	if !term.IsTerminal(int(os.Stderr.Fd())) {
		return false
	}
	return colorEnabledFromConfig(cfg)
}
