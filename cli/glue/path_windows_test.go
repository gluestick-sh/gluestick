//go:build windows

package main

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestPathShowJSON(t *testing.T) {
	root := t.TempDir()
	setJSONTestFlags(t, root)

	out := captureStdout(t, func() {
		if err := pathShowCmd.RunE(pathShowCmd, nil); err != nil {
			t.Fatalf("path show: %v", err)
		}
	})

	var res struct {
		BinDir string `json:"bin_dir"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if want := filepath.Join(root, "shims"); res.BinDir != want {
		t.Fatalf("bin_dir = %q, want %q", res.BinDir, want)
	}
}

func TestPathCheckJSON(t *testing.T) {
	root := t.TempDir()
	setJSONTestFlags(t, root)
	binDir := filepath.Join(root, "shims")

	type checkResult struct {
		InPath              bool   `json:"in_path"`
		BinDir              string `json:"bin_dir"`
		StoreAliasShadowing bool   `json:"store_alias_shadowing"`
		OK                  bool   `json:"ok"`
	}

	t.Run("in path", func(t *testing.T) {
		t.Setenv("PATH", binDir)
		out := captureStdout(t, func() {
			if err := pathCheckCmd.RunE(pathCheckCmd, nil); err != nil {
				t.Fatalf("path check: %v", err)
			}
		})
		var res checkResult
		if err := json.Unmarshal([]byte(out), &res); err != nil {
			t.Fatalf("invalid JSON: %v\n%s", err, out)
		}
		if !res.InPath || res.StoreAliasShadowing || !res.OK {
			t.Fatalf("in_path/shadowing/ok = %v/%v/%v, want true/false/true\n%s",
				res.InPath, res.StoreAliasShadowing, res.OK, out)
		}
	})

	t.Run("not in path", func(t *testing.T) {
		t.Setenv("PATH", `C:\Windows\System32`)
		out := captureStdout(t, func() {
			err := pathCheckCmd.RunE(pathCheckCmd, nil)
			if err == nil {
				t.Fatal("expected failure when shims dir is not in PATH")
			}
			if code := exitCode(err); code != 1 {
				t.Fatalf("exitCode = %d, want 1", code)
			}
		})
		var res checkResult
		if err := json.Unmarshal([]byte(out), &res); err != nil {
			t.Fatalf("invalid JSON: %v\n%s", err, out)
		}
		if res.InPath || res.OK {
			t.Fatalf("in_path/ok = %v/%v, want false/false\n%s", res.InPath, res.OK, out)
		}
	})
}

func TestEnsureDirFirstInPathList(t *testing.T) {
	shims := `C:\Users\xuc\.glue\shims`
	apps := `C:\Users\xuc\AppData\Local\Microsoft\WindowsApps`

	t.Run("prepends when missing", func(t *testing.T) {
		got, changed := ensureDirFirstInPathList(apps, shims)
		if !changed {
			t.Fatal("expected change")
		}
		want := shims + ";" + apps
		if got != want {
			t.Fatalf("got %q want %q", got, want)
		}
	})

	t.Run("moves existing entry to front", func(t *testing.T) {
		current := apps + ";" + shims + `;C:\Windows`
		got, changed := ensureDirFirstInPathList(current, shims)
		if !changed {
			t.Fatal("expected change")
		}
		want := shims + ";" + apps + `;C:\Windows`
		if got != want {
			t.Fatalf("got %q want %q", got, want)
		}
	})

	t.Run("no change when already first", func(t *testing.T) {
		current := shims + ";" + apps
		got, changed := ensureDirFirstInPathList(current, shims)
		if changed {
			t.Fatalf("unexpected change: %q", got)
		}
	})
}

func TestPathDirPrecedes(t *testing.T) {
	shims := `C:\Users\xuc\.glue\shims`
	apps := `C:\Users\xuc\AppData\Local\Microsoft\WindowsApps`
	path := apps + ";" + shims
	if !pathDirPrecedes(path, apps, shims) {
		t.Fatal("WindowsApps should precede shims")
	}
	if pathDirPrecedes(path, shims, apps) {
		t.Fatal("shims should not precede WindowsApps")
	}
}
