package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/gluestick-sh/core/shim"
)

func TestUninstallOrphanShimsOnly(t *testing.T) {
	root := t.TempDir()
	eng, err := NewEngine(&EngineConfig{RootDir: root})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	defer eng.Close()

	pkgName := "comet"
	shimsMetaDir := filepath.Join(root, "shims-meta")
	shimsDir := filepath.Join(root, "shims")
	for _, dir := range []string{shimsMetaDir, shimsDir} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}

	cfg := shim.Config{
		Name:    pkgName,
		Command: filepath.Join(root, "apps", pkgName, "current", "comet.exe"),
		Path:    filepath.Join(root, "apps", pkgName, "current", "comet.exe"),
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

	if _, err := eng.Uninstall(context.Background(), &UninstallRequest{
		Request: Request{Name: pkgName},
	}, nil); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}

	if _, err := os.Stat(filepath.Join(shimsMetaDir, pkgName+".json")); !os.IsNotExist(err) {
		t.Fatalf("shim meta still exists: %v", err)
	}
	if _, err := os.Stat(filepath.Join(shimsDir, pkgName+".exe")); !os.IsNotExist(err) {
		t.Fatalf("shim exe still exists: %v", err)
	}
}
