// Duplicate runtime detection for the readiness view: two installs of the same
// tool on PATH (scoop Node.js plus nvm, two Pythons, ...) make agents run a
// different binary than the human expects. Detection only — glue never removes
// a tool, so this check stays advisory and its --fix step is a report.
package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gluestick-sh/core/message"
)

// DuplicateTool is one runtime resolving from more than one PATH entry.
type DuplicateTool struct {
	Name  string   `json:"name"`
	Paths []string `json:"paths"` // full file paths, PATH order
}

// agentDuplicateTools are the runtimes worth detecting; exe names are the
// Windows entry points (cmd shims count too, e.g. scoop's npm.cmd).
var agentDuplicateTools = []struct {
	Name string
	Exes []string
}{
	{"node", []string{"node.exe"}},
	{"npm", []string{"npm.cmd", "npm.exe"}},
	{"python", []string{"python.exe"}},
	{"git", []string{"git.exe"}},
}

// agentFindDuplicateTools scans PATH entries (in order) for repeated runtimes.
// Entries inside the glue data root are glue's own shims/store and never count
// as duplicates; path comparisons are cleaned and case-folded first.
func agentFindDuplicateTools(dataRoot string) []DuplicateTool {
	rootKey := agentPathKey(dataRoot)
	seenDirs := map[string]bool{}
	dirs := []string{}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		dir = strings.TrimSpace(dir)
		if dir == "" {
			continue
		}
		key := agentPathKey(dir)
		if seenDirs[key] {
			continue
		}
		seenDirs[key] = true
		if rootKey != "" && (key == rootKey || strings.HasPrefix(key, rootKey+string(filepath.Separator))) {
			continue
		}
		dirs = append(dirs, dir)
	}
	dups := []DuplicateTool{}
	for _, tool := range agentDuplicateTools {
		paths := []string{}
		for _, dir := range dirs {
			for _, exe := range tool.Exes {
				if full := filepath.Join(dir, exe); agentFileExists(full) {
					paths = append(paths, full)
					break // one hit per directory is enough
				}
			}
		}
		if len(paths) > 1 {
			dups = append(dups, DuplicateTool{Name: tool.Name, Paths: paths})
		}
	}
	return dups
}

// agentPathKey normalizes a directory for comparison (clean + case-fold).
func agentPathKey(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	return strings.ToLower(filepath.Clean(p))
}

// agentFileExists reports whether path is an existing regular file.
func agentFileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// agentCheckDuplicates reports runtimes that resolve from more than one PATH
// entry. Advisory: glue never uninstalls a tool; --fix only surfaces the list.
func agentCheckDuplicates(dataRoot string) DoctorCheck {
	c := DoctorCheck{ID: message.AgentCheckDuplicates, Level: AgentLevelAdvisory}
	dups := agentFindDuplicateTools(dataRoot)
	c.Data = map[string]any{"duplicates": dups}
	if len(dups) == 0 {
		c.OK = true
		c.Status = AgentStatusPass
		c.DetailKey = message.AgentDuplicatesNone
		return c
	}
	names := make([]string, 0, len(dups))
	for _, d := range dups {
		names = append(names, fmt.Sprintf("%s (%d)", d.Name, len(d.Paths)))
	}
	c.Status = AgentStatusFail
	c.DetailKey = message.AgentDuplicatesFound
	c.DetailText = strings.Join(names, ", ")
	c.HintKey = message.AgentHintDuplicates
	c.Hint = doctorHint(c.HintKey)
	return c
}
