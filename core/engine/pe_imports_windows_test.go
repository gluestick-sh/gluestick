//go:build windows

package engine

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDllAvailableOnSystem(t *testing.T) {
	if !dllAvailableOnSystem("NETAPI32.dll") {
		t.Fatal("expected NETAPI32.dll on system")
	}
	if dllAvailableOnSystem("libc++.dll") {
		t.Fatal("libc++.dll should not be treated as system")
	}
}

func TestMissingSameDirPeImports_goneovimARM64(t *testing.T) {
	exe := filepath.Join(os.TempDir(), "goneovim-full", "goneovim-v0.6.17-windows-arm64", "goneovim.exe")
	if _, err := os.Stat(exe); err != nil {
		t.Skip("extract goneovim arm64 zip to TEMP/goneovim-full to run this test")
	}
	missing, err := missingSameDirPeImports(exe)
	if err != nil {
		t.Fatalf("missingSameDirPeImports: %v", err)
	}
	found := false
	for _, dll := range missing {
		if dll == "libc++.dll" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected libc++.dll missing, got %v", missing)
	}
}

func TestMissingSameDirPeImports_goneovimX64(t *testing.T) {
	exe := filepath.Join(os.TempDir(), "goneovim-x64-full", "goneovim-v0.6.17-windows-x86_64", "goneovim.exe")
	if _, err := os.Stat(exe); err != nil {
		t.Skip("extract goneovim x64 zip to TEMP/goneovim-x64-full to run this test")
	}
	missing, err := missingSameDirPeImports(exe)
	if err != nil {
		t.Fatalf("missingSameDirPeImports: %v", err)
	}
	if len(missing) > 0 {
		t.Fatalf("unexpected missing DLLs: %v", missing)
	}
}
