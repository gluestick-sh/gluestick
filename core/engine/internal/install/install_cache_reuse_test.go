package install

import (
	"strings"
	"testing"

	"github.com/gluestick-sh/core/cache"
	"github.com/gluestick-sh/core/store"
)

func TestCacheArtifactConflictsWithDownload_godotArchMismatch(t *testing.T) {
	dl := "Godot_v4.6.3-stable_win64.exe.zip"
	if !cacheArtifactConflictsWithDownload("Godot_v4.6.3-stable_windows_arm64_console.exe", dl) {
		t.Fatal("arm64 artifact should conflict with win64 download")
	}
	if cacheArtifactConflictsWithDownload("Godot_v4.6.3-stable_win64.exe", dl) {
		t.Fatal("win64 artifact should not conflict with win64 download")
	}
}

func TestCacheReusableForInstall_rejectsWrongArchIndex(t *testing.T) {
	store, err := store.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store.Prereqs()

	hash, err := store.Write(strings.NewReader("arm64"))
	if err != nil {
		t.Fatal(err)
	}
	entry := &cache.PackageEntry{
		Version: "4.6.3",
		Files: map[string]string{
			hash: "Godot_v4.6.3-stable_windows_arm64_console.exe",
		},
	}
	if cacheReusableForInstall(store, entry, "4.6.3", "Godot_v4.6.3-stable_win64.exe.zip", "") {
		t.Fatal("expected arch mismatch to fail reuse")
	}
}
