package main

import (
	"sync"

	"github.com/gluestick-sh/core/engine"
	"github.com/gluestick-sh/core/verbose"
)

// installReporter returns progress reporting for install/uninstall operations.
// Verbose mode uses the full console reporter; default mode prints one line per phase.
func installReporter() engine.ProgressReporter {
	if jsonOutputEnabled() {
		return engine.NewSilentReporter()
	}
	if verbose.Enabled() {
		return engine.NewConsoleReporter(true)
	}
	return newCLIInstallReporter()
}

type cliInstallReporter struct {
	mu        sync.Mutex
	lastPhase map[string]string
}

func newCLIInstallReporter() *cliInstallReporter {
	return &cliInstallReporter{lastPhase: make(map[string]string)}
}

func (r *cliInstallReporter) ReportProgress(event engine.ProgressEvent) {
	if event.Message == "" {
		return
	}
	switch event.Status {
	case engine.StatusRunning:
		phaseKey := string(event.Phase)
		if event.Phase == engine.PhaseDownload && event.Percentage > 0 {
			return
		}
		r.mu.Lock()
		prev := r.lastPhase[event.Package]
		if prev == phaseKey {
			r.mu.Unlock()
			return
		}
		r.lastPhase[event.Package] = phaseKey
		r.mu.Unlock()
		verbose.Progressf("  → %s\n", event.Message)
	case engine.StatusFailed:
		verbose.Progressf("  %s %s\n", markFail, event.Message)
		if event.Error != nil {
			verbose.Progressf("    %v\n", event.Error)
		}
	}
}
