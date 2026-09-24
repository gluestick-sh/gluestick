package install

import (
	"strings"

	etypes "github.com/gluestick-sh/core/engine/types"
	"github.com/gluestick-sh/core/manifest"
)

func installArchitecture(req *etypes.InstallRequest, m *manifest.Manifest) string {
	if req == nil || m == nil {
		return ""
	}
	if architectureExplicit(req) {
		return m.ArchitectureForInstall(strings.TrimSpace(req.Options["architecture"]))
	}
	return m.SelectedArchitecture()
}

func architectureExplicit(req *etypes.InstallRequest) bool {
	if req == nil || req.Options == nil {
		return false
	}
	return strings.TrimSpace(req.Options["architecture"]) != ""
}
