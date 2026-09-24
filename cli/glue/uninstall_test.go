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

// TestUninstallJSON_errorCode verifies the structured error contract in JSON mode:
// uninstalling a missing package exits 1 with a non-empty error and code.
func TestUninstallJSON_errorCode(t *testing.T) {
	root := t.TempDir()
	setJSONTestFlags(t, root)
	uninstallCmd.SetContext(context.Background())

	out := captureStdout(t, func() {
		err := runUninstall(uninstallCmd, []string{"definitely-missing"})
		if err == nil {
			t.Fatal("expected failure for missing package")
		}
		if code := exitCode(err); code != 1 {
			t.Fatalf("exitCode = %d, want 1", code)
		}
	})

	var res struct {
		Command string `json:"command"`
		OK      bool   `json:"ok"`
		Results []struct {
			Ref   string `json:"ref"`
			Error string `json:"error"`
			Code  string `json:"code"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if res.Command != "uninstall" || res.OK {
		t.Fatalf("command/ok = %q/%v, want uninstall/false\n%s", res.Command, res.OK, out)
	}
	if len(res.Results) != 1 {
		t.Fatalf("results len = %d, want 1", len(res.Results))
	}
	item := res.Results[0]
	if item.Error == "" {
		t.Fatalf("expected non-empty error: %+v\n%s", item, out)
	}
	if item.Code != "package_not_installed" {
		t.Fatalf("code = %q, want package_not_installed\n%s", item.Code, out)
	}
}
