package types

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestResultMarshalJSON(t *testing.T) {
	r := Result{
		Name:     "upx",
		Version:  "5.0.0",
		Status:   StatusSuccess,
		Duration: 1500 * time.Millisecond,
		Error:    errors.New("ignored when success"),
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m["name"] != "upx" || m["status"] != "success" {
		t.Fatalf("unexpected payload: %v", m)
	}
	if m["durationMs"] != float64(1500) {
		t.Fatalf("durationMs = %v", m["durationMs"])
	}
}
