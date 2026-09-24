//go:build !windows

package cache

// fileRefKeyForPath is unavailable on non-Windows hosts.
func fileRefKeyForPath(path string) (fileRefKey, bool) {
	return "", false
}
