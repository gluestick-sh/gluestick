package main

// CLI progress reporting for glue cache gc (progress bar on stderr).

import (
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/schollz/progressbar/v3"
	"github.com/gluestick-sh/core/cache"
	"github.com/gluestick-sh/core/message"
)

var gcProgressBarKeys = map[string]struct{}{
	message.GCScanningStore:        {},
	message.GCScanningAppsMerged:     {},
	message.GCDeletingOrphansBatch:   {},
	message.GCDeletingOrphan:         {},
}

// stderrOut flushes after each write so progress bar updates show on Windows.
var stderrOut io.Writer = &flushWriter{w: os.Stderr}

type flushWriter struct {
	w io.Writer
}

func (f *flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if file, ok := f.w.(*os.File); ok {
		_ = file.Sync()
	}
	return n, err
}

type cliCacheGCReporter struct {
	mu     sync.Mutex
	bar    *progressbar.ProgressBar
	barKey string
}

func newCLICacheGCReporter() cache.GCProgressReporter {
	r := &cliCacheGCReporter{}
	return r.report
}

func (r *cliCacheGCReporter) report(ev cache.GCProgressEvent) {
	msg := ev.Message()
	if msg == "" {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, useBar := gcProgressBarKeys[ev.MessageKey]; useBar && ev.Percent > 0 && ev.Percent < 100 {
		r.updateBar(ev.MessageKey, msg, ev.Percent)
		return
	}

	r.finishBar()
	fmt.Printf("  %s\n", msg)
}

func (r *cliCacheGCReporter) updateBar(key, desc string, pct float64) {
	pctInt := max(0, min(100, int64(pct)))

	if r.bar == nil || r.barKey != key {
		r.finishBar()
		r.barKey = key
		r.bar = progressbar.NewOptions64(
			100,
			progressbar.OptionSetDescription(desc),
			progressbar.OptionSetWriter(stderrOut),
			progressbar.OptionFullWidth(),
			progressbar.OptionSetRenderBlankState(true),
			progressbar.OptionEnableColorCodes(colorEnabled()),
			progressbar.OptionSetElapsedTime(true),
			progressbar.OptionShowCount(),
			progressbar.OptionOnCompletion(func() {
				fmt.Fprintln(os.Stderr)
			}),
		)
	} else {
		r.bar.Describe(desc)
	}
	_ = r.bar.Set64(pctInt)
}

func (r *cliCacheGCReporter) finishBar() {
	if r.bar == nil {
		return
	}
	_ = r.bar.Finish()
	r.bar = nil
	r.barKey = ""
}
