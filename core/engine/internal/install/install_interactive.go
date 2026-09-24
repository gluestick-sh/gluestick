package install

import (
	"regexp"
	"strings"

	etypes "github.com/gluestick-sh/core/engine/types"
)

const installOptionInteractive = "interactive"

func installInteractive(req *etypes.InstallRequest) bool {
	if req == nil || req.Options == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(req.Options[installOptionInteractive]), "true")
}

// adaptInstallerScriptForInteractive rewrites common Scoop silent-install patterns so
// external setup UIs remain visible (e.g. Cygwin setup.exe).
func adaptInstallerScriptForInteractive(hooks []string) []string {
	if len(hooks) == 0 {
		return hooks
	}
	out := make([]string, len(hooks))
	for i, line := range hooks {
		s := line
		s = reInstallerWindowHidden.ReplaceAllString(s, "")
		s = reInstallerQuietMode.ReplaceAllString(s, "")
		s = strings.ReplaceAll(s, ", ,", ", ")
		s = strings.ReplaceAll(s, ",)", ")")
		s = strings.ReplaceAll(s, "(,", "(")
		out[i] = strings.TrimSpace(s)
	}
	return out
}

var (
	reInstallerWindowHidden = regexp.MustCompile(`(?i)-WindowStyle\s+('Hidden'|"Hidden"|Hidden)\s*`)
	reInstallerQuietMode    = regexp.MustCompile(`(?i)['"]--quiet-mode['"]\s*,?\s*`)
)
