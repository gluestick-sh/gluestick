//go:build !windows

package apps

import "fmt"

// LinkCurrent is unavailable on non-Windows hosts.
func LinkCurrent(pkgRoot, version string) error {
	return fmt.Errorf("current link not supported on this platform")
}

// ReadCurrent is unavailable on non-Windows hosts.
func ReadCurrent(pkgRoot string) (string, error) {
	return "", fmt.Errorf("current link not supported on this platform")
}

func removeCurrentLink(current string) error {
	return fmt.Errorf("current link not supported on this platform")
}
