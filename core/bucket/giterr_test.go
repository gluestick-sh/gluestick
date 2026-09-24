package bucket

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestFormatGitError(t *testing.T) {
	raw := "git fetch failed: exit status 128\nfatal: unable to access 'https://example.com/': timeout"
	got := FormatGitError(raw)
	want := "fatal: unable to access 'https://example.com/': timeout"
	if got != want {
		t.Fatalf("FormatGitError = %q, want %q", got, want)
	}
	wrapped := "update 'dorado': git fetch failed: exit status 128 (fatal: unable to access 'https://github.com/chawyehsu/dorado/': schannel: server closed abruptly (missing close_notify))"
	got = FormatGitError(wrapped)
	if !strings.Contains(got, "fatal: unable to access") || !strings.Contains(got, "schannel") {
		t.Fatalf("FormatGitError wrapped = %q", got)
	}
	if got := FormatGitError(""); got != "check failed" {
		t.Fatalf("empty = %q", got)
	}
}

func TestFormatErr(t *testing.T) {
	err := fmt.Errorf("update 'dorado': %w", errors.New("git fetch failed: exit status 128 (fatal: schannel: server closed abruptly)"))
	got := FormatErr(err)
	if !strings.Contains(got, "schannel") {
		t.Fatalf("FormatErr = %q", got)
	}
}
