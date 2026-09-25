package main

import (
	"bytes"
	"context"
	"encoding/json"
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

// TestRunListJSON_emptyPackagesIsArray pins the JSON contract for an empty
// install: `packages` must be [] rather than null, so agents can iterate it
// without a nil check (all other commands already emit []).
func TestRunListJSON_emptyPackagesIsArray(t *testing.T) {
	root := t.TempDir()
	setJSONTestFlags(t, root)
	listCmd.SetContext(context.Background())
	t.Cleanup(func() { listCmd.SetContext(nil) })

	out := captureStdout(t, func() {
		if err := runList(listCmd, nil); err != nil {
			t.Fatalf("list: %v", err)
		}
	})

	var res struct {
		Packages []json.RawMessage `json:"packages"`
		Count    int               `json:"count"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if res.Count != 0 {
		t.Fatalf("count = %d, want 0", res.Count)
	}
	if res.Packages == nil {
		t.Fatalf("packages must be an empty array, not null: %s", out)
	}
	if len(res.Packages) != 0 {
		t.Fatalf("packages = %s, want []", out)
	}
}

// TestRunListAllJSON_emptyPackagesIsArray is the `list --all` counterpart.
func TestRunListAllJSON_emptyPackagesIsArray(t *testing.T) {
	root := t.TempDir()
	setJSONTestFlags(t, root)
	listCmd.SetContext(context.Background())
	t.Cleanup(func() { listCmd.SetContext(nil) })

	oldAll := listAll
	listAll = true
	t.Cleanup(func() { listAll = oldAll })

	out := captureStdout(t, func() {
		if err := runList(listCmd, nil); err != nil {
			t.Fatalf("list --all: %v", err)
		}
	})

	var res struct {
		Packages    []json.RawMessage `json:"packages"`
		Count       int               `json:"count"`
		AllVersions bool              `json:"allVersions"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if !res.AllVersions || res.Count != 0 {
		t.Fatalf("allVersions/count = %v/%d, want true/0\n%s", res.AllVersions, res.Count, out)
	}
	if res.Packages == nil || len(res.Packages) != 0 {
		t.Fatalf("packages must be an empty array, not null: %s", out)
	}
}
