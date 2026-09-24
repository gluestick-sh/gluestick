//go:build !windows

package envset

import "fmt"

func applyUser(vars map[string]string) error {
	return fmt.Errorf("persistent user environment variables are only supported on Windows")
}

func removeUser(names []string) error {
	return fmt.Errorf("persistent user environment variables are only supported on Windows")
}
