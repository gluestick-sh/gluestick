package main

import (
	"encoding/json"
	"os"

	"github.com/spf13/cobra"
	"github.com/gluestick-sh/core/engine"
)

func jsonOutputEnabled() bool {
	if rootCmd == nil {
		return false
	}
	v, err := rootCmd.PersistentFlags().GetBool("json")
	return err == nil && v
}

func emitJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

type jsonCommandResult struct {
	Command string           `json:"command"`
	OK      bool             `json:"ok"`
	Results []jsonResultItem `json:"results,omitempty"`
	Error   string           `json:"error,omitempty"`
}

type jsonResultItem struct {
	Ref     string         `json:"ref"`
	Result  *engine.Result `json:"result,omitempty"`
	Error   string         `json:"error,omitempty"`
}

func jsonOperationResult(command string, items []jsonResultItem) error {
	ok := true
	for _, item := range items {
		if item.Error != "" {
			ok = false
			break
		}
		if item.Result != nil && item.Result.Status == engine.StatusFailed {
			ok = false
			break
		}
	}
	return emitJSON(jsonCommandResult{
		Command: command,
		OK:      ok,
		Results: items,
	})
}

func jsonResultItemFromInstall(ref string, result *engine.Result, err error) jsonResultItem {
	item := jsonResultItem{Ref: ref}
	if err != nil {
		item.Error = err.Error()
		return item
	}
	if result != nil && result.Error != nil {
		item.Error = result.Error.Error()
		item.Result = result
		return item
	}
	item.Result = result
	return item
}

// initJSONOutput configures the root --json flag and hooks.
func initJSONOutput() {
	rootCmd.PersistentFlags().Bool("json", false, "machine-readable JSON on stdout (no colors or progress)")
	cobra.OnInitialize(func() {
		if !jsonOutputEnabled() {
			return
		}
		setColorEnabled(false)
	})
}
