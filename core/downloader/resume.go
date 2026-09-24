package downloader

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/schollz/progressbar/v3"

	"github.com/gluestick-sh/core/config"
	"github.com/gluestick-sh/core/humanize"
	"github.com/gluestick-sh/core/verbose"
)

// stderrOut wraps stderr so progress bar updates flush immediately on Windows.
var stderrOut io.Writer = &flushWriter{w: os.Stderr}

// progressBarOut clears the current stderr row before each bar render. progressbar with
// OptionUseANSICodes(true) skips pre-clear and only appends on Windows, which garbles output.
var progressBarOut io.Writer = &progressBarWriter{w: stderrOut}

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

type progressBarWriter struct {
	w io.Writer
}

func (p *progressBarWriter) Write(b []byte) (int, error) {
	if len(b) > 0 && string(b) != "\r" {
		clearProgressLine(p.w)
	}
	return p.w.Write(b)
}

type partialMeta struct {
	URL       string `json:"url"`
	SourceURL string `json:"source_url,omitempty"` // actual URL used (mirror or direct)
	TotalSize int64  `json:"total_size,omitempty"`
}

func (d *Downloader) partialPaths(task Task) (partPath, metaPath string) {
	id := partialID(task.URL)
	dir := filepath.Join(d.store.Path(), ".partial")
	return filepath.Join(dir, id+".part"), filepath.Join(dir, id+".meta.json")
}

func partialID(url string) string {
	sum := sha256.Sum256([]byte(url))
	return hex.EncodeToString(sum[:16])
}

func loadPartialMeta(metaPath string) (partialMeta, error) {
	data, err := os.ReadFile(metaPath)
	if err != nil {
		return partialMeta{}, err
	}
	var meta partialMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return partialMeta{}, err
	}
	return meta, nil
}

func savePartialMeta(metaPath string, meta partialMeta) error {
	data, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	return os.WriteFile(metaPath, data, 0644)
}

func parseContentRange(header string) (start, end, total int64, ok bool) {
	// bytes 500-999/1000  or  bytes 500-999/*
	if !strings.HasPrefix(header, "bytes ") {
		return 0, 0, 0, false
	}
	parts := strings.SplitN(header, " ", 2)
	if len(parts) != 2 {
		return 0, 0, 0, false
	}
	rangeAndTotal := strings.SplitN(parts[1], "/", 2)
	if len(rangeAndTotal) != 2 {
		return 0, 0, 0, false
	}
	rangeParts := strings.SplitN(rangeAndTotal[0], "-", 2)
	if len(rangeParts) != 2 {
		return 0, 0, 0, false
	}
	start, err := strconv.ParseInt(rangeParts[0], 10, 64)
	if err != nil {
		return 0, 0, 0, false
	}
	end, err = strconv.ParseInt(rangeParts[1], 10, 64)
	if err != nil {
		return 0, 0, 0, false
	}
	if rangeAndTotal[1] != "*" {
		total, err = strconv.ParseInt(rangeAndTotal[1], 10, 64)
		if err != nil {
			return 0, 0, 0, false
		}
	}
	return start, end, total, true
}

