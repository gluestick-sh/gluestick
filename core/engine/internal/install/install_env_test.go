package install

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/gluestick-sh/core/manifest"
	"github.com/gluestick-sh/core/shim"
)

func TestCreatePackageShims_binDefaultArgs(t *testing.T) {
	root := t.TempDir()
	installDir := filepath.Join(root, "apps", "git", "2.0.0")
	if err := os.MkdirAll(installDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(installDir, "git.exe"), []byte("git"), 0755); err != nil {
		t.Fatal(err)
	}

	shimMgr, err := shim.NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	shimsMetaDir := filepath.Join(root, "shims-meta")
	stub := filepath.Join(root, "shim.exe")
	if err := os.WriteFile(stub, []byte("stub"), 0755); err != nil {
		t.Fatal(err)
	}

	m := &manifest.Manifest{
		Bin: []interface{}{"[git.exe,git,--version]"},
		Env: map[string]string{"GIT_CONFIG_GLOBAL": "$dir\\etc\\gitconfig"},
	}
	if err := createPackageShims(shimMgr, shimsMetaDir, "git", installDir, installDir, m); err != nil {
		t.Fatalf("createPackageShims: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(shimsMetaDir, "git.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg shim.Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Args) != 1 || cfg.Args[0] != "--version" {
		t.Fatalf("cfg.Args = %v, want [--version]", cfg.Args)
	}
	wantEnv := filepath.Join(installDir, "etc", "gitconfig")
	if cfg.Env["GIT_CONFIG_GLOBAL"] != wantEnv {
		t.Fatalf("cfg.Env[GIT_CONFIG_GLOBAL] = %q, want %q", cfg.Env["GIT_CONFIG_GLOBAL"], wantEnv)
	}
}

func TestExpandManifestEnvValue(t *testing.T) {
	got := ExpandManifestEnvValue(`$dir\bin`, `C:\apps\pkg\1.0`)
	if got != `C:\apps\pkg\1.0\bin` {
		t.Fatalf("ExpandManifestEnvValue() = %q", got)
	}
}
