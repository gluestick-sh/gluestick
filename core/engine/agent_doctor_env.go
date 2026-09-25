// Environment readiness checks for the grouped glue doctor view: the machine
// and shell sections (roadmap section 3.3). Everything here is advisory — the
// six Glue plumbing checks decide readiness and the process exit code.
package engine

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/gluestick-sh/core/message"
	"github.com/gluestick-sh/core/shim"
)

// agentCheckMachineOS verifies the Windows build is new enough for current
// agent toolchains (Windows 10 build 10240 / 1809 is the floor).
func agentCheckMachineOS() DoctorCheck {
	c := DoctorCheck{ID: message.AgentCheckMachineOS, Level: AgentLevelAdvisory}
	product, build := agentOSInfo()
	c.Data = map[string]any{"product": product, "build": build}
	switch {
	case build == 0:
		c.Status = AgentStatusSkipped
		c.DetailKey = message.AgentMachineOSUnknown
		c.DetailText = "version query unavailable"
	case build < 10240:
		c.Status = AgentStatusFail
		c.DetailKey = message.AgentMachineOSUnsupported
		c.DetailText = agentOSName(product, build)
		c.HintKey = message.AgentHintMachineOS
		c.Hint = doctorHint(c.HintKey)
	default:
		c.OK = true
		c.Status = AgentStatusPass
		c.DetailKey = message.AgentMachineOSOK
		c.DetailText = agentOSName(product, build)
	}
	return c
}

// agentOSName renders "Windows 11, build 26100" even when ProductName is stale.
func agentOSName(product string, build int) string {
	if product == "" {
		product = "Windows"
	}
	return fmt.Sprintf("%s, build %d", product, build)
}

// agentCheckMachineArch reports the CPU architecture (informational pass: the
// running binary proves the architecture works).
func agentCheckMachineArch() DoctorCheck {
	c := DoctorCheck{ID: message.AgentCheckMachineArch, Level: AgentLevelAdvisory}
	label := map[string]string{"amd64": "x64", "386": "x86", "arm64": "ARM64"}[runtime.GOARCH]
	if label == "" {
		label = runtime.GOARCH
	}
	c.OK = true
	c.Status = AgentStatusPass
	c.DetailKey = message.AgentMachineArchOK
	c.DetailText = label
	c.Data = map[string]any{"goarch": runtime.GOARCH}
	return c
}

// agentCheckShellPWSH prefers PowerShell 7 (pwsh); absence is advisory because
// Windows PowerShell 5.1 still exists, just with older language support.
func agentCheckShellPWSH() DoctorCheck {
	c := DoctorCheck{ID: message.AgentCheckShellPWSH, Level: AgentLevelAdvisory}
	path, err := exec.LookPath("pwsh")
	if err != nil {
		c.Status = AgentStatusFail
		c.DetailKey = message.AgentShellPWSHMissing
		c.DetailText = "Windows PowerShell 5.1 only"
		c.HintKey = message.AgentHintPWSH
		c.Hint = doctorHint(c.HintKey)
		return c
	}
	version := agentPWSHVersion(path)
	c.OK = true
	c.Status = AgentStatusPass
	c.DetailKey = message.AgentShellPWSHOK
	c.DetailText = version
	if version == "" {
		c.DetailText = path
	}
	c.Data = map[string]any{"path": path, "version": version}
	return c
}

// agentCheckShellUTF8 guards against child processes (PowerShell 5.1,
// chcp-default consoles) emitting codepage text that breaks agents parsing
// their output. Prefers the attached console's output codepage, falling back
// to the system ANSI codepage when glue runs piped.
func agentCheckShellUTF8() DoctorCheck {
	c := DoctorCheck{ID: message.AgentCheckShellUTF8, Level: AgentLevelAdvisory}
	cp, ok := agentOutputCodepage()
	if !ok {
		c.Status = AgentStatusSkipped
		c.DetailKey = message.AgentShellUTF8NA
		c.DetailText = "codepage query unavailable"
		return c
	}
	c.Data = map[string]any{"codepage": cp}
	if cp == 65001 {
		c.OK = true
		c.Status = AgentStatusPass
		c.DetailKey = message.AgentShellUTF8OK
		c.DetailText = fmt.Sprintf("codepage %d", cp)
		return c
	}
	c.Status = AgentStatusFail
	c.DetailKey = message.AgentShellUTF8Not
	c.DetailText = fmt.Sprintf("codepage %d", cp)
	c.HintKey = message.AgentHintUTF8
	c.Hint = doctorHint(c.HintKey)
	return c
}

