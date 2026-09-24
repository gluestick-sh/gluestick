package install

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const removeRetries = 5

// RemoveAll deletes path and all children. It clears read-only attributes and retries
// when files are temporarily locked (common on Windows after extracting Electron apps).
func RemoveAll(path string) error {
	if err := makeRemovable(path); err != nil {
		return err
	}
	var lastErr error
	for attempt := range removeRetries {
		lastErr = os.RemoveAll(path)
		if lastErr == nil {
			return nil
		}
		if !isRetryableRemoveErr(lastErr) {
			return lastErr
		}
		_ = makeRemovable(path)
		time.Sleep(time.Duration(100*(attempt+1)) * time.Millisecond)
	}
	return lastErr
}

func makeRemovable(root string) error {
	if _, err := os.Stat(root); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		return makePathRemovable(path, d.IsDir())
	})
}

func isRetryableRemoveErr(err error) bool {
	if errors.Is(err, os.ErrPermission) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "access is denied") ||
		strings.Contains(msg, "used by another process") ||
		strings.Contains(msg, "sharing violation")
}
