package uninstall

import (
	"testing"
)

func TestUninstallHooksNeedSettleTime(t *testing.T) {
	if !uninstallHooksNeedSettleTime([]string{"Stop-Service -Name Tailscale -Force"}) {
		t.Fatal("Stop-Service should need settle time")
	}
	if !uninstallHooksNeedSettleTime([]string{"tailscaled.exe uninstall-system-daemon"}) {
		t.Fatal("uninstall-system-daemon should need settle time")
	}
	if uninstallHooksNeedSettleTime([]string{"reg import \"$dir\\remove.reg\""}) {
		t.Fatal("reg import alone should not need settle time")
	}
}