// downloadToPartial downloads task.URL into a resumable .part file.
// On success the caller must remove the partial files after ingesting into cache store.
func (d *Downloader) downloadToPartial(ctx context.Context, task Task) (partPath string, totalSize int64, err error) {
	partPath, metaPath := d.partialPaths(task)
	if err := os.MkdirAll(filepath.Dir(partPath), 0755); err != nil {
		return "", 0, fmt.Errorf("create partial dir: %w", err)
	}

	if isFTPURL(task.URL) {
		_ = os.Remove(partPath)
		_ = os.Remove(metaPath)
		totalSize, err := d.downloadFTPToPartial(ctx, task, partPath, metaPath)
		return partPath, totalSize, err
	}

	meta := partialMeta{URL: task.URL}
	if existing, err := loadPartialMeta(metaPath); err == nil {
		if existing.URL != task.URL {
			_ = os.Remove(partPath)
			_ = os.Remove(metaPath)
		} else {
			meta = existing
		}
	}

	offset := int64(0)
	if info, err := os.Stat(partPath); err == nil {
		offset = info.Size()
	}

	// Partials without source_url cannot be resumed across mirror vs direct hosts.
	if offset > 0 && meta.SourceURL == "" {
		if len(d.ghProxies) > 0 && config.IsGitHubURL(task.URL) {
			verbose.Fprintf("Discarding partial without source_url (%s)\n", humanize.FormatBytes(offset))
			_ = os.Remove(partPath)
			_ = os.Remove(metaPath)
			offset = 0
			meta = partialMeta{URL: task.URL}
		} else {
			meta.SourceURL = task.URL
		}
	}

	if meta.TotalSize > 0 && offset >= meta.TotalSize {
		return partPath, meta.TotalSize, nil
	}

	urls := urlsForResume(task.URL, d.ghProxies, meta.SourceURL, offset)
	var lastErr error
	for i, fetchURL := range urls {
		if i > 0 {
			offset = 0
			meta = partialMeta{URL: task.URL}
			_ = os.Remove(partPath)
			_ = os.Remove(metaPath)
		}

		if offset == 0 && d.parallelDownload && d.workers > 1 {
			total := meta.TotalSize
			var rangeOK bool
			if total <= 0 {
				total, rangeOK = d.probeURLForParallel(ctx, fetchURL)
			} else {
				_, rangeOK = d.probeURLForParallel(ctx, fetchURL)
			}
			if rangeOK && total >= effectiveParallelMinBytes() {
				totalSize, usedURL, err := d.downloadParallelToPartial(ctx, task, fetchURL, partPath, metaPath, total)
				if err == nil {
					if usedURL != "" {
						meta.SourceURL = usedURL
						meta.URL = task.URL
						meta.TotalSize = totalSize
						_ = savePartialMeta(metaPath, meta)
					}
					return partPath, totalSize, nil
				}
				verbose.Fprintf("Parallel download failed, using single connection: %v\n", err)
				_ = os.Remove(partPath)
				_ = os.Remove(metaPath)
				meta = partialMeta{URL: task.URL}
			}
		}

		if offset > 0 && d.parallelDownload && d.workers > 1 {
			total := meta.TotalSize
			if total <= 0 && offset >= minParallelResumeBytes {
				total = d.probeContentLength(ctx, task.URL)
			}
			if shouldParallelResume(total, offset) {
				var rangeOK bool
				if total <= 0 {
					total, rangeOK = d.probeURLForParallel(ctx, fetchURL)
				} else {
					_, rangeOK = d.probeURLForParallel(ctx, fetchURL)
				}
				if rangeOK && total > 0 {
					totalSize, usedURL, err := d.downloadParallelResumeToPartial(ctx, task, fetchURL, partPath, metaPath, total, offset)
					if err == nil {
						if usedURL != "" {
							meta.SourceURL = usedURL
							meta.URL = task.URL
							meta.TotalSize = totalSize
							_ = savePartialMeta(metaPath, meta)
						}
						return partPath, totalSize, nil
					}
					verbose.Fprintf("Parallel resume failed, using single connection: %v\n", err)
				}
			}
		}

		totalSize, usedURL, err := d.fetchToPartialURL(ctx, task, fetchURL, partPath, metaPath, meta, offset)
		if err == nil {
			if usedURL != "" {
				meta.SourceURL = usedURL
				meta.URL = task.URL
				meta.TotalSize = totalSize
				_ = savePartialMeta(metaPath, meta)
			}
			return partPath, totalSize, nil
		}
		lastErr = err
		if offset > 0 {
			break
		}
	}
	return "", 0, lastErr
}

func effectiveBarMax(maxSize, offset int64) int64 {
	if maxSize > 0 {
		return maxSize
	}
	if offset > 0 {
		// Unknown total while resuming: avoid a 1 GiB placeholder that shows ~1%.
		return offset * 8
	}
	return 1024 * 1024 * 1024
}

func makeDownloadBar(description string, maxSize, offset int64) *progressbar.ProgressBar {
	maxSize = effectiveBarMax(maxSize, offset)
	return progressbar.NewOptions64(maxSize,
		progressbar.OptionSetDescription(description),
		progressbar.OptionSetWriter(progressBarOut),
		progressbar.OptionShowCount(),
		progressbar.OptionFullWidth(),
		progressbar.OptionSetRenderBlankState(true),
		progressbar.OptionEnableColorCodes(true),
		progressbar.OptionClearOnFinish(),
	)
}

