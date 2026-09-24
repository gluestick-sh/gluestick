package install

import (
	"github.com/gluestick-sh/core/engine/internal/runtime"
	etypes "github.com/gluestick-sh/core/engine/types"

	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestIsDarkHelperPackage(t *testing.T) {
	if !isDarkHelperPackage("dark") || !isDarkHelperPackage("wix") || !isDarkHelperPackage(" Dark ") {
		t.Fatal("expected dark/wix to be helper packages")
	}
	if isDarkHelperPackage("python") {
		t.Fatal("python should not be a dark helper package")
	}
}

func TestEnsureDarkWithProfSkipsWhenNotNeeded(t *testing.T) {
	e := &runtime.Engine{Config: &etypes.EngineConfig{RootDir: t.TempDir()}}
	path, err := ensureDarkWithProf(e, context.Background(), nil, []string{"Expand-7zipArchive -Path $dir\\$fname"}, "python")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if path != "" {
		t.Fatalf("expected empty path, got %q", path)
	}
}

func TestDarkExecutableReady(t *testing.T) {
	root := t.TempDir()
	lone := filepath.Join(root, "dark.exe")
	if err := os.WriteFile(lone, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if darkExecutableReady(lone) {
		t.Fatal("dark.exe without wix.dll should not be ready")
	}
	wixDir := filepath.Join(root, "wix")
	if err := os.MkdirAll(wixDir, 0755); err != nil {
		t.Fatal(err)
	}
	ready := filepath.Join(wixDir, "dark.exe")
	for _, name := range []string{"dark.exe", "wix.dll"} {
		if err := os.WriteFile(filepath.Join(wixDir, name), []byte(name), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if !darkExecutableReady(ready) {
		t.Fatal("dark.exe with wix.dll should be ready")
	}
}
