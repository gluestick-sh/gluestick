//go:build windows

package install

import (
	"os"

	"golang.org/x/sys/windows"
)

func makePathRemovable(path string, isDir bool) error {
	_ = os.Chmod(path, 0666)
	if isDir {
		_ = os.Chmod(path, 0755)
	}

	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil
	}
	attrs, err := windows.GetFileAttributes(p)
	if err != nil {
		return nil
	}
	if attrs&windows.FILE_ATTRIBUTE_READONLY == 0 {
		return nil
	}
	attrs &^= windows.FILE_ATTRIBUTE_READONLY
	if err := windows.SetFileAttributes(p, attrs); err != nil {
		return nil
	}
	return nil
}
