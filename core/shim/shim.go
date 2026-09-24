// Package shim creates and runs Scoop-style executable shims that forward
// invocations (with optional default args and env) to installed package binaries.
package shim

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Config represents the shim configuration for a single executable
type Config struct {
	Name    string            `json:"name"`              // Display name
	Command string            `json:"command"`           // Actual command to run
	Args    []string          `json:"args,omitempty"`    // Default arguments (optional)
	Env     map[string]string `json:"env,omitempty"`     // Package env vars applied at launch
	Path    string            `json:"path"`              // Path to the executable
}

// CreateOpts optional settings when creating a shim.
type CreateOpts struct {
	Args []string
	Env  map[string]string
}

// Manager handles shim creation and management
type Manager struct {
	rootDir  string
	shimsDir string
	binDir   string // Where shims are installed (added to PATH)
}

// NewManager creates a new shim manager
func NewManager(rootDir string) (*Manager, error) {
	binDir := filepath.Join(rootDir, "shims")
	shimsDir := filepath.Join(rootDir, "shims-meta")

	if err := os.MkdirAll(binDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create shims directory: %w", err)
	}
	if err := os.MkdirAll(shimsDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create shims-meta directory: %w", err)
	}

	if stub := resolveShimStubPath(rootDir); stub != "" {
		_ = cacheShimStub(rootDir, stub)
	}

	return &Manager{
		rootDir:  rootDir,
		shimsDir: shimsDir,
		binDir:   binDir,
	}, nil
}

// BinDir returns the directory that should be added to PATH
func (m *Manager) BinDir() string {
	return m.binDir
}

// InPath checks if the bin directory is in PATH
func (m *Manager) InPath() bool {
	path := os.Getenv("PATH")
	paths := filepath.SplitList(path)

	for _, p := range paths {
		if strings.EqualFold(p, m.binDir) {
			return true
		}
	}
	return false
}

// Create creates a shim for the given executable
// On Windows, this creates a small .exe that reads the config and execs the target
func (m *Manager) Create(name, targetPath string, opts ...CreateOpts) error {
	var o CreateOpts
	if len(opts) > 0 {
		o = opts[0]
	}
	config := Config{
		Name:    name,
		Command: targetPath,
		Args:    o.Args,
		Env:     o.Env,
		Path:    targetPath,
	}

	// Write config file
	configPath := filepath.Join(m.shimsDir, name+".json")
	configData, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}

	if err := os.WriteFile(configPath, configData, 0644); err != nil {
		return fmt.Errorf("write config: %w", err)
	}

	// Create the shim executable (copy of the prebuilt shim runner)
	shimPath := filepath.Join(m.binDir, name+".exe")
	if err := m.createWindowsShim(shimPath, name); err != nil {
		return fmt.Errorf("create shim: %w", err)
	}

	return nil
}

// Remove removes a shim
func (m *Manager) Remove(name string) error {
	// Remove config
	configPath := filepath.Join(m.shimsDir, name+".json")
	os.Remove(configPath)

	// Remove shim executable
	shimPath := filepath.Join(m.binDir, name+".exe")
	os.Remove(shimPath)

	return nil
}

// List returns all installed shims
func (m *Manager) List() ([]Config, error) {
	entries, err := os.ReadDir(m.shimsDir)
	if err != nil {
		return nil, err
	}

	var configs []Config
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}

		configPath := filepath.Join(m.shimsDir, e.Name())
		data, err := os.ReadFile(configPath)
		if err != nil {
			continue
		}

		var cfg Config
		if err := json.Unmarshal(data, &cfg); err != nil {
			continue
		}

		configs = append(configs, cfg)
	}

	return configs, nil
}

// Run executes a shim by name
func (m *Manager) Run(name string, args ...string) error {
	// Load config
	configPath := filepath.Join(m.shimsDir, name+".json")
	data, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("shim config not found: %w", err)
	}

	var config Config
	if err := json.Unmarshal(data, &config); err != nil {
		return fmt.Errorf("invalid shim config: %w", err)
	}

	// Build command
	cmdArgs := append(config.Args, args...)
	cmd := exec.Command(config.Command, cmdArgs...)
	// Inherit the caller's working directory (Scoop shim semantics). Setting Dir to
	// the executable folder breaks tools like npm that resolve package.json from cwd.
	cmd.Env = envForCommand(config.Env)

	// Proxy stdio
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// Run and propagate exit code
	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			os.Exit(exitErr.ExitCode())
		}
		return fmt.Errorf("run command: %w", err)
	}

	return nil
}

func envForCommand(extra map[string]string) []string {
	if len(extra) == 0 {
		return nil
	}
	env := os.Environ()
	for k, v := range extra {
		env = append(env, k+"="+v)
	}
	return env
}

// createWindowsShim copies the prebuilt shim runner.
func (m *Manager) createWindowsShim(shimPath, name string) error {
	stubPath := resolveShimStubPath(m.rootDir)
	if stubPath == "" {
		return m.createBatchShim(shimPath, name)
	}

	src, err := os.ReadFile(stubPath)
	if err != nil {
		return fmt.Errorf("read shim stub: %w", err)
	}

	if err := os.WriteFile(shimPath, src, 0755); err != nil {
		return fmt.Errorf("write shim: %w", err)
	}

	return nil
}

// createBatchShim creates a fallback batch file shim when shim.exe stub is unavailable.
func (m *Manager) createBatchShim(shimPath, name string) error {
	batchPath := strings.TrimSuffix(shimPath, ".exe") + ".bat"
	runner := "glue"
	if execPath, err := os.Executable(); err == nil {
		base := strings.ToLower(filepath.Base(execPath))
		if strings.HasPrefix(base, "gluestick") || strings.HasPrefix(base, "glue") {
			runner = execPath
		}
	}
	content := fmt.Sprintf("@echo off\r\n\"%s\" shim-run %s %%*\r\n", runner, name)
	return os.WriteFile(batchPath, []byte(content), 0755)
}

// Cleanup removes all shims
func (m *Manager) Cleanup() error {
	// Remove all shims
	configs, err := m.List()
	if err != nil {
		return err
	}

	for _, cfg := range configs {
		name := strings.TrimSuffix(filepath.Base(cfg.Path), ".json")
		m.Remove(name)
	}

	return nil
}
