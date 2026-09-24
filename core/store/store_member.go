package store

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
)

// ErrNeedReaderRetry means the content hash is known but the object is not in the store;
// the caller must supply a fresh reader for Write.
var ErrNeedReaderRetry = errors.New("store: reader retry required")

// maxSmallMemberBytes is the largest zip member buffered in memory for single-pass ingest.
const maxSmallMemberBytes = 2 << 20

// HashReader returns the SHA-256 hex digest of r without storing it.
func (s *Store) HashReader(r io.Reader) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return "", fmt.Errorf("hash stream: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// WriteMember stores r in cache store. When uncompressedSize is known and small, a single read
// hashes and stores (or skips when dedupLikely and the blob already exists).
// For large members with dedupLikely, hashes first and returns ErrNeedReaderRetry when storage is needed.
func (s *Store) WriteMember(r io.Reader, uncompressedSize int64, dedupLikely bool) (string, error) {
	if uncompressedSize >= 0 && uncompressedSize <= maxSmallMemberBytes {
		buf := make([]byte, uncompressedSize)
		if _, err := io.ReadFull(r, buf); err != nil {
			return "", fmt.Errorf("read member: %w", err)
		}
		sum := sha256.Sum256(buf)
		hash := hex.EncodeToString(sum[:])
		if dedupLikely && s.Has(hash) {
			return hash, nil
		}
		if err := s.putPrehashed(hash, buf); err != nil {
			return "", err
		}
		return hash, nil
	}
	if dedupLikely {
		hash, err := s.HashReader(r)
		if err != nil {
			return "", err
		}
		if s.Has(hash) {
			return hash, nil
		}
		return hash, ErrNeedReaderRetry
	}
	return s.Write(r)
}

func (s *Store) putPrehashed(hash string, data []byte) error {
	finalPath := s.ObjectPath(hash)
	if _, err := os.Stat(finalPath); err == nil {
		return nil
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}

	tmpFile, err := os.CreateTemp(s.rootDir, ".tmp-")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	tmpName := tmpFile.Name()

	if _, err := tmpFile.Write(data); err != nil {
		tmpFile.Close()
		os.Remove(tmpName)
		return fmt.Errorf("failed to write data: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("failed to close temp file: %w", err)
	}

	if err := os.Rename(tmpName, finalPath); err != nil {
		if _, statErr := os.Stat(finalPath); statErr == nil {
			os.Remove(tmpName)
			return nil
		}
		os.Remove(tmpName)
		return fmt.Errorf("failed to move object to %s: %w", finalPath, err)
	}
	return nil
}
