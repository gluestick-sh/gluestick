package install

import (
	"strings"
	"testing"

	etypes "github.com/gluestick-sh/core/engine/types"
)

func TestAdaptInstallerScriptForInteractive_Cygwin(t *testing.T) {
	hooks := []string{
		"$install_args = @(",
		"    '--site', 'https://mirrors.kernel.org/sourceware/cygwin/'",
		"    '--no-admin', '--no-shortcuts', '--quiet-mode', '--upgrade-also'",
		"    '--local-package-dir', \"$persist_dir\\packages\", '--root', \"$persist_dir\\root\"",
		")",
		"Start-Process -FilePath \"$dir\\cygwin-setup.exe\" -ArgumentList $install_args -WindowStyle 'Hidden' -Wait",
	}
	got := adaptInstallerScriptForInteractive(hooks)
	joined := strings.Join(got, "\n")
	if strings.Contains(joined, "quiet-mode") {
		t.Fatalf("quiet-mode should be removed:\n%s", joined)
	}
	if strings.Contains(strings.ToLower(joined), "windowstyle") {
		t.Fatalf("WindowStyle Hidden should be removed:\n%s", joined)
	}
	if !strings.Contains(joined, "Start-Process") {
		t.Fatal("Start-Process should remain")
	}
}

func TestInstallInteractive(t *testing.T) {
	if installInteractive(nil) {
		t.Fatal("nil request should be unattended")
	}
	req := &etypes.InstallRequest{Request: etypes.Request{Options: map[string]string{"interactive": "true"}}}
	if !installInteractive(req) {
		t.Fatal("expected interactive")
	}
}
