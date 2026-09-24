package manifest

import "strings"

// Scoop manifest architecture keys.
const (
	ArchARM64  = "arm64"
	Arch64bit  = "64bit"
	Arch32bit  = "32bit"
	win11Build = 22000
)

// architectureCandidates returns Scoop-style architecture preference for this host.
func architectureCandidates(hostArch string, winBuild int) []string {
	switch hostArch {
	case ArchARM64:
		if winBuild >= win11Build {
			return []string{ArchARM64, Arch64bit, Arch32bit}
		}
		return []string{ArchARM64, Arch32bit}
	case Arch64bit:
		return []string{Arch64bit, Arch32bit}
	case Arch32bit:
		return []string{Arch32bit}
	default:
		return []string{Arch64bit, Arch32bit}
	}
}

// SelectedArchitecture picks the manifest architecture block for this host.
func (m *Manifest) SelectedArchitecture() string {
	if m.Architecture == nil {
		return ""
	}
	for _, arch := range architectureCandidates(hostArchitecture(), hostWindowsBuild()) {
		block, ok := m.Architecture[arch].(map[string]interface{})
		if !ok {
			continue
		}
		if archBlockIsSelectable(block) {
			return arch
		}
	}
	return ""
}

// AvailableArchitectures lists manifest architecture keys that have download URLs.
func (m *Manifest) AvailableArchitectures() []string {
	if m == nil || m.Architecture == nil {
		return nil
	}
	var out []string
	for _, arch := range []string{ArchARM64, Arch64bit, Arch32bit} {
		block, ok := m.Architecture[arch].(map[string]interface{})
		if ok && archBlockIsSelectable(block) {
			out = append(out, arch)
		}
	}
	return out
}

// ArchitectureForInstall returns override when valid, otherwise host-selected architecture.
func (m *Manifest) ArchitectureForInstall(override string) string {
	if override != "" {
		block, ok := m.Architecture[override].(map[string]interface{})
		if ok && archBlockIsSelectable(block) {
			return override
		}
	}
	return m.SelectedArchitecture()
}

// FallbackArchitecture returns the next host-preferred architecture with a
// download URL after current. Used when the preferred arch's URL 404s
// (e.g. autoupdate templates an arm64 URL onto a version that never shipped one).
func (m *Manifest) FallbackArchitecture(current string) string {
	return m.fallbackArchitectureForHost(current, hostArchitecture(), hostWindowsBuild())
}

func (m *Manifest) fallbackArchitectureForHost(current, hostArch string, winBuild int) string {
	if m == nil || m.Architecture == nil || current == "" {
		return ""
	}
	seenCurrent := false
	for _, arch := range architectureCandidates(hostArch, winBuild) {
		if !seenCurrent {
			if arch == current {
				seenCurrent = true
			}
			continue
		}
		block, ok := m.Architecture[arch].(map[string]interface{})
		if !ok {
			continue
		}
		if archBlockHasURL(block) {
			return arch
		}
	}
	return ""
}

func (m *Manifest) selectedArchBlock() map[string]interface{} {
	return m.archBlockForInstall("")
}

func (m *Manifest) archBlockForInstall(override string) map[string]interface{} {
	if override != "" {
		block, ok := m.Architecture[override].(map[string]interface{})
		if ok && archBlockIsSelectable(block) {
			return block
		}
		return nil
	}
	arch := m.SelectedArchitecture()
	if arch == "" {
		return nil
	}
	block, _ := m.Architecture[arch].(map[string]interface{})
	return block
}

func archBlockHasURL(block map[string]interface{}) bool {
	urls := stringSliceFromField(block["url"])
	return len(urls) > 0
}

// archBlockIsSelectable reports architecture blocks used for install (URL or arch-only metadata like extract_dir).
func archBlockIsSelectable(block map[string]interface{}) bool {
	if archBlockHasURL(block) {
		return true
	}
	if s, ok := block["extract_dir"].(string); ok && strings.TrimSpace(s) != "" {
		return true
	}
	if s, ok := block["extract_to"].(string); ok && strings.TrimSpace(s) != "" {
		return true
	}
	if len(stringSliceFromField(block["hash"])) > 0 {
		return true
	}
	return false
}

func stringSliceFromField(v interface{}) []string {
	if v == nil {
		return nil
	}
	switch val := v.(type) {
	case string:
		if val == "" {
			return nil
		}
		return []string{val}
	case []interface{}:
		var out []string
		for _, item := range val {
			if s, ok := item.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	case []string:
		var out []string
		for _, s := range val {
			if s != "" {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}
