package uninstall

import (
	"strings"
	"testing"

	"github.com/gluestick-sh/core/procutil"
)

func TestProcessesBlockingUninstallError(t *testing.T) {
	err := processesBlockingUninstallError("itch", "26.13.0", []procutil.RunningProcess{
		{PID: 1234, Name: "itch.exe"},
		{PID: 5678, Name: "itch-helper.exe"},
	})
	msg := err.Error()
	if !strings.Contains(msg, "itch@26.13.0") {
		t.Fatalf("message = %q", msg)
	}
	if !strings.Contains(msg, "itch.exe (PID 1234)") {
		t.Fatalf("message = %q", msg)
	}
	if !strings.Contains(msg, "Task Manager") {
		t.Fatalf("message = %q", msg)
	}
}
