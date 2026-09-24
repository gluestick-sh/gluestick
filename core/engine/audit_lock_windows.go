//go:build windows

package engine

import (
	"os"

	"golang.org/x/sys/windows"
)

// lockAuditFile takes a blocking exclusive lock over the audit lock file.
func lockAuditFile(file *os.File) error {
	return windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, &windows.Overlapped{})
}

// unlockAuditFile releases the lock taken by lockAuditFile.
func unlockAuditFile(file *os.File) error {
	return windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, &windows.Overlapped{})
}
