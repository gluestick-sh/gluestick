package downloader

import (
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"
	"strings"

	"github.com/gluestick-sh/core/store"
)

// manifestUsesCacheStoreHash reports whether the manifest digest equals the cache store object key (SHA-256).
func manifestUsesCacheStoreHash(algo string) bool {
	switch strings.ToLower(algo) {
	case "", "sha256", "sha2":
		return true
	default:
		return false
	}
}

func newManifestHash(algo string) (hash.Hash, error) {
	switch strings.ToLower(algo) {
	case "sha1":
		return sha1.New(), nil
	case "md5":
		return md5.New(), nil
	case "sha512":
		return sha512.New(), nil
	default:
		return nil, fmt.Errorf("unsupported hash algorithm: %s", algo)
	}
}

func wrapManifestDigest(r io.Reader, algo string) (io.Reader, func() (string, error)) {
	h, err := newManifestHash(algo)
	if err != nil {
		return r, func() (string, error) {
			return "", err
		}
	}
	return io.TeeReader(r, h), func() (string, error) {
		return hex.EncodeToString(h.Sum(nil)), nil
	}
}

func verifyManifestDigest(algo, expected, actual string) error {
	if expected == "" {
		return nil
	}
	if !strings.EqualFold(actual, expected) {
		return fmt.Errorf("integrity check failed: hash mismatch (expected %s:%s, got %s)", algo, expected, actual)
	}
	return nil
}

func verifyTaskDigest(task Task, casHash string, manifestFinish func() (string, error)) error {
	if task.HashValue == "" {
		return nil
	}
	if manifestUsesCacheStoreHash(task.HashAlgo) {
		return verifyManifestDigest(task.HashAlgo, task.HashValue, casHash)
	}
	if manifestFinish == nil {
		return fmt.Errorf("manifest hash (%s) was not computed during ingest", task.HashAlgo)
	}
	actual, err := manifestFinish()
	if err != nil {
		return err
	}
	return verifyManifestDigest(task.HashAlgo, task.HashValue, actual)
}

func taskWithExpectedHash(task Task, expectedHash string) Task {
	if task.HashValue == "" && expectedHash != "" {
		task.HashValue = expectedHash
		if task.HashAlgo == "" {
			task.HashAlgo = "sha256"
		}
	}
	return task
}

// VerifyDownloadResult checks that downloaded content matches the manifest digest
// read before download. Call before install/extract continues.
func VerifyDownloadResult(store *store.Store, task Task, result Result) error {
	if result.Error != nil {
		return result.Error
	}
	return VerifyArchiveObject(store, task, result.Hash)
}

// VerifyArchiveObject checks a cache store object against the manifest digest on task.
func VerifyArchiveObject(store *store.Store, task Task, casHash string) error {
	if task.HashValue == "" {
		return nil
	}
	if casHash == "" {
		return fmt.Errorf("integrity check failed: no content hash to verify against %s:%s",
			task.HashAlgo, task.HashValue)
	}
	if manifestUsesCacheStoreHash(task.HashAlgo) {
		return verifyManifestDigest(task.HashAlgo, task.HashValue, casHash)
	}
	return verifyObjectManifestDigest(store, task.HashAlgo, task.HashValue, casHash)
}

func verifyObjectManifestDigest(store *store.Store, algo, expected, casHash string) error {
	f, err := os.Open(store.ObjectPath(casHash))
	if err != nil {
		return fmt.Errorf("integrity check failed: open content: %w", err)
	}
	defer f.Close()

	h, err := newManifestHash(algo)
	if err != nil {
		return err
	}
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("integrity check failed: read content: %w", err)
	}
	actual := hex.EncodeToString(h.Sum(nil))
	return verifyManifestDigest(algo, expected, actual)
}

// ManifestHashWriter returns a digest writer for non-cache store manifest algorithms during zip ingest.
func ManifestHashWriter(hashAlgo, hashValue string) (hash.Hash, []io.Writer) {
	if hashValue == "" || manifestUsesCacheStoreHash(hashAlgo) {
		return nil, nil
	}
	h, err := newManifestHash(hashAlgo)
	if err != nil {
		return nil, nil
	}
	return h, []io.Writer{h}
}
