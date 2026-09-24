package main

import (
	"errors"
	"testing"
)

func TestExitCode_usage(t *testing.T) {
	if exitCode(wrapUsageError(errors.New("unknown flag: --nope"))) != 2 {
		t.Fatal("expected exit code 2 for usage error")
	}
	if exitCode(errors.New(`unknown command "foo" for "glue"`)) != 2 {
		t.Fatal("expected exit code 2 for unknown command")
	}
	if exitCode(reportedFail()) != 1 {
		t.Fatal("expected exit code 1 for reported failure")
	}
	if exitCode(nil) != 0 {
		t.Fatal("expected exit code 0")
	}
}

func TestFormatCatalogRef(t *testing.T) {
	if got := formatCatalogRef("main", "git"); got != "git" {
		t.Fatalf("main ref = %q", got)
	}
	if got := formatCatalogRef("extras", "zotero"); got != "extras/zotero" {
		t.Fatalf("extras ref = %q", got)
	}
}

func TestFilterPrefix(t *testing.T) {
	got := filterPrefix([]string{"git", "go", "golang"}, "g")
	if len(got) != 3 {
		t.Fatalf("filterPrefix = %v", got)
	}
}
