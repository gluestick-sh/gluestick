// Package humanize formats byte sizes, durations, and timestamps for CLI output.
package humanize

import (
	"fmt"
	"time"
)

// FormatBytes formats a byte count using binary (1024) units with IEC suffixes (KiB, MiB, …).
func FormatBytes(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(bytes)/float64(div), "KMGTPE"[exp])
}

// FormatDuration formats a duration for progress logs (ms, seconds, or minutes+seconds).
func FormatDuration(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%d ms", d.Milliseconds())
	}
	if d < time.Minute {
		return fmt.Sprintf("%.1f s", d.Seconds())
	}
	return fmt.Sprintf("%dm %ds", int(d.Minutes()), int(d.Seconds())%60)
}

// FormatCacheDate parses a cache index installed timestamp and returns a local display time.
// Empty input returns "-"; unrecognized strings are returned unchanged.
func FormatCacheDate(installed string) string {
	if installed == "" {
		return "-"
	}
	const out = "2006-01-02 15:04:05"
	for _, layout := range []string{time.RFC3339, time.RFC3339Nano, "2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, installed); err == nil {
			return t.Local().Format(out)
		}
	}
	return installed
}
