package manifest

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ErrNoAutoupdate is returned when install@version is requested without autoupdate.
var ErrNoAutoupdate = errors.New("manifest does not have autoupdate capability")

// ForVersion returns a copy of the manifest with autoupdate applied for the given version.
// Used by glue install app@version (Scoop generate_user_manifest).
func (m *Manifest) ForVersion(appName, version string) (*Manifest, error) {
	if m == nil {
		return nil, fmt.Errorf("manifest is nil")
	}
	if version == "" {
		return nil, fmt.Errorf("version is required")
	}
	if version == m.Version {
		return m, nil
	}
	if len(m.Autoupdate) == 0 {
		return nil, fmt.Errorf("%w (couldn't install %s@%s)", ErrNoAutoupdate, appName, version)
	}

	clone, err := cloneManifest(m)
	if err != nil {
		return nil, err
	}
	clone.Version = version

	subs := VersionSubstitutions(version)
	if err := applyAutoupdate(clone, appName, version, subs); err != nil {
		return nil, fmt.Errorf("autoupdate %s@%s: %w", appName, version, err)
	}

	if clone.GetURL() == "" {
		return nil, fmt.Errorf("autoupdate produced no download URL for %s@%s", appName, version)
	}
	return clone, nil
}

func cloneManifest(m *Manifest) (*Manifest, error) {
	data, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	var clone Manifest
	if err := json.Unmarshal(data, &clone); err != nil {
		return nil, err
	}
	return &clone, nil
}

func applyAutoupdate(m *Manifest, appName, version string, subs map[string]string) error {
	au := m.Autoupdate

	// Root-level fields (non-architecture).
	for _, key := range []string{"url", "extract_dir"} {
		if raw, ok := au[key]; ok {
			if err := setManifestField(m, key, raw, subs); err != nil {
				return err
			}
		}
	}

	// Architecture-specific URL / extract_dir.
	if auArch, ok := au["architecture"].(map[string]interface{}); ok && m.Architecture != nil {
		for archName, archVal := range auArch {
			archUp, ok := archVal.(map[string]interface{})
			if !ok {
				continue
			}
			archBase, ok := m.Architecture[archName].(map[string]interface{})
			if !ok {
				continue
			}
			for _, key := range []string{"url", "extract_dir"} {
				if raw, ok := archUp[key]; ok {
					val, err := substituteValue(raw, subs)
					if err != nil {
						return err
					}
					archBase[key] = val
				}
			}
		}
	}

	// Hash (global and/or per-arch) after URLs are set.
	if err := applyAutoupdateHash(m, appName, version, au, subs); err != nil {
		return err
	}

	return nil
}

func setManifestField(m *Manifest, key string, raw interface{}, subs map[string]string) error {
	val, err := substituteValue(raw, subs)
	if err != nil {
		return err
	}
	switch key {
	case "url":
		m.URL = val
	case "extract_dir":
		if s, ok := val.(string); ok {
			m.ExtractDir = s
		}
	}
	return nil
}

func substituteValue(raw interface{}, subs map[string]string) (interface{}, error) {
	switch v := raw.(type) {
	case string:
		return Substitute(v, subs), nil
	case []interface{}:
		out := make([]interface{}, len(v))
		for i, item := range v {
			s, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("unsupported array element type %T", item)
			}
			out[i] = Substitute(s, subs)
		}
		return out, nil
	case []string:
		out := make([]string, len(v))
		for i, s := range v {
			out[i] = Substitute(s, subs)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("unsupported autoupdate value type %T", raw)
	}
}

func applyAutoupdateHash(m *Manifest, appName, version string, au map[string]interface{}, subs map[string]string) error {
	hashSpec, hasGlobal := au["hash"]
	if !hasGlobal && m.Architecture == nil {
		return nil
	}

	if m.Architecture != nil {
		for archName, archVal := range m.Architecture {
			archMap, ok := archVal.(map[string]interface{})
			if !ok {
				continue
			}
			downloadURL, _ := archMap["url"].(string)
			if downloadURL == "" {
				if s, ok := archMap["url"].([]interface{}); ok && len(s) > 0 {
					downloadURL, _ = s[0].(string)
				}
			}
			archSubs := cloneSubs(subs)
			archSubs["$url"] = downloadURL
			archSubs["$baseurl"] = stripURLFilename(downloadURL)

			spec := hashSpec
			if auArch, ok := au["architecture"].(map[string]interface{}); ok {
				if archUp, ok := auArch[archName].(map[string]interface{}); ok {
					if h, ok := archUp["hash"]; ok {
						spec = h
					}
				}
			}
			if spec == nil {
				if autoupdateHasURL(au) {
					delete(archMap, "hash")
				}
				continue
			}
			hash, err := resolveAutoupdateHash(spec, archSubs)
			if err != nil {
				return fmt.Errorf("hash for %s (%s): %w", appName, archName, err)
			}
			archMap["hash"] = hash
		}
		return nil
	}

	hash, err := resolveAutoupdateHash(hashSpec, subs)
	if err != nil {
		return fmt.Errorf("hash for %s: %w", appName, err)
	}
	m.Hash = hash
	return nil
}

func cloneSubs(s map[string]string) map[string]string {
	out := make(map[string]string, len(s)+4)
	for k, v := range s {
		out[k] = v
	}
	return out
}

func stripURLFilename(url string) string {
	if q := strings.Index(url, "?"); q >= 0 {
		url = url[:q]
	}
	if h := strings.Index(url, "#"); h >= 0 {
		url = url[:h]
	}
	if i := strings.LastIndex(url, "/"); i >= 0 {
		return url[:i]
	}
	return url
}

func autoupdateHasURL(au map[string]interface{}) bool {
	if _, ok := au["url"]; ok {
		return true
	}
	auArch, ok := au["architecture"].(map[string]interface{})
	if !ok {
		return false
	}
	for _, archVal := range auArch {
		if archUp, ok := archVal.(map[string]interface{}); ok {
			if _, ok := archUp["url"]; ok {
				return true
			}
		}
	}
	return false
}
