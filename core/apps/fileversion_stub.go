//go:build !windows

package apps

import "fmt"

// ReadFileVersion is unavailable on non-Windows hosts.
func ReadFileVersion(path string) (string, error) {
	return "", fmt.Errorf("file version not supported on this platform: %s", path)
}
