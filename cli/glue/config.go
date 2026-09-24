package main

import (
	"fmt"
	"os"
	"strconv"
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
		agent, err := config.ReadAgent(root)
		if err != nil {
			return err
		}
		audit, err := config.ReadAudit(root)
		if err != nil {
			return err
		}

		key := args[0]
		value, set, err := configResolvedValue(cfg, agent, audit, key)
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

		if isAgentConfigKey(key) {
			agent, err := config.ReadAgent(root)
			if err != nil {
				return emitConfigError("config_set", key, err)
			}
			stored, err := setAgentConfigValue(&agent, key, value)
			if err != nil {
				return emitConfigError("config_set", key, err)
			}
			if err := config.WriteAgent(root, agent); err != nil {
				return emitConfigSaveError("config_set", key, fmt.Errorf("save config: %w", err))
			}
			if jsonOutputEnabled() {
				return emitJSON(map[string]any{"command": "config_set", "ok": true, "key": key, "value": stored})
			}
			fmt.Printf("Set %s = %v\n", key, stored)
			return nil
		}

		if isAuditConfigKey(key) {
			audit, err := config.ReadAudit(root)
			if err != nil {
				return emitConfigError("config_set", key, err)
			}
			stored, err := setAuditConfigValue(&audit, key, value)
			if err != nil {
				return emitConfigError("config_set", key, err)
			}
			if err := config.WriteAudit(root, audit); err != nil {
				return emitConfigSaveError("config_set", key, fmt.Errorf("save config: %w", err))
			}
			if jsonOutputEnabled() {
				return emitJSON(map[string]any{"command": "config_set", "ok": true, "key": key, "value": stored})
			}
			fmt.Printf("Set %s = %v\n", key, stored)
			return nil
		}

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
				"unknown config key: %s\n\nAvailable keys:\n  github_proxy\n  parallel_download\n  color\n  verbose\n  agent.auto_yes\n  agent.policy.mode\n  agent.policy.deny\n  agent.policy.protected\n  audit.max_bytes\n  audit.keep_segments", key))
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

		if isAgentConfigKey(key) {
			agent, err := config.ReadAgent(root)
			if err != nil {
				return emitConfigError("config_unset", key, err)
			}
			_, wasSet, err := resolveAgentConfigValue(agent, key)
			if err != nil {
				return emitConfigError("config_unset", key, err)
			}
			if !wasSet {
				if jsonOutputEnabled() {
					return emitJSON(map[string]any{"command": "config_unset", "ok": true, "key": key, "was_set": false})
				}
				fmt.Printf("%s is not set\n", key)
				return nil
			}
			resetAgentConfigValue(&agent, key)
			if err := config.WriteAgent(root, agent); err != nil {
				return emitConfigSaveError("config_unset", key, fmt.Errorf("save config: %w", err))
			}
			if jsonOutputEnabled() {
				return emitJSON(map[string]any{"command": "config_unset", "ok": true, "key": key, "was_set": true})
			}
			fmt.Printf("Unset %s\n", key)
			return nil
		}

		if isAuditConfigKey(key) {
			audit, err := config.ReadAudit(root)
			if err != nil {
				return emitConfigError("config_unset", key, err)
			}
			_, _, err = resolveAuditConfigValue(audit, key)
			if err != nil {
				return emitConfigError("config_unset", key, err)
			}
			resetAuditConfigValue(&audit, key)
			if err := config.WriteAudit(root, audit); err != nil {
				return emitConfigSaveError("config_unset", key, fmt.Errorf("save config: %w", err))
			}
			if jsonOutputEnabled() {
				return emitJSON(map[string]any{"command": "config_unset", "ok": true, "key": key, "was_set": true})
			}
			fmt.Printf("Unset %s\n", key)
			return nil
		}

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
		agent, err := config.ReadAgent(root)
		if err != nil {
			return err
		}
		audit, err := config.ReadAudit(root)
		if err != nil {
			return err
		}

		if jsonOutputEnabled() {
			parallel, parallelSet := configTriBool(cfg.ParallelDownload, true)
			colorValue, colorSet := configTriBool(cfg.Color, true)
			verboseValue, verboseSet := configTriBool(cfg.Verbose, false)
			return emitJSON(map[string]any{
				"command":                "config_list",
				"github_proxy":           cfg.GitHubProxy,
				"github_proxy_set":       cfg.GitHubProxy != "",
				"parallel_download":      parallel,
				"parallel_download_set":  parallelSet,
				"color":                  colorValue,
				"color_set":              colorSet,
				"verbose":                verboseValue,
				"verbose_set":            verboseSet,
				"agent_auto_yes":         agent.AutoYes,
				"agent_policy_mode":      agent.Policy.Mode,
				"agent_policy_deny":      agent.Policy.Deny,
				"agent_policy_protected": agent.Policy.Protected,
				"audit_max_bytes":        audit.MaxBytes,
				"audit_keep_segments":    audit.KeepSegments,
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
		fmt.Printf("  agent.auto_yes = %v\n", agent.AutoYes)
		fmt.Printf("  agent.policy.mode = %s\n", agent.Policy.Mode)
		fmt.Printf("  agent.policy.deny = %s\n", strings.Join(agent.Policy.Deny, ","))
		fmt.Printf("  agent.policy.protected = %s\n", strings.Join(agent.Policy.Protected, ","))
		fmt.Printf("  audit.max_bytes = %d\n", audit.MaxBytes)
		fmt.Printf("  audit.keep_segments = %d\n", audit.KeepSegments)

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
func configResolvedValue(cfg *config.Basics, agent config.AgentSettings, audit config.AuditSettings, key string) (any, bool, error) {
	if isAgentConfigKey(key) {
		return resolveAgentConfigValue(agent, key)
	}
	if isAuditConfigKey(key) {
		return resolveAuditConfigValue(audit, key)
	}
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

// agentConfigKeys are the config.json agent keys exposed by `glue config`.
var agentConfigKeys = map[string]bool{
	"agent.auto_yes":         true,
	"agent.policy.mode":      true,
	"agent.policy.deny":      true,
	"agent.policy.protected": true,
}

func isAgentConfigKey(key string) bool { return agentConfigKeys[key] }

// resolveAgentConfigValue returns the value and set flag for an agent key.
func resolveAgentConfigValue(settings config.AgentSettings, key string) (any, bool, error) {
	switch key {
	case "agent.auto_yes":
		return settings.AutoYes, settings.AutoYes, nil
	case "agent.policy.mode":
		return settings.Policy.Mode, true, nil
	case "agent.policy.deny":
		return strings.Join(settings.Policy.Deny, ","), len(settings.Policy.Deny) > 0, nil
	case "agent.policy.protected":
		return strings.Join(settings.Policy.Protected, ","), len(settings.Policy.Protected) > 0, nil
	}
	return nil, false, fmt.Errorf("unknown config key: %s", key)
}

// setAgentConfigValue mutates one agent key from its CLI string form.
func setAgentConfigValue(settings *config.AgentSettings, key, value string) (any, error) {
	switch key {
	case "agent.auto_yes":
		enabled, err := parseConfigBool(value)
		if err != nil {
			return nil, err
		}
		settings.AutoYes = enabled
		return enabled, nil
	case "agent.policy.mode":
		mode := strings.ToLower(strings.TrimSpace(value))
		if mode != config.AgentPolicyModeStrict && mode != config.AgentPolicyModeConfirm && mode != config.AgentPolicyModeAuto {
			return nil, fmt.Errorf("agent.policy.mode must be strict, confirm or auto")
		}
		settings.Policy.Mode = mode
		return mode, nil
	case "agent.policy.deny":
		settings.Policy.Deny = splitConfigList(value)
		return settings.Policy.Deny, nil
	case "agent.policy.protected":
		settings.Policy.Protected = splitConfigList(value)
		return settings.Policy.Protected, nil
	}
	return nil, fmt.Errorf("unknown config key: %s", key)
}

// resetAgentConfigValue restores one agent key to its safe default.
func resetAgentConfigValue(settings *config.AgentSettings, key string) {
	switch key {
	case "agent.auto_yes":
		settings.AutoYes = false
	case "agent.policy.mode":
		settings.Policy.Mode = config.AgentPolicyModeConfirm
	case "agent.policy.deny":
		settings.Policy.Deny = []string{}
	case "agent.policy.protected":
		settings.Policy.Protected = []string{}
	}
}

func splitConfigList(value string) []string {
	out := []string{}
	for _, part := range strings.Split(value, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// auditConfigKeys are the config.json audit keys exposed by `glue config`.
// Both keys report as "set" because the reader always resolves defaults.
var auditConfigKeys = map[string]bool{
	"audit.max_bytes":     true,
	"audit.keep_segments": true,
}

func isAuditConfigKey(key string) bool { return auditConfigKeys[key] }

// resolveAuditConfigValue returns the value and set flag for an audit key.
func resolveAuditConfigValue(settings config.AuditSettings, key string) (any, bool, error) {
	switch key {
	case "audit.max_bytes":
		return settings.MaxBytes, true, nil
	case "audit.keep_segments":
		return settings.KeepSegments, true, nil
	}
	return nil, false, fmt.Errorf("unknown config key: %s", key)
}

// setAuditConfigValue mutates one audit key from its CLI string form. Because a
// leading "-" would be parsed as a flag by cobra, the negative sentinels are
// also reachable as keywords: `off` (disable rotation) and `all` (keep every
// segment). Literal negatives still work after a `--` separator.
func setAuditConfigValue(settings *config.AuditSettings, key, value string) (any, error) {
	raw := strings.ToLower(strings.TrimSpace(value))
	switch key {
	case "audit.max_bytes":
		if raw == "off" || raw == "none" || raw == "disabled" {
			settings.MaxBytes = config.AuditRotationDisabled
			return settings.MaxBytes, nil
		}
		maxBytes, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("audit.max_bytes must be a whole number of bytes, or off to disable rotation")
		}
		settings.MaxBytes = maxBytes
		return config.NormalizeAuditSettings(*settings).MaxBytes, nil
	case "audit.keep_segments":
		if raw == "all" || raw == "unlimited" {
			settings.KeepSegments = config.AuditKeepAllSegments
			return settings.KeepSegments, nil
		}
		keep, err := strconv.Atoi(raw)
		if err != nil {
			return nil, fmt.Errorf("audit.keep_segments must be a whole number, or all to keep every segment")
		}
		if keep > config.MaxAuditKeepSegments {
			return nil, fmt.Errorf("audit.keep_segments must be <= %d", config.MaxAuditKeepSegments)
		}
		settings.KeepSegments = keep
		return config.NormalizeAuditSettings(*settings).KeepSegments, nil
	}
	return nil, fmt.Errorf("unknown config key: %s", key)
}

// resetAuditConfigValue restores one audit key to its built-in default.
func resetAuditConfigValue(settings *config.AuditSettings, key string) {
	switch key {
	case "audit.max_bytes":
		settings.MaxBytes = config.DefaultAuditMaxBytes
	case "audit.keep_segments":
		settings.KeepSegments = config.DefaultAuditKeepSegments
	}
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
