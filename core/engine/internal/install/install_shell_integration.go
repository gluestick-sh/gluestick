package install

import (
	"fmt"

	"github.com/gluestick-sh/core/manifest"
)

// RefreshPackageShellIntegration rebuilds start-menu shortcuts and applies install-context.reg when present.
func RefreshPackageShellIntegration(installDir string, m *manifest.Manifest, installArch string) error {
	if err := repairSevenZipArm64Layout(installDir); err != nil {
		return fmt.Errorf("repair 7zip arm64 layout: %w", err)
	}
	if err := createPackageShortcuts(installDir, m, installArch); err != nil {
		return err
	}
	applyInstallContextRegistry(installDir)
	return nil
}
