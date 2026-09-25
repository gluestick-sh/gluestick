package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
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

// TestRunBucketListJSON verifies the --json output of glue bucket list against
// local fake bucket dirs (offline: registered without git metadata).
func TestRunBucketListJSON(t *testing.T) {
	root := t.TempDir()
	setJSONTestFlags(t, root)

	for _, name := range []string{"main", "extras"} {
		dir := filepath.Join(root, "buckets", name, "bucket")
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "stub.json"), []byte(`{"version": "1.0"}`), 0644); err != nil {
			t.Fatal(err)
		}
	}

	out := captureStdout(t, func() {
		if err := bucketListCmd.RunE(bucketListCmd, nil); err != nil {
			t.Fatalf("bucket list: %v", err)
		}
	})

	var res struct {
		Buckets []struct {
			Name    string `json:"name"`
			RepoURL string `json:"repo_url"`
		} `json:"buckets"`
		Count int `json:"count"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if res.Count != 2 || len(res.Buckets) != 2 {
		t.Fatalf("count/len = %d/%d, want 2/2\n%s", res.Count, len(res.Buckets), out)
	}
	for _, b := range res.Buckets {
		if b.Name != "main" && b.Name != "extras" {
			t.Fatalf("unexpected bucket name %q\n%s", b.Name, out)
		}
		if b.RepoURL != "" {
			t.Fatalf("non-git bucket %q should have empty repo_url, got %q", b.Name, b.RepoURL)
		}
	}
}

// TestRunBucketKnownJSON verifies the --json output of glue bucket known.
func TestRunBucketKnownJSON(t *testing.T) {
	setJSONTestFlags(t, t.TempDir())

	out := captureStdout(t, func() {
		if err := bucketKnownCmd.RunE(bucketKnownCmd, nil); err != nil {
			t.Fatalf("bucket known: %v", err)
		}
	})

	var res struct {
		Buckets []struct {
			Name    string `json:"name"`
			RepoURL string `json:"repo_url"`
		} `json:"buckets"`
		Count int `json:"count"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if res.Count == 0 || len(res.Buckets) != res.Count {
		t.Fatalf("count/len mismatch: %d/%d", res.Count, len(res.Buckets))
	}
	for _, b := range res.Buckets {
		if b.Name == "" || b.RepoURL == "" {
			t.Fatalf("known bucket entries must have name and url: %+v", b)
		}
	}
}

// TestRunBucketAddJSON_idempotent verifies adding an already-installed bucket
// is a no-op success with already_installed=true (offline, no git clone).
func TestRunBucketAddJSON_idempotent(t *testing.T) {
	root := t.TempDir()
	setJSONTestFlags(t, root)

	dir := filepath.Join(root, "buckets", "main", "bucket")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stub.json"), []byte(`{"version": "1.0"}`), 0644); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		if err := bucketAddCmd.RunE(bucketAddCmd, []string{"main"}); err != nil {
			t.Fatalf("bucket add: %v", err)
		}
	})

	var res struct {
		Command          string `json:"command"`
		OK               bool   `json:"ok"`
		Name             string `json:"name"`
		AlreadyInstalled bool   `json:"already_installed"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if !res.OK || res.Name != "main" || !res.AlreadyInstalled {
		t.Fatalf("ok/name/already_installed = %v/%q/%v\n%s", res.OK, res.Name, res.AlreadyInstalled, out)
	}
}

// TestRunBucketCheckJSON_empty verifies bucket check --json with no buckets.
func TestRunBucketCheckJSON_empty(t *testing.T) {
	root := t.TempDir()
	setJSONTestFlags(t, root)

	out := captureStdout(t, func() {
		if err := runBucketCheck(bucketCheckCmd, nil); err != nil {
			t.Fatalf("bucket check: %v", err)
		}
	})

	var res struct {
		Buckets []json.RawMessage `json:"buckets"`
		Failed  int               `json:"failed"`
		OK      bool              `json:"ok"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if !res.OK || res.Failed != 0 || res.Buckets == nil {
		t.Fatalf("ok/failed/buckets = %v/%d/%v\n%s", res.OK, res.Failed, res.Buckets, out)
	}
}

