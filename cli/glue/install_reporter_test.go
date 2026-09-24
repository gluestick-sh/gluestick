package main

import (
	"testing"

	"github.com/gluestick-sh/core/engine"
)

func TestCLIInstallReporter_skipsDownloadPercentUpdates(t *testing.T) {
	r := newCLIInstallReporter()
	r.ReportProgress(engine.ProgressEvent{
		Phase:   engine.PhaseDownload,
		Package: "git",
		Status:  engine.StatusRunning,
		Message: "Downloading",
	})
	r.ReportProgress(engine.ProgressEvent{
		Phase:      engine.PhaseDownload,
		Package:    "git",
		Status:     engine.StatusRunning,
		Message:    "Downloading",
		Percentage: 42,
	})
	// second event should not panic; light mode ignores percentage updates
}
