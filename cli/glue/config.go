package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/gluestick-sh/core/bucket"
	"github.com/gluestick-sh/core/engine"
	"github.com/gluestick-sh/core/config"
	"github.com/gluestick-sh/core/verbose"
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
		var value string
		switch key {
		case "github_proxy":
			value = cfg.GitHubProxy
		case "verbose":
			value = formatVerbose(cfg.Verbose)
		case "parallel_download":
			value = formatParallelDownload(cfg.ParallelDownload)
		case "color":
			value = formatColor(cfg.Color)
		default:
			return fmt.Errorf("unknown config key: %s", key)
		}

		if key == "github_proxy" && value == "" {
			fmt.Println("(not set)")
		} else {
			fmt.Println(value)
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

		switch key {
		case "github_proxy":
			cfg.GitHubProxy = value
			fmt.Printf("Set github_proxy = %s\n", value)
		case "verbose":
			enabled, err := parseConfigBool(value)
			if err != nil {
				return err
			}
			cfg.Verbose = &enabled
			fmt.Printf("Set verbose = %t\n", enabled)
		case "parallel_download":
			enabled, err := parseConfigBool(value)
			if err != nil {
				return err
			}
			cfg.ParallelDownload = &enabled
			fmt.Printf("Set parallel_download = %t\n", enabled)
		case "color":
			enabled, err := parseConfigBool(value)
			if err != nil {
				return err
			}
			cfg.Color = &enabled
			fmt.Printf("Set color = %t\n", enabled)
		default:
			return fmt.Errorf("unknown config key: %s\n\nAvailable keys:\n  github_proxy\n  parallel_download\n  color\n  verbose", key)
		}

		if err := saveConfig(root, cfg); err != nil {
			return fmt.Errorf("save config: %w", err)
		}
		applyConfig(cfg)

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

		switch key {
		case "github_proxy":
			if cfg.GitHubProxy == "" {
				fmt.Printf("github_proxy is not set\n")
				return nil
			}
			cfg.GitHubProxy = ""
			fmt.Printf("Unset github_proxy\n")
		case "verbose":
			if cfg.Verbose == nil {
				fmt.Printf("verbose is not set\n")
				return nil
			}
			cfg.Verbose = nil
			fmt.Printf("Unset verbose (default: disabled)\n")
		case "parallel_download":
			if cfg.ParallelDownload == nil {
				fmt.Printf("parallel_download is not set\n")
				return nil
			}
			cfg.ParallelDownload = nil
			fmt.Printf("Unset parallel_download (default: enabled)\n")
		case "color":
			if cfg.Color == nil {
				fmt.Printf("color is not set\n")
				return nil
			}
			cfg.Color = nil
			fmt.Printf("Unset color (default: enabled)\n")
		default:
			return fmt.Errorf("unknown config key: %s", key)
		}

		if err := saveConfig(root, cfg); err != nil {
			return fmt.Errorf("save config: %w", err)
		}
		applyConfig(cfg)

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
}

func loadConfig(root string) (*config.Basics, error) {
	return config.ReadBasics(root)
}

func saveConfig(root string, cfg *config.Basics) error {
	return config.WriteBasics(root, cfg)
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
