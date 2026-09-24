package engine

import (
	"strings"
	"testing"

	"github.com/gluestick-sh/core/manifest"
)

func TestTailscaleManifestPreUninstallHooks(t *testing.T) {
	raw := `{
		"version": "1.98.4",
		"url": "https://example.com/tailscale.msi",
		"hash": "sha256:0000000000000000000000000000000000000000000000000000000000000000",
		"pre_uninstall": [
			"if (!(is_admin)) { error 'Admin rights are required to uninstall'; break }",
			"Stop-Process -Name 'tailscale-ipn' -Force -ErrorAction SilentlyContinue | Out-Null",
			"Stop-Service -Name 'Tailscale' -Force -ErrorAction SilentlyContinue | Out-Null"
		],
		"uninstaller": {
			"script": [
				"tailscaled.exe uninstall-system-daemon",
				"if ($cmd -eq 'uninstall') { reg import \"$dir\\remove-startup.reg\" }"
			]
		}
	}`
	m, err := manifest.Parse(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(m.PreUninstallHooks()) != 3 {
		t.Fatalf("pre_uninstall = %#v", m.PreUninstallHooks())
	}
	if len(m.UninstallerScriptHooks()) != 2 {
		t.Fatalf("uninstaller.script = %#v", m.UninstallerScriptHooks())
	}
}
