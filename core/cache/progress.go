package cache

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/gluestick-sh/core/message"
)

// GCProgressEvent is a structured cache GC progress update for GUI i18n.
type GCProgressEvent struct {
	Phase       string
	MessageKey  string
	MessageArgs map[string]any
	Percent     float64
}

// GCProgressReporter receives cache GC phase updates.
type GCProgressReporter func(GCProgressEvent)

// GC phase labels
const (
	GCPhasePrepare  = "prepare"
	GCPhaseCollect  = "collect"
	GCPhaseScan     = "scan"
	GCPhaseDelete   = "delete"
	GCPhaseComplete = "complete"
)

// reportGC is a helper that reports GC progress events if reporter is not nil.
// Initializes args map if nil, then calls reporter with phase, key, args, and percent.
func reportGC(report GCProgressReporter, phase, key string, args map[string]any, pct float64) {
	if report == nil {
		return
	}
	if args == nil {
		args = map[string]any{}
	}
	report(GCProgressEvent{
		Phase:       phase,
		MessageKey:  key,
		MessageArgs: args,
		Percent:     pct,
	})
}

// Message returns the English fallback for the event.
func (e GCProgressEvent) Message() string {
	return message.FormatEN(e.MessageKey, e.MessageArgs)
}

// FriendlyDisplayPath shortens a path for progress messages (~/.glue/...).
func FriendlyDisplayPath(path string) string {
	if path == "" {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	rel, err := filepath.Rel(home, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return path
	}
	return "~/" + filepath.ToSlash(rel)
}
