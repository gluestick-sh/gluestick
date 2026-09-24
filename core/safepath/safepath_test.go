package safepath

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestValidateManifestRelPath(t *testing.T) {
	tests := []struct {
		in    string
		want  string
		isErr bool
	}{
		{"", "", false},
		{".", "", false},
		{`IDE\bin`, "IDE/bin", false},
		{`../outside`, "", true},
		{`foo/../../etc`, "", true},
		{`/abs`, "", true},
	}
	for _, tc := range tests {
		got, err := ValidateManifestRelPath(tc.in)
		if tc.isErr {
			if err == nil || !errors.Is(err, ErrUnsafePath) {
				t.Fatalf("ValidateManifestRelPath(%q) err = %v, want ErrUnsafePath", tc.in, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("ValidateManifestRelPath(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("ValidateManifestRelPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestJoinUnderBase(t *testing.T) {
	base := t.TempDir()
	target, err := JoinUnderBase(base, "app/bin/tool.exe")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(base, "app", "bin", "tool.exe")
	if target != want {
		t.Fatalf("got %q want %q", target, want)
	}
	if _, err := JoinUnderBase(base, "../escape"); err == nil {
		t.Fatal("expected traversal error")
	}
}
