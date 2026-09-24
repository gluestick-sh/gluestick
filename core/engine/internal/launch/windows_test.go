//go:build windows

package launch

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestStartInCmdWindow(t *testing.T) {
	dir := t.TempDir()
	bat := filepath.Join(dir, "hello.cmd")
	if err := os.WriteFile(bat, []byte("@echo launched\r\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := startInCmdWindow(bat); err != nil {
		t.Fatalf("startInCmdWindow: %v", err)
	}
}

func TestExecStartConsoleSeparateArgs(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "hello.cmd")
	if err := os.WriteFile(exe, []byte("@echo glue-launch-ok\r\n"), 0644); err != nil {
		t.Fatal(err)
	}
	abs := filepath.Clean(exe)
	script := buildCmdLaunchScript(abs)
	cmd := exec.Command("cmd", "/c", "start", "", "/D", filepath.Dir(abs), "cmd", "/k", script)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
}

func TestBuildCmdLaunchScript(t *testing.T) {
	got := buildCmdLaunchScript(`C:\glue\apps\git\2.47.0\cmd\git.exe`)
	want := `title Command Prompt && C:\glue\apps\git\2.47.0\cmd\git.exe`
	if got != want {
		t.Fatalf("buildCmdLaunchScript() = %q, want %q", got, want)
	}

	gotCmd := buildCmdLaunchScript(`C:\glue\shims\npm.cmd`)
	wantCmd := `title Command Prompt && call C:\glue\shims\npm.cmd`
	if gotCmd != wantCmd {
		t.Fatalf("buildCmdLaunchScript(cmd) = %q, want %q", gotCmd, wantCmd)
	}

	spaced := buildCmdLaunchScript(`C:\Program Files\node\node.exe`)
	if !strings.Contains(spaced, `"C:\Program Files\node\node.exe"`) {
		t.Fatalf("expected quoted spaced path, got %q", spaced)
	}
}

func TestQuoteCmdToken(t *testing.T) {
	got := quoteCmdToken(`C:\apps\7 zip\7z.exe`)
	want := `"C:\apps\7 zip\7z.exe"`
	if got != want {
		t.Fatalf("quoteCmdToken() = %q, want %q", got, want)
	}
}
