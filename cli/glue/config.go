package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/gluestick-sh/core/bucket"
	"github.com/gluestick-sh/core/config"
	"github.com/gluestick-sh/core/engine"
	"github.com/gluestick-sh/core/verbose"
	"github.com/spf13/cobra"
)

var configCmd = &cobra.Command{
	Use:   "config",
	Short: "Manage glue configuration",
}

var configGetCmd = &cobra.Command{
	Use:   "get <key>",
	Short: "Get a configuration value",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		root := glueRoot()

		cfg, err := loadConfig(root)
		if err != nil {
			return err
		}

		key := args[0]
		value, set, err := configResolvedValue(cfg, key)
		if err != nil {
			return emitConfigError("config_get", key, err)
		}

		if jsonOutputEnabled() {
			return emitJSON(map[string]any{"command": "config_get", "key": key, "value": value, "set": set})
		}

		if key == "github_proxy" && value == "" {
			fmt.Println("(not set)")
		} else {
			fmt.Println(formatConfigValue(key, value))
		}
		return nil
	},
}

var configSetCmd = &cobra.Command{
	Use:   "set <key> <value>",
	Short: "Set a configuration value",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		root := glueRoot()

		cfg, err := loadConfig(root)
		if err != nil {
			return err
		}

		key := args[0]
		value := args[1]

		var stored any
		switch key {
		case "github_proxy":
			stored = value
			cfg.GitHubProxy = value
		case "verbose":
			enabled, err := parseConfigBool(value)
			if err != nil {
				return emitConfigError("config_set", key, err)
			}
			stored = enabled
			cfg.Verbose = &enabled
		case "parallel_download":
			enabled, err := parseConfigBool(value)
			if err != nil {
				return emitConfigError("config_set", key, err)
			}
			stored = enabled
			cfg.ParallelDownload = &enabled
		case "color":
			enabled, err := parseConfigBool(value)
			if err != nil {
				return emitConfigError("config_set", key, err)
			}
			stored = enabled
			cfg.Color = &enabled
		default:
			return emitConfigError("config_set", key, fmt.Errorf(
				"unknown config key: %s\n\nAvailable keys:\n  github_proxy\n  parallel_download\n  color\n  verbose", key))
		}

		if err := saveConfig(root, cfg); err != nil {
			return emitConfigSaveError("config_set", key, fmt.Errorf("save config: %w", err))
		}
		applyConfig(cfg)

		if jsonOutputEnabled() {
			return emitJSON(map[string]any{"command": "config_set", "ok": true, "key": key, "value": stored})
		}
		fmt.Printf("Set %s = %v\n", key, stored)
		return nil
	},
}

var configUnsetCmd = &cobra.Command{
	Use:   "unset <key>",
	Short: "Unset a configuration value",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		root := glueRoot()

		cfg, err := loadConfig(root)
		if err != nil {
			return err
		}

		key := args[0]

		var wasSet bool
		switch key {
		case "github_proxy":
			wasSet = cfg.GitHubProxy != ""
			cfg.GitHubProxy = ""
			if !wasSet {
				if jsonOutputEnabled() {
					return emitJSON(map[string]any{"command": "config_unset", "ok": true, "key": key, "was_set": false})
				}
				fmt.Printf("github_proxy is not set\n")
				return nil
			}
		case "verbose":
			wasSet = cfg.Verbose != nil
			cfg.Verbose = nil
			if !wasSet {
				if jsonOutputEnabled() {
					return emitJSON(map[string]any{"command": "config_unset", "ok": true, "key": key, "was_set": false})
				}
				fmt.Printf("verbose is not set\n")
				return nil
			}
		case "parallel_download":
			wasSet = cfg.ParallelDownload != nil
			cfg.ParallelDownload = nil
			if !wasSet {
				if jsonOutputEnabled() {
					return emitJSON(map[string]any{"command": "config_unset", "ok": true, "key": key, "was_set": false})
				}
				fmt.Printf("parallel_download is not set\n")
				return nil
			}
		case "color":
			wasSet = cfg.Color != nil
			cfg.Color = nil
			if !wasSet {
				if jsonOutputEnabled() {
					return emitJSON(map[string]any{"command": "config_unset", "ok": true, "key": key, "was_set": false})
				}
				fmt.Printf("color is not set\n")
				return nil
			}
		default:
			return emitConfigError("config_unset", key, fmt.Errorf("unknown config key: %s", key))
		}

		if err := saveConfig(root, cfg); err != nil {
			return emitConfigSaveError("config_unset", key, fmt.Errorf("save config: %w", err))
		}
		applyConfig(cfg)

		if jsonOutputEnabled() {
			return emitJSON(map[string]any{"command": "config_unset", "ok": true, "key": key, "was_set": wasSet})
		}
		fmt.Printf("Unset %s\n", key)
		return nil
	},
}

var configListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all configuration values",
	RunE: func(cmd *cobra.Command, args []string) error {
		root := glueRoot()

		cfg, err := loadConfig(root)
		if err != nil {
			return err
		}

		if jsonOutputEnabled() {
			parallel, parallelSet := configTriBool(cfg.ParallelDownload, true)
			colorValue, colorSet := configTriBool(cfg.Color, true)
			verboseValue, verboseSet := configTriBool(cfg.Verbose, false)
			return emitJSON(map[string]any{
				"command":               "config_list",
				"github_proxy":          cfg.GitHubProxy,
				"github_proxy_set":      cfg.GitHubProxy != "",
				"parallel_download":     parallel,
				"parallel_download_set": parallelSet,
				"color":                 colorValue,
				"color_set":             colorSet,
				"verbose":               verboseValue,
				"verbose_set":           verboseSet,
			})
		}

		fmt.Printf("%sConfiguration:%s\n", colorBlue, colorReset)
		if cfg.GitHubProxy == "" {
			fmt.Println("  github_proxy = (not set, direct GitHub)")
		} else {
			fmt.Printf("  github_proxy = %s\n", cfg.GitHubProxy)
		}
		fmt.Printf("  parallel_download = %s\n", formatParallelDownload(cfg.ParallelDownload))
		fmt.Printf("  color = %s\n", formatColor(cfg.Color))
		fmt.Printf("  verbose = %s\n", formatVerbose(cfg.Verbose))

		return nil
	},
}

func init() {
	rootCmd.AddCommand(configCmd)
	configCmd.AddCommand(configGetCmd)
	configCmd.AddCommand(configSetCmd)
	configCmd.AddCommand(configUnsetCmd)
	configCmd.AddCommand(configListCmd)
	for _, c := range []*cobra.Command{configGetCmd, configSetCmd, configUnsetCmd, configListCmd} {
		c.SilenceUsage = true
		c.SilenceErrors = true
	}
}

// emitConfigError routes a config argument error through the JSON envelope when
// --json is set (stable code: invalid_argument), or returns it verbatim otherwise.
func emitConfigError(command, key string, err error) error {
	if jsonOutputEnabled() {
		if emitErr := emitJSON(map[string]any{
			"command": command,
			"ok":      false,
			"key":     key,
			"error":   err.Error(),
			"code":    "invalid_argument",
		}); emitErr != nil {
			return emitErr
		}
		return reportedFail()
	}
	return err
}

// configResolvedValue returns the resolved value and set flag for a config key.
// Boolean keys resolve to real booleans (defaults applied); github_proxy is a string.
func configResolvedValue(cfg *config.Basics, key string) (any, bool, error) {
	switch key {
	case "github_proxy":
		return cfg.GitHubProxy, cfg.GitHubProxy != "", nil
	case "verbose":
		v, set := configTriBool(cfg.Verbose, false)
		return v, set, nil
	case "parallel_download":
		v, set := configTriBool(cfg.ParallelDownload, true)
		return v, set, nil
	case "color":
		v, set := configTriBool(cfg.Color, true)
		return v, set, nil
	default:
		return nil, false, fmt.Errorf("unknown config key: %s", key)
	}
}

