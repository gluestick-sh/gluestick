// Package safepath validates manifest-relative and archive member paths against traversal.
package safepath

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// ErrUnsafePath reports a path that escapes its intended base or contains ".." segments.
var ErrUnsafePath = errors.New("unsafe path")

// ValidateManifestRelPath normalizes a Scoop manifest-relative path and rejects traversal.
// Empty or "." paths are allowed and normalize to "".
func ValidateManifestRelPath(p string) (string, error) {
	p = filepath.ToSlash(strings.TrimSpace(p))
	if p == "" || p == "." {
		return "", nil
	}
	if filepath.IsAbs(p) || strings.HasPrefix(p, "/") || (len(p) >= 2 && p[1] == ':') {
		return "", fmt.Errorf("%w: absolute path %q", ErrUnsafePath, p)
	}
	p = strings.Trim(p, `/\`)
	if p == "" {
		return "", nil
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return "", fmt.Errorf("%w: path traversal in %q", ErrUnsafePath, p)
		}
		if seg == "" {
			return "", fmt.Errorf("%w: empty segment in %q", ErrUnsafePath, p)
		}
	}
	return p, nil
}

// JoinUnderBase joins rel under base and ensures the result stays within base.
func JoinUnderBase(base, rel string) (string, error) {
	safe, err := ValidateManifestRelPath(rel)
	if err != nil {
		return "", err
	}
	if safe == "" {
		return filepath.Clean(base), nil
	}
	target := filepath.Join(base, filepath.FromSlash(safe))
	target = filepath.Clean(target)
	baseClean := filepath.Clean(base)
	relToBase, err := filepath.Rel(baseClean, target)
	if err != nil {
		return "", err
	}
	if relToBase == ".." || strings.HasPrefix(relToBase, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %q escapes %q", ErrUnsafePath, rel, base)
	}
	return target, nil
}
