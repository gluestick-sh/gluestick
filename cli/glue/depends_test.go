package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestRunDependsJSON_errorPath verifies the --json error envelope: an unknown
// bucket ref fails offline with a per-ref error while the command exits 1.
func TestRunDependsJSON_errorPath(t *testing.T) {
	root := t.TempDir()
	setJSONTestFlags(t, root)
	dependsCmd.SetContext(context.Background())

	out := captureStdout(t, func() {
		err := runDepends(dependsCmd, []string{"nosuchbucket/missing-pkg"})
		if err == nil {
			t.Fatal("expected failure for unknown bucket ref")
		}
		if code := exitCode(err); code != 1 {
			t.Fatalf("exitCode = %d, want 1", code)
		}
	})

	var res struct {
		Command string `json:"command"`
		OK      bool   `json:"ok"`
		Plans   []struct {
			Ref         string `json:"ref"`
			Package     string `json:"package"`
			Depends     []any  `json:"depends"`
			Suggestions []any  `json:"suggestions"`
			Error       string `json:"error"`
			Code        string `json:"code"`
		} `json:"plans"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if res.Command != "depends" || res.OK {
		t.Fatalf("command=%q ok=%v, want command=depends ok=false", res.Command, res.OK)
	}
	if len(res.Plans) != 1 {
		t.Fatalf("plans len = %d, want 1", len(res.Plans))
	}
	plan := res.Plans[0]
	if plan.Ref != "nosuchbucket/missing-pkg" {
		t.Fatalf("ref = %q, want nosuchbucket/missing-pkg", plan.Ref)
	}
	if plan.Error == "" {
		t.Fatal("expected non-empty error for missing bucket ref")
	}
	if plan.Code != "bucket_not_installed" {
		t.Fatalf("code = %q, want bucket_not_installed", plan.Code)
	}
	if plan.Depends == nil || plan.Suggestions == nil {
		t.Fatalf("depends/suggestions must be empty arrays, not null: %s", out)
	}
}

// TestRunDependsJSON_successPath verifies the --json success envelope against a
// local fake bucket (offline: no git, no network).
func TestRunDependsJSON_successPath(t *testing.T) {
	root := t.TempDir()
	setJSONTestFlags(t, root)
	dependsCmd.SetContext(context.Background())

	bucketManifestDir := filepath.Join(root, "buckets", "demo", "bucket")
	if err := os.MkdirAll(bucketManifestDir, 0755); err != nil {
		t.Fatal(err)
	}
	manifest := `{"version": "1.0.0", "url": "https://example.com/hello.zip", "hash": "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"}`
	if err := os.WriteFile(filepath.Join(bucketManifestDir, "hello.json"), []byte(manifest), 0644); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		if err := runDepends(dependsCmd, []string{"demo/hello"}); err != nil {
			t.Fatalf("runDepends: %v", err)
		}
	})

	var res struct {
		Command string `json:"command"`
		OK      bool   `json:"ok"`
		Plans   []struct {
			Ref         string `json:"ref"`
			Package     string `json:"package"`
			Depends     []any  `json:"depends"`
			Suggestions []any  `json:"suggestions"`
			Error       string `json:"error"`
		} `json:"plans"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if res.Command != "depends" || !res.OK {
		t.Fatalf("command=%q ok=%v, want command=depends ok=true", res.Command, res.OK)
	}
	if len(res.Plans) != 1 {
		t.Fatalf("plans len = %d, want 1", len(res.Plans))
	}
	plan := res.Plans[0]
	if plan.Ref != "demo/hello" || plan.Package != "demo/hello" {
		t.Fatalf("ref/package = %q/%q, want demo/hello", plan.Ref, plan.Package)
	}
	if plan.Error != "" {
		t.Fatalf("unexpected error: %q", plan.Error)
	}
	if len(plan.Depends) != 0 || len(plan.Suggestions) != 0 {
		t.Fatalf("expected empty depends/suggestions, got %v/%v", plan.Depends, plan.Suggestions)
	}
}
