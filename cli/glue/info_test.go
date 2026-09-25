package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/gluestick-sh/core/engine"
)

func TestPrintPackageInfo_fields(t *testing.T) {
	oldOut := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w

	printPackageInfo(&engine.InstalledPackageDetail{
		Name:        "git",
		Version:     "2.45.0",
		InstallPath: `C:\glue\apps\git\2.45.0`,
		CurrentPath: `C:\glue\apps\git\current`,
		Size:        1024,
		FileCount:   2,
		Shims:       []string{"git", "git-gui"},
		Bucket:      "main",
		Description: "Git SCM",
	})

	w.Close()
	os.Stdout = oldOut

	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"git@2.45.0",
		"Installed",
		`C:\glue\apps\git\2.45.0`,
		"git, git-gui",
		"main",
		"Git SCM",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}

// TestInfoJSON_allFailedEmptyPackages pins the JSON contract when no requested
// package resolves: `packages` is [] (not null) and the command exits 1.
func TestInfoJSON_allFailedEmptyPackages(t *testing.T) {
	root := t.TempDir()
	setJSONTestFlags(t, root)
	infoCmd.SetContext(context.Background())
	t.Cleanup(func() { infoCmd.SetContext(nil) })

	out := captureStdout(t, func() {
		err := runInfo(infoCmd, []string{"definitely-missing"})
		if err == nil {
			t.Fatal("expected failure for a missing package")
		}
		if code := exitCode(err); code != 1 {
			t.Fatalf("exitCode = %d, want 1", code)
		}
	})

	var res struct {
		Packages []json.RawMessage `json:"packages"`
		Count    int               `json:"count"`
		Failed   []string          `json:"failed"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if res.Count != 0 || len(res.Failed) != 1 {
		t.Fatalf("count/failed = %d/%v\n%s", res.Count, res.Failed, out)
	}
	if res.Packages == nil {
		t.Fatalf("packages must be an empty array, not null: %s", out)
	}
}
