//go:build !windows

package install

import "strings"

func decodePowerShellOutput(b []byte) string {
	return strings.TrimSpace(string(b))
}