// completeDownloadBar removes the in-place progress line after download finishes.
func completeDownloadBar(bar *progressbar.ProgressBar) {
	if bar == nil {
		return
	}
	_ = bar.Clear()
	clearProgressLine(stderrOut)
}

// clearProgressLine erases a carriage-return progress row. progressbar.Clear only
// writes "\r" on Windows, so a longer bar leaves suffix text when the next line is shorter.
func clearProgressLine(w io.Writer) {
	_, _ = io.WriteString(w, "\r\033[2K")
}

func setDownloadBarProgress(bar *progressbar.ProgressBar, maxSize, offset int64) {
	if bar == nil {
		return
	}
	if maxSize > 0 {
		bar.ChangeMax64(maxSize)
	}
	if offset > 0 {
		bar.Reset()
		bar.Add64(offset)
	} else {
		bar.Add64(0)
	}
}

func setDownloadBarProgressLocked(mu *sync.Mutex, bar *progressbar.ProgressBar, maxSize, offset int64) {
	if mu != nil {
		mu.Lock()
		defer mu.Unlock()
	}
	setDownloadBarProgress(bar, maxSize, offset)
}

func (d *Downloader) probeContentLength(ctx context.Context, canonicalURL string) int64 {
	probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	for _, url := range d.urlsForTask(canonicalURL) {
		if total := d.headContentLength(probeCtx, url); total > 0 {
			return total
		}
		if total := d.rangeProbeContentLength(probeCtx, url); total > 0 {
			return total
		}
	}
	return 0
}

func (d *Downloader) headContentLength(ctx context.Context, url string) int64 {
	resp, _, err := d.doRequestWithFallback(ctx, http.MethodHead, []string{url}, nil)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()

	if resp.ContentLength > 0 {
		return resp.ContentLength
	}
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		if n, err := strconv.ParseInt(cl, 10, 64); err == nil {
			return n
		}
	}
	return 0
}

