//go:build windows

package manifest

import "testing"

func TestHostArchitectureWoW64OnARM64(t *testing.T) {
	t.Setenv("PROCESSOR_ARCHITECTURE", "AMD64")
	t.Setenv("PROCESSOR_ARCHITEW6432", "ARM64")
	if got := hostArchitecture(); got != ArchARM64 {
		t.Fatalf("hostArchitecture() = %q, want arm64", got)
	}
}
