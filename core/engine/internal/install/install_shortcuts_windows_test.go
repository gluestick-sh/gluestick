//go:build windows

package install

import (
	"path/filepath"
	"testing"
)

func TestShortcutLinkPath(t *testing.T) {
	got := shortcutLinkPath(`C:\menu\Glue Apps`, `7-Zip\7-Zip File Manager`)
	want := filepath.Join(`C:\menu\Glue Apps`, `7-Zip`, `7-Zip File Manager.lnk`)
	if got != want {
		t.Fatalf("shortcutLinkPath = %q, want %q", got, want)
	}
}
