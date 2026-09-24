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
		{"glue", "", false},          // real install: ~/.glue
		{"glue-alpha", "alpha", true}, // dev build: ~/.glue-alpha
		{"glue-dev2", "dev2", true},
		{"glue-test-x", "test-x", true},
		{"glue-", "", false},      // empty suffix is not allowed
		{"glue-x.y", "", false},   // dots rejected (also covers test binaries like glue.test)
		{"glue.test", "", false},  // go test binary name must stay on the default root
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
