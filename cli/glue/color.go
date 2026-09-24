package main

// Terminal color toggles and ANSI escape codes for CLI status marks (✓/✗).

import "github.com/gluestick-sh/core/config"

const (
	ansiReset  = "\033[0m"
	ansiGreen  = "\033[32m"
	ansiRed    = "\033[31m"
	ansiBlue   = "\033[34m"
	ansiYellow = "\033[33m"
)

var (
	colorReset  string
	colorGreen  string
	colorRed    string
	colorBlue   string
	colorYellow string
	markFail    string
	markSuccess string
	markWarn    string
	markSkip    string
)

func init() {
	setColorEnabled(true)
}

func setColorEnabled(enabled bool) {
	if enabled {
		colorReset = ansiReset
		colorGreen = ansiGreen
		colorRed = ansiRed
		colorBlue = ansiBlue
		colorYellow = ansiYellow
		initConsoleColor()
	} else {
		colorReset = ""
		colorGreen = ""
		colorRed = ""
		colorBlue = ""
		colorYellow = ""
	}
	markFail = colorRed + "✗" + colorReset
	markSuccess = colorGreen + "✓" + colorReset
	markWarn = colorYellow + "⚠" + colorReset
	markSkip = colorBlue + "—" + colorReset
}

func colorEnabledFromConfig(cfg *config.Basics) bool {
	if cfg != nil && cfg.Color != nil {
		return *cfg.Color
	}
	return true
}
