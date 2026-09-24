package engine_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gluestick-sh/core/device"
	"github.com/gluestick-sh/core/engine"
)

func TestNewEngineEnsuresDevice(t *testing.T) {
	root := t.TempDir()
	eng, err := engine.NewEngine(&engine.EngineConfig{RootDir: root})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	defer eng.Close()

	info, err := eng.DeviceInfo()
	if err != nil {
		t.Fatal(err)
	}
	if info.DeviceID == "" {
		t.Fatal("empty device id")
	}
	if _, err := os.Stat(filepath.Join(root, device.FileName)); err != nil {
		t.Fatal(err)
	}
	if err := eng.TouchDeviceClient("desktop", "0.1.7"); err != nil {
		t.Fatal(err)
	}
	if err := eng.SetDeviceDisplayName("Laptop"); err != nil {
		t.Fatal(err)
	}
	info, err = eng.DeviceInfo()
	if err != nil {
		t.Fatal(err)
	}
	if info.DisplayName != "Laptop" {
		t.Fatalf("displayName = %q", info.DisplayName)
	}
	if info.Clients["desktop"].AppVersion != "0.1.7" {
		t.Fatalf("clients = %#v", info.Clients)
	}
}
