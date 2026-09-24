package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestListInstalledPackages_usesAppsDirNotStaleCache(t *testing.T) {
	root := t.TempDir()
	storeDir := filepath.Join(root, "store")
	if err := os.MkdirAll(storeDir, 0755); err != nil {
		t.Fatal(err)
	}

	gitDir := filepath.Join(root, "apps", "git", "2.0.0")
	if err := os.MkdirAll(gitDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "git.exe"), []byte("git"), 0644); err != nil {
		t.Fatal(err)
	}

	e, err := NewEngine(&EngineConfig{RootDir: root})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	if err := e.Cache.Add("vim", "9.1.0000", map[string]string{"abc": "vim.exe"}, 1024); err != nil {
		t.Fatal(err)
	}

	pkgs, err := e.listInstalledPackages(context.Background(), ListOptions{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(pkgs) != 0 {
		t.Fatalf("len(packages) = %d, want 0 (incomplete disk-only and cache-only rows excluded)", len(pkgs))
	}
}

func TestListInstalledPackages_listsRegisteredInstall(t *testing.T) {
	root := t.TempDir()
	storeDir := filepath.Join(root, "store")
	if err := os.MkdirAll(storeDir, 0755); err != nil {
		t.Fatal(err)
	}

	gitDir := filepath.Join(root, "apps", "git", "2.0.0")
	if err := os.MkdirAll(gitDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "git.exe"), []byte("git"), 0644); err != nil {
		t.Fatal(err)
	}

	e, err := NewEngine(&EngineConfig{RootDir: root})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	if err := e.Cache.Add("git", "2.0.0", map[string]string{"abc": "git.exe"}, 1024); err != nil {
		t.Fatal(err)
	}
	if err := e.Cache.AddInstalled("git", "2.0.0", gitDir, 1024, nil); err != nil {
		t.Fatal(err)
	}

	pkgs, err := e.listInstalledPackages(context.Background(), ListOptions{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(pkgs) != 1 || pkgs[0].Name != "git" {
		t.Fatalf("packages = %+v, want git only", pkgs)
	}
}