func (d *Downloader) rangeProbeContentLength(ctx context.Context, url string) int64 {
	resp, _, err := d.doGETWithFallback(ctx, []string{url}, func(req *http.Request) {
		req.Header.Set("Range", "bytes=0-0")
	})
	if err != nil {
		return 0
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusPartialContent {
		if _, _, total, ok := parseContentRange(resp.Header.Get("Content-Range")); ok && total > 0 {
			return total
		}
	}
	if resp.ContentLength > 0 {
		return resp.ContentLength
	}
	return 0
}

func doRequestWaitWithBar(ctx context.Context, client *http.Client, req *http.Request, bar *progressbar.ProgressBar, barMu *sync.Mutex) (*http.Response, error) {
	_ = ctx
	_ = bar
	_ = barMu
	return client.Do(req)
}

func downloadBarDescription(filename string, offset int64) string {
	if offset > 0 {
		return fmt.Sprintf("%s (resume %s)", filename, humanize.FormatBytes(offset))
	}
	return filename
}

func (d *Downloader) fetchToPartialURL(ctx context.Context, task Task, fetchURL, partPath, metaPath string, meta partialMeta, offset int64) (totalSize int64, usedURL string, err error) {
	bar := makeDownloadBar(downloadBarDescription(task.Filename, offset), meta.TotalSize, offset)
	defer completeDownloadBar(bar)

	var barMu sync.Mutex
	setDownloadBarProgressLocked(&barMu, bar, meta.TotalSize, offset)
	pc := newProgressCounter(ctx, task.Filename, meta.TotalSize, offset, bar, &barMu)
	pc.emit()
	usedURL = fetchURL

	// Best-effort size probe while resuming without saved total_size.
	if offset > 0 && meta.TotalSize <= 0 {
		go func() {
			if total := d.probeContentLength(ctx, task.URL); total > 0 {
				setDownloadBarProgressLocked(&barMu, bar, total, offset)
				pc.setTotal(total)
				_ = savePartialMeta(metaPath, partialMeta{URL: task.URL, SourceURL: fetchURL, TotalSize: total})
			}
		}()
	}

	req, err := http.NewRequestWithContext(ctx, "GET", fetchURL, nil)
	if err != nil {
		return 0, "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("User-Agent", d.userAgent)
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}

	resp, err := doRequestWaitWithBar(ctx, d.client, req, bar, &barMu)
	if err != nil {
		return 0, "", fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusPartialContent:
		// append below
	case http.StatusOK:
		if offset > 0 {
			offset = 0
			_ = os.Truncate(partPath, 0)
			bar.Reset()
		}
	case http.StatusRequestedRangeNotSatisfiable:
		if meta.TotalSize > 0 && offset >= meta.TotalSize {
			return meta.TotalSize, usedURL, nil
		}
		_ = os.Remove(partPath)
		_ = os.Remove(metaPath)
		return d.fetchToPartialURL(ctx, task, fetchURL, partPath, metaPath, partialMeta{URL: task.URL, SourceURL: fetchURL}, 0)
	default:
		return 0, "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, resp.Status)
	}

	if resp.StatusCode == http.StatusPartialContent {
		if start, _, total, ok := parseContentRange(resp.Header.Get("Content-Range")); ok {
			if start != offset {
				return 0, "", fmt.Errorf("unexpected Content-Range start %d, expected %d", start, offset)
			}
			if total > 0 {
				meta.TotalSize = total
			}
		}
	} else if resp.ContentLength > 0 {
		meta.TotalSize = resp.ContentLength
	} else if cl := resp.Header.Get("Content-Length"); cl != "" {
		if n, err := strconv.ParseInt(cl, 10, 64); err == nil {
			meta.TotalSize = n
		}
	}

	setDownloadBarProgressLocked(&barMu, bar, meta.TotalSize, offset)
	pc.setTotal(meta.TotalSize)
	meta.URL = task.URL
	meta.SourceURL = fetchURL
	if meta.TotalSize > 0 {
		_ = savePartialMeta(metaPath, meta)
	}

	flags := os.O_CREATE | os.O_WRONLY
	file, err := os.OpenFile(partPath, flags, 0644)
	if err != nil {
		return 0, "", fmt.Errorf("open partial file: %w", err)
	}
	defer file.Close()

	if offset > 0 {
		if _, err := file.Seek(offset, io.SeekStart); err != nil {
			return 0, "", fmt.Errorf("seek partial file: %w", err)
		}
	} else {
		if err := file.Truncate(0); err != nil {
			return 0, "", fmt.Errorf("truncate partial file: %w", err)
		}
	}

	buf := d.acquireCopyBuf()
	written, err := io.CopyBuffer(file, pc.reader(resp.Body), buf)
	d.releaseCopyBuf(buf)
	if err != nil {
		_ = file.Sync()
		return 0, "", fmt.Errorf("write partial file: %w", err)
	}

	finalSize := offset + written
	if err := file.Sync(); err != nil {
		return 0, "", fmt.Errorf("sync partial file: %w", err)
	}

	meta.URL = task.URL
	meta.SourceURL = fetchURL
	if meta.TotalSize <= 0 {
		meta.TotalSize = finalSize
	}
	if err := savePartialMeta(metaPath, meta); err != nil {
		return 0, "", fmt.Errorf("save partial meta: %w", err)
	}

	if meta.TotalSize > 0 && finalSize != meta.TotalSize {
		return 0, "", fmt.Errorf("incomplete download: got %d bytes, expected %d", finalSize, meta.TotalSize)
	}

	return meta.TotalSize, usedURL, nil
}

func removePartial(partPath, metaPath string) {
	_ = os.Remove(partPath)
	_ = os.Remove(metaPath)
}

// ClearPartial removes resumable .part and .meta.json files for a download task.
func (d *Downloader) ClearPartial(task Task) {
	partPath, metaPath := d.partialPaths(task)
	removePartial(partPath, metaPath)
}

// ClearAllPartials removes every in-progress download under the store
// (resumable .part/.meta.json files and abandoned zip spool temps).
// Returns how many files were deleted and total bytes freed.
func (d *Downloader) ClearAllPartials() (removed int, freedBytes int64, err error) {
	dir := filepath.Join(d.store.Path(), ".partial")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, 0, nil
		}
		return 0, 0, err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		info, infoErr := entry.Info()
		size := int64(0)
		if infoErr == nil {
			size = info.Size()
		}
		if rmErr := os.Remove(path); rmErr == nil {
			removed++
			freedBytes += size
		}
	}
	return removed, freedBytes, nil
}
