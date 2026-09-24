//go:build !windows

package launch

func peLaunchKind(path string) LaunchKind {
	return LaunchKindGUI
}

func readPESubsystem(path string) (ok bool, console bool) {
	return false, false
}
