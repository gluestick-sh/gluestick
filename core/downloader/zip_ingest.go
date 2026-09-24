package downloader

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/gluestick-sh/core/archmember"
	"github.com/gluestick-sh/core/store"
	"github.com/gluestick-sh/core/verbose"
)

func (d *Downloader) ingestZipSpooled(ctx context.Context, r io.Reader, hashAlgo, hashValue string) (string, map[string]string, error) {
	dir := filepath.Join(d.store.Path(), ".partial")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", nil, fmt.Errorf("create zip spool dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, "zip-ingest-")
	if err != nil {
		return "", nil, fmt.Errorf("create zip spool: %w", err)
	}
	tmpPath := tmp.Name()
	removeTmp := true
	defer func() {
		_ = tmp.Close()
		if removeTmp {
			_ = os.Remove(tmpPath)
		}
	}()

	zipHash := sha256.New()
	writers := []io.Writer{tmp, zipHash}
	manifestH, manifestWriters := manifestHashWriters(hashAlgo, hashValue)
	writers = append(writers, manifestWriters...)

	if _, err := d.copyFromReader(io.MultiWriter(writers...), r); err != nil {
		return "", nil, fmt.Errorf("spool zip: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return "", nil, fmt.Errorf("sync zip spool: %w", err)
	}
	info, err := tmp.Stat()
	if err != nil {
		return "", nil, err
	}

	zipHashSum := fmt.Sprintf("%x", zipHash.Sum(nil))
	if err := verifyZipManifestAfterHash(hashAlgo, hashValue, zipHashSum, manifestH); err != nil {
		return "", nil, err
	}

	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return "", nil, fmt.Errorf("rewind zip spool: %w", err)
	}

	sum, files, err := d.ingestZipFile(ctx, tmp, info.Size(), hashAlgo, hashValue, tmpPath, zipHashSum, true, false)
	if err != nil {
		return "", nil, err
	}
	if _, statErr := os.Stat(tmpPath); os.IsNotExist(statErr) {
		removeTmp = false
	}
	return sum, files, nil
}

// ingestZipFile reads a zip from a seekable file on disk. When preHashed is false, one streaming pass
// computes digests; members are extracted to cache store; adoptPath (if set) may be renamed into cache store as the zip blob.
func (d *Downloader) ingestZipFile(ctx context.Context, f *os.File, size int64, hashAlgo, hashValue, adoptPath, precomputedZipHash string, preHashed, dedupLikely bool) (string, map[string]string, error) {
	var zipHashSum string
	var manifestH hash.Hash

	if preHashed {
		zipHashSum = precomputedZipHash
	} else {
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return "", nil, fmt.Errorf("rewind zip: %w", err)
		}
		zipHash := sha256.New()
		writers := []io.Writer{zipHash}
		var manifestWriters []io.Writer
		manifestH, manifestWriters = manifestHashWriters(hashAlgo, hashValue)
		writers = append(writers, manifestWriters...)
		if _, err := d.copyFromReader(io.Discard, io.TeeReader(f, io.MultiWriter(writers...))); err != nil {
			return "", nil, fmt.Errorf("read zip: %w", err)
		}
		zipHashSum = fmt.Sprintf("%x", zipHash.Sum(nil))
		if err := verifyZipManifestAfterHash(hashAlgo, hashValue, zipHashSum, manifestH); err != nil {
			return "", nil, err
		}
	}

	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", nil, fmt.Errorf("rewind zip: %w", err)
	}

	zipReader, err := zip.NewReader(f, size)
	if err != nil {
		return "", nil, fmt.Errorf("open zip: %w", err)
	}

	var members []*zip.File
	for _, file := range zipReader.File {
		if file.Mode()&os.ModeDir != 0 {
			continue
		}
		if archmember.IsDirectoryPlaceholder(file.Name, file.UncompressedSize64) {
			continue
		}
		members = append(members, file)
	}
	if len(members) == 0 {
		if err := f.Close(); err != nil {
			return "", nil, fmt.Errorf("close zip file: %w", err)
		}
		sourcePath := adoptPath
		if sourcePath == "" {
			sourcePath = f.Name()
		}
		if err := d.ensureZipBlobInCacheStore(zipHashSum, sourcePath); err != nil {
			return "", nil, err
		}
		return zipHashSum, map[string]string{}, nil
	}

	workers := zipIngestWorkers(d.workers, len(members))

	files := make(map[string]string, len(members))
	var totalBytes atomic.Int64
	var mu sync.Mutex
	total := int64(len(members))
	var processed atomic.Int64
	onProgress := zipIngestProgressFrom(ctx)
	throttle := NewThrottledProgress(func(done, tot int64, _ string) {
		if onProgress != nil {
			onProgress(done, tot)
		}
	})
	if onProgress != nil {
		onProgress(0, total)
	}

	reportProgress := func() {
		if throttle != nil {
			throttle.Report(processed.Load(), total, "")
		}
	}

	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	var firstErr error
	var errOnce sync.Once

	for _, member := range members {
		wg.Add(1)
		go func(file *zip.File) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			memberSize := int64(file.UncompressedSize64)
			hash, err := d.ingestZipMember(file, dedupLikely)
			if err != nil {
				errOnce.Do(func() { firstErr = fmt.Errorf("write %s to cache store: %w", file.Name, err) })
				return
			}
			mu.Lock()
			files[archmember.NormalizeMember(file.Name)] = hash
			mu.Unlock()
			totalBytes.Add(memberSize)
			processed.Add(1)
			reportProgress()
		}(member)
	}
	wg.Wait()

	if firstErr != nil {
		d.discardCacheStoreHashes(mapValues(files)...)
		return "", nil, firstErr
	}
	if onProgress != nil {
		onProgress(total, total)
	}

	if err := f.Close(); err != nil {
		d.discardCacheStoreHashes(mapValues(files)...)
		return "", nil, fmt.Errorf("close zip file: %w", err)
	}
	sourcePath := adoptPath
	if sourcePath == "" {
		sourcePath = f.Name()
	}
	if err := d.ensureZipBlobInCacheStore(zipHashSum, sourcePath); err != nil {
		d.discardCacheStoreHashes(mapValues(files)...)
		return "", nil, err
	}
	if info, err := os.Stat(d.store.ObjectPath(zipHashSum)); err == nil {
		totalBytes.Add(info.Size())
	}
	if err := saveZipMemberIndex(d.store.Path(), zipHashSum, files, totalBytes.Load()); err != nil {
		verbose.Fprintf("save zip member index: %v\n", err)
	}
	d.recordManifestHashAlias(hashAlgo, hashValue, zipHashSum)

	return zipHashSum, files, nil
}

