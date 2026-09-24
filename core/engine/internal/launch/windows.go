//go:build windows

package launch

import (
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/gluestick-sh/core/procutil"
)

const cmdWindowTitle = "Command Prompt"

func openLaunchFile(absPath string, kind LaunchKind) error {
	switch kind {
	case LaunchKindConsole:
		return startInCmdWindow(absPath)
	case LaunchKindGUI:
		return startGUIExecutable(absPath)
	default:
		return errLaunchNotOpenable
	}
}

func startInCmdWindow(abs string) error {
	abs = filepath.Clean(abs)
	dir := filepath.Dir(abs)
	script := buildCmdLaunchScript(abs)
	// Let Go quote each argv for CreateProcess; avoid hand-built cmd strings (breaks backslashes).
	cmd := exec.Command("cmd", "/c", "start", "", "/D", dir, "cmd", "/k", script)
	procutil.HideWindow(cmd)
	return cmd.Start()
}

func buildCmdLaunchScript(abs string) string {
	abs = filepath.Clean(abs)
	lower := strings.ToLower(abs)
	var run string
	switch {
	case strings.HasSuffix(lower, ".jar"):
		run = "java -jar " + quoteCmdToken(abs)
	case strings.HasSuffix(lower, ".bat"), strings.HasSuffix(lower, ".cmd"):
		run = "call " + quoteCmdToken(abs)
	default:
		run = quoteCmdToken(abs)
	}
	return "title " + cmdWindowTitle + " && " + run
}

func quoteCmdToken(token string) string {
	token = filepath.Clean(token)
	if strings.ContainsAny(token, " \t&|<>^\"") {
		return `"` + strings.ReplaceAll(token, `"`, `""`) + `"`
	}
	return token
}

func startGUIExecutable(abs string) error {
	abs = filepath.Clean(abs)
	if strings.HasSuffix(strings.ToLower(abs), ".jar") {
		cmd := exec.Command("cmd", "/c", "start", "", "javaw", "-jar", abs)
		cmd.Dir = filepath.Dir(abs)
		procutil.HideWindow(cmd)
		return cmd.Start()
	}
	cmd := exec.Command("cmd", "/c", "start", "", abs)
	cmd.Dir = filepath.Dir(abs)
	procutil.HideWindow(cmd)
	return cmd.Start()
}
