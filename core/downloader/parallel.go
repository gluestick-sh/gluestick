package downloader

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/gluestick-sh/core/humanize"
	"github.com/gluestick-sh/core/verbose"
)

// minParallelChunkSize is the minimum per-connection chunk for parallel downloads.
const minParallelChunkSize = 4 << 20 // 4 MiB

// minParallelResumeBytes is the minimum remaining bytes to resume with parallel ranges.
const minParallelResumeBytes = 32 << 20 // 32 MiB

// minParallelChunkSizeOverride is used by tests when > 0.
var minParallelChunkSizeOverride int64

func effectiveMinChunkSize() int64 {
	if minParallelChunkSizeOverride > 0 {
		return minParallelChunkSizeOverride
	}
	return minParallelChunkSize
}

// parallelDownloadMinBytes overrides GrabStyleMaxBytes for parallel threshold when > 0 (tests only).
var parallelDownloadMinBytes int64

func effectiveParallelMinBytes() int64 {
	if parallelDownloadMinBytes > 0 {
		return parallelDownloadMinBytes
	}
	return int64(GrabStyleMaxBytes) + 1
}

type byteRange struct {
	start int64
	end   int64 // inclusive
}

// planChunks splits total bytes into contiguous ranges for parallel download.
func planChunks(total int64, workers int) []byteRange {
	if total < effectiveParallelMinBytes() || workers < 2 {
		return nil
	}
	w := parallelWorkersForSize(total, workers)
	return planChunksRange(0, total-1, w)
}

// planChunksRange splits [rangeStart, rangeEnd] (inclusive) into up to workers ranges.
func planChunksRange(rangeStart, rangeEnd int64, workers int) []byteRange {
	if workers < 2 || rangeEnd < rangeStart {
		return nil
	}
	span := rangeEnd - rangeStart + 1
	if span < effectiveMinChunkSize()*2 {
		return nil
	}
	maxChunks := int(span / effectiveMinChunkSize())
	if maxChunks < 2 {
		return nil
	}
	n := workers
	if n > maxChunks {
		n = maxChunks
	}
	chunkSize := span / int64(n)
	ranges := make([]byteRange, 0, n)
	off := rangeStart
	for i := 0; i < n; i++ {
		end := off + chunkSize - 1
		if i == n-1 {
			end = rangeEnd
		}
		ranges = append(ranges, byteRange{start: off, end: end})
		off = end + 1
	}
	return ranges
}

func shouldParallelResume(total, offset int64) bool {
	if total <= offset {
		return false
	}
	return total-offset >= minParallelResumeBytes
}

