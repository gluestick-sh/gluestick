package apps

import (
	"io/fs"
	"path/filepath"
)

// CountInstallDir returns file count and total size for a package version directory.
func CountInstallDir(pkgDir string) (fileCount int, totalSize int64) {
	_ = filepath.WalkDir(pkgDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && d.Name() == CurrentLinkName {
			return filepath.SkipDir
		}
		if d.IsDir() {
			return nil
		}
		fileCount++
		if info, statErr := d.Info(); statErr == nil {
			totalSize += info.Size()
		}
		return nil
	})
	return fileCount, totalSize
}
