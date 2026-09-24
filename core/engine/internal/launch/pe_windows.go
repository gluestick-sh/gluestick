//go:build windows

package launch

import (
	"encoding/binary"
	"io"
	"os"
)

const (
	peSubsystemGUI     = 2
	peSubsystemConsole = 3
	peOptionalSubsysOff = 68
)

func peLaunchKind(path string) LaunchKind {
	ok, console := readPESubsystem(path)
	if !ok {
		return LaunchKindConsole
	}
	if console {
		return LaunchKindConsole
	}
	return LaunchKindGUI
}

func readPESubsystem(path string) (ok bool, console bool) {
	f, err := os.Open(path)
	if err != nil {
		return false, false
	}
	defer f.Close()

	dos := make([]byte, 64)
	if _, err := io.ReadFull(f, dos); err != nil || dos[0] != 'M' || dos[1] != 'Z' {
		return false, false
	}

	peOffset := int(binary.LittleEndian.Uint32(dos[0x3C:]))
	if peOffset <= 0 || peOffset > 8*1024*1024 {
		return false, false
	}

	sig := make([]byte, 4)
	if _, err := f.ReadAt(sig, int64(peOffset)); err != nil || string(sig) != "PE\x00\x00" {
		return false, false
	}

	subsys := make([]byte, 2)
	offset := peOffset + 24 + peOptionalSubsysOff
	if _, err := f.ReadAt(subsys, int64(offset)); err != nil {
		return false, false
	}
	switch binary.LittleEndian.Uint16(subsys) {
	case peSubsystemConsole:
		return true, true
	case peSubsystemGUI:
		return true, false
	default:
		return true, false
	}
}
