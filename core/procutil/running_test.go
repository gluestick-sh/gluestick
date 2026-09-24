package procutil

import (
	"path/filepath"
	"testing"
)

func TestMergeRunningProcesses(t *testing.T) {
	merged := mergeRunningProcesses(
		[]RunningProcess{{PID: 1, Name: "a.exe"}},
		[]RunningProcess{{PID: 1, Name: "a.exe"}, {PID: 2, Name: "b.exe"}},
	)
	if len(merged) != 2 {
		t.Fatalf("mergeRunningProcesses len = %d, want 2", len(merged))
	}
}

func TestPathUnderDir(t *testing.T) {
	root := filepath.FromSlash(`C:\Users\me\.glue\apps\itch\26.13.0`)
	tests := []struct {
		exe  string
		want bool
	}{
		{filepath.FromSlash(`C:\Users\me\.glue\apps\itch\26.13.0\itch.exe`), true},
		{filepath.FromSlash(`C:\Users\me\.glue\apps\itch\26.13.0\locales\en-US.pak`), true},
		{filepath.FromSlash(`C:\Users\me\.glue\apps\itch\current\itch.exe`), false},
		{filepath.FromSlash(`C:\Users\me\.glue\apps\other\1.0\app.exe`), false},
	}
	for _, tc := range tests {
		if got := pathUnderDir(tc.exe, root); got != tc.want {
			t.Errorf("pathUnderDir(%q, %q) = %v, want %v", tc.exe, root, got, tc.want)
		}
	}
}
