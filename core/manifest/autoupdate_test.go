package manifest

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestVersionSubstitutions(t *testing.T) {
	s := VersionSubstitutions("9.2.0545")
	if s["$version"] != "9.2.0545" {
		t.Fatalf("version: %q", s["$version"])
	}
	if s["$majorVersion"] != "9" || s["$minorVersion"] != "2" || s["$patchVersion"] != "0545" {
		t.Fatalf("semver parts: %+v", s)
	}
	if got := Substitute("vim/vim$majorVersion$minorVersion", s); got != "vim/vim92" {
		t.Fatalf("extract_dir template = %q", got)
	}
}

func TestForVersionUVLike(t *testing.T) {
	const digest = "bf8e0021336b7c77bd80a078b612125f385b08f541437edaea8c8ca9e574db0d"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".sha256") {
			_, _ = w.Write([]byte(digest + "\n"))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	base := strings.TrimPrefix(srv.URL, "https://")
	_ = base

	m := &Manifest{
		Version: "0.11.18",
		Architecture: map[string]interface{}{
			"64bit": map[string]interface{}{
				"url":  srv.URL + "/uv-0.11.18-x86_64.zip",
				"hash": "old",
			},
		},
		Autoupdate: map[string]interface{}{
			"architecture": map[string]interface{}{
				"64bit": map[string]interface{}{
					"url": srv.URL + "/uv-$version-x86_64.zip",
				},
			},
			"hash": map[string]interface{}{
				"url": "$url.sha256",
			},
		},
	}

	out, err := m.ForVersion("uv", "0.11.16")
	if err != nil {
		t.Fatal(err)
	}
	if out.Version != "0.11.16" {
		t.Fatalf("version = %q", out.Version)
	}
	url := out.GetURL()
	if !strings.Contains(url, "/uv-0.11.16-x86_64.zip") {
		t.Fatalf("url = %q", url)
	}
	if out.GetHash() != digest {
		t.Fatalf("hash = %q want %s", out.GetHash(), digest)
	}
}

func TestForVersionVimLikeClearsStaleHash(t *testing.T) {
	m := &Manifest{
		Version: "9.2.0586",
		Architecture: map[string]interface{}{
			"64bit": map[string]interface{}{
				"url":  "https://example.com/gvim_9.2.0586_x64.zip",
				"hash": "stalehash",
			},
		},
		Autoupdate: map[string]interface{}{
			"architecture": map[string]interface{}{
				"64bit": map[string]interface{}{
					"url": "https://example.com/gvim_$version_x64.zip",
				},
			},
			"extract_dir": "vim/vim$majorVersion$minorVersion",
		},
	}
	out, err := m.ForVersion("vim", "9.2.0580")
	if err != nil {
		t.Fatal(err)
	}
	if out.GetHash() != "" {
		t.Fatalf("expected empty hash, got %q", out.GetHash())
	}
	if out.GetExtractDir() != "vim/vim92" {
		t.Fatalf("extract_dir = %q", out.GetExtractDir())
	}
}

func TestForVersionNoAutoupdate(t *testing.T) {
	m := &Manifest{
		Version: "1.0.0",
		URL:     "https://example.com/a.zip",
		Hash:    "abc",
	}
	_, err := m.ForVersion("app", "0.9.0")
	if err == nil || !strings.Contains(err.Error(), "autoupdate") {
		t.Fatalf("err = %v", err)
	}
}
