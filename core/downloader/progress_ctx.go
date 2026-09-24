package downloader

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/schollz/progressbar/v3"

	"github.com/gluestick-sh/core/progress"
)

// DownloadProgressFunc reports download/ingest byte progress (downloaded includes resume offset).
type DownloadProgressFunc func(downloaded, total int64, message string)

// ZipIngestProgressFunc reports zip member ingest progress (processed/total files).
type ZipIngestProgressFunc func(processed, total int64)

type progressCtxKey struct{}
type zipIngestProgressCtxKey struct{}

// ContextWithDownloadProgress attaches a progress callback to ctx for the current download.
func ContextWithDownloadProgress(ctx context.Context, fn DownloadProgressFunc) context.Context {
	if fn == nil {
		return ctx
	}
	return context.WithValue(ctx, progressCtxKey{}, fn)
}

func downloadProgressFrom(ctx context.Context) DownloadProgressFunc {
	if ctx == nil {
		return nil
	}
	if fn, ok := ctx.Value(progressCtxKey{}).(DownloadProgressFunc); ok && fn != nil {
		return fn
	}
	if h := progress.HandlerFrom(ctx); h != nil && h.Bytes != nil {
		return h.Bytes
	}
	return nil
}

// ContextWithZipIngestProgress attaches a zip member ingest callback to ctx.
func ContextWithZipIngestProgress(ctx context.Context, fn ZipIngestProgressFunc) context.Context {
	if fn == nil {
		return ctx
	}
	return context.WithValue(ctx, zipIngestProgressCtxKey{}, fn)
}

func zipIngestProgressFrom(ctx context.Context) ZipIngestProgressFunc {
	if ctx == nil {
		return nil
	}
	if fn, ok := ctx.Value(zipIngestProgressCtxKey{}).(ZipIngestProgressFunc); ok && fn != nil {
		return fn
	}
	if h := progress.HandlerFrom(ctx); h != nil && h.Files != nil {
		return h.Files
	}
	return nil
}

const progressEmitInterval = 150 * time.Millisecond

// ThrottledProgress rate-limits progress callbacks for UI/event consumers.
type ThrottledProgress struct {
	fn             DownloadProgressFunc
	mu             sync.Mutex
	lastEmit       time.Time
	lastPct        int
	lastDownloaded int64
	lastTotal      int64
	lastMessage    string
}

// NewThrottledProgress wraps fn with throttling; pass nil to disable reporting.
func NewThrottledProgress(fn DownloadProgressFunc) *ThrottledProgress {
	if fn == nil {
		return nil
	}
	return &ThrottledProgress{fn: fn}
}

// Report emits progress when the percentage changes or the throttle window elapses.
func (t *ThrottledProgress) Report(downloaded, total int64, message string) {
	if t == nil || t.fn == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	pct := -1
	if total > 0 {
		pct = int(float64(downloaded) / float64(total) * 100)
		if pct > 100 {
			pct = 100
		}
	}
	now := time.Now()
	if !t.lastEmit.IsZero() &&
		now.Sub(t.lastEmit) < progressEmitInterval &&
		pct >= 0 && pct == t.lastPct &&
		downloaded == t.lastDownloaded {
		return
	}
	t.lastEmit = now
	t.lastPct = pct
	t.lastDownloaded = downloaded
	t.lastTotal = total
	t.lastMessage = message
	t.fn(downloaded, total, message)
}

// Flush forces the last progress values to be emitted (e.g. at phase boundaries).
func (t *ThrottledProgress) Flush() {
	if t == nil || t.fn == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.lastEmit.IsZero() {
		return
	}
	t.fn(t.lastDownloaded, t.lastTotal, t.lastMessage)
}

type progressCounter struct {
	ctx      context.Context
	throttle *ThrottledProgress
	bar      *progressbar.ProgressBar
	barMu    *sync.Mutex
	label    string
	offset   int64
	total    atomic.Int64
	written  atomic.Int64
}

func newProgressCounter(ctx context.Context, label string, total, offset int64, bar *progressbar.ProgressBar, barMu *sync.Mutex) *progressCounter {
	pc := &progressCounter{
		ctx:      ctx,
		throttle: NewThrottledProgress(downloadProgressFrom(ctx)),
		bar:      bar,
		barMu:    barMu,
		label:    label,
		offset:   offset,
	}
	if total > 0 {
		pc.total.Store(total)
	}
	return pc
}

func (p *progressCounter) setTotal(total int64) {
	if total <= 0 {
		return
	}
	p.total.Store(total)
	p.emit()
}

func (p *progressCounter) add(n int64) {
	if n <= 0 {
		return
	}
	p.written.Add(n)
	if p.bar != nil {
		if p.barMu != nil {
			p.barMu.Lock()
			_ = p.bar.Add64(n)
			p.barMu.Unlock()
		} else {
			_ = p.bar.Add64(n)
		}
	}
	p.emit()
}

func (p *progressCounter) emit() {
	if p.throttle == nil {
		return
	}
	downloaded := p.offset + p.written.Load()
	total := p.total.Load()
	p.throttle.Report(downloaded, total, downloadProgressMessage(p.label, downloaded, total))
}

func (p *progressCounter) writer(w io.Writer) io.Writer {
	if p == nil || (p.throttle == nil && p.bar == nil) {
		return w
	}
	return &countingWriter{counter: p, w: w}
}

func (p *progressCounter) reader(r io.Reader) io.Reader {
	if p == nil || (p.throttle == nil && p.bar == nil) {
		return r
	}
	return &countingReader{counter: p, r: r}
}

type countingWriter struct {
	counter *progressCounter
	w       io.Writer
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	if n > 0 {
		c.counter.add(int64(n))
	}
	return n, err
}

type countingReader struct {
	counter *progressCounter
	r       io.Reader
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 {
		c.counter.add(int64(n))
	}
	return n, err
}

func downloadProgressMessage(label string, downloaded, total int64) string {
	if total > 0 && downloaded >= total {
		return ""
	}
	if label != "" {
		return label
	}
	return ""
}
