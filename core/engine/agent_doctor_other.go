//go:build !windows

package engine

// bootstrapPossible reports whether missing install helpers (git, 7-Zip) can be
// downloaded automatically; Glue bootstraps them on Windows only.
func bootstrapPossible() bool { return false }

// WindowsAppsDir returns "" on non-Windows platforms (Store aliases do not exist).
func WindowsAppsDir() string { return "" }

// StoreAliasShadowsShims is always false on non-Windows platforms.
func StoreAliasShadowsShims(string) bool { return false }

// agentOSInfo reports no version on non-Windows builds; the check skips.
func agentOSInfo() (string, int) { return "", 0 }

// agentOutputCodepage is unavailable outside Windows; the check skips.
func agentOutputCodepage() (int, bool) { return 0, false }

// agentPowerShellExecutionPolicy is unavailable outside Windows; the check skips.
func agentPowerShellExecutionPolicy() (string, bool) { return "", false }

// agentVolumeFileSystem is unavailable outside Windows; the check skips.
func agentVolumeFileSystem(string) (string, bool) { return "", false }

// agentDriveIsRemote is always false on non-Windows platforms.
func agentDriveIsRemote(string) bool { return false }

// agentPWSHVersion has no PE version info outside Windows.
func agentPWSHVersion(string) string { return "" }
