//go:build !windows

package engine

import "os"

// lockAuditFile is a no-op on non-Windows builds (core targets Windows; this
// keeps cross-compiles green and unit tests portable).
func lockAuditFile(*os.File) error { return nil }

// unlockAuditFile is the matching no-op.
func unlockAuditFile(*os.File) error { return nil }
