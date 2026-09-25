// Agent client detection for the readiness report (roadmap section 3.3).
//
// `agents` keeps its meaning: agent CLIs that resolve on PATH, because readiness
// is about agents that can drive the whole install -> PATH -> doctor -> run
// chain unattended. Machines driven from inside an IDE still deserve
// visibility, so the check also carries data.clients: MCP-capable clients
// detected on disk plus whether they are already wired to Glue's MCP server.
// The probes are advisory-only and never change the verdict.
package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// AgentClientProbe is one data.clients row: an MCP-capable client found (or not
// found) on this machine, and whether its MCP config already registers Glue.
type AgentClientProbe struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"` // cli | ide-extension | desktop
	Detected bool   `json:"detected"`
	Evidence string `json:"evidence,omitempty"` // the path that proved the client is installed
	Wired    bool   `json:"wired"`              // a "glue" entry exists in Config
	Config   string `json:"config,omitempty"`   // MCP config file checked (empty when none exists)
}

// agentClientEnv are the roots the probes derive paths from, so tests can point
// them at a temp directory instead of the real user profile.
type agentClientEnv struct {
	UserProfile string
	AppData     string
}

// currentAgentClientEnv reads the real profile roots (empty on non-Windows,
// which simply yields "not detected" rows).
func currentAgentClientEnv() agentClientEnv {
	return agentClientEnv{
		UserProfile: os.Getenv("USERPROFILE"),
		AppData:     os.Getenv("APPDATA"),
	}
}

// mcpConfigSpec is one MCP config file to inspect for a Glue entry.
type mcpConfigSpec struct {
	path string
	// key is the top-level object holding servers: "mcpServers" for Cline,
	// Claude Code/Desktop and Cursor, "servers" for VS Code.
	key string
}

// agentClientSpec describes one client: the paths that prove it is installed and
// the MCP config files Glue would be registered in.
type agentClientSpec struct {
	name    string
	kind    string
	markers []string
	configs []mcpConfigSpec
}

// clientPaths builds derived paths under root; an empty root yields no paths so
// a blank APPDATA/USERPROFILE cannot produce bogus relative paths.
func clientPaths(root string, parts ...string) []string {
	if strings.TrimSpace(root) == "" {
		return nil
	}
	return []string{filepath.Join(append([]string{root}, parts...)...)}
}

// configAt builds the single MCP config spec for root\parts... with the given
// top-level servers key.
func configAt(root, serversKey string, parts ...string) []mcpConfigSpec {
	paths := clientPaths(root, parts...)
	if paths == nil {
		return nil
	}
	return []mcpConfigSpec{{path: paths[0], key: serversKey}}
}

// agentClientSpecs is the client matrix Glue reports: exactly the clients and
// config locations documented in docs/mcp.md.
func agentClientSpecs(env agentClientEnv) []agentClientSpec {
	var specs []agentClientSpec

	// Cline lives in the VS Code family globalStorage and keeps its MCP servers
	// in the extension's own settings file.
	clineMarkers := []string{}
	for _, variant := range []string{"Code", "Code - Insiders", "Cursor"} {
		clineMarkers = append(clineMarkers, clientPaths(env.AppData, variant, "User", "globalStorage", "saoudrizwan.claude-dev")...)
	}
	clineConfigs := make([]mcpConfigSpec, 0, len(clineMarkers))
	for _, marker := range clineMarkers {
		clineConfigs = append(clineConfigs, mcpConfigSpec{
			path: filepath.Join(marker, "settings", "cline_mcp_settings.json"),
			key:  "mcpServers",
		})
	}
	specs = append(specs, agentClientSpec{
		name:    "Cline",
		kind:    "ide-extension",
		markers: clineMarkers,
		configs: clineConfigs,
	})

	specs = append(specs, agentClientSpec{
		name:    "GitHub Copilot Chat",
		kind:    "ide-extension",
		markers: clientPaths(env.AppData, "Code", "User", "globalStorage", "github.copilot-chat"),
		configs: configAt(env.AppData, "servers", "Code", "User", "mcp.json"),
	})

	specs = append(specs, agentClientSpec{
		name:    "Cursor agent",
		kind:    "ide-extension",
		markers: clientPaths(env.AppData, "Cursor", "User", "globalStorage", "anysphere.cursor-agent-worker"),
		configs: configAt(env.UserProfile, "mcpServers", ".cursor", "mcp.json"),
	})

	specs = append(specs, agentClientSpec{
		name:    "Claude Desktop",
		kind:    "desktop",
		markers: clientPaths(env.AppData, "Claude", "claude_desktop_config.json"),
		configs: configAt(env.AppData, "mcpServers", "Claude", "claude_desktop_config.json"),
	})

	specs = append(specs, agentClientSpec{
		// The CLI itself is covered by data.tools; this row is the on-disk
		// profile that also holds user-scope MCP servers.
		name:    "Claude Code (profile)",
		kind:    "cli",
		markers: clientPaths(env.UserProfile, ".claude.json"),
		configs: configAt(env.UserProfile, "mcpServers", ".claude.json"),
	})

	return specs
}

// probeAgentClients detects the client matrix and reports Glue wiring. The
// result always covers every spec so readers see what was probed, mirroring the
// data.tools rows.
func probeAgentClients(env agentClientEnv) []AgentClientProbe {
	specs := agentClientSpecs(env)
	probes := make([]AgentClientProbe, 0, len(specs))
	for _, spec := range specs {
		probe := AgentClientProbe{Name: spec.name, Kind: spec.kind}
		for _, marker := range spec.markers {
			if _, err := os.Stat(marker); err == nil {
				probe.Detected = true
				probe.Evidence = marker
				break
			}
		}
		for _, cfg := range spec.configs {
			wired, exists := mcpConfigRegistersGlue(cfg)
			if !exists {
				continue
			}
			probe.Config = cfg.path
			probe.Wired = wired
			break
		}
		probes = append(probes, probe)
	}
	return probes
}

// mcpConfigRegistersGlue reports whether the config file exists, parses, and has
// a "glue" server under its servers key. exists is false when the file is absent
// or unreadable, so callers can tell "no config" from "config without glue".
func mcpConfigRegistersGlue(spec mcpConfigSpec) (wired bool, exists bool) {
	if strings.TrimSpace(spec.path) == "" {
		return false, false
	}
	data, err := os.ReadFile(spec.path)
	if err != nil {
		return false, false
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(data, &doc); err != nil {
		return false, false
	}
	serversRaw, hasKey := doc[spec.key]
	if !hasKey {
		return false, true
	}
	var servers map[string]json.RawMessage
	if err := json.Unmarshal(serversRaw, &servers); err != nil {
		return false, true
	}
	_, hasGlue := servers["glue"]
	return hasGlue, true
}
