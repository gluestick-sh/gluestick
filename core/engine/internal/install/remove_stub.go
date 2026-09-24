//go:build !windows

package install

import "os"

func makePathRemovable(path string, isDir bool) error {
	if isDir {
		return os.Chmod(path, 0755)
	}
	return os.Chmod(path, 0666)
}
