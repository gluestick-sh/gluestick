package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gluestick-sh/core/apps"
	"github.com/gluestick-sh/core/engine"
	"github.com/gluestick-sh/core/manifest"
)

func TestResetPackage_viaEngine(t *testing.T) {
	root := t.TempDir()
	pkgName := "node"
	pkgRoot := apps.PkgRoot(root, pkgName)
	for _, ver := range []string{"20.0.0", "22.0.0"} {
		dir := filepath.Join(pkgRoot, ver)
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "node.exe"), []byte(ver), 0644); err != nil {
			t.Fatal(err)
		}
		if err := apps.SaveInstallRecord(dir, "main", &manifest.Manifest{Version: ver, Bin: []interface{}{"node.exe"}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := apps.LinkCurrent(pkgRoot, "20.0.0"); err != nil {
		t.Fatal(err)
	}

	eng, err := engine.NewEngine(&engine.EngineConfig{RootDir: root})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	if err := eng.ResetPackage(pkgName + "@22.0.0"); err != nil {
		t.Fatalf("ResetPackage: %v", err)
	}
	info, err := eng.GetPackageVersions(pkgName)
	if err != nil {
		t.Fatal(err)
	}
	if info.ActiveVersion != "22.0.0" {
		t.Fatalf("active = %q, want 22.0.0", info.ActiveVersion)
	}
}
