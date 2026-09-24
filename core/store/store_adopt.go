package store

import (
	"fmt"
	"os"
	"path/filepath"
)

// Adopt registers an on-disk file in cache store by hashing it and creating a hardlink at the
// content-addressed path. The source file remains at path (e.g. under apps/).
func (s *Store) Adopt(path string) (string, error) {
	hash, err := s.HashForPath(path)
	if err != nil {
		return "", fmt.Errorf("hash %s: %w", path, err)
	}
	dest := s.ObjectPath(hash)
	if _, err := os.Stat(dest); err == nil {
		return hash, nil
	} else if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
		return "", err
	}
	if err := os.Link(path, dest); err != nil {
		if copyErr := materializeCopy(path, dest); copyErr != nil {
			if _, statErr := os.Stat(dest); statErr == nil {
				return hash, nil
			}
			return "", fmt.Errorf("adopt link %s: %w (copy: %v)", path, err, copyErr)
		}
	}
	return hash, nil
}
