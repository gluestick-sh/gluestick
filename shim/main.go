// Command shim is the tiny launcher that Gluestick installs as <name>.exe on
// PATH. At runtime it reads its JSON config from <data-root>/shims-meta/<name>.json
// and execs the real target, proxying stdio and propagating the exit code.
//
// The data root is resolved in this order:
//  1. GLUE_DATA_ROOT env override (explicit isolation or portable roots)
//  2. relative to this shim's own location: <root>/shims/<name>.exe resolves
//     <root>/shims-meta/<name>.json (identical to the legacy path for normal
//     installs under ~/.glue, and makes isolated roots like ~/.glue-alpha work)
//  3. legacy fallback: ~/.glue/shims-meta/<name>.json
//
// The compiled binary (shim.exe) is copied by github.com/gluestick-sh/core for every shim
// it creates. This program is intentionally dependency-free (standard library
// only) so the resulting executable stays small.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Config is the on-disk shim configuration for a single executable.
//
// IMPORTANT: this struct is a shared contract with github.com/gluestick-sh/core's
// shim.Config. Keep the JSON field names in sync across both projects.
type Config struct {
	Name    string            `json:"name"`          // Display name
	Command string            `json:"command"`       // Actual command to run
	Args    []string          `json:"args,omitempty"` // Default arguments (optional)
	Env     map[string]string `json:"env,omitempty"`  // Package env vars applied at launch
	Path    string            `json:"path"`          // Path to the executable
}

func main() {
	shimName := filepath.Base(os.Args[0])
	shimName = strings.TrimSuffix(shimName, ".exe")

	selfPath, exeErr := os.Executable()
	if exeErr != nil {
		selfPath = os.Args[0]
	}

	cfg, err := loadShimConfig(configSearchPaths(selfPath, shimName), shimName)
	if err != nil {
		fatal(err)
	}

	runShim(cfg)
}

// configSearchPaths returns candidate shim config paths for shimName, in
// resolution order (see the package comment).
func configSearchPaths(selfPath, shimName string) []string {
	var paths []string
	if root, ok := os.LookupEnv("GLUE_DATA_ROOT"); ok && root != "" {
		paths = append(paths, filepath.Join(root, "shims-meta", shimName+".json"))
	}
	if selfPath != "" {
		root := filepath.Dir(filepath.Dir(selfPath))
		paths = append(paths, filepath.Join(root, "shims-meta", shimName+".json"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(home, ".glue", "shims-meta", shimName+".json"))
	}
	return paths
}

// loadShimConfig reads and parses the first existing shim config among paths.
// A config that exists but fails to parse is a hard error (no silent fallback).
func loadShimConfig(paths []string, shimName string) (*Config, error) {
	var lastErr error = fmt.Errorf("shim config not found for %q", shimName)
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			lastErr = err
			continue
		}
		var cfg Config
		if err := json.Unmarshal(data, &cfg); err != nil {
			return nil, fmt.Errorf("invalid shim config %s: %w", p, err)
		}
		return &cfg, nil
	}
	return nil, fmt.Errorf("shim config not found: %w", lastErr)
}

// runShim execs the configured target, proxying stdio and the exit code.
func runShim(cfg *Config) {
	args := append(cfg.Args, os.Args[1:]...)
	cmd := exec.Command(cfg.Command, args...)
	if len(cfg.Env) > 0 {
		env := os.Environ()
		for k, v := range cfg.Env {
			env = append(env, k+"="+v)
		}
		cmd.Env = env
	}

	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			os.Exit(exitErr.ExitCode())
		}
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "glue shim error: %v\n", err)
	os.Exit(1)
}
