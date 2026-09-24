//go:build !windows

package manifest

func hostArchitecture() string {
	return Arch64bit
}

func hostWindowsBuild() int {
	return 0
}
