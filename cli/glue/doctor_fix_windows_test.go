//go:build windows

package main

import (
	"errors"
	"strings"
	"testing"
)

// stubFixSeams replaces the console/registry seams so unit tests never touch
// the machine; returns a restore func for t.Cleanup.
func stubFixSeams(t *testing.T, consoleCP func() uint32, setConsole func() error, nls func(string) error) {
	t.Helper()
	oldCP, oldSet, oldNLS := doctorConsoleOutputCP, doctorSetConsoleUTF8, editNlsCodePage
	t.Cleanup(func() {
		doctorConsoleOutputCP, doctorSetConsoleUTF8, editNlsCodePage = oldCP, oldSet, oldNLS
	})
	if consoleCP != nil {
		doctorConsoleOutputCP = consoleCP
	}
	if setConsole != nil {
		doctorSetConsoleUTF8 = setConsole
	}
	if nls != nil {
		editNlsCodePage = nls
	}
}

func TestFixUTF8Codepage_appliesConsoleAndSystem(t *testing.T) {
	stubFixSeams(t,
		func() uint32 { return 936 },
		func() error { return nil },
		func(string) error { return nil })
	applied, detail, errText := fixUTF8Codepage()
	if !applied || errText != "" {
		t.Fatalf("applied/err = %v/%q, want true/empty (detail: %s)", applied, errText, detail)
	}
	for _, want := range []string{"console", "system ACP/OEMCP"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail %q missing %q", detail, want)
		}
	}
}

func TestFixUTF8Codepage_failsSoftWithoutElevation(t *testing.T) {
	stubFixSeams(t,
		func() uint32 { return 0 }, // piped: no attached console
		func() error { return errors.New("no console") },
		func(string) error { return errors.New("access is denied") })
	applied, detail, errText := fixUTF8Codepage()
	if applied {
		t.Fatalf("applied = true without console or elevation (detail: %s)", detail)
	}
	if !strings.Contains(errText, "elevated") {
		t.Fatalf("error = %q, want an elevated-shell hint", errText)
	}
}
