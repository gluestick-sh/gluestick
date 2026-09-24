//go:build windows

package main

import "testing"

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
