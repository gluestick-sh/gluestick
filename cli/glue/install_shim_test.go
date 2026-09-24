package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gluestick-sh/core/manifest"
	"github.com/gluestick-sh/core/shim"
)

func TestCreatePackageShims_missingEnvAddPathDir(t *testing.T) {
	root := t.TempDir()
	installDir := filepath.Join(root, "pnpm", "11.4.0")
	if err := os.MkdirAll(installDir, 0755); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(installDir, "pnpm.exe")
	if err := os.WriteFile(exe, []byte("@echo off\r\n"), 0755); err != nil {
		t.Fatal(err)
	}

	shimMgr, err := shim.NewManager(root)
	if err != nil {
		t.Fatal(err)
	}

	m := &manifest.Manifest{
		Bin:        []interface{}{"pnpm.exe"},
		EnvAddPath: "bin",
	}

	if err := createPackageShims(shimMgr, filepath.Join(root, "shims-meta"), "pnpm", installDir, installDir, m); err != nil {
		t.Fatalf("createPackageShims: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "shims-meta", "pnpm.json")); err != nil {
		t.Fatalf("pnpm shim config: %v", err)
	}
}
