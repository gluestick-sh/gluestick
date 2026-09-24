package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gluestick-sh/core/apps"
)

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	oldOut := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	fn()
	w.Close()
	os.Stdout = oldOut

	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func TestRunListAllVersions_countsPackagesNotVersions(t *testing.T) {
	root := t.TempDir()
	appsDir := filepath.Join(root, "apps")
	if err := os.MkdirAll(appsDir, 0755); err != nil {
		t.Fatal(err)
	}

	// pnpm: two versions — counts as one package.
	pnpmRoot := apps.PkgRoot(root, "pnpm")
	for _, ver := range []string{"11.7.0", "11.6.0"} {
		verDir := filepath.Join(pnpmRoot, ver)
		if err := os.MkdirAll(verDir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(verDir, "stub"), nil, 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := apps.LinkCurrent(pnpmRoot, "11.7.0"); err != nil {
		t.Fatal(err)
	}

	// vim: one version.
	vimRoot := apps.PkgRoot(root, "vim")
	vimDir := filepath.Join(vimRoot, "9.2.0663")
	if err := os.MkdirAll(vimDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vimDir, "stub"), nil, 0644); err != nil {
		t.Fatal(err)
	}
	if err := apps.LinkCurrent(vimRoot, "9.2.0663"); err != nil {
		t.Fatal(err)
	}

	// freecad: empty package dir — should not be counted.
	if err := os.MkdirAll(apps.PkgRoot(root, "freecad"), 0755); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		if err := runListAllVersions(root, nil); err != nil {
			t.Fatal(err)
		}
	})

	if !strings.Contains(out, "2 packages installed") {
		t.Fatalf("expected package count 2 (not versions or empty dirs), got:\n%s", out)
	}
}
