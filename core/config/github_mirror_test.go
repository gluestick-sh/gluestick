package config

import (
	"testing"
)

func TestIsGitHubURL(t *testing.T) {
	if !IsGitHubURL("https://github.com/foo/bar/releases/download/1.0/x.zip") {
		t.Fatal("expected github.com URL")
	}
	if IsGitHubURL("https://example.com/x.zip") {
		t.Fatal("expected non-GitHub URL")
	}
}

func TestMirrorURLs(t *testing.T) {
	url := "https://github.com/foo/bar/releases/download/1.0/x.zip"
	got := MirrorURLs(url, []string{"https://mirror.test/", ""})
	if len(got) != 2 {
		t.Fatalf("got %d urls", len(got))
	}
	if got[0] != "https://mirror.test/https://github.com/foo/bar/releases/download/1.0/x.zip" {
		t.Fatalf("mirror: %q", got[0])
	}
	if got[1] != url {
		t.Fatalf("direct: %q", got[1])
	}
	if direct := MirrorURLs("https://example.com/x", []string{"https://mirror.test/"}); len(direct) != 1 || direct[0] != "https://example.com/x" {
		t.Fatalf("non-github: %#v", direct)
	}
}

func TestLoadProxiesEnvPrecedence(t *testing.T) {
	t.Setenv("GITHUB_PROXY", "https://env.test/")
	root := t.TempDir()
	if err := WriteConfigGitHubProxy(root, "https://cfg.test/"); err != nil {
		t.Fatal(err)
	}
	got := LoadProxies(root)
	if len(got) != 1 || got[0] != "https://env.test/" {
		t.Fatalf("got %#v", got)
	}
}

func TestLoadProxiesUnsetIsDirectOnly(t *testing.T) {
	t.Setenv("GITHUB_PROXY", "")
	root := t.TempDir()
	if LoadProxies(root) != nil {
		t.Fatal("expected nil proxies")
	}
}
