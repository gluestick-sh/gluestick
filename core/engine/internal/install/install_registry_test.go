package install

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestApplyInstallContextRegistry(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("requires Windows")
	}
	dir := t.TempDir()
	reg := "Windows Registry Editor Version 5.00\r\n\r\n[-HKEY_CURRENT_USER\\Software\\ExampleGlueTest]\r\n"
	if err := os.WriteFile(filepath.Join(dir, "install-context.reg"), []byte(reg), 0644); err != nil {
		t.Fatal(err)
	}
	applyInstallContextRegistry(dir)
}
