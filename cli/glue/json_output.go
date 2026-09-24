package main

import (
	"encoding/json"
	"errors"
	"os"
	"strings"

	"github.com/gluestick-sh/core/apperr"
	"github.com/gluestick-sh/core/engine"
	"github.com/spf13/cobra"
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
	Ref    string         `json:"ref"`
	Result *engine.Result `json:"result,omitempty"`
	Error  string         `json:"error,omitempty"`
	Code   string         `json:"code,omitempty"` // stable machine-readable error code (apperr mapping)
	Hint   string         `json:"hint,omitempty"` // optional remediation hint
}

// jsonErrorInfo maps an error to a stable machine-readable code and optional hint.
// Codes are part of the agent contract (roadmap §3.2) and must only be added, never renamed.
func jsonErrorInfo(err error) (code, hint string) {
	if err == nil {
		return "", ""
	}
	var suggest *apperr.ManifestSuggest
	var bucketMissing *apperr.BucketNotInstalled
	switch {
	case errors.As(err, &suggest), errors.Is(err, apperr.ErrManifestNotFound):
		code = "manifest_not_found"
		if suggest != nil {
			hint = strings.Join(suggest.Hints, "\n")
		}
	case errors.Is(err, apperr.ErrManifestAmbiguous):
		code = "manifest_ambiguous"
	case errors.As(err, &bucketMissing), errors.Is(err, apperr.ErrBucketNotInstalled):
		code = "bucket_not_installed"
	case errors.Is(err, apperr.ErrPackageNotInstalled):
		code = "package_not_installed"
	case errors.Is(err, errUsage):
		code = "usage"
	default:
		code = "unknown"
	}
	return code, hint
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
		item.Code, item.Hint = jsonErrorInfo(err)
		return item
	}
	if result != nil && result.Error != nil {
		item.Error = result.Error.Error()
		item.Code, item.Hint = jsonErrorInfo(result.Error)
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
