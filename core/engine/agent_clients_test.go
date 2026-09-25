package engine

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gluestick-sh/core/message"
)

// TestAgentActionForCheck pins the next-action split for buckets: "add main"
// only when no bucket is installed, otherwise the reason's own remediation.
func TestAgentActionForCheck(t *testing.T) {
	cases := []struct {
		check DoctorCheck
		want  string
	}{
		{DoctorCheck{ID: message.AgentCheckBuckets, DetailKey: message.AgentBucketsEmpty}, "glue bucket add main"},
		{DoctorCheck{ID: message.AgentCheckBuckets, DetailKey: message.AgentBucketsIndexNotReady}, "glue doctor"},
		{DoctorCheck{ID: message.AgentCheckBuckets, DetailKey: message.AgentBucketsNoManifests}, "glue bucket update"},
		{DoctorCheck{ID: message.AgentCheckShimPath}, "glue path setup"},
		{DoctorCheck{ID: "not_a_blocking_check"}, ""},
	}
	for _, tc := range cases {
		if got := agentActionForCheck(tc.check); got != tc.want {
			t.Fatalf("agentActionForCheck(%s/%s) = %q, want %q", tc.check.ID, tc.check.DetailKey, got, tc.want)
		}
	}
}

// TestBucketsCheckVerdict pins the four outcomes behind the blocking `buckets`
// check, especially the hint split: "add main" only helps when no bucket is
// installed, while an installed-but-unindexed bucket needs a rescan (the state
// observed when another glue process held the data root).
func TestBucketsCheckVerdict(t *testing.T) {
	cases := []struct {
		name       string
		bucketN    int
		indexed    int
		total      int
		indexReady bool
		wantOK     bool
		wantDetail string
		wantHint   string
	}{
		{
			name:       "no buckets",
			bucketN:    0,
			indexReady: false,
			wantDetail: message.AgentBucketsEmpty,
			wantHint:   message.AgentHintBuckets,
		},
		{
			name:       "buckets present, index not ready",
			bucketN:    2,
			indexed:    0,
			total:      0,
			indexReady: false,
			wantDetail: message.AgentBucketsIndexNotReady,
			wantHint:   message.AgentHintBucketsIndexBusy,
		},
		{
			name:       "index ready but empty",
			bucketN:    1,
			indexed:    0,
			total:      0,
			indexReady: true,
			wantDetail: message.AgentBucketsNoManifests,
			wantHint:   message.AgentHintBucketsNoManifests,
		},
		{
			name:       "ready",
			bucketN:    2,
			indexed:    2,
			total:      4026,
			indexReady: true,
			wantOK:     true,
			wantDetail: message.AgentBucketsOK,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := bucketsCheckVerdict(tc.bucketN, tc.indexed, tc.total, tc.indexReady)
			if got.ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (%+v)", got.ok, tc.wantOK, got)
			}
			if got.detailKey != tc.wantDetail {
				t.Fatalf("detailKey = %q, want %q", got.detailKey, tc.wantDetail)
			}
			if got.hintKey != tc.wantHint {
				t.Fatalf("hintKey = %q, want %q", got.hintKey, tc.wantHint)
			}
			if got.detail == "" {
				t.Fatal("every verdict must carry a readable detail")
			}
		})
	}

	// The unindexed case must not advise installing a bucket that exists.
	unindexed := bucketsCheckVerdict(2, 0, 0, false)
	if unindexed.hintKey == message.AgentHintBuckets {
		t.Fatal("installed-but-unindexed buckets must not get the add-a-bucket hint")
	}
}

// detected on disk and whether their config already registers glue. The probes
// read a caller-supplied env so the test never touches the real profile.
func TestProbeAgentClients(t *testing.T) {
	profile := t.TempDir()
	appData := t.TempDir()

	writeFile := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}

	// Cline: installed and wired to glue (servers live in the extension settings).
	writeFile(filepath.Join(appData, "Code", "User", "globalStorage", "saoudrizwan.claude-dev", "settings", "cline_mcp_settings.json"),
		`{"mcpServers":{"glue":{"command":"C:\\glue-alpha.exe","args":["mcp"]}}}`)

	// Cursor: agent worker present, global MCP config exists without glue.
	if err := os.MkdirAll(filepath.Join(appData, "Cursor", "User", "globalStorage", "anysphere.cursor-agent-worker"), 0755); err != nil {
		t.Fatal(err)
	}
	writeFile(filepath.Join(profile, ".cursor", "mcp.json"), `{"mcpServers":{"other":{}}}`)

	// Claude Desktop: config file exists (that is the install marker) but has no servers.
	writeFile(filepath.Join(appData, "Claude", "claude_desktop_config.json"), `{"preferences":{"coworkWebSearchEnabled":true}}`)

	probes := probeAgentClients(agentClientEnv{UserProfile: profile, AppData: appData})
	byName := map[string]AgentClientProbe{}
	for _, p := range probes {
		byName[p.Name] = p
	}
	if len(probes) != 5 {
		t.Fatalf("probes = %d, want the 5 documented clients: %+v", len(probes), probes)
	}

	cline := byName["Cline"]
	if !cline.Detected || !cline.Wired || cline.Kind != "ide-extension" {
		t.Fatalf("Cline probe = %+v, want detected+wired ide-extension", cline)
	}
	if cline.Config == "" {
		t.Fatalf("Cline must report the config it inspected: %+v", cline)
	}

	cursor := byName["Cursor agent"]
	if !cursor.Detected || cursor.Wired || cursor.Config == "" {
		t.Fatalf("Cursor probe = %+v, want detected with an unregistered config", cursor)
	}

	desktop := byName["Claude Desktop"]
	if !desktop.Detected || desktop.Wired || desktop.Config == "" {
		t.Fatalf("Claude Desktop probe = %+v, want detected, not wired", desktop)
	}

	copilot := byName["GitHub Copilot Chat"]
	if copilot.Detected || copilot.Wired || copilot.Config != "" {
		t.Fatalf("Copilot probe = %+v, want undetected without a config file", copilot)
	}

	claudeCode := byName["Claude Code (profile)"]
	if claudeCode.Detected || claudeCode.Wired {
		t.Fatalf("Claude Code profile probe = %+v, want undetected (no ~/.claude.json in the temp profile)", claudeCode)
	}
}

// TestProbeAgentClients_emptyEnv keeps the probes safe when the profile roots are
// missing (non-Windows builds, stripped environments).
func TestProbeAgentClients_emptyEnv(t *testing.T) {
	probes := probeAgentClients(agentClientEnv{})
	if len(probes) == 0 {
		t.Fatal("expected the full client matrix even without roots")
	}
	for _, p := range probes {
		if p.Detected || p.Wired || p.Config != "" || p.Evidence != "" {
			t.Fatalf("probe %+v must stay empty without profile roots", p)
		}
	}
}

// TestAgentCheckAgents_carriesClients proves the agents check still decides on
// CLIs while attaching the client matrix as advisory data.
func TestAgentCheckAgents_carriesClients(t *testing.T) {
	check := agentCheckAgents()
	clients, ok := check.Data["clients"].([]AgentClientProbe)
	if !ok || len(clients) == 0 {
		t.Fatalf("agents check must carry data.clients: %+v", check.Data)
	}
	tools, ok := check.Data["tools"].([]CommonToolProbe)
	if !ok || len(tools) != 3 {
		t.Fatalf("agents check must keep the CLI probe rows: %+v", check.Data["tools"])
	}
	if check.Level != AgentLevelAdvisory {
		t.Fatalf("agents must stay advisory: %+v", check)
	}
}