// configTriBool resolves a tri-state bool pointer against its default.
func configTriBool(v *bool, def bool) (value, set bool) {
	if v == nil {
		return def, false
	}
	return *v, true
}

// formatConfigValue renders a resolved config value for text mode.
func formatConfigValue(key string, value any) string {
	switch v := value.(type) {
	case bool:
		if v {
			return "true"
		}
		return "false"
	case string:
		return v
	default:
		return fmt.Sprintf("%v", value)
	}
}

func loadConfig(root string) (*config.Basics, error) {
	return config.ReadBasics(root)
}

func saveConfig(root string, cfg *config.Basics) error {
	// The data root may not exist yet (e.g. config set is the first command run);
	// create it so config is always writable on a fresh install.
	if err := os.MkdirAll(root, 0755); err != nil {
		return fmt.Errorf("create data root: %w", err)
	}
	return config.WriteBasics(root, cfg)
}

// emitConfigSaveError routes a config persistence failure through the JSON envelope.
func emitConfigSaveError(command, key string, err error) error {
	if jsonOutputEnabled() {
		if emitErr := emitJSON(map[string]any{
			"command": command,
			"ok":      false,
			"key":     key,
			"error":   err.Error(),
			"code":    "config_write_failed",
		}); emitErr != nil {
			return emitErr
		}
		return reportedFail()
	}
	return err
}

func parseConfigBool(value string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "true", "1", "yes", "on":
		return true, nil
	case "false", "0", "no", "off", "":
		return false, nil
	default:
		return false, fmt.Errorf("invalid boolean %q (use true or false)", value)
	}
}

func applyConfig(cfg *config.Basics) {
	if cfg == nil {
		verbose.Set(resolveVerbose(nil))
		terminalColorEnabled = resolveTerminalColor(nil)
		setColorEnabled(terminalColorEnabled)
		bucket.SetColorEnabled(true)
		engine.SetColorEnabled(true)
		return
	}
	verbose.Set(resolveVerbose(cfg))
	enabled := resolveTerminalColor(cfg)
	terminalColorEnabled = enabled
	setColorEnabled(enabled)
	bucket.SetColorEnabled(enabled)
	engine.SetColorEnabled(enabled)
}

// resolveVerbose: CLI -v/--verbose, then GLUE_VERBOSE, then config.json verbose.
func resolveVerbose(cfg *config.Basics) bool {
	if v, err := rootCmd.PersistentFlags().GetBool("verbose"); err == nil && v {
		return true
	}
	if v := strings.TrimSpace(os.Getenv("GLUE_VERBOSE")); v != "" {
		enabled, err := parseConfigBool(v)
		if err == nil {
			return enabled
		}
	}
	if cfg != nil && cfg.Verbose != nil {
		return *cfg.Verbose
	}
	return false
}

func formatVerbose(v *bool) string {
	if v == nil {
		return "false (default)"
	}
	if *v {
		return "true"
	}
	return "false"
}

func formatParallelDownload(v *bool) string {
	if v == nil {
		return "true (default)"
	}
	if *v {
		return "true"
	}
	return "false"
}

func formatColor(v *bool) string {
	if v == nil {
		return "true (default)"
	}
	if *v {
		return "true"
	}
	return "false"
}

// parallelDownloadEnabled resolves parallel_download from env, config, default true.
func parallelDownloadEnabled(cfg *config.Basics) bool {
	if v := strings.TrimSpace(os.Getenv("GLUE_PARALLEL_DOWNLOAD")); v != "" {
		enabled, err := parseConfigBool(v)
		if err == nil {
			return enabled
		}
	}
	if cfg != nil && cfg.ParallelDownload != nil {
		return *cfg.ParallelDownload
	}
	return true
}
