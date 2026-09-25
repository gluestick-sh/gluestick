package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/gluestick-sh/core/config"
)

// configKey is the single declaration of one config.json key exposed by
// `glue config`: its dotted name, its flat JSON key, and the get/set/unset
// operations over the loaded settings. The get/set/unset/list handlers and the
// "available keys" help all derive from this table, so adding a key is a
// one-entry change instead of edits across several switches and the help text.
type configKey struct {
	name      string // dotted key, e.g. "agent.policy.mode"
	jsonKey   string // flat key used by `config list --json`, e.g. "agent_policy_mode"
	emitSet   bool   // also emit "<jsonKey>_set" in `config list --json`
	get       func() (any, bool)
	listValue func() any // optional: `config list --json` value; falls back to get when nil
	set       func(string) (any, error)
	unset     func() bool // resets the key and reports whether it was previously set
	text      func() string
	write     func() error // persists the mutated settings (plus side effects)
}

// configKeyTable builds the key registry against one loaded config snapshot.
func configKeyTable(cfg *config.Basics, agent *config.AgentSettings, audit *config.AuditSettings, root string) []configKey {
	writeBasics := func() error {
		if err := saveConfig(root, cfg); err != nil {
			return err
		}
		applyConfig(cfg)
		return nil
	}
	writeAgent := func() error { return config.WriteAgent(root, *agent) }
	writeAudit := func() error { return config.WriteAudit(root, *audit) }

	return []configKey{
		{
			name:    "github_proxy",
			jsonKey: "github_proxy",
			emitSet: true,
			get:     func() (any, bool) { return cfg.GitHubProxy, cfg.GitHubProxy != "" },
			set:     func(v string) (any, error) { cfg.GitHubProxy = v; return v, nil },
			unset:   func() bool { was := cfg.GitHubProxy != ""; cfg.GitHubProxy = ""; return was },
			text: func() string {
				if cfg.GitHubProxy == "" {
					return "github_proxy = (not set, direct GitHub)"
				}
				return fmt.Sprintf("github_proxy = %s", cfg.GitHubProxy)
			},
			write: writeBasics,
		},
		{
			name:    "parallel_download",
			jsonKey: "parallel_download",
			emitSet: true,
			get:     func() (any, bool) { return configTriBool(cfg.ParallelDownload, true) },
			set:     boolSetter(&cfg.ParallelDownload),
			unset:   boolUnsetter(&cfg.ParallelDownload),
			text: func() string {
				return fmt.Sprintf("parallel_download = %s", formatParallelDownload(cfg.ParallelDownload))
			},
			write: writeBasics,
		},
		{
			name:    "color",
			jsonKey: "color",
			emitSet: true,
			get:     func() (any, bool) { return configTriBool(cfg.Color, true) },
			set:     boolSetter(&cfg.Color),
			unset:   boolUnsetter(&cfg.Color),
			text:    func() string { return fmt.Sprintf("color = %s", formatColor(cfg.Color)) },
			write:   writeBasics,
		},
		{
			name:    "verbose",
			jsonKey: "verbose",
			emitSet: true,
			get:     func() (any, bool) { return configTriBool(cfg.Verbose, false) },
			set:     boolSetter(&cfg.Verbose),
			unset:   boolUnsetter(&cfg.Verbose),
			text:    func() string { return fmt.Sprintf("verbose = %s", formatVerbose(cfg.Verbose)) },
			write:   writeBasics,
		},
		{
			name:    "agent.auto_yes",
			jsonKey: "agent_auto_yes",
			get:     func() (any, bool) { return agent.AutoYes, agent.AutoYes },
			set: func(v string) (any, error) {
				b, err := parseConfigBool(v)
				if err != nil {
					return nil, err
				}
				agent.AutoYes = b
				return b, nil
			},
			unset: func() bool { was := agent.AutoYes; agent.AutoYes = false; return was },
			text:  func() string { return fmt.Sprintf("agent.auto_yes = %v", agent.AutoYes) },
			write: writeAgent,
		},
		{
			name:    "agent.policy.mode",
			jsonKey: "agent_policy_mode",
			get:     func() (any, bool) { return agent.Policy.Mode, true },
			set: func(v string) (any, error) {
				mode := strings.ToLower(strings.TrimSpace(v))
				if mode != config.AgentPolicyModeStrict && mode != config.AgentPolicyModeConfirm && mode != config.AgentPolicyModeAuto {
					return nil, fmt.Errorf("agent.policy.mode must be strict, confirm or auto")
				}
				agent.Policy.Mode = mode
				return mode, nil
			},
			unset: func() bool { agent.Policy.Mode = config.AgentPolicyModeConfirm; return true },
			text:  func() string { return fmt.Sprintf("agent.policy.mode = %s", agent.Policy.Mode) },
			write: writeAgent,
		},
		{
			name:      "agent.policy.deny",
			jsonKey:   "agent_policy_deny",
			get:       func() (any, bool) { return strings.Join(agent.Policy.Deny, ","), len(agent.Policy.Deny) > 0 },
			listValue: func() any { return agent.Policy.Deny },
			set:       func(v string) (any, error) { agent.Policy.Deny = splitConfigList(v); return agent.Policy.Deny, nil },
			unset:     func() bool { was := len(agent.Policy.Deny) > 0; agent.Policy.Deny = []string{}; return was },
			text:      func() string { return fmt.Sprintf("agent.policy.deny = %s", strings.Join(agent.Policy.Deny, ",")) },
			write:     writeAgent,
		},
		{
			name:      "agent.policy.protected",
			jsonKey:   "agent_policy_protected",
			get:       func() (any, bool) { return strings.Join(agent.Policy.Protected, ","), len(agent.Policy.Protected) > 0 },
			listValue: func() any { return agent.Policy.Protected },
			set: func(v string) (any, error) {
				agent.Policy.Protected = splitConfigList(v)
				return agent.Policy.Protected, nil
			},
			unset: func() bool { was := len(agent.Policy.Protected) > 0; agent.Policy.Protected = []string{}; return was },
			text: func() string {
				return fmt.Sprintf("agent.policy.protected = %s", strings.Join(agent.Policy.Protected, ","))
			},
			write: writeAgent,
		},
		{
			name:    "audit.max_bytes",
			jsonKey: "audit_max_bytes",
			get:     func() (any, bool) { return audit.MaxBytes, true },
			set: func(v string) (any, error) {
				raw := strings.ToLower(strings.TrimSpace(v))
				if raw == "off" || raw == "none" || raw == "disabled" {
					audit.MaxBytes = config.AuditRotationDisabled
					return audit.MaxBytes, nil
				}
				maxBytes, err := strconv.ParseInt(raw, 10, 64)
				if err != nil {
					return nil, fmt.Errorf("audit.max_bytes must be a whole number of bytes, or off to disable rotation")
				}
				audit.MaxBytes = maxBytes
				return config.NormalizeAuditSettings(*audit).MaxBytes, nil
			},
			unset: func() bool { audit.MaxBytes = config.DefaultAuditMaxBytes; return true },
			text:  func() string { return fmt.Sprintf("audit.max_bytes = %d", audit.MaxBytes) },
			write: writeAudit,
		},
		{
			name:    "audit.keep_segments",
			jsonKey: "audit_keep_segments",
			get:     func() (any, bool) { return audit.KeepSegments, true },
			set: func(v string) (any, error) {
				raw := strings.ToLower(strings.TrimSpace(v))
				if raw == "all" || raw == "unlimited" {
					audit.KeepSegments = config.AuditKeepAllSegments
					return audit.KeepSegments, nil
				}
				keep, err := strconv.Atoi(raw)
				if err != nil {
					return nil, fmt.Errorf("audit.keep_segments must be a whole number, or all to keep every segment")
				}
				if keep > config.MaxAuditKeepSegments {
					return nil, fmt.Errorf("audit.keep_segments must be <= %d", config.MaxAuditKeepSegments)
				}
				audit.KeepSegments = keep
				return config.NormalizeAuditSettings(*audit).KeepSegments, nil
			},
			unset: func() bool { audit.KeepSegments = config.DefaultAuditKeepSegments; return true },
			text:  func() string { return fmt.Sprintf("audit.keep_segments = %d", audit.KeepSegments) },
			write: writeAudit,
		},
		{
			name:    "audit.verify_interval_hours",
			jsonKey: "audit_verify_interval_hours",
			get:     func() (any, bool) { return audit.VerifyIntervalHours, true },
			set: func(v string) (any, error) {
				raw := strings.ToLower(strings.TrimSpace(v))
				if raw == "off" || raw == "never" || raw == "disabled" {
					audit.VerifyIntervalHours = config.AuditVerifyDisabled
					return audit.VerifyIntervalHours, nil
				}
				hours, err := strconv.Atoi(raw)
				if err != nil {
					return nil, fmt.Errorf("audit.verify_interval_hours must be a whole number of hours, or off to disable the scheduled verification")
				}
				audit.VerifyIntervalHours = hours
				return config.NormalizeAuditSettings(*audit).VerifyIntervalHours, nil
			},
			unset: func() bool { audit.VerifyIntervalHours = config.DefaultAuditVerifyIntervalHours; return true },
			text:  func() string { return fmt.Sprintf("audit.verify_interval_hours = %d", audit.VerifyIntervalHours) },
			write: writeAudit,
		},
	}
}

// boolSetter builds a tri-state bool setter for a *bool field.
func boolSetter(dst **bool) func(string) (any, error) {
	return func(v string) (any, error) {
		b, err := parseConfigBool(v)
		if err != nil {
			return nil, err
		}
		*dst = &b
		return b, nil
	}
}

// boolUnsetter builds a tri-state bool unsetter for a *bool field.
func boolUnsetter(dst **bool) func() bool {
	return func() bool {
		was := *dst != nil
		*dst = nil
		return was
	}
}

// findConfigKey returns the entry for a dotted key name, or nil if unknown.
func findConfigKey(table []configKey, name string) *configKey {
	for i := range table {
		if table[i].name == name {
			return &table[i]
		}
	}
	return nil
}

// configAvailableKeys renders the ordered dotted-key list for the "unknown key"
// help on `config set`.
func configAvailableKeys(table []configKey) string {
	var b strings.Builder
	for _, k := range table {
		b.WriteString("\n  " + k.name)
	}
	return b.String()
}
