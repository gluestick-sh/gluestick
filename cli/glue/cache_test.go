package main

import (
	"errors"
	"testing"

	"github.com/gluestick-sh/core/engine"
)

func TestClearCacheIndexByName_noneFound(t *testing.T) {
	root := t.TempDir()
	eng, err := engine.NewEngine(&engine.EngineConfig{RootDir: root})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	err = clearCacheIndexByName(eng, []string{"nonexistent"})
	if !errors.Is(err, errReported) {
		t.Fatalf("want errReported, got %v", err)
	}
}
