package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestConfigSearchPaths_relativeToShimLocation verifies that a shim resolves its
// config relative to its own location, so isolated roots (e.g. ~/.glue-alpha)
// work without any environment setup.
func TestConfigSearchPaths_relativeToShimLocation(t *testing.T) {
	root := filepath.FromSlash(`C:/Users/me/.glue-alpha`)
	self := filepath.Join(root, "shims", "git.exe")

	t.Setenv("GLUE_DATA_ROOT", "")
	paths := configSearchPaths(self, "git")

	if len(paths) == 0 {
		t.Fatal("expected at least one candidate path")
	}
	want := filepath.Join(root, "shims-meta", "git.json")
	if paths[0] != want {
		t.Fatalf("paths[0] = %q, want %q (relative resolution must come first)", paths[0], want)
	}
}

// TestConfigSearchPaths_envOverride verifies GLUE_DATA_ROOT takes precedence.
func TestConfigSearchPaths_envOverride(t *testing.T) {
	override := filepath.FromSlash(`C:/data/portable-root`)
	t.Setenv("GLUE_DATA_ROOT", override)

	paths := configSearchPaths(filepath.FromSlash(`C:/somewhere/shims/git.exe`), "git")
	want := filepath.Join(override, "shims-meta", "git.json")
	if len(paths) == 0 || paths[0] != want {
		t.Fatalf("paths[0] = %v, want %q", paths, want)
	}
}

// TestLoadShimConfig_fallsBackAcrossPaths verifies the first existing config wins.
func TestLoadShimConfig_fallsBackAcrossPaths(t *testing.T) {
	root := t.TempDir()
	metaDir := filepath.Join(root, "shims-meta")
	if err := os.MkdirAll(metaDir, 0755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(metaDir, "git.json")
	if err := os.WriteFile(cfgPath, []byte(`{"name":"git","command":"C:/tools/git/git.exe","path":"C:/tools/git/git.exe"}`), 0644); err != nil {
		t.Fatal(err)
	}

	missing := filepath.Join(t.TempDir(), "shims-meta", "git.json")
	cfg, err := loadShimConfig([]string{missing, cfgPath}, "git")
	if err != nil {
		t.Fatalf("loadShimConfig: %v", err)
	}
	if cfg.Command != "C:/tools/git/git.exe" {
		t.Fatalf("command = %q", cfg.Command)
	}
}

// TestLoadShimConfig_noneFound verifies the error when no candidate config exists.
func TestLoadShimConfig_noneFound(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "shims-meta", "git.json")
	if _, err := loadShimConfig([]string{missing}, "git"); err == nil {
		t.Fatal("expected error when no shim config exists")
	}
}
