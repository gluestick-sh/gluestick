//go:build !windows

package launch

import "fmt"

func openLaunchFile(absPath string, kind LaunchKind) error {
	return fmt.Errorf("launch not supported on this platform")
}
