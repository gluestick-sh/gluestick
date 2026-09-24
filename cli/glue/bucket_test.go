package main

import (
	"bytes"
	"io"
	"os"
	"testing"

	"github.com/gluestick-sh/core/bucket"
	"github.com/gluestick-sh/core/git"
)

func TestShortCommit(t *testing.T) {
	if got := shortCommit("abcdef1234567890"); got != "abcdef1" {
		t.Fatalf("shortCommit = %q, want abcdef1", got)
	}
	if got := shortCommit("abc"); got != "abc" {
		t.Fatalf("shortCommit short = %q", got)
	}
}

func TestFormatGitError(t *testing.T) {
	raw := "git fetch failed: exit status 128\nfatal: unable to access 'https://example.com/': timeout"
	got := bucket.FormatGitError(raw)
	want := "fatal: unable to access 'https://example.com/': timeout"
	if got != want {
		t.Fatalf("FormatGitError = %q, want %q", got, want)
	}
	if got := bucket.FormatGitError(""); got != "check failed" {
		t.Fatalf("empty = %q", got)
	}
}

func TestPrintBucketCheckResult(t *testing.T) {
	cases := []struct {
		name   string
		status git.UpdateStatus
		want   string
	}{
		{
			name:   "failed",
			status: git.UpdateStatus{OK: false, ErrMsg: "git fetch failed: exit status 128 (fatal: network error)"},
			want:   "fatal: network error",
		},
		{
			name:   "updates",
			status: git.UpdateStatus{OK: true, HasUpdates: true, LocalCommit: "aaa1111", RemoteCommit: "bbb2222"},
			want:   "main  aaa1111 → bbb2222",
		},
		{
			name:   "synced",
			status: git.UpdateStatus{OK: true, LocalCommit: "ccc3333"},
			want:   "main  ccc3333 (up to date)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			old := os.Stdout
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			os.Stdout = w
			printBucketCheckResult("main", tc.status)
			if tc.name == "failed" {
				printBucketCheckResult("extras", git.UpdateStatus{OK: false, ErrMsg: "network timeout"})
			}
			_ = w.Close()
			os.Stdout = old
			var buf bytes.Buffer
			_, _ = io.Copy(&buf, r)
			_ = r.Close()
			if !bytes.Contains(buf.Bytes(), []byte(tc.want)) {
				t.Fatalf("output = %q, want substring %q", buf.String(), tc.want)
			}
		})
	}
}