// agentCheckShellExecPolicy reads the per-user PowerShell execution policy.
// Agents run .ps1 install scripts, so Restricted/AllSigned slows them down;
// this stays advisory because glue itself is a Go binary and unaffected.
func agentCheckShellExecPolicy() DoctorCheck {
	c := DoctorCheck{ID: message.AgentCheckShellExecPolicy, Level: AgentLevelAdvisory}
	value, known := agentPowerShellExecutionPolicy()
	if !known {
		c.Status = AgentStatusSkipped
		c.DetailKey = message.AgentShellExecPolicyNA
		c.DetailText = "registry query unavailable"
		return c
	}
	c.Data = map[string]any{"policy": value}
	switch strings.ToLower(value) {
	case "remotesigned", "unrestricted", "bypass", "process":
		c.OK = true
		c.Status = AgentStatusPass
		c.DetailKey = message.AgentShellExecPolicyOK
		c.DetailText = value
	default:
		// "" (never configured) behaves like Restricted on client Windows.
		c.Status = AgentStatusFail
		c.DetailKey = message.AgentShellExecPolicyWarn
		c.DetailText = value
		if c.DetailText == "" {
			c.DetailText = "not configured (defaults to Restricted)"
		}
		c.HintKey = message.AgentHintExecPolicy
		c.Hint = doctorHint(c.HintKey)
	}
	return c
}

// agentCheckShellGitBash warns only when a Git Bash (or other distro bash) on
// PATH resolves before the glue shims and can therefore shadow them. A Git Bash
// behind the shim directory is harmless, so the check passes and
// `glue doctor --fix` (path_setup) can genuinely resolve a failure. WSL's
// System32\bash.exe launcher is excluded here and covered by ws_wsl instead.
func agentCheckShellGitBash(root string) DoctorCheck {
	c := DoctorCheck{ID: message.AgentCheckShellGitBash, Level: AgentLevelAdvisory}
	path, err := exec.LookPath("bash")
	if err != nil {
		c.OK = true
		c.Status = AgentStatusPass
		c.DetailKey = message.AgentShellGitBashAbsent
		c.Data = map[string]any{"found": false}
		return c
	}
	c.Data = map[string]any{"found": true, "path": path}
	if gitBashShadowsShims(path, agentShimDir(root), os.Getenv("PATH")) {
		c.Status = AgentStatusFail
		c.DetailKey = message.AgentShellGitBashPresent
		c.DetailText = path
		c.HintKey = message.AgentHintGitBash
		c.Hint = doctorHint(c.HintKey)
		c.Data["shadow"] = true
		return c
	}
	c.OK = true
	c.Status = AgentStatusPass
	c.DetailKey = message.AgentShellGitBashBehind
	c.DetailText = path
	c.Data["shadow"] = false
	return c
}

// agentShimDir resolves the glue shims directory for root ("" when unknown).
func agentShimDir(root string) string {
	mgr, err := shim.NewManager(root)
	if err != nil {
		return ""
	}
	return mgr.BinDir()
}

