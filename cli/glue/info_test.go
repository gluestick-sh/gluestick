package main

import (
	"bytes"
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
