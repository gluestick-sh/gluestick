package verbose

import "testing"

func TestSetEnabledFprintf(t *testing.T) {
	Set(false)
	Fprintf("hidden\n")
	Set(true)
	// smoke: should not panic
	Fprintf("visible\n")
	if !Enabled() {
		t.Fatal("expected verbose enabled")
	}
	Set(false)
	if Enabled() {
		t.Fatal("expected verbose disabled")
	}
}
