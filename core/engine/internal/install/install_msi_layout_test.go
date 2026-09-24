package install

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestMergeInstallerSourceDir(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("robocopy merge requires Windows")
	}
	root := t.TempDir()
	installDir := filepath.Join(root, "python", "3.14.6")
	libDir := filepath.Join(installDir, "Lib")
	idleDir := filepath.Join(installDir, "SourceDir", "Lib", "idlelib")
	for _, dir := range []string{libDir, idleDir} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(libDir, "site-packages"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(libDir, "site-packages", ".keep"), nil, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(idleDir, "idle.bat"), []byte("@echo off\r\n"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := mergeInstallerSourceDir(installDir); err != nil {
		t.Fatalf("mergeInstallerSourceDir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(installDir, "Lib", "idlelib", "idle.bat")); err != nil {
		t.Fatalf("idle.bat not merged: %v", err)
	}
	if _, err := os.Stat(filepath.Join(installDir, "SourceDir")); !os.IsNotExist(err) {
		t.Fatal("SourceDir should be removed after merge")
	}
}

func TestResolveInstalledBinPathSourceDirFallback(t *testing.T) {
	root := t.TempDir()
	idle := filepath.Join(root, "SourceDir", "Lib", "idlelib", "idle.bat")
	if err := os.MkdirAll(filepath.Dir(idle), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(idle, []byte("@echo off\r\n"), 0644); err != nil {
		t.Fatal(err)
	}
	path, rel, ok := resolveInstalledBinPath(root, `Lib\idlelib\idle.bat`, "")
	if !ok {
		t.Fatal("expected SourceDir fallback to resolve idle.bat")
	}
	if rel != filepath.Join("SourceDir", "Lib", "idlelib", "idle.bat") {
		t.Fatalf("rel = %q", rel)
	}
	if path != idle {
		t.Fatalf("path = %q", path)
	}
}
