package main

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
)

func TestJSONOutputEnabled(t *testing.T) {
	if jsonOutputEnabled() {
		t.Fatal("expected false before flag parse")
	}
	t.Cleanup(func() {
		_ = rootCmd.PersistentFlags().Set("json", "false")
	})
	if err := rootCmd.PersistentFlags().Set("json", "true"); err != nil {
		t.Fatal(err)
	}
	if !jsonOutputEnabled() {
		t.Fatal("expected true after flag set")
	}
}

func TestEmitJSON(t *testing.T) {
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	if err := emitJSON(map[string]any{"ok": true, "count": 1}); err != nil {
		t.Fatal(err)
	}
	w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r); err != nil {
		t.Fatal(err)
	}
	if !json.Valid(buf.Bytes()) {
		t.Fatalf("invalid json: %s", buf.String())
	}
}
