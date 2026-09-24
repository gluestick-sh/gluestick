//go:build !windows

package install

import "fmt"

func linkDirectoryJunction(linkPath, targetPath string) error {
	return fmt.Errorf("directory junction not supported on this platform")
}

func removeDirectoryLink(linkPath string) error {
	return fmt.Errorf("directory junction not supported on this platform")
}
