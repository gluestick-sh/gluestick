package engine

import (
	"testing"
	"time"
)

func TestEngineCreation(t *testing.T) {
	root := t.TempDir()
	eng, err := NewEngine(&EngineConfig{RootDir: root})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	defer eng.Close()

	if eng.Config == nil || eng.Config.RootDir != root {
		t.Fatalf("Config = %#v", eng.Config)
	}
	if eng.Cache == nil || eng.BucketRegistry == nil || eng.Store == nil {
		t.Fatal("expected core subsystems to be initialized")
	}
}

// TestProgressReporter tests the progress reporter interface
func TestProgressReporter(t *testing.T) {
	// Test ConsoleReporter
	reporter := NewConsoleReporter(true)

	// Test reporting progress events
	event := ProgressEvent{
		Phase:     PhaseResolve,
		Package:   "test-package",
		Status:    StatusRunning,
		Message:   "Test message",
		Percentage: 50.0,
		Bytes:     1000,
		TotalBytes: 2000,
		Timestamp:  time.Now(),
	}

	reporter.ReportProgress(event)

	// Test MultiReporter
	consoleReporter := NewConsoleReporter(true)
	silentReporter := NewSilentReporter()
	multiReporter := NewMultiReporter(consoleReporter, silentReporter)

	multiReporter.ReportProgress(event)

	// Test CallbackReporter
	var callbackCalled bool
	callbackReporter := NewCallbackReporter(func(e ProgressEvent) {
		callbackCalled = true
	})

	callbackReporter.ReportProgress(event)

	if !callbackCalled {
		t.Error("Callback reporter was not called")
	}

	// Test ProgressTracker
	tracker := NewProgressTracker(reporter)
	tracker.Start(PhaseDownload, "test", "Starting download")
	tracker.Update(PhaseDownload, "test", "Downloading", 25.0, 250, 1000, nil)
	tracker.Complete(PhaseDownload, "test", "Download complete")

	// Test BufferedReporter
	buffered := NewBufferedReporter(reporter)
	buffered.ReportProgress(event)
	buffered.ReportProgress(event)

	events := buffered.Events()
	if len(events) != 2 {
		t.Errorf("Expected 2 events, got %d", len(events))
	}

	buffered.Flush()

	eventsAfterFlush := buffered.Events()
	if len(eventsAfterFlush) != 0 {
		t.Errorf("Expected 0 events after flush, got %d", len(eventsAfterFlush))
	}
}

func TestEngineStats(t *testing.T) {
	root := t.TempDir()
	eng, err := NewEngine(&EngineConfig{RootDir: root})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	defer eng.Close()

	stats := eng.Stats()
	if stats == nil {
		t.Fatal("Stats() returned nil")
	}
	if stats.TotalPackages != 0 || stats.SuccessfulOps != 0 || stats.FailedOps != 0 {
		t.Fatalf("fresh engine stats = %+v, want zeros", stats)
	}
}