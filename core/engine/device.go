package engine

import (
	"fmt"

	"github.com/gluestick-sh/core/device"
)

// DeviceInfo returns the stable identity for this engine's glue root.
func (e *Engine) DeviceInfo() (*device.Info, error) {
	if e == nil || e.Config == nil {
		return nil, fmt.Errorf("engine not configured")
	}
	return device.Ensure(e.Config.RootDir)
}

// SetDeviceDisplayName updates the user-facing name for this glue root.
func (e *Engine) SetDeviceDisplayName(name string) error {
	if e == nil || e.Config == nil {
		return fmt.Errorf("engine not configured")
	}
	return device.SetDisplayName(e.Config.RootDir, name)
}

// TouchDeviceClient records that a client (cli/desktop) used this glue root.
func (e *Engine) TouchDeviceClient(client, appVersion string) error {
	if e == nil || e.Config == nil {
		return fmt.Errorf("engine not configured")
	}
	return device.TouchClient(e.Config.RootDir, client, appVersion)
}
