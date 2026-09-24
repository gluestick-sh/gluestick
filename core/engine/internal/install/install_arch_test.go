package install

import (
	"testing"

	etypes "github.com/gluestick-sh/core/engine/types"
	"github.com/gluestick-sh/core/manifest"
)

func TestInstallArchitectureOverride(t *testing.T) {
	m := &manifest.Manifest{
		Architecture: map[string]interface{}{
			manifest.Arch64bit: map[string]interface{}{
				"url": "https://example.com/x64.exe",
			},
			manifest.ArchARM64: map[string]interface{}{
				"url": "https://example.com/arm64.exe",
			},
		},
	}
	req := &etypes.InstallRequest{
		Request: etypes.Request{Options: map[string]string{"architecture": manifest.Arch64bit}},
	}
	if got := installArchitecture(req, m); got != manifest.Arch64bit {
		t.Fatalf("got %q want 64bit", got)
	}
	if urls := m.GetURLsForInstall(installArchitecture(req, m)); len(urls) != 1 || urls[0] != "https://example.com/x64.exe" {
		t.Fatalf("urls = %v", urls)
	}
	if !architectureExplicit(req) {
		t.Fatal("expected explicit architecture")
	}
	if architectureExplicit(&etypes.InstallRequest{}) {
		t.Fatal("empty request is not explicit")
	}
}
