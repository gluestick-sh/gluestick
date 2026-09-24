package extractor

import (
	"bytes"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// progress7zParser parses 7-Zip -bsp1 progress lines from stdout.
// Example: " 12% 1/2 - Extracting file.dat", " 45% 2 + archive.7z"
type progress7zParser struct {
	mu       sync.Mutex
	regex    *regexp.Regexp
	current  int
	callback func(percent int)
	partial  []byte
}

func newProgress7zParser(callback func(percent int)) *progress7zParser {
	if callback == nil {
		return nil
	}
	return &progress7zParser{
		regex:    regexp.MustCompile(`(\d+)%`),
		callback: callback,
		current:  -1,
	}
}

func (p *progress7zParser) Write(b []byte) (int, error) {
	if p == nil {
		return len(b), nil
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	p.partial = append(p.partial, b...)
	for {
		idx := bytes.IndexByte(p.partial, '\n')
		if idx < 0 {
			break
		}
		line := string(p.partial[:idx])
		p.partial = p.partial[idx+1:]
		if len(p.partial) > 0 && p.partial[0] == '\r' {
			p.partial = p.partial[1:]
		}
		p.parseLineLocked(line)
	}
	return len(b), nil
}

func (p *progress7zParser) parseLineLocked(line string) {
	line = trimProgressLine(line)
	if line == "" {
		return
	}

	matches := p.regex.FindStringSubmatch(line)
	if len(matches) < 2 {
		return
	}

	percent, err := strconv.Atoi(matches[1])
	if err != nil || percent < 0 || percent > 100 {
		return
	}

	if percent != p.current {
		p.current = percent
		p.callback(percent)
	}
}

func trimProgressLine(line string) string {
	line = strings.TrimSpace(line)
	if line == "" {
		return ""
	}
	// 7z may prefix progress with a carriage return on updates.
	for len(line) > 0 && (line[0] == '\r' || line[0] == '\n') {
		line = line[1:]
	}
	return line
}

// scaleExtractPercent maps a stage-local 0-100% value into an overall 0-100% range.
func scaleExtractPercent(percent, stage, totalStages int) int {
	if totalStages <= 1 {
		return percent
	}
	if stage < 1 {
		stage = 1
	}
	if stage > totalStages {
		stage = totalStages
	}
	if percent < 0 {
		percent = 0
	}
	if percent > 100 {
		percent = 100
	}
	span := 100 / totalStages
	base := (stage - 1) * span
	return base + (percent * span / 100)
}
