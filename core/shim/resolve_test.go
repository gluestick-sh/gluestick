package shim

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveShimStubPath_cachedInGlueRoot(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "shim-resolve-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	stub := filepath.Join(tmpDir, "shim.exe")
	if err := os.WriteFile(stub, []byte("stub"), 0755); err != nil {
		t.Fatal(err)
	}

	got := resolveShimStubPath(tmpDir)
	if got != stub {
		t.Fatalf("resolveShimStubPath = %q, want %q", got, stub)
	}
}

func TestResolveShimStubPath_devLayout(t *testing.T) {
	repoRoot, err := os.MkdirTemp("", "shim-repo-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(repoRoot)

	stub := filepath.Join(repoRoot, "shim", "shim.exe")
	if err := os.MkdirAll(filepath.Dir(stub), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stub, []byte("stub"), 0755); err != nil {
		t.Fatal(err)
	}

	origWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(repoRoot); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origWd)

	got := resolveShimStubPath("")
	if got != stub {
		t.Fatalf("resolveShimStubPath from repo root = %q, want %q", got, stub)
	}
}

func TestWalkUpShimStub_devLayout(t *testing.T) {
	repoRoot, err := os.MkdirTemp("", "shim-repo-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(repoRoot)

	stub := filepath.Join(repoRoot, "shim", "shim.exe")
	if err := os.MkdirAll(filepath.Dir(stub), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stub, []byte("stub"), 0755); err != nil {
		t.Fatal(err)
	}

	nested := filepath.Join(repoRoot, "cli", "glue")
	if err := os.MkdirAll(nested, 0755); err != nil {
		t.Fatal(err)
	}

	want := filepath.Clean(stub)
	for _, c := range walkUpShimStub(nested) {
		if filepath.Clean(c) == want {
			return
		}
	}
	t.Fatalf("walkUpShimStub(%q) did not include %q", nested, stub)
}

func TestCacheShimStub(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "shim-cache-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	srcDir, err := os.MkdirTemp("", "shim-src-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(srcDir)

	src := filepath.Join(srcDir, "shim.exe")
	if err := os.WriteFile(src, []byte("stub-bytes"), 0755); err != nil {
		t.Fatal(err)
	}

	if err := cacheShimStub(tmpDir, src); err != nil {
		t.Fatal(err)
	}

	dest := filepath.Join(tmpDir, "shim.exe")
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "stub-bytes" {
		t.Fatalf("cached stub content = %q", data)
	}
}
