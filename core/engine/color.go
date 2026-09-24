package engine

import "github.com/gluestick-sh/core/engine/internal/install"

// SetColorEnabled toggles ANSI color codes in engine progress output.
func SetColorEnabled(enabled bool) {
	install.SetColorEnabled(enabled)
}
