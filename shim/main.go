// Command shim is the tiny launcher that Gluestick installs as <name>.exe on
// PATH. At runtime it reads its JSON config from ~/.glue/shims-meta/<name>.json
// and execs the real target, proxying stdio and propagating the exit code.
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

	home, err := os.UserHomeDir()
	if err != nil {
		fatal(err)
	}

	configPath := filepath.Join(home, ".glue", "shims-meta", shimName+".json")

	data, err := os.ReadFile(configPath)
	if err != nil {
		fatal(fmt.Errorf("shim config not found: %w", err))
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		fatal(fmt.Errorf("invalid shim config: %w", err))
	}

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