// probeURLForParallel returns file size and whether the server accepts byte ranges.
func (d *Downloader) probeURLForParallel(ctx context.Context, url string) (total int64, ok bool) {
	total = d.headContentLength(ctx, url)
	if total <= 0 {
		total = d.rangeProbeContentLength(ctx, url)
	}
	if total < effectiveParallelMinBytes() && total > 0 {
		// Still probe range support; resume path may parallelize remaining bytes.
	}

	resp, _, err := d.doGETWithFallback(ctx, []string{url}, func(req *http.Request) {
		req.Header.Set("Range", "bytes=0-0")
	})
	if err != nil {
		return total, false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return total, resp.StatusCode == http.StatusPartialContent
}

func (d *Downloader) downloadParallelToPartial(
	ctx context.Context,
	task Task,
	fetchURL, partPath, metaPath string,
	total int64,
) (int64, string, error) {
	return d.downloadParallelRangeToPartial(ctx, task, fetchURL, partPath, metaPath, total, 0)
}

func (d *Downloader) downloadParallelResumeToPartial(
	ctx context.Context,
	task Task,
	fetchURL, partPath, metaPath string,
	total, offset int64,
) (int64, string, error) {
	if !shouldParallelResume(total, offset) {
		return 0, "", fmt.Errorf("parallel resume: remaining too small")
	}
	return d.downloadParallelRangeToPartial(ctx, task, fetchURL, partPath, metaPath, total, offset)
}

func (d *Downloader) downloadParallelRangeToPartial(
	ctx context.Context,
	task Task,
	fetchURL, partPath, metaPath string,
	total, completedOffset int64,
) (int64, string, error) {
	workers := parallelWorkersForSize(total, d.workers)
	chunks := planChunksRange(completedOffset, total-1, workers)
	if len(chunks) < 2 {
		return 0, "", fmt.Errorf("parallel: not enough chunks")
	}

	if err := os.MkdirAll(filepath.Dir(partPath), 0755); err != nil {
		return 0, "", fmt.Errorf("create partial dir: %w", err)
	}
	f, err := os.OpenFile(partPath, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return 0, "", fmt.Errorf("open partial file: %w", err)
	}
	if err := f.Truncate(total); err != nil {
		f.Close()
		return 0, "", fmt.Errorf("allocate partial file: %w", err)
	}
	f.Close()

	label := "parallel"
	if completedOffset > 0 {
		label = "parallel resume"
	}
	verbose.Fprintf("%s download %s: %d connections, %s total, %s done\n",
		label, task.Filename, len(chunks), humanize.FormatBytes(total), humanize.FormatBytes(completedOffset))

	bar := makeDownloadBar(fmt.Sprintf("%s (%s)", task.Filename, label), total, completedOffset)
	defer completeDownloadBar(bar)
	var barMu sync.Mutex
	setDownloadBarProgressLocked(&barMu, bar, total, completedOffset)
	pc := newProgressCounter(ctx, task.Filename, total, completedOffset, bar, &barMu)
	pc.emit()

	var wg sync.WaitGroup
	var failOnce atomic.Bool
	var firstErr error
	var errMu sync.Mutex

	recordErr := func(err error) {
		if err == nil {
			return
		}
		errMu.Lock()
		defer errMu.Unlock()
		if firstErr == nil {
			firstErr = err
			failOnce.Store(true)
		}
	}

	for _, chunk := range chunks {
		if failOnce.Load() {
			break
		}
		wg.Add(1)
		go func(ch byteRange) {
			defer wg.Done()
			if failOnce.Load() {
				return
			}
			if err := d.downloadChunk(ctx, fetchURL, partPath, ch, pc, &barMu); err != nil {
				recordErr(err)
			}
		}(chunk)
	}
	wg.Wait()

	if firstErr != nil {
		return 0, fetchURL, firstErr
	}

	info, err := os.Stat(partPath)
	if err != nil {
		return 0, fetchURL, fmt.Errorf("stat partial: %w", err)
	}
	if info.Size() != total {
		return 0, fetchURL, fmt.Errorf("parallel download incomplete: got %d, want %d", info.Size(), total)
	}

	meta := partialMeta{URL: task.URL, SourceURL: fetchURL, TotalSize: total}
	if err := savePartialMeta(metaPath, meta); err != nil {
		return 0, fetchURL, fmt.Errorf("save partial meta: %w", err)
	}
	return total, fetchURL, nil
}

func (d *Downloader) downloadChunk(
	ctx context.Context,
	fetchURL, partPath string,
	ch byteRange,
	pc *progressCounter,
	barMu *sync.Mutex,
) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fetchURL, nil)
	if err != nil {
		return fmt.Errorf("create range request: %w", err)
	}
	req.Header.Set("User-Agent", d.userAgent)
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", ch.start, ch.end))

	resp, err := d.client.Do(req)
	if err != nil {
		return fmt.Errorf("chunk %d-%d: %w", ch.start, ch.end, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("chunk %d-%d: HTTP %d", ch.start, ch.end, resp.StatusCode)
	}

	file, err := os.OpenFile(partPath, os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("open partial for chunk: %w", err)
	}
	defer file.Close()

	if _, err := file.Seek(ch.start, io.SeekStart); err != nil {
		return fmt.Errorf("seek chunk: %w", err)
	}

	var writer io.Writer = file
	if pc != nil {
		writer = pc.writer(file)
	}

	written, err := d.copyFromReader(writer, resp.Body)
	if err != nil {
		return fmt.Errorf("write chunk: %w", err)
	}
	expected := ch.end - ch.start + 1
	if written != expected {
		return fmt.Errorf("chunk size mismatch: wrote %d, expected %d", written, expected)
	}
	return nil
}
