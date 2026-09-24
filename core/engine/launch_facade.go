package engine

import "github.com/gluestick-sh/core/engine/internal/launch"

// These aliases re-export the launch package's public types.
type (
	LaunchTarget    = launch.LaunchTarget
	LaunchCandidate = launch.LaunchCandidate
	LaunchKind      = launch.LaunchKind
	LaunchSource    = launch.LaunchSource
)

// These constants re-export the launch kind and launch source values.
const (
	LaunchKindConsole = launch.LaunchKindConsole
	LaunchKindGUI     = launch.LaunchKindGUI
	LaunchKindSkip    = launch.LaunchKindSkip

	LaunchSourceShortcut = launch.LaunchSourceShortcut
	LaunchSourceBin      = launch.LaunchSourceBin
	LaunchSourceScan     = launch.LaunchSourceScan
)

// ListLaunchCandidates returns all discovered executables and effective launch kinds.
func (e *Engine) ListLaunchCandidates(pkgName string) ([]LaunchCandidate, error) {
	return launch.ListLaunchCandidates(e.Engine, pkgName)
}

// ListLaunchTargets returns executables the user can open (not marked hidden).
func (e *Engine) ListLaunchTargets(pkgName string) ([]LaunchTarget, error) {
	return launch.ListLaunchTargets(e.Engine, pkgName)
}

// OpenLaunchTarget runs an exe/bat/cmd that belongs to pkgName.
func (e *Engine) OpenLaunchTarget(pkgName, absPath string) error {
	return launch.OpenLaunchTarget(e.Engine, pkgName, absPath)
}

// PackageIconPath returns the executable whose embedded icon should represent the package.
func (e *Engine) PackageIconPath(pkgName string) (string, error) {
	return launch.PackageIconPath(e.Engine, pkgName)
}

// PackageInstallDir returns the active install directory for pkgName.
func (e *Engine) PackageInstallDir(pkgName string) (string, error) {
	return launch.PackageInstallDir(e.Engine, pkgName)
}

// SetLaunchPreference sets how a launcher is opened (gui, console, skip, or auto).
func (e *Engine) SetLaunchPreference(pkgName, relPath, kind string) error {
	return launch.SetLaunchPreference(e.Engine, pkgName, relPath, kind)
}

// SetLaunchPreferences applies multiple launch preference updates in one atomic write.
func (e *Engine) SetLaunchPreferences(pkgName string, updates map[string]string) error {
	return launch.SetLaunchPreferences(e.Engine, pkgName, updates)
}

// RemoveLaunchEntry drops a discovered launcher from the open-program menu.
func (e *Engine) RemoveLaunchEntry(pkgName, relPath string) error {
	return launch.RemoveLaunchEntry(e.Engine, pkgName, relPath)
}

// AddLaunchEntry restores a launcher to the open-program menu.
func (e *Engine) AddLaunchEntry(pkgName, relPath, kind string) error {
	return launch.AddLaunchEntry(e.Engine, pkgName, relPath, kind)
}
