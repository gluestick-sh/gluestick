// Package version holds Gluestick CLI release metadata.
package version

import (
	"fmt"
	"runtime/debug"
	"time"
)

// These may be overridden at link time via:
// -ldflags "-X github.com/gluestick-sh/cli/version.Version=..." etc.
//
// When not injected (e.g. a plain `go build`), Commit and Date fall back to the
// VCS metadata that Go embeds automatically via runtime/debug.ReadBuildInfo.
var (
	Version = "0.1.0"
	Commit  = ""
	Date    = ""
)

func init() {
	if Commit != "" && Date != "" {
		return
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return
	}
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			if Commit == "" {
				Commit = s.Value
			}
		case "vcs.time":
			if Date == "" {
				Date = s.Value
			}
		}
	}
}

// CLIVersion is shown by glue -v / cobra --version.
func CLIVersion() string {
	commit := Commit
	if commit == "" {
		commit = "none"
	} else if len(commit) > 12 {
		commit = commit[:12]
	}
	date := Date
	if date == "" {
		date = "unknown"
	} else if t, err := time.Parse(time.RFC3339, date); err == nil {
		date = t.UTC().Format("2006-01-02T15:04:05Z")
	}
	return fmt.Sprintf("%s (commit: %s, built: %s)", Version, commit, date)
}
