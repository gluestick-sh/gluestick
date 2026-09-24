// Package downloader fetches package artifacts into the cache store with mirror fallback.
package downloader

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gluestick-sh/core/manifest"
	"github.com/gluestick-sh/core/store"
	"github.com/gluestick-sh/core/verbose"
)

// copyBufferSize is the HTTP body copy buffer (matches get-tuned benchmark).
const copyBufferSize = 128 * 1024

// Task represents a single download task
type Task struct {
	URL       string
	Filename  string
	Checksum  string // Deprecated: use HashValue
	HashAlgo  string // manifest hash algorithm: sha256, sha512, sha1, md5
	HashValue string // manifest expected digest (no algo prefix)
}

// Result represents the result of a download task
type Result struct {
	Task          Task
	Hash          string
	Size          int64
	Error         error
	Files         map[string]string // Extracted files: relativePath -> cache store hash
	ZipTotalBytes int64             // sum of member + archive bytes (from zip index)
	Timing        Timing
	FromStore     bool // blob reused from cache store without HTTP
	// ZipMembersIndexed is set when a cached zip blob was indexed into per-member cache store entries (not loaded from .zip-index).
	ZipMembersIndexed bool
}

// Downloader implements concurrent downloading with streaming decompression
type Downloader struct {
	client           *http.Client
	store            *store.Store
	workers          int
	userAgent        string
	ghProxies        []string // GitHub mirror prefixes; empty uses direct URLs only
	parallelDownload bool     // HTTP range parallel download for files > GrabStyleMaxBytes
	copyBufPool      sync.Pool
}

// Options configures the Downloader
type Options func(*Downloader)

// WithWorkers sets the number of concurrent download workers
func WithWorkers(n int) Options {
	return func(d *Downloader) {
		d.workers = NormalizeUserWorkers(n)
	}
}

// WithUserAgent sets the HTTP user agent
func WithUserAgent(ua string) Options {
	return func(d *Downloader) {
		d.userAgent = ua
	}
}

// WithGitHubProxies sets mirror prefixes for GitHub download URLs.
// Use config.LoadProxies(root) to match glue config and environment.
func WithGitHubProxies(proxies []string) Options {
	return func(d *Downloader) {
		d.SetGitHubProxies(proxies)
	}
}

// SetWorkers sets parallel download / range connection worker count.
func (d *Downloader) SetWorkers(n int) {
	d.workers = NormalizeUserWorkers(n)
}

// SetGitHubProxies replaces GitHub mirror prefixes used for downloads.
func (d *Downloader) SetGitHubProxies(proxies []string) {
	if len(proxies) == 0 {
		d.ghProxies = nil
		return
	}
	d.ghProxies = append([]string(nil), proxies...)
}

// WithParallelDownload enables or disables parallel range downloads for files above 64 MiB.
func WithParallelDownload(enabled bool) Options {
	return func(d *Downloader) {
		d.parallelDownload = enabled
	}
}

// ResolveDownloadURLs returns the URL(s) that will be tried for a canonical download URL.
func (d *Downloader) ResolveDownloadURLs(canonical string) []string {
	return d.urlsForTask(canonical)
}

func newTunedTransport() *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		DisableCompression:    true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   maxParallelConnections,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
	}
}

func (d *Downloader) acquireCopyBuf() []byte {
	if v := d.copyBufPool.Get(); v != nil {
		return v.([]byte)
	}
	return make([]byte, copyBufferSize)
}

func (d *Downloader) releaseCopyBuf(buf []byte) {
	if len(buf) == copyBufferSize {
		d.copyBufPool.Put(buf)
	}
}

func (d *Downloader) copyFromReader(w io.Writer, r io.Reader) (int64, error) {
	buf := d.acquireCopyBuf()
	defer d.releaseCopyBuf(buf)
	return io.CopyBuffer(w, r, buf)
}

// NewDownloader creates a new concurrent downloader
func NewDownloader(store *store.Store, opts ...Options) *Downloader {
	d := &Downloader{
		client: &http.Client{
			Transport: newTunedTransport(),
		},
		store:            store,
		workers:          DefaultWorkers,
		userAgent:        "Glue/1.0",
		parallelDownload: true,
	}

	for _, opt := range opts {
		opt(d)
	}

	return d
}

// DownloadAll downloads all tasks concurrently
func (d *Downloader) DownloadAll(ctx context.Context, tasks []Task) []Result {
	results := make([]Result, len(tasks))
	var wg sync.WaitGroup

	// Worker pool with semaphores to limit concurrency
	sem := make(chan struct{}, d.workers)

	for i, task := range tasks {
		wg.Add(1)
		go func(idx int, t Task) {
			defer wg.Done()
			sem <- struct{}{}        // Acquire
			defer func() { <-sem }() // Release

			results[idx] = d.download(ctx, t)
		}(i, task)
	}

	wg.Wait()
	return results
}

// DownloadWithCache downloads a file, checking the cache store first.
// skipStoreReuse forces a network download even when the blob already exists (e.g. glue install --force).
// Supports SHA-256 (cache store key), SHA-512, SHA-1, and MD5 manifest hashes.
// For non-SHA-256 hashes, downloads are verified but not cached by content hash.
func (d *Downloader) DownloadWithCache(ctx context.Context, task Task, expectedHash string, skipStoreReuse bool) Result {
	task = taskWithExpectedHash(task, expectedHash)
	result := Result{Task: task}

	if !skipStoreReuse {
		if casHash, ok := d.lookupStoreBlob(task); ok {
			result.Hash = casHash
			result.FromStore = true
			if info, err := os.Stat(d.store.ObjectPath(casHash)); err == nil {
				result.Size = info.Size()
			}
			if manifest.ShouldNativeZipIngest(task.Filename, task.URL) {
				d.attachZipMemberIndex(&result, casHash)
			}
			return result
		}
	}

	return d.download(ctx, task)
}

