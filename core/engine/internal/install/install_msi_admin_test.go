package install

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestMsiexecPath(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows only")
	}
	p, err := msiexecPath()
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(p) != "msiexec.exe" {
		t.Fatalf("path = %q", p)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("stat %q: %v", p, err)
	}
}
