package launch

import (
	"os"
	"testing"
)

func TestLaunchIndexKindPrefersOpenableOverSkip(t *testing.T) {
	root := t.TempDir()
	path := launchIndexPath(root)
	if err := os.MkdirAll(root, 0755); err != nil {
		t.Fatal(err)
	}
	raw := `{"packages":{"notepad4":{"Notepad4.exe":"gui","notepad4.exe":"skip","matepath.exe":"skip"}}}`
	if err := os.WriteFile(path, []byte(raw), 0644); err != nil {
		t.Fatal(err)
	}
	idx, err := loadLaunchIndex(path)
	if err != nil {
		t.Fatal(err)
	}
	kind, ok := idx.kind("notepad4", "Notepad4.exe")
	if !ok || kind != LaunchKindGUI {
		t.Fatalf("Notepad4.exe: got (%q, %v), want gui", kind, ok)
	}
	kind, ok = idx.kind("notepad4", "notepad4.exe")
	if !ok || kind != LaunchKindGUI {
		t.Fatalf("notepad4.exe: got (%q, %v), want gui", kind, ok)
	}
}
