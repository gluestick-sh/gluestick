package downloader

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// streamZipBlobToCacheStore stores the zip archive in cache store without per-member ingest.
// Member indexing happens after install via adoptInstallDirToStore.
func (d *Downloader) streamZipBlobToCacheStore(ctx context.Context, r io.Reader, hashAlgo, hashValue string) (string, error) {
	if f, ok := r.(*os.File); ok {
		info, err := f.Stat()
		if err == nil && info.Size() > 0 {
			return d.storeZipBlobFile(f, info.Size(), hashAlgo, hashValue, f.Name())
		}
	}
	return d.storeZipBlobSpooled(ctx, r, hashAlgo, hashValue)
}

// storeZipBlobSpooled streams a zip archive to a temporary file while computing SHA256 hash,
// verifies manifest hash, stores the blob in cache store, and records hash alias.
// Returns SHA256 hash of the zip blob.
func (d *Downloader) storeZipBlobSpooled(ctx context.Context, r io.Reader, hashAlgo, hashValue string) (string, error) {
	dir := filepath.Join(d.store.Path(), ".partial")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("create zip spool dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, "zip-blob-")
	if err != nil {
		return "", fmt.Errorf("create zip spool: %w", err)
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
		return "", fmt.Errorf("spool zip: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}

	zipHashSum := fmt.Sprintf("%x", zipHash.Sum(nil))
	if err := verifyZipManifestAfterHash(hashAlgo, hashValue, zipHashSum, manifestH); err != nil {
		return "", err
	}
	if err := d.ensureZipBlobInCacheStore(zipHashSum, tmpPath); err != nil {
		return "", err
	}
	if _, statErr := os.Stat(tmpPath); os.IsNotExist(statErr) {
		removeTmp = false
	}
	d.recordManifestHashAlias(hashAlgo, hashValue, zipHashSum)
	return zipHashSum, nil
}

// storeZipBlobFile processes a zip file from an os.File by computing SHA256 hash,
// verifying manifest hash, storing the blob in cache store, and recording hash alias.
// Returns SHA256 hash of the zip blob.
func (d *Downloader) storeZipBlobFile(f *os.File, size int64, hashAlgo, hashValue, adoptPath string) (string, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", fmt.Errorf("rewind zip: %w", err)
	}
	zipHash := sha256.New()
	writers := []io.Writer{zipHash}
	manifestH, manifestWriters := manifestHashWriters(hashAlgo, hashValue)
	writers = append(writers, manifestWriters...)
	if _, err := d.copyFromReader(io.Discard, io.TeeReader(f, io.MultiWriter(writers...))); err != nil {
		return "", fmt.Errorf("read zip: %w", err)
	}
	zipHashSum := fmt.Sprintf("%x", zipHash.Sum(nil))
	if err := verifyZipManifestAfterHash(hashAlgo, hashValue, zipHashSum, manifestH); err != nil {
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("close zip: %w", err)
	}
	sourcePath := adoptPath
	if sourcePath == "" {
		sourcePath = f.Name()
	}
	if err := d.ensureZipBlobInCacheStore(zipHashSum, sourcePath); err != nil {
		return "", err
	}
	d.recordManifestHashAlias(hashAlgo, hashValue, zipHashSum)
	return zipHashSum, nil
}

// SaveZipMemberIndex persists relPath→hash member map for fast reinstall.
func (d *Downloader) SaveZipMemberIndex(zipHash string, files map[string]string, totalBytes int64) error {
	return saveZipMemberIndex(d.store.Path(), zipHash, files, totalBytes)
}

// ResolveZipMemberIndex returns a link-ready member map when the index and cache store blobs match.
func (d *Downloader) ResolveZipMemberIndex(zipHash string) (map[string]string, int64, bool) {
	idx, ok := loadZipMemberIndex(d.store.Path(), zipHash)
	if !ok {
		return nil, 0, false
	}
	if !ZipMemberIndexReady(d.store, idx) {
		RemoveZipMemberIndex(d.store.Path(), zipHash)
		return nil, 0, false
	}
	return idx.Files, idx.TotalBytes, true
}

// attachZipMemberIndex attaches ZIP member index data to a download result.
// Populates result.Files and result.ZipTotalBytes if the index is available.
func (d *Downloader) attachZipMemberIndex(result *Result, zipHash string) {
	files, total, ok := d.ResolveZipMemberIndex(zipHash)
	if !ok {
		return
	}
	result.Files = files
	result.ZipTotalBytes = total
}