func (d *Downloader) ingestZipMember(file *zip.File, dedupLikely bool) (string, error) {
	size := int64(file.UncompressedSize64)
	rc, err := file.Open()
	if err != nil {
		return "", fmt.Errorf("open %s in zip: %w", file.Name, err)
	}
	hash, err := d.store.WriteMember(rc, size, dedupLikely)
	rc.Close()
	if err == nil {
		return hash, nil
	}
	if !errors.Is(err, store.ErrNeedReaderRetry) {
		return "", err
	}
	rc2, err := file.Open()
	if err != nil {
		return "", fmt.Errorf("reopen %s in zip: %w", file.Name, err)
	}
	defer rc2.Close()
	return d.store.Write(rc2)
}

func manifestHashWriters(hashAlgo, hashValue string) (hash.Hash, []io.Writer) {
	return ManifestHashWriter(hashAlgo, hashValue)
}

func verifyZipManifestAfterHash(hashAlgo, hashValue, zipHashSum string, manifestH hash.Hash) error {
	if hashValue == "" {
		return nil
	}
	if manifestUsesCacheStoreHash(hashAlgo) {
		return verifyManifestDigest(hashAlgo, hashValue, zipHashSum)
	}
	if manifestH != nil {
		actual := hex.EncodeToString(manifestH.Sum(nil))
		return verifyManifestDigest(hashAlgo, hashValue, actual)
	}
	return nil
}

// ensureZipBlobInCacheStore places the raw zip bytes at the cache store path for zipHashSum.
// When sourcePath is set, it is renamed (or copied) instead of loading into memory.
func (d *Downloader) ensureZipBlobInCacheStore(zipHashSum, sourcePath string) error {
	zipPath := d.store.ObjectPath(zipHashSum)
	if _, err := os.Stat(zipPath); err == nil {
		return nil
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	if sourcePath == "" {
		return fmt.Errorf("save zip to cache store: no source file")
	}
	if err := os.MkdirAll(filepath.Dir(zipPath), 0755); err != nil {
		return err
	}
	if err := os.Rename(sourcePath, zipPath); err == nil {
		return nil
	}
	if err := copyFileBuffered(sourcePath, zipPath, d.acquireCopyBuf, d.releaseCopyBuf); err != nil {
		return fmt.Errorf("save zip to cache store: %w", err)
	}
	return nil
}

func copyFileBuffered(src, dst string, acquire func() []byte, release func([]byte)) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	defer out.Close()

	buf := acquire()
	defer release(buf)
	_, err = io.CopyBuffer(out, in, buf)
	return err
}