// gitBashShadowsShims reports whether a non-WSL bashPath resolves from a
// directory that precedes shimDir on pathList. An unknown shimDir or a WSL
// System32 launcher is never shadowing.
func gitBashShadowsShims(bashPath, shimDir, pathList string) bool {
	lower := strings.ToLower(filepath.Clean(bashPath))
	if strings.Contains(lower, `\windows\system32\`) || strings.Contains(lower, `\syswow64\`) {
		return false
	}
	if shimDir == "" {
		return false
	}
	return PathDirPrecedes(pathList, filepath.Dir(bashPath), shimDir)
}

// agentCheckAgents reports which agent CLIs resolve on PATH. Readiness never
// depends on their presence (glue prepares the machine for any agent), so the
// check passes once at least one is installed and stays advisory otherwise.
//
// The verdict is CLI-only on purpose: readiness means an agent can drive the
// whole chain unattended. Data carries a second view for machines run from an
// IDE: data.clients lists the MCP-capable clients detected on disk and whether
// they already register glue (advisory-only, never changes the verdict).
func agentCheckAgents() DoctorCheck {
	c := DoctorCheck{ID: message.AgentCheckAgents, Level: AgentLevelAdvisory}
	specs := []struct {
		display string
		cmd     string
	}{
		{"Claude Code", "claude"},
		{"Codex", "codex"},
		{"OpenCode", "opencode"},
	}
	tools := make([]CommonToolProbe, 0, len(specs))
	found := []string{}
	for _, spec := range specs {
		p := CommonToolProbe{Name: spec.display}
		if resolved, err := exec.LookPath(spec.cmd); err == nil {
			p.Found = true
			p.Path = resolved
			found = append(found, spec.cmd)
		}
		tools = append(tools, p)
	}
	clients := probeAgentClients(currentAgentClientEnv())
	c.Data = map[string]any{"tools": tools, "found": found, "clients": clients}
	if len(found) > 0 {
		c.OK = true
		c.DetailKey = message.AgentAgentsFound
		c.DetailText = strings.Join(found, ", ")
	} else {
		c.DetailKey = message.AgentAgentsNone
		c.DetailText = "claude, codex, opencode not found"
		c.HintKey = message.AgentHintAgents
		c.Hint = doctorHint(c.HintKey)
	}
	c.Status = statusFromOK(c.OK)
	return c
}

// agentCheckWorkspaceFS verifies the data root lives on NTFS: FAT-family
// filesystems break filenames that package manifests rely on.
func agentCheckWorkspaceFS(root string) DoctorCheck {
	c := DoctorCheck{ID: message.AgentCheckWorkspaceFS, Level: AgentLevelAdvisory}
	volume := filepath.VolumeName(root)
	if volume == "" {
		c.Status = AgentStatusSkipped
		c.DetailKey = message.AgentWorkspaceFSUnknown
		c.DetailText = "no drive volume in data root path"
		return c
	}
	fsName, ok := agentVolumeFileSystem(volume + string(filepath.Separator))
	if !ok {
		c.Status = AgentStatusSkipped
		c.DetailKey = message.AgentWorkspaceFSUnknown
		c.DetailText = volume
		return c
	}
	c.Data = map[string]any{"volume": volume, "filesystem": fsName}
	// The key text already says the filesystem name; DetailText only adds the volume.
	detail := volume
	if strings.EqualFold(fsName, "NTFS") {
		c.OK = true
		c.Status = AgentStatusPass
		c.DetailKey = message.AgentWorkspaceFSOK
		c.DetailText = detail
		return c
	}
	c.Status = AgentStatusFail
	c.DetailKey = message.AgentWorkspaceFSOther
	c.DetailText = detail
	c.HintKey = message.AgentHintWorkspaceFS
	c.Hint = doctorHint(c.HintKey)
	return c
}

// agentCheckWorkspaceWSL warns when WSL is installed: agents may end up
// operating through \\wsl$ paths where shims and git behave differently.
func agentCheckWorkspaceWSL() DoctorCheck {
	c := DoctorCheck{ID: message.AgentCheckWorkspaceWSL, Level: AgentLevelAdvisory}
	path, err := exec.LookPath("wsl.exe")
	if err != nil {
		c.OK = true
		c.Status = AgentStatusPass
		c.DetailKey = message.AgentWorkspaceWSLAbsent
		c.Data = map[string]any{"installed": false}
		return c
	}
	c.Status = AgentStatusFail
	c.DetailKey = message.AgentWorkspaceWSLPresent
	c.DetailText = path
	c.HintKey = message.AgentHintWorkspaceWSL
	c.Hint = doctorHint(c.HintKey)
	c.Data = map[string]any{"installed": true, "path": path}
	return c
}

// agentCheckWorkspacePath fails when the data root sits on UNC/WSL/network
// locations where zero-interaction installs silently break.
func agentCheckWorkspacePath(root string) DoctorCheck {
	c := DoctorCheck{ID: message.AgentCheckWorkspacePath, Level: AgentLevelAdvisory}
	issue := agentWorkspacePathIssue(root)
	remote := agentDriveIsRemote(root)
	c.Data = map[string]any{"issue": issue, "remote_drive": remote}
	if issue == "" && !remote {
		c.OK = true
		c.Status = AgentStatusPass
		c.DetailKey = message.AgentWorkspacePathOK
		return c
	}
	c.Status = AgentStatusFail
	c.DetailKey = message.AgentWorkspacePathBad
	c.DetailText = root
	c.HintKey = message.AgentHintWorkspacePath
	c.Hint = doctorHint(c.HintKey)
	return c
}

// agentWorkspacePathIssue classifies path shapes that break agent work: WSL
// locations (\\wsl$\ / \\wsl.localhost\) and any other UNC path. Pure string
// logic so tests cover it without touching the filesystem.
func agentWorkspacePathIssue(path string) string {
	p := strings.ToLower(strings.TrimSpace(path))
	switch {
	case strings.HasPrefix(p, `\\wsl$\`) || strings.HasPrefix(p, `\\wsl.localhost\`):
		return "wsl"
	case strings.HasPrefix(p, `\\`):
		return "unc"
	default:
		return ""
	}
}
