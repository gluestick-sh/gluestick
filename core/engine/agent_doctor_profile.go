// PowerShell profile integration for the readiness view: a marked glue block
// keeps shims first on PATH for new shells, switches the console to UTF-8, and
// replaces PS 5.1's ASCII stdin default that mangles piped agent input. The
// check only reads profiles; --fix writes them (with a backup), never by hand.
package engine

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/gluestick-sh/core/message"
)

// GlueProfileMarker delimits the block glue writes into PowerShell profiles;
// the check and the fix both key off it, so updates stay idempotent.
const (
	GlueProfileMarker = "# >>> glue >>>"
	glueProfileEnd    = "# <<< glue <<<"
)

// glueDocumentsDirs returns candidate Documents folders (OneDrive-redirected
// known folders included), deduplicated, in preference order.
func glueDocumentsDirs() []string {
	dirs := []string{}
	seen := map[string]bool{}
	add := func(p string) {
		if p == "" {
			return
		}
		key := strings.ToLower(filepath.Clean(p))
		if seen[key] {
			return
		}
		seen[key] = true
		dirs = append(dirs, p)
	}
	if od := os.Getenv("OneDrive"); od != "" {
		add(filepath.Join(od, "Documents"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		add(filepath.Join(home, "Documents"))
	}
	return dirs
}

// glueProfilePath resolves one shell's profile file ("WindowsPowerShell" or
// "PowerShell"): an existing profile wins, otherwise an existing Documents
// folder, otherwise the first candidate.
func glueProfilePath(sub string) string {
	rel := filepath.Join(sub, "Microsoft.PowerShell_profile.ps1")
	dirs := glueDocumentsDirs()
	for _, dir := range dirs {
		if p := filepath.Join(dir, rel); agentFileExists(p) {
			return p
		}
	}
	for _, dir := range dirs {
		if st, err := os.Stat(dir); err == nil && st.IsDir() {
			return filepath.Join(dir, rel)
		}
	}
	if len(dirs) > 0 {
		return filepath.Join(dirs[0], rel)
	}
	return ""
}

// GluePowerShellProfiles lists the profiles readiness expects: always the
// Windows PowerShell 5.1 profile, plus the pwsh profile when PowerShell 7 is
// installed (or already has a profile of its own).
func GluePowerShellProfiles() []string {
	targets := []string{}
	if p := glueProfilePath("WindowsPowerShell"); p != "" {
		targets = append(targets, p)
	}
	if p := glueProfilePath("PowerShell"); p != "" {
		if _, err := exec.LookPath("pwsh"); err == nil || agentFileExists(p) {
			targets = append(targets, p)
		}
	}
	return targets
}

// GlueProfileBlock renders the content between GlueProfileMarker lines.
// shimsDir may be empty to omit the PATH guard.
func GlueProfileBlock(shimsDir string) string {
	var b strings.Builder
	b.WriteString(GlueProfileMarker + "\n")
	if shimsDir != "" {
		escaped := strings.ReplaceAll(shimsDir, "'", "''")
		fmt.Fprintf(&b,
			"if (-not (($env:Path -split ';') -contains '%s')) { $env:Path = '%s;' + $env:Path }\n",
			escaped, escaped)
	}
	b.WriteString("chcp 65001 > $null\n")
	b.WriteString("$OutputEncoding = [System.Text.UTF8Encoding]::new($false)\n")
	b.WriteString(glueProfileEnd + "\n")
	return b.String()
}

// GlueProfileHasBlock reports whether the profile exists and carries the glue
// block (both markers present).
func GlueProfileHasBlock(path string) bool {
	if path == "" {
		return false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return bytes.Contains(data, []byte(GlueProfileMarker)) && bytes.Contains(data, []byte(glueProfileEnd))
}

// agentCheckShellProfile verifies every target profile carries the glue block.
// Advisory: interactive users may manage profiles by hand; --fix writes them.
func agentCheckShellProfile() DoctorCheck {
	c := DoctorCheck{ID: message.AgentCheckShellIntegration, Level: AgentLevelAdvisory}
	targets := GluePowerShellProfiles()
	ready := 0
	for _, t := range targets {
		if GlueProfileHasBlock(t) {
			ready++
		}
	}
	c.Data = map[string]any{"targets": targets, "ready": ready}
	c.DetailText = fmt.Sprintf("%d of %d profile(s)", ready, len(targets))
	if len(targets) > 0 && ready == len(targets) {
		c.OK = true
		c.Status = AgentStatusPass
		c.DetailKey = message.AgentShellProfileOK
		return c
	}
	c.Status = AgentStatusFail
	c.DetailKey = message.AgentShellProfileMissing
	c.HintKey = message.AgentHintShellProfile
	c.Hint = doctorHint(c.HintKey)
	return c
}
