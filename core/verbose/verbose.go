// Package verbose holds process-wide operational logging for glue core and CLI.
package verbose

import (
	"fmt"
	"os"
	"sync/atomic"
)

var enabled atomic.Bool

// Set toggles verbose operational logging for the current process.
func Set(on bool) {
	enabled.Store(on)
}

// Enabled reports whether verbose logging is on.
func Enabled() bool {
	return enabled.Load()
}

// Fprintf writes to stderr when verbose is enabled.
func Fprintf(format string, args ...any) {
	if !enabled.Load() {
		return
	}
	fmt.Fprintf(os.Stderr, "  "+format, args...)
}

// Progressf writes normal operational progress to stderr (always shown).
func Progressf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format, args...)
}
