package bucket

import (
	"strings"
)

// FormatGitError returns the most useful single-line message from git stderr/output.
func FormatGitError(msg string) string {
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return "check failed"
	}
	for _, line := range strings.Split(msg, "\n") {
		line = strings.TrimSpace(line)
		if idx := strings.Index(line, "fatal:"); idx >= 0 {
			out := strings.TrimSpace(line[idx:])
			return strings.TrimSuffix(out, ")")
		}
		if strings.HasPrefix(line, "fatal:") {
			return line
		}
	}
	return strings.Join(strings.Fields(msg), " ")
}

// FormatErr formats an error chain for bucket update/check UI.
func FormatErr(err error) string {
	if err == nil {
		return ""
	}
	return FormatGitError(err.Error())
}
