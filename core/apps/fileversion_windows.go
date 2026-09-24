//go:build windows

package apps

import (
	"fmt"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ReadFileVersion returns the PE FileVersion string (e.g. "8.1.4087.64") for path.
func ReadFileVersion(path string) (string, error) {
	path = filepath.Clean(path)
	size, err := windows.GetFileVersionInfoSize(path, nil)
	if err != nil {
		return "", err
	}
	if size == 0 {
		return "", fmt.Errorf("no version info: %s", path)
	}
	buf := make([]byte, size)
	if err := windows.GetFileVersionInfo(path, 0, size, unsafe.Pointer(&buf[0])); err != nil {
		return "", err
	}
	var fixed *windows.VS_FIXEDFILEINFO
	var length uint32
	if err := windows.VerQueryValue(unsafe.Pointer(&buf[0]), `\`, unsafe.Pointer(&fixed), &length); err != nil {
		return "", err
	}
	if fixed == nil || length == 0 {
		return "", fmt.Errorf("empty fixed file info: %s", path)
	}
	major := fixed.FileVersionMS >> 16
	minor := fixed.FileVersionMS & 0xffff
	build := fixed.FileVersionLS >> 16
	patch := fixed.FileVersionLS & 0xffff
	return fmt.Sprintf("%d.%d.%d.%d", major, minor, build, patch), nil
}
