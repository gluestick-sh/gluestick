package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/gluestick-sh/core/engine"
	"github.com/gluestick-sh/core/shim"
)

func TestUninstall_orphanShimsOnly(t *testing.T) {
	root := t.TempDir()
	eng, err := engine.NewEngine(&engine.EngineConfig{RootDir: root})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	pkgName := "orphan"
	shimsMetaDir := filepath.Join(root, "shims-meta")
	shimsDir := filepath.Join(root, "shims")
	for _, dir := range []string{shimsMetaDir, shimsDir} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}

	cfg := shim.Config{
		Name: pkgName,
		Path: filepath.Join(root, "apps", pkgName, "current", pkgName+".exe"),
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(shimsMetaDir, pkgName+".json"), data, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(shimsDir, pkgName+".exe"), []byte("stub"), 0644); err != nil {
		t.Fatal(err)
	}

	_, err = eng.Uninstall(context.Background(), &engine.UninstallRequest{
		Request: engine.Request{Name: pkgName},
	}, engine.NewSilentReporter())
	if err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if _, err := os.Stat(filepath.Join(shimsMetaDir, pkgName+".json")); !os.IsNotExist(err) {
		t.Fatalf("shim meta still exists: %v", err)
	}
}
