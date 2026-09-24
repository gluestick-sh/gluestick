package git

import (
	"bytes"
	"regexp"
	"strconv"
	"strings"
)

// Progress line buffering and percent parsing for git clone/pull stderr output.

var gitPercentRE = regexp.MustCompile(`(\d+)%`)

// ProgressCallback receives git stderr progress lines and an optional percentage.
type ProgressCallback func(message string, percent float64)

type lineBufferWriter struct {
	buf []byte
	fn  func(line string)
}

func (w *lineBufferWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexAny(w.buf, "\r\n")
		if i < 0 {
			break
		}
		line := strings.TrimSpace(string(w.buf[:i]))
		w.buf = w.buf[i+1:]
		if line != "" && w.fn != nil {
			w.fn(line)
		}
	}
	return len(p), nil
}

func parseGitProgressLine(line string) (message string, percent float64) {
	message = line
	if m := gitPercentRE.FindStringSubmatch(line); len(m) == 2 {
		if p, err := strconv.ParseFloat(m[1], 64); err == nil {
			percent = p
		}
	}
	return message, percent
}
