//go:build windows

package apps

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestReadFileVersion_systemExe(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows only")
	}
	candidates := []string{
		filepath.Join(os.Getenv("SystemRoot"), "System32", "notepad.exe"),
		filepath.Join(os.Getenv("SystemRoot"), "System32", "cmd.exe"),
	}
	var lastErr error
	for _, path := range candidates {
		ver, err := ReadFileVersion(path)
		if err == nil && ver != "" {
			t.Logf("%s -> %s", path, ver)
			return
		}
		lastErr = err
	}
	t.Fatalf("ReadFileVersion failed for system exes: %v", lastErr)
}
