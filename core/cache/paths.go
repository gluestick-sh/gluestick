package cache

import (
	"path/filepath"
	"strings"
)

// IsHiddenInstallPath reports paths that should not be linked into apps/ or indexed.
func IsHiddenInstallPath(relPath string) bool {
	if relPath == "" {
		return true
	}
	name := filepath.Base(relPath)
	return strings.HasPrefix(name, ".")
}
