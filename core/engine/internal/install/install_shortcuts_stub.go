//go:build !windows

package install

import "github.com/gluestick-sh/core/manifest"

func createPackageShortcuts(installDir string, m *manifest.Manifest, installArch string) error {
	return nil
}

// RemovePackageShortcuts is unavailable on non-Windows hosts.
func RemovePackageShortcuts(m *manifest.Manifest, installArch string) error {
	return nil
}
