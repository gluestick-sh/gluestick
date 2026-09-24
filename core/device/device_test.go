package device

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsureCreatesStableID(t *testing.T) {
	root := t.TempDir()
	a, err := Ensure(root)
	if err != nil {
		t.Fatal(err)
	}
	if !validDeviceID(a.DeviceID) {
		t.Fatalf("deviceId = %q", a.DeviceID)
	}
	if a.CreatedAt == "" {
		t.Fatal("missing createdAt")
	}
	if a.Platform.OS == "" || a.Platform.Arch == "" {
		t.Fatalf("platform = %#v", a.Platform)
	}

	b, err := Ensure(root)
	if err != nil {
		t.Fatal(err)
	}
	if a.DeviceID != b.DeviceID {
		t.Fatalf("deviceId changed: %q -> %q", a.DeviceID, b.DeviceID)
	}
	if a.CreatedAt != b.CreatedAt {
		t.Fatalf("createdAt changed: %q -> %q", a.CreatedAt, b.CreatedAt)
	}
}

func TestGetMissing(t *testing.T) {
	root := t.TempDir()
	_, err := Get(root)
	if !os.IsNotExist(err) {
		t.Fatalf("Get: err = %v, want not exist", err)
	}
}

func TestSetDisplayNameAndTouchClient(t *testing.T) {
	root := t.TempDir()
	if err := SetDisplayName(root, "Office PC"); err != nil {
		t.Fatal(err)
	}
	info, err := Get(root)
	if err != nil {
		t.Fatal(err)
	}
	if info.DisplayName != "Office PC" {
		t.Fatalf("displayName = %q", info.DisplayName)
	}
	if DisplayLabel(info) != "Office PC" {
		t.Fatalf("DisplayLabel = %q", DisplayLabel(info))
	}

	if err := TouchClient(root, "desktop", "0.1.7"); err != nil {
		t.Fatal(err)
	}
	if err := TouchClient(root, "CLI", "0.1.0"); err != nil {
		t.Fatal(err)
	}
	info, err = Get(root)
	if err != nil {
		t.Fatal(err)
	}
	if info.Clients["desktop"].AppVersion != "0.1.7" {
		t.Fatalf("desktop client = %#v", info.Clients["desktop"])
	}
	if info.Clients["cli"].AppVersion != "0.1.0" {
		t.Fatalf("cli client = %#v", info.Clients["cli"])
	}
	if info.Clients["cli"].LastSeenAt == "" {
		t.Fatal("missing lastSeenAt")
	}
}

func TestCorruptFileIsQuarantined(t *testing.T) {
	root := t.TempDir()
	path := Path(root)
	if err := os.WriteFile(path, []byte("{not-json"), 0644); err != nil {
		t.Fatal(err)
	}
	info, err := Ensure(root)
	if err != nil {
		t.Fatal(err)
	}
	if !validDeviceID(info.DeviceID) {
		t.Fatalf("deviceId = %q", info.DeviceID)
	}
	matches, _ := filepath.Glob(filepath.Join(root, "device.json.corrupt.*"))
	if len(matches) != 1 {
		t.Fatalf("quarantine files = %v", matches)
	}
}

func TestInvalidDeviceIDRejected(t *testing.T) {
	root := t.TempDir()
	path := Path(root)
	bad := Info{
		SchemaVersion: 1,
		DeviceID:      "not-a-device",
		CreatedAt:     "2026-01-01T00:00:00Z",
	}
	data, _ := json.Marshal(bad)
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
	info, err := Ensure(root)
	if err != nil {
		t.Fatal(err)
	}
	if info.DeviceID == "not-a-device" {
		t.Fatal("expected regenerate on invalid id")
	}
}

func TestDisplayNameTooLong(t *testing.T) {
	root := t.TempDir()
	long := strings.Repeat("名", maxDisplayLen+1)
	if err := SetDisplayName(root, long); err == nil {
		t.Fatal("expected error for long display name")
	}
}

func TestEnsureEmptyRoot(t *testing.T) {
	if _, err := Ensure(""); err == nil {
		t.Fatal("expected error")
	}
}

func TestTouchClientValidation(t *testing.T) {
	root := t.TempDir()
	if err := TouchClient(root, "", "1"); err == nil {
		t.Fatal("expected empty client error")
	}
	if err := TouchClient(root, "../x", "1"); err == nil {
		t.Fatal("expected invalid client error")
	}
}
