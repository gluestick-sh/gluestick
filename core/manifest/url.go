package manifest

import (
	"net/url"
	"path"
	"strings"
)

// ParsedURL describes how to fetch and name a Scoop-style download URL.
type ParsedURL struct {
	FetchURL  string // HTTP URL without fragment
	LocalName string // Local filename (fragment rename or URL basename)
	Extension string // Lower-case extension for install routing
}

// IsScoopArchiveAlias reports dl.zip / dl.7z fragments used to extract installers with 7-Zip.
func IsScoopArchiveAlias(name string) bool {
	switch strings.ToLower(path.Base(name)) {
	case "dl.zip", "dl.7z":
		return true
	default:
		return false
	}
}

// ShouldNativeZipIngest reports whether the downloader should stream-ingest as ZIP.
// Scoop dl.zip aliases are 7z SFX installers, not standard zip archives.
func ShouldNativeZipIngest(filename, fetchURL string) bool {
	if IsScoopArchiveAlias(filename) {
		return false
	}
	lower := strings.ToLower(filename)
	if strings.HasSuffix(lower, ".zip") || strings.HasSuffix(lower, ".nupkg") {
		return true
	}
	if fetchURL != "" {
		if u, err := url.Parse(fetchURL); err == nil {
			base := strings.ToLower(path.Base(u.Path))
			return strings.HasSuffix(base, ".zip") || strings.HasSuffix(base, ".nupkg")
		}
	}
	return false
}

// ParseURL interprets a manifest download URL, including Scoop #/ rename fragments.
// Shorthand "#dl.zip" (missing slash) is normalized to "#/dl.zip".
func ParseURL(raw string) (ParsedURL, error) {
	raw = normalizeFragment(raw)
	u, err := url.Parse(raw)
	if err != nil {
		return ParsedURL{}, err
	}

	fetch := *u
	fetch.Fragment = ""
	fetchURL := fetch.String()

	localName := path.Base(u.Path)
	if u.Fragment != "" {
		frag := strings.TrimPrefix(u.Fragment, "/")
		if frag != "" {
			localName = path.Base(frag)
		}
	}

	return ParsedURL{
		FetchURL:  fetchURL,
		LocalName: localName,
		Extension: extensionForName(localName),
	}, nil
}

func normalizeFragment(raw string) string {
	if strings.Contains(raw, "#/") {
		return raw
	}
	if i := strings.Index(raw, "#"); i >= 0 {
		frag := raw[i+1:]
		if frag != "" && !strings.HasPrefix(frag, "/") {
			return raw[:i] + "#/" + frag
		}
	}
	return raw
}

func extensionForName(name string) string {
	if IsScoopMsiAlias(name) {
		return ".msi_"
	}
	lower := strings.ToLower(name)
	if strings.HasSuffix(lower, ".7z.exe") {
		return ".7z.exe"
	}
	if strings.HasSuffix(lower, ".tar.gz") {
		return ".tar.gz"
	}
	if strings.HasSuffix(lower, ".tar.bz2") {
		return ".tar.bz2"
	}
	if strings.HasSuffix(lower, ".tar.xz") {
		return ".tar.xz"
	}
	if strings.HasSuffix(lower, ".tgz") {
		return ".tar"
	}
	ext := path.Ext(name)
	return strings.ToLower(ext)
}
