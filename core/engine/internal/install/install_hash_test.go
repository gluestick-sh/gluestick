package install

import (
	"strings"
	"testing"
)

func TestParseHash(t *testing.T) {
	tests := []struct {
		in       string
		wantAlgo string
		wantVal  string
	}{
		{"sha512:abc123", "sha512", "abc123"},
		{"sha256:deadbeef", "sha256", "deadbeef"},
		{"0123456789abcdef0123456789abcdef", "md5", "0123456789abcdef0123456789abcdef"},
		{"0123456789012345678901234567890123456789", "sha1", "0123456789012345678901234567890123456789"},
		{
			"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			"sha256",
			"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		},
		{
			strings.Repeat("a", 128),
			"sha512",
			strings.Repeat("a", 128),
		},
	}
	for _, tc := range tests {
		algo, val := parseHash(tc.in)
		if algo != tc.wantAlgo || val != tc.wantVal {
			t.Fatalf("parseHash(%q) = (%q, %q), want (%q, %q)", tc.in, algo, val, tc.wantAlgo, tc.wantVal)
		}
	}
}
