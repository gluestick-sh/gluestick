package manifest

import "strings"

// VersionSubstitutions returns Scoop-style variables for autoupdate templates.
func VersionSubstitutions(version string) map[string]string {
	firstPart := version
	if i := strings.Index(version, "-"); i >= 0 {
		firstPart = version[:i]
	}
	lastPart := version
	if i := strings.LastIndex(version, "-"); i >= 0 {
		lastPart = version[i+1:]
	}
	parts := strings.Split(firstPart, ".")
	part := func(i int) string {
		if i < len(parts) {
			return parts[i]
		}
		return ""
	}

	subs := map[string]string{
		"$version":              version,
		"$dotVersion":           normalizeVersionSep(version, '.'),
		"$underscoreVersion":    normalizeVersionSep(version, '_'),
		"$dashVersion":          normalizeVersionSep(version, '-'),
		"$cleanVersion":         strings.NewReplacer(".", "", "_", "", "-", "").Replace(version),
		"$majorVersion":         part(0),
		"$minorVersion":         part(1),
		"$patchVersion":         part(2),
		"$buildVersion":         part(3),
		"$preReleaseVersion":    lastPart,
	}
	return subs
}

func normalizeVersionSep(version string, sep byte) string {
	var b strings.Builder
	for i := 0; i < len(version); i++ {
		c := version[i]
		if c == '.' || c == '_' || c == '-' {
			b.WriteByte(sep)
		} else {
			b.WriteByte(c)
		}
	}
	return b.String()
}

// Substitute replaces Scoop autoupdate variables in s.
func Substitute(s string, subs map[string]string) string {
	if s == "" || len(subs) == 0 {
		return s
	}
	keys := []string{
		"$matchHead", "$matchTail",
		"$majorVersion", "$minorVersion", "$patchVersion", "$buildVersion",
		"$preReleaseVersion",
		"$underscoreVersion", "$dashVersion", "$dotVersion", "$cleanVersion",
		"$version",
		"$basenameNoExt", "$basename", "$urlNoExt", "$baseurl", "$url",
	}
	out := s
	for _, k := range keys {
		if v, ok := subs[k]; ok {
			out = strings.ReplaceAll(out, k, v)
		}
	}
	return out
}
