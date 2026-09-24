package downloader

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/gluestick-sh/core/manifest"
)

// DirectStreamMaxBytes is the size below which fresh downloads stream HTTP → store
// without writing a .partial file. At or above this size, glue uses a single tuned
// GET + CopyBuffer to a resumable .partial file (grab-style); parallel range
// downloads apply only above GrabStyleMaxBytes when enabled.
const DirectStreamMaxBytes = 64 << 20 // 64 MiB

// GrabStyleMaxBytes is the upper bound for single-connection tuned downloads.
// Files at or below this size never use parallel range requests.
const GrabStyleMaxBytes = 64 << 20 // 64 MiB

// directStreamMaxBytesOverride forces a different direct-stream limit when > 0 (tests only).
var directStreamMaxBytesOverride int64

// effectiveDirectStreamMaxBytes returns the direct stream limit, allowing test override.
func effectiveDirectStreamMaxBytes() int64 {
	if directStreamMaxBytesOverride > 0 {
		return directStreamMaxBytesOverride
	}
	return int64(DirectStreamMaxBytes)
}

// hasPartialResume checks if a task has an existing partial download that can be resumed.
func (d *Downloader) hasPartialResume(task Task) bool {
	partPath, _ := d.partialPaths(task)
	info, err := os.Stat(partPath)
	return err == nil && info.Size() > 0
}

// tryDirectStream downloads with a single GET when size is known and ≤ DirectStreamMaxBytes.
// Returns handled=false to fall back to the resumable .partial path.
func (d *Downloader) tryDirectStream(ctx context.Context, task Task) (Result, bool) {
	if isFTPURL(task.URL) {
		return Result{}, false
	}
	if d.hasPartialResume(task) {
		return Result{}, false
	}

	start := time.Now()

	urls := d.urlsForTask(task.URL)
	resp, _, err := d.doGETWithFallback(ctx, urls, nil)
	if err != nil {
		return Result{Task: task, Error: fmt.Errorf("download failed: %w", err)}, true
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return Result{Task: task, Error: fmt.Errorf("HTTP %d: %s", resp.StatusCode, resp.Status)}, true
	}

	if resp.ContentLength <= 0 || resp.ContentLength > effectiveDirectStreamMaxBytes() {
		resp.Body.Close()
		return Result{}, false
	}

	result := d.ingestResponseBody(ctx, task, resp.Body, resp.ContentLength)
	result.Task = task
	result.Timing.addNetwork(time.Since(start))
	return result, true
}

// ingestResponseBody processes a download response body, storing it in the cache and
// handling ZIP archives specially. Returns the result with hash, size, and any error.
func (d *Downloader) ingestResponseBody(ctx context.Context, task Task, body io.Reader, totalSize int64) Result {
	var result Result

	isZip := manifest.ShouldNativeZipIngest(task.Filename, task.URL)

	if isZip {
		hash, err := d.streamZipBlobToCacheStore(ctx, body, task.HashAlgo, task.HashValue)
		result.Hash = hash
		result.Error = err
		if err == nil {
			d.attachZipMemberIndex(&result, hash)
		}
	} else {
		reader := io.Reader(body)
		var manifestFinish func() (string, error)
		if task.HashValue != "" && !manifestUsesCacheStoreHash(task.HashAlgo) {
			reader, manifestFinish = wrapManifestDigest(body, task.HashAlgo)
		}
		if totalSize >= 1<<20 {
			bar := makeDownloadBar(task.Filename, totalSize, 0)
			defer completeDownloadBar(bar)
			setDownloadBarProgress(bar, totalSize, 0)
			pc := newProgressCounter(ctx, task.Filename, totalSize, 0, bar, nil)
			pc.emit()
			reader = pc.reader(reader)
		} else if fn := downloadProgressFrom(ctx); fn != nil {
			pc := newProgressCounter(ctx, task.Filename, totalSize, 0, nil, nil)
			pc.emit()
			reader = pc.reader(reader)
		}
		hash, err := d.store.Write(reader)
		result.Hash = hash
		if err != nil {
			result.Error = err
		} else if verr := d.finalizeIngest(task, &result, manifestFinish); verr != nil {
			result.Error = verr
		}
	}

	if result.Error == nil && result.Hash != "" {
		if info, err := os.Stat(d.store.ObjectPath(result.Hash)); err == nil {
			result.Size = info.Size()
		} else if totalSize > 0 {
			result.Size = totalSize
		}
	} else if result.Size <= 0 && totalSize > 0 {
		result.Size = totalSize
	}

	return result
}
