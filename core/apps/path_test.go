package apps

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseInstallFilePath(t *testing.T) {
	root := t.TempDir()
	pkgRoot := PkgRoot(root, "vim")
	installDir := filepath.Join(pkgRoot, "9.2")
	if err := os.MkdirAll(installDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := LinkCurrent(pkgRoot, "9.2"); err != nil {
		t.Fatal(err)
	}

	exe := filepath.Join(installDir, "vim.exe")
	pkg, ver := ParseInstallFilePath(exe)
	if pkg != "vim" || ver != "9.2" {
		t.Fatalf("ParseInstallFilePath(%q) = (%q, %q), want (vim, 9.2)", exe, pkg, ver)
	}

	currentExe := filepath.Join(pkgRoot, CurrentLinkName, "vim.exe")
	if _, err := ReadCurrent(pkgRoot); err != nil {
		t.Skipf("ReadCurrent unavailable in test env: %v", err)
	}
	pkg, ver = ParseInstallFilePath(currentExe)
	if pkg != "vim" || ver != "9.2" {
		t.Fatalf("current path = (%q, %q), want (vim, 9.2)", pkg, ver)
	}

	// Segment "apps" in a parent dir name must not match.
	decoy := filepath.Join(root, "myapps", "vim", "9.2", "vim.exe")
	if pkg, ver := ParseInstallFilePath(decoy); pkg != "" || ver != "" {
		t.Fatalf("decoy path = (%q, %q), want empty", pkg, ver)
	}
}
