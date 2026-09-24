// Package store implements content-addressable storage (CAS): files are keyed by
// their SHA-256 hash so identical content is stored once and installs can hardlink
// blobs instead of copying them.
package store

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Store implements Content-Addressable Storage
// Files are stored by their SHA-256 hash, enabling instant hardlink installs
type Store struct {
	rootDir string
}

// NewStore creates a new cache store instance
func NewStore(rootDir string) (*Store, error) {
	if err := os.MkdirAll(rootDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create cache store directory: %w", err)
	}
	return &Store{rootDir: rootDir}, nil
}

// Path returns the storage root directory
func (s *Store) Path() string {
	return s.rootDir
}

// Write streams data to the cache store, returning the SHA-256 hash
func (s *Store) Write(r io.Reader) (string, error) {
	// First pass: compute hash
	hash := sha256.New()

	// Create unique temporary file to avoid conflicts in concurrent scenarios
	tmpFile, err := os.CreateTemp(s.rootDir, ".tmp-")
	if err != nil {
		return "", fmt.Errorf("failed to create temp file: %w", err)
	}
	tmpName := tmpFile.Name()

	// Tee: write to both file and hash
	multi := io.MultiWriter(tmpFile, hash)

	if _, err := io.Copy(multi, r); err != nil {
		tmpFile.Close()
		os.Remove(tmpName)
		return "", fmt.Errorf("failed to write data: %w", err)
	}

	// Close file BEFORE rename on Windows (no fsync: immutable cache store blobs; crash leaves .tmp- orphans)
	if err := tmpFile.Close(); err != nil {
		return "", fmt.Errorf("failed to close temp file: %w", err)
	}

	// Get final hash
	sum := hex.EncodeToString(hash.Sum(nil))

	// Move to final location
	finalPath := s.ObjectPath(sum)
	if err := os.Rename(tmpName, finalPath); err != nil {
		// May already exist
		if _, statErr := os.Stat(finalPath); statErr == nil {
			os.Remove(tmpName)
			return sum, nil
		}
		os.Remove(tmpName)
		return "", fmt.Errorf("failed to move object to %s: %w", finalPath, err)
	}

	return sum, nil
}

// ObjectPath returns the full path for a given hash
func (s *Store) ObjectPath(hash string) string {
	// Use two-level directory structure for better performance:
	// store/ab/cdef... instead of store/abcdef...
	// This reduces directory entry count
	if len(hash) < 4 {
		return filepath.Join(s.rootDir, hash)
	}
	prefix := hash[:2]
	return filepath.Join(s.rootDir, prefix, hash[2:])
}

// Has checks if an object exists in the store
func (s *Store) Has(hash string) bool {
	_, err := os.Stat(s.ObjectPath(hash))
	return err == nil
}

// Link creates a hardlink from the stored object to the target path.
// When hardlinks are unavailable (cross-volume, exFAT, etc.), it falls back to copy.
func (s *Store) Link(hash, targetPath string) error {
	srcPath := s.ObjectPath(hash)

	// Ensure source exists
	if _, err := os.Stat(srcPath); err != nil {
		return fmt.Errorf("source object not found: %s: %w", hash, err)
	}

	// Create target directory
	if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
		return fmt.Errorf("failed to create target directory: %w", err)
	}

	// Remove existing target if present
	os.Remove(targetPath)

	// Create hardlink (os.Link calls CreateHardLink on Windows)
	if err := os.Link(srcPath, targetPath); err != nil {
		if copyErr := materializeCopy(srcPath, targetPath); copyErr != nil {
			return fmt.Errorf("failed to create hardlink: %w (copy fallback: %v)", err, copyErr)
		}
	}

	return nil
}

func materializeCopy(srcPath, targetPath string) error {
	tmpPath := targetPath + ".tmp-" + linkTempSuffix()
	if err := copyFileContents(srcPath, tmpPath); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, targetPath); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return nil
}

func linkTempSuffix() string {
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		return "0000"
	}
	return hex.EncodeToString(buf)
}

func copyFileContents(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}

	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// Delete removes an object from the store. Missing objects are ignored.
func (s *Store) Delete(hash string) error {
	err := os.Remove(s.ObjectPath(hash))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// HashForPath computes the SHA-256 hash of a file
func (s *Store) HashForPath(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	hash := sha256.New()
	if _, err := io.Copy(hash, f); err != nil {
		return "", err
	}

	return hex.EncodeToString(hash.Sum(nil)), nil
}

// PayloadSize returns the indexed payload bytes for a file hash. It prefers the
// cache store object size when the blob exists; otherwise it uses fileSize for extracted
// files that are not stored in cache store (archive-only installs).
func (s *Store) PayloadSize(hash string, fileSize int64) int64 {
	if st, err := os.Stat(s.ObjectPath(hash)); err == nil {
		return st.Size()
	}
	return fileSize
}

// Prereqs creates the prefix directory structure for fast object storage
func (s *Store) Prereqs() error {
	// Create 256 prefix directories (00-ff in hex)
	for i := 0; i < 256; i++ {
		prefix := fmt.Sprintf("%02x", i)
		dir := filepath.Join(s.rootDir, prefix)
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
	}
	return nil
}
