package install

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestScoopMoveFlattenInstallDir(t *testing.T) {
	dir := t.TempDir()
	wrapper := filepath.Join(dir, "FreeCAD_1.1.1")
	if err := os.MkdirAll(filepath.Join(wrapper, "bin"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(wrapper, "Mod"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wrapper, "bin", "FreeCAD.exe"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	prefix, err := scoopMoveFlattenInstallDir(dir)
	if err != nil {
		t.Fatalf("scoopMoveFlattenInstallDir: %v", err)
	}
	if prefix != "FreeCAD_1.1.1/" {
		t.Fatalf("prefix = %q", prefix)
	}
	if _, err := os.Stat(filepath.Join(dir, "bin", "FreeCAD.exe")); err != nil {
		t.Fatalf("flattened exe missing: %v", err)
	}
	if installNeedsMoveFlatten(dir) {
		t.Fatal("expected flatten to complete")
	}
}

func TestInstallNeedsMoveFlattenSingleWrapperWithLooseFile(t *testing.T) {
	dir := t.TempDir()
	wrapper := filepath.Join(dir, "FreeCAD_1.1.1")
	if err := os.MkdirAll(filepath.Join(wrapper, "bin"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wrapper, "readme.txt"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if !installNeedsMoveFlatten(dir) {
		t.Fatal("expected flatten when single wrapper has loose files")
	}
}

func TestInstallNeedsMoveFlattenSkipsBinOnlyLayout(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0755); err != nil {
		t.Fatal(err)
	}
	if installNeedsMoveFlatten(dir) {
		t.Fatal("bin-only layout should not flatten")
	}
}

func TestPatchInstallHookPaths(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "FreeCAD_1.1.1", "bin")
	if err := os.MkdirAll(nested, 0755); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(nested, "FreeCAD.exe")
	if err := os.WriteFile(exe, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	hooks := []string{
		`startmenu_shortcut "$original_dir\bin\FreeCAD.exe"`,
		`shim "$original_dir\bin\FreeCADCmd.exe" $global 'FreeCADCmd'`,
	}
	patched := patchInstallHookPaths(dir, hooks)
	if !strings.Contains(patched[0], exe) {
		t.Fatalf("shortcut hook = %q, want path %q", patched[0], exe)
	}
}

func TestEnsureScoopMoveFlattenInstallDir(t *testing.T) {
	dir := t.TempDir()
	wrapper := filepath.Join(dir, "FreeCAD_1.1.1")
	if err := os.MkdirAll(filepath.Join(wrapper, "bin"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wrapper, "bin", "FreeCAD.exe"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{"h1": "FreeCAD_1.1.1/bin/FreeCAD.exe"}
	if err := ensureScoopMoveFlattenInstallDir(dir, files); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "bin", "FreeCAD.exe")); err != nil {
		t.Fatalf("flattened exe missing: %v", err)
	}
	if files["h1"] != "bin/FreeCAD.exe" {
		t.Fatalf("remapped path = %q", files["h1"])
	}
}

func TestPostInstallNeedsFileIndexRefresh(t *testing.T) {
	if postInstallNeedsFileIndexRefresh([]string{`shim "$dir\bin\tool.exe" $global 'tool'`}) {
		t.Fatal("shim-only hook should not require refresh")
	}
	if !postInstallNeedsFileIndexRefresh([]string{`Set-Content "$dir\cfg.ini" "x"`}) {
		t.Fatal("Set-Content should require refresh")
	}
}

func TestRemapInstalledFilePaths(t *testing.T) {
	files := map[string]string{
		"h1": "FreeCAD/bin/FreeCAD.exe",
		"h2": "FreeCAD/Mod/x",
	}
	remapInstalledFilePaths(files, "FreeCAD/")
	if files["h1"] != "bin/FreeCAD.exe" {
		t.Fatalf("h1 = %q", files["h1"])
	}
}
