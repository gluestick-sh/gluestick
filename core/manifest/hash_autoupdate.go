package manifest

import (
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

var hashHexRe = regexp.MustCompile(`^[a-fA-F0-9]{32,128}$`)

// resolveAutoupdateHash resolves hash from a string template or {url, regex} object.
func resolveAutoupdateHash(spec interface{}, subs map[string]string) (string, error) {
	switch v := spec.(type) {
	case string:
		h := strings.TrimSpace(Substitute(v, subs))
		if h == "" {
			return "", fmt.Errorf("empty hash template")
		}
		return normalizeHashValue(h), nil
	case map[string]interface{}:
		return fetchHashFromSpec(v, subs)
	default:
		return "", fmt.Errorf("unsupported autoupdate hash type %T", spec)
	}
}

func fetchHashFromSpec(spec map[string]interface{}, subs map[string]string) (string, error) {
	rawURL, _ := spec["url"].(string)
	if rawURL == "" {
		return "", fmt.Errorf("autoupdate hash missing url")
	}
	hashURL := Substitute(rawURL, subs)
	if hashURL == "" {
		return "", fmt.Errorf("empty hash url after substitution")
	}

	body, err := fetchText(hashURL)
	if err != nil {
		return "", err
	}

	regexPat, _ := spec["regex"].(string)
	if regexPat == "" {
		if find, _ := spec["find"].(string); find != "" {
			regexPat = find
		}
	}
	if regexPat == "" {
		// e.g. GitHub .sha256 sidecar files are a single hex digest.
		body = strings.TrimSpace(body)
		if hashHexRe.MatchString(body) {
			return normalizeHashValue(body), nil
		}
		return "", fmt.Errorf("hash file at %s is not a bare digest (set regex in manifest)", hashURL)
	}

	regexPat = Substitute(regexPat, subs)
	re, err := regexp.Compile(regexPat)
	if err != nil {
		return "", fmt.Errorf("invalid hash regex: %w", err)
	}
	m := re.FindStringSubmatch(body)
	if len(m) < 2 {
		return "", fmt.Errorf("hash regex did not match in %s", hashURL)
	}
	return normalizeHashValue(strings.TrimSpace(m[1])), nil
}

func normalizeHashValue(hash string) string {
	hash = strings.TrimSpace(hash)
	lower := strings.ToLower(hash)
	if strings.HasPrefix(lower, "sha256:") {
		return lower[7:]
	}
	if strings.HasPrefix(lower, "sha1:") {
		return lower[5:]
	}
	if strings.HasPrefix(lower, "md5:") {
		return lower[4:]
	}
	if strings.HasPrefix(lower, "sha512:") {
		return lower[7:]
	}
	return lower
}

func fetchText(url string) (string, error) {
	client := &http.Client{Timeout: 60 * time.Second}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "glue/0.1")

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch hash: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("fetch hash: HTTP %d from %s", resp.StatusCode, url)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", err
	}
	return string(data), nil
}