// ZipIndexTotalBytes returns cached install byte total from the zip member index.
func (d *Downloader) ZipIndexTotalBytes(zipHash string) int64 {
	idx, ok := loadZipMemberIndex(d.store.Path(), zipHash)
	if !ok {
		return 0
	}
	return idx.TotalBytes
}

// IngestZipFromStore builds per-member cache store entries from an existing zip blob (no network).
// When members are already in the store, only hashes are computed (no duplicate writes).
func (d *Downloader) IngestZipFromStore(ctx context.Context, zipHash, hashAlgo, hashValue string) (map[string]string, error) {
	return d.ingestZipFromCacheStore(ctx, zipHash, hashAlgo, hashValue)
}

// ingestZipFromCacheStore builds per-member cache store entries from an existing zip blob (no network).
func (d *Downloader) ingestZipFromCacheStore(ctx context.Context, zipHash, hashAlgo, hashValue string) (map[string]string, error) {
	zipPath := d.store.ObjectPath(zipHash)
	f, err := os.Open(zipPath)
	if err != nil {
		return nil, fmt.Errorf("open zip from store: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	_, files, err := d.ingestZipFile(ctx, f, info.Size(), hashAlgo, hashValue, zipPath, zipHash, true, true)
	if err != nil {
		return nil, err
	}
	return files, nil
}

// download handles a single download with resumable partial files and streaming decompression.
func (d *Downloader) download(ctx context.Context, task Task) Result {
	if result, ok := d.tryDirectStream(ctx, task); ok {
		return result
	}

	result := Result{Task: task}

	partPath, metaPath := d.partialPaths(task)
	netStart := time.Now()
	partFile, totalSize, err := d.downloadToPartial(ctx, task)
	result.Timing.addNetwork(time.Since(netStart))
	if err != nil {
		result.Error = err
		return result
	}

	isZip := manifest.ShouldNativeZipIngest(task.Filename, task.URL)

	f, err := os.Open(partFile)
	if err != nil {
		result.Error = fmt.Errorf("open downloaded file: %w", err)
		return result
	}

	casStart := time.Now()
	if isZip {
		verbose.Fprintf("Starting zip blob store, isZip=%v, size=%d\n", isZip, totalSize)
		casStart := time.Now()
		hash, err := d.streamZipBlobToCacheStore(ctx, f, task.HashAlgo, task.HashValue)
		result.Timing.addStoreIngest(time.Since(casStart))
		result.Hash = hash
		result.Error = err
		if err == nil {
			d.attachZipMemberIndex(&result, hash)
		}
	} else {
		reader := io.Reader(f)
		var manifestFinish func() (string, error)
		if task.HashValue != "" && !manifestUsesCacheStoreHash(task.HashAlgo) {
			reader, manifestFinish = wrapManifestDigest(f, task.HashAlgo)
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
	result.Timing.addStoreIngest(time.Since(casStart))
	if closeErr := f.Close(); closeErr != nil && result.Error == nil && !isZip {
		result.Error = fmt.Errorf("close downloaded file: %w", closeErr)
	}

	if result.Error == nil {
		removePartial(partPath, metaPath)
	}

	if result.Hash != "" {
		if info, err := os.Stat(d.store.ObjectPath(result.Hash)); err == nil {
			result.Size = info.Size()
		} else if result.Size <= 0 {
			result.Size = totalSize
		}
	} else if result.Size <= 0 {
		result.Size = totalSize
	}

	return result
}

func (d *Downloader) finalizeIngest(task Task, result *Result, manifestFinish func() (string, error)) error {
	if err := verifyTaskDigest(task, result.Hash, manifestFinish); err != nil {
		d.discardIngested(*result)
		result.Hash = ""
		result.Files = nil
		return err
	}
	d.recordManifestHashAlias(task.HashAlgo, task.HashValue, result.Hash)
	return nil
}

func (d *Downloader) discardIngested(result Result) {
	d.discardCacheStoreHashes(result.Hash)
	for _, h := range result.Files {
		d.discardCacheStoreHashes(h)
	}
}

func (d *Downloader) discardCacheStoreHashes(hashes ...string) {
	for _, h := range hashes {
		if h != "" {
			_ = d.store.Delete(h)
		}
	}
}

func mapValues(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

// InstallFromCacheStore installs a downloaded package from cache store to target directory
// using hardlinks (instant, zero-copy)
func (d *Downloader) InstallFromCacheStore(ctx context.Context, hash, targetDir string, files []string) error {
	for _, file := range files {
		// Compute hash for each file (in production, we'd track this)
		// For now, assume the hash points to the root
		targetPath := filepath.Join(targetDir, file)

		// For each file, we'd need its individual hash
		// This is a simplified version - in production, maintain a manifest
		if err := d.store.Link(hash, targetPath); err != nil {
			return fmt.Errorf("link %s: %w", file, err)
		}
	}
	return nil
}
