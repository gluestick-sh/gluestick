package shim

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNewManager(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "shim-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	m, err := NewManager(tmpDir)
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}

	if m.BinDir() == "" {
		t.Error("BinDir should not be empty")
	}

	expectedBinDir := filepath.Join(tmpDir, "shims")
	if m.BinDir() != expectedBinDir {
		t.Errorf("BinDir = %s, want %s", m.BinDir(), expectedBinDir)
	}
}

func TestShimCreateAndRemove(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "shim-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	m, err := NewManager(tmpDir)
	if err != nil {
		t.Fatal(err)
	}

	// Create a shim
	err = m.Create("testapp", "C:\\Windows\\System32\\notepad.exe", CreateOpts{Args: []string{"-multi"}})
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	// Check config file exists
	configPath := filepath.Join(tmpDir, "shims-meta", "testapp.json")
	if _, err := os.Stat(configPath); err != nil {
		t.Errorf("config file not created: %v", err)
	}

	// List shims
	configs, err := m.List()
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}

	if len(configs) != 1 {
		t.Errorf("List returned %d configs, want 1", len(configs))
	}

	if configs[0].Name != "testapp" {
		t.Errorf("config name = %s, want testapp", configs[0].Name)
	}
	if len(configs[0].Args) != 1 || configs[0].Args[0] != "-multi" {
		t.Errorf("config args = %v, want [-multi]", configs[0].Args)
	}

	// Remove shim
	if err := m.Remove("testapp"); err != nil {
		t.Fatalf("Remove failed: %v", err)
	}

	// Verify removed
	if _, err := os.Stat(configPath); err == nil {
		t.Error("config file still exists after Remove")
	}
}

func TestShimListEmpty(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "shim-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	m, err := NewManager(tmpDir)
	if err != nil {
		t.Fatal(err)
	}

	configs, err := m.List()
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}

	if len(configs) != 0 {
		t.Errorf("List returned %d configs, want 0", len(configs))
	}
}
