package install

import "testing"

func TestMarks_plainWhenColorDisabled(t *testing.T) {
	SetColorEnabled(false)
	if got := SuccessMark(); got != "✓" {
		t.Fatalf("SuccessMark() = %q, want plain checkmark", got)
	}
	if got := FailedMark(); got != "✗" {
		t.Fatalf("FailedMark() = %q, want plain cross", got)
	}
}

func TestMarks_coloredWhenEnabled(t *testing.T) {
	SetColorEnabled(true)
	if got := SuccessMark(); got == "✓" || got == "" {
		t.Fatalf("SuccessMark() = %q, want ANSI-wrapped checkmark", got)
	}
	if got := FailedMark(); got == "✗" || got == "" {
		t.Fatalf("FailedMark() = %q, want ANSI-wrapped cross", got)
	}
}
