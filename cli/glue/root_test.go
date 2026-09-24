package main

import "testing"

// TestExecutableDataDirSuffix verifies the exe-name → data-dir suffix rule that
// keeps dev builds (glue-alpha.exe → ~/.glue-alpha) isolated from real installs.
func TestExecutableDataDirSuffix(t *testing.T) {
	cases := []struct {
		name   string
		suffix string
		ok     bool
	}{
		{"glue", "", false},           // real install: ~/.glue
		{"glue-alpha", "alpha", true}, // dev build: ~/.glue-alpha
		{"glue-dev2", "dev2", true},
		{"glue-test-x", "test-x", true},
		{"glue-", "", false},     // empty suffix is not allowed
		{"glue-x.y", "", false},  // dots rejected (also covers test binaries like glue.test)
		{"glue.test", "", false}, // go test binary name must stay on the default root
		{"other", "", false},
		{"", "", false},
	}
	for _, tc := range cases {
		gotSuffix, gotOK := executableDataDirSuffix(tc.name)
		if gotSuffix != tc.suffix || gotOK != tc.ok {
			t.Fatalf("executableDataDirSuffix(%q) = (%q, %v), want (%q, %v)",
				tc.name, gotSuffix, gotOK, tc.suffix, tc.ok)
		}
	}
}

// TestSuppressesStartupToolNotes pins which commands own their git/7z
// reporting: glue doctor and glue env must not print startup tool notes on top
// of their own report.
func TestSuppressesStartupToolNotes(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want bool
	}{
		{"doctor", []string{"doctor"}, true},
		{"doctor fix", []string{"doctor", "--fix"}, true},
		{"env", []string{"env"}, true},
		{"env with global flag", []string{"--json", "env"}, true},
		{"mcp", []string{"mcp"}, true},
		{"install", []string{"install", "git"}, false},
		{"search", []string{"search", "git"}, false},
		{"no args", nil, false},
	}
	for _, tc := range cases {
		if got := suppressesStartupToolNotes(tc.args); got != tc.want {
			t.Errorf("%s: suppressesStartupToolNotes(%q) = %v, want %v", tc.name, tc.args, got, tc.want)
		}
	}
}
