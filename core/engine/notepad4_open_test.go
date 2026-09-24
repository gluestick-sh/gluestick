package engine

import (
	"github.com/gluestick-sh/core/engine/internal/launch"
	"github.com/gluestick-sh/core/engine/internal/runtime"

	"os"
	"path/filepath"
	"testing"
)

func TestNotepad4EffectiveLaunchKindOpenable(t *testing.T) {
	root := os.Getenv("GLUE_ROOT")
	if root == "" {
		root = filepath.Join(os.Getenv("USERPROFILE"), ".glue")
	}
	installDir := filepath.Join(root, "apps", "notepad4", "current")
	exe := filepath.Join(installDir, "Notepad4.exe")
	if _, err := os.Stat(exe); err != nil {
		t.Skip("notepad4 not installed")
	}
	e := &Engine{Engine: &runtime.Engine{Config: &EngineConfig{RootDir: root}}}
	for _, rel := range []string{"Notepad4.exe", "notepad4.exe"} {
		kind, ok := launch.LaunchIndexKind(e.Engine, "notepad4", rel)
		if !ok {
			t.Fatalf("%s: no launch index entry", rel)
		}
		if !kind.Openable() {
			t.Fatalf("%s: kind %q is not openable", rel, kind)
		}
	}
	kind := launch.EffectiveLaunchKind(e.Engine, "notepad4", installDir, exe, nil, LaunchSourceBin)
	if !kind.Openable() {
		t.Fatalf("effectiveLaunchKind: got %q", kind)
	}
}