// TestRunBucketUpdateJSON_empty verifies bucket update --json with no buckets
// (updates nothing, still ok).
func TestRunBucketUpdateJSON_empty(t *testing.T) {
	root := t.TempDir()
	setJSONTestFlags(t, root)

	out := captureStdout(t, func() {
		if err := bucketUpdateCmd.RunE(bucketUpdateCmd, nil); err != nil {
			t.Fatalf("bucket update: %v", err)
		}
	})

	var res struct {
		Command string   `json:"command"`
		OK      bool     `json:"ok"`
		Updated []string `json:"updated"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if res.Command != "bucket_update" || !res.OK || res.Updated == nil {
		t.Fatalf("command/ok/updated = %q/%v/%v\n%s", res.Command, res.OK, res.Updated, out)
	}
}

// TestRunBucketListJSON_empty verifies the --json output with no buckets installed.
func TestRunBucketListJSON_empty(t *testing.T) {
	root := t.TempDir()
	setJSONTestFlags(t, root)

	out := captureStdout(t, func() {
		if err := bucketListCmd.RunE(bucketListCmd, nil); err != nil {
			t.Fatalf("bucket list: %v", err)
		}
	})

	var res struct {
		Buckets []json.RawMessage `json:"buckets"`
		Count   int               `json:"count"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if res.Count != 0 {
		t.Fatalf("count = %d, want 0", res.Count)
	}
	if res.Buckets == nil {
		t.Fatalf("buckets must be an empty array, not null: %s", out)
	}
}

// TestRunBucketRemoveJSON_missingBucket pins the JSON envelope for
// `glue bucket remove` on a bucket that is not installed: stable code
// (bucket_not_found), ok=false and exit 1 — stdout carries only JSON.
func TestRunBucketRemoveJSON_missingBucket(t *testing.T) {
	root := t.TempDir()
	setJSONTestFlags(t, root)

	out := captureStdout(t, func() {
		err := bucketRemoveCmd.RunE(bucketRemoveCmd, []string{"no-such-bucket"})
		if err == nil {
			t.Fatal("expected failure for a missing bucket")
		}
		if code := exitCode(err); code != 1 {
			t.Fatalf("exitCode = %d, want 1", code)
		}
	})

	var res struct {
		Command string `json:"command"`
		OK      bool   `json:"ok"`
		Results []struct {
			Ref   string `json:"ref"`
			Error string `json:"error"`
			Code  string `json:"code"`
			Hint  string `json:"hint"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if res.Command != "bucket_remove" || res.OK {
		t.Fatalf("command/ok = %q/%v, want bucket_remove/false\n%s", res.Command, res.OK, out)
	}
	if len(res.Results) != 1 {
		t.Fatalf("results = %+v, want exactly one entry\n%s", res.Results, out)
	}
	item := res.Results[0]
	if item.Ref != "no-such-bucket" || item.Code != "bucket_not_found" || item.Error == "" || item.Hint == "" {
		t.Fatalf("result item = %+v, want ref/code/error/hint set\n%s", item, out)
	}
}

// TestRunBucketRemoveJSON_success removes a locally registered (non-git) bucket
// directory: ok=true, one success item, exit 0, directory gone.
func TestRunBucketRemoveJSON_success(t *testing.T) {
	root := t.TempDir()
	setJSONTestFlags(t, root)

	bucketDir := filepath.Join(root, "buckets", "demo")
	if err := os.MkdirAll(filepath.Join(bucketDir, "bucket"), 0755); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		if err := bucketRemoveCmd.RunE(bucketRemoveCmd, []string{"demo"}); err != nil {
			t.Fatalf("bucket remove: %v", err)
		}
	})

	var res struct {
		Command string `json:"command"`
		OK      bool   `json:"ok"`
		Results []struct {
			Ref   string `json:"ref"`
			Error string `json:"error"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if res.Command != "bucket_remove" || !res.OK || len(res.Results) != 1 {
		t.Fatalf("remove result = %+v\n%s", res, out)
	}
	if res.Results[0].Ref != "demo" || res.Results[0].Error != "" {
		t.Fatalf("result item = %+v, want ref=demo without error\n%s", res.Results[0], out)
	}
	if _, err := os.Stat(bucketDir); !os.IsNotExist(err) {
		t.Fatalf("bucket dir still present after remove (err=%v)", err)
	}
}

// TestRunBucketRemoveJSON_partialFailure mixes a removable bucket with a missing
// one: ok=false (exit 1) while the removable bucket still goes away.
func TestRunBucketRemoveJSON_partialFailure(t *testing.T) {
	root := t.TempDir()
	setJSONTestFlags(t, root)

	bucketDir := filepath.Join(root, "buckets", "demo")
	if err := os.MkdirAll(filepath.Join(bucketDir, "bucket"), 0755); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		err := bucketRemoveCmd.RunE(bucketRemoveCmd, []string{"demo", "no-such-bucket"})
		if err == nil {
			t.Fatal("expected failure when one bucket is missing")
		}
		if code := exitCode(err); code != 1 {
			t.Fatalf("exitCode = %d, want 1", code)
		}
	})

	var res struct {
		OK      bool `json:"ok"`
		Results []struct {
			Ref   string `json:"ref"`
			Code  string `json:"code"`
			Error string `json:"error"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if res.OK || len(res.Results) != 2 {
		t.Fatalf("ok/results = %v/%+v\n%s", res.OK, res.Results, out)
	}
	byRef := map[string]string{}
	for _, item := range res.Results {
		byRef[item.Ref] = item.Code
	}
	if byRef["demo"] != "" {
		t.Fatalf("demo must succeed, code = %q\n%s", byRef["demo"], out)
	}
	if byRef["no-such-bucket"] != "bucket_not_found" {
		t.Fatalf("no-such-bucket code = %q, want bucket_not_found\n%s", byRef["no-such-bucket"], out)
	}
	if _, err := os.Stat(bucketDir); !os.IsNotExist(err) {
		t.Fatalf("bucket dir still present after remove (err=%v)", err)
	}
}
