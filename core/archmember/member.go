// Package archmember normalizes paths of members inside ZIP/7z archives (not on-disk archive file paths).
package archmember

import (
	"path/filepath"
	"strings"
)

// NormalizeMember converts archive entry paths to forward-slash form for maps and install layout.
func NormalizeMember(name string) string {
	return filepath.ToSlash(strings.TrimSpace(name))
}

// IsDirectoryPlaceholder reports .NET-style zero-byte folder markers (e.g. "runtimes\\win-x64\\").
func IsDirectoryPlaceholder(name string, size uint64) bool {
	name = strings.TrimSpace(name)
	trimmed := strings.TrimRight(name, `/\`)
	if trimmed == "" {
		return true
	}
	if len(name) > len(trimmed) {
		return true
	}
	if size == 0 && !strings.Contains(filepath.Base(NormalizeMember(trimmed)), ".") && strings.ContainsAny(name, `/\`) {
		return true
	}
	return false
}

// IsDirectoryPlaceholderName is for link-time filtering when entry size is unknown.
func IsDirectoryPlaceholderName(name string) bool {
	name = strings.TrimSpace(name)
	trimmed := strings.TrimRight(name, `/\`)
	if trimmed == "" {
		return true
	}
	return len(name) > len(trimmed)
}

// Depth returns the number of path segments (1 for "app.exe", 3 for "a/b/c.exe").
func Depth(relPath string) int {
	relPath = NormalizeMember(relPath)
	relPath = strings.Trim(relPath, "/")
	if relPath == "" {
		return 0
	}
	return strings.Count(relPath, "/") + 1
}

// SingleRootPrefix reports whether every member shares exactly one top-level directory
// (typical 7z/zip layout). The returned prefix includes a trailing slash, e.g. "FreeCAD/".
func SingleRootPrefix(members map[string]string) (prefix string, ok bool) {
	if len(members) == 0 {
		return "", false
	}
	var root string
	for relPath := range members {
		rel := NormalizeMember(relPath)
		rel = strings.Trim(rel, "/")
		if rel == "" || IsDirectoryPlaceholderName(relPath) {
			continue
		}
		i := strings.Index(rel, "/")
		if i < 0 {
			return "", false
		}
		top := rel[:i]
		if root == "" {
			root = top
		} else if top != root {
			return "", false
		}
	}
	if root == "" {
		return "", false
	}
	return root + "/", true
}

// StripRootPrefix returns a copy of members with a shared top-level directory removed.
func StripRootPrefix(members map[string]string, prefix string) map[string]string {
	prefix = NormalizeMember(prefix)
	if prefix == "" {
		return members
	}
	out := make(map[string]string, len(members))
	for relPath, hash := range members {
		rel := NormalizeMember(relPath)
		if after, ok := strings.CutPrefix(rel, prefix); ok {
			rel = after
		}
		if rel == "" || IsDirectoryPlaceholderName(rel) {
			continue
		}
		out[rel] = hash
	}
	return out
}
