package config

import (
	"os"
	"strings"
)

// LoadProxies returns GitHub mirror prefixes from env or config.
// When unset, returns nil and downloads use direct GitHub (no built-in mirrors).
// Precedence: GITHUB_PROXY, config.json github_proxy.
func LoadProxies(rootDir string) []string {
	if v := envGitHubProxy(); v != "" {
		return splitProxies(v)
	}
	cfg, err := readConfigFile(rootDir)
	if err != nil || cfg == nil || cfg.GitHubProxy == "" {
		return nil
	}
	return splitProxies(cfg.GitHubProxy)
}

// EnvGitHubProxy returns GITHUB_PROXY when set in the environment.
func EnvGitHubProxy() string {
	return strings.TrimSpace(os.Getenv("GITHUB_PROXY"))
}

func envGitHubProxy() string {
	return EnvGitHubProxy()
}

func splitProxies(value string) []string {
	parts := strings.Split(value, ",")
	var out []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// IsGitHubURL reports whether url is a GitHub-hosted HTTPS asset we can mirror.
func IsGitHubURL(url string) bool {
	for _, prefix := range []string{
		"https://github.com/",
		"https://raw.githubusercontent.com/",
		"https://objects.githubusercontent.com/",
		"https://codeload.github.com/",
	} {
		if strings.HasPrefix(url, prefix) {
			return true
		}
	}
	return false
}

// MirrorURLs returns URLs to try for downloading url.
// Non-GitHub URLs are returned unchanged. Each proxy prefix is prepended to the full GitHub URL.
// An empty proxy prefix means the original URL (direct).
func MirrorURLs(url string, proxies []string) []string {
	if !IsGitHubURL(url) {
		return []string{url}
	}
	if len(proxies) == 0 {
		return []string{url}
	}
	seen := make(map[string]struct{}, len(proxies)+1)
	var urls []string
	for _, proxy := range proxies {
		var candidate string
		if proxy == "" {
			candidate = url
		} else {
			candidate = strings.TrimSuffix(proxy, "/") + "/" + url
		}
		if _, ok := seen[candidate]; ok {
			continue
		}
		seen[candidate] = struct{}{}
		urls = append(urls, candidate)
	}
	return urls
}
