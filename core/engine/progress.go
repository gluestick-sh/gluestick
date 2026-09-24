package engine

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/gluestick-sh/core/verbose"
)

// ConsoleReporter prints phase progress to stderr (not used by the CLI).
// The CLI uses SilentReporter and relies on install_run progress output instead.
type ConsoleReporter struct {
	enabled bool
	mu      sync.Mutex
}

// NewConsoleReporter creates a new console progress reporter
func NewConsoleReporter(enabled bool) *ConsoleReporter {
	return &ConsoleReporter{enabled: enabled}
}

// ReportProgress prints progress events to console
func (r *ConsoleReporter) ReportProgress(event ProgressEvent) {
	if !r.enabled {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	switch event.Status {
	case StatusRunning:
		if event.Phase == PhaseResolve {
			verbose.Progressf("  %s %s...\n", runningIcon, event.Message)
		} else {
			// Show progress bar for download and extract
			if event.Percentage > 0 {
				bar := r.createProgressBar(event.Percentage)
				verbose.Progressf("\r  %s %s: %s %d%%", runningIcon, event.Message, bar, int(event.Percentage))
			} else {
				verbose.Progressf("\r  %s %s...", runningIcon, event.Message)
			}
		}
	case StatusSuccess:
		if event.Phase == PhaseComplete {
			verbose.Progressf("\n  %s Done\n", successIcon)
		} else {
			verbose.Progressf("\n  %s %s\n", successIcon, event.Message)
		}
	case StatusFailed:
		verbose.Progressf("\n  %s %s\n", failIcon, event.Message)
		if event.Error != nil {
			verbose.Progressf("    Error: %v\n", event.Error)
		}
	case StatusSkipped:
		verbose.Progressf("  %s %s\n", skipIcon, event.Message)
	case StatusInfo:
		verbose.Progressf("  %s %s\n", infoIcon, event.Message)
	}
}

// createProgressBar creates a simple text progress bar
func (r *ConsoleReporter) createProgressBar(percentage float64) string {
	width := 30
	completed := int(percentage * float64(width) / 100)
	bar := strings.Repeat("=", completed) + strings.Repeat(" ", width-completed)
	return fmt.Sprintf("[%s]", bar)
}

// MultiReporter is a progress reporter that can report to multiple reporters
type MultiReporter struct {
	reporters []ProgressReporter
}

// NewMultiReporter creates a new multi reporter
func NewMultiReporter(reporters ...ProgressReporter) *MultiReporter {
	return &MultiReporter{reporters: reporters}
}

// ReportProgress reports to all registered reporters
func (m *MultiReporter) ReportProgress(event ProgressEvent) {
	for _, reporter := range m.reporters {
		reporter.ReportProgress(event)
	}
}

// AddReporter adds a reporter to the multi reporter
func (m *MultiReporter) AddReporter(reporter ProgressReporter) {
	m.reporters = append(m.reporters, reporter)
}

// CallbackReporter reports progress by calling a callback function
type CallbackReporter struct {
	callback func(ProgressEvent)
}

// NewCallbackReporter creates a new callback reporter
func NewCallbackReporter(callback func(ProgressEvent)) *CallbackReporter {
	return &CallbackReporter{callback: callback}
}

// ReportProgress calls the callback with the progress event
func (r *CallbackReporter) ReportProgress(event ProgressEvent) {
	if r.callback != nil {
		r.callback(event)
	}
}

// SilentReporter is a progress reporter that does nothing
type SilentReporter struct{}

// NewSilentReporter creates a new silent reporter
func NewSilentReporter() *SilentReporter {
	return &SilentReporter{}
}

// ReportProgress does nothing
func (r *SilentReporter) ReportProgress(event ProgressEvent) {
	// Silently ignore all progress events
}

// ProgressTracker tracks and manages progress for an operation
type ProgressTracker struct {
	reporter ProgressReporter
	current  Phase
	start    time.Time
}

// NewProgressTracker creates a new progress tracker
func NewProgressTracker(reporter ProgressReporter) *ProgressTracker {
	return &ProgressTracker{
		reporter: reporter,
		start:    time.Now(),
	}
}

// Start starts tracking progress for a phase
func (t *ProgressTracker) Start(phase Phase, packageName, message string) {
	t.current = phase
	t.reportProgress(phase, packageName, StatusRunning, message, 0, 0, 0, nil)
}

// Update updates the progress with new information
func (t *ProgressTracker) Update(phase Phase, packageName, message string, percentage float64, bytes, totalBytes int64, err error) {
	t.reportProgress(phase, packageName, StatusRunning, message, percentage, bytes, totalBytes, err)
}

// Complete marks the phase as complete
func (t *ProgressTracker) Complete(phase Phase, packageName, message string) {
	t.reportProgress(phase, packageName, StatusSuccess, message, 100, 0, 0, nil)
}

// Error marks the phase as failed
func (t *ProgressTracker) Error(phase Phase, packageName, message string, err error) {
	t.reportProgress(phase, packageName, StatusFailed, message, 0, 0, 0, err)
}

// Skip marks the phase as skipped
func (t *ProgressTracker) Skip(phase Phase, packageName, message string) {
	t.reportProgress(phase, packageName, StatusSkipped, message, 0, 0, 0, nil)
}

// Info reports an informational message
func (t *ProgressTracker) Info(phase Phase, packageName, message string) {
	t.reportProgress(phase, packageName, StatusInfo, message, 0, 0, 0, nil)
}

// reportProgress is the internal method to report progress
func (t *ProgressTracker) reportProgress(phase Phase, packageName string, status Status, message string, percentage float64, bytes, totalBytes int64, err error) {
	event := ProgressEvent{
		Phase:      phase,
		Package:    packageName,
		Status:     status,
		Message:    message,
		Percentage: percentage,
		Bytes:      bytes,
		TotalBytes: totalBytes,
		Error:      err,
		Timestamp:  time.Now(),
	}

	if t.reporter != nil {
		t.reporter.ReportProgress(event)
	}
}

// GetDuration returns the duration since tracking started
func (t *ProgressTracker) GetDuration() time.Duration {
	return time.Since(t.start)
}

// Icons for console output
const (
	runningIcon = "\u2192" // ?
	successIcon = "\u2713" // ?
	failIcon    = "\u2717" // ?
	skipIcon    = "\u2014" // ?
	infoIcon    = "i"
)

// ProgressEventLogger logs progress events to a writer
type ProgressEventLogger struct {
	writer io.Writer
}

// NewProgressEventLogger creates a new progress event logger
func NewProgressEventLogger(w io.Writer) *ProgressEventLogger {
	return &ProgressEventLogger{writer: w}
}

// ReportProgress writes the progress event to the writer
func (l *ProgressEventLogger) ReportProgress(event ProgressEvent) {
	fmt.Fprintf(l.writer, "[%s] %s %s: %s\n",
		event.Timestamp.Format("15:04:05"),
		event.Package,
		event.Phase,
		event.Message,
	)
}

// BufferedReporter buffers progress events and reports them when flushed
type BufferedReporter struct {
	events []ProgressEvent
	mu     sync.Mutex
	reporter ProgressReporter
}

// NewBufferedReporter creates a new buffered reporter
func NewBufferedReporter(reporter ProgressReporter) *BufferedReporter {
	return &BufferedReporter{
		reporter: reporter,
	}
}

// ReportProgress buffers the event
func (b *BufferedReporter) ReportProgress(event ProgressEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.events = append(b.events, event)
}

// Flush reports all buffered events
func (b *BufferedReporter) Flush() {
	b.mu.Lock()
	defer b.mu.Unlock()

	for _, event := range b.events {
		if b.reporter != nil {
			b.reporter.ReportProgress(event)
		}
	}
	b.events = b.events[:0] // Clear the buffer
}

// Events returns all buffered events
func (b *BufferedReporter) Events() []ProgressEvent {
	b.mu.Lock()
	defer b.mu.Unlock()

	// Return a copy
	events := make([]ProgressEvent, len(b.events))
	copy(events, b.events)
	return events
}