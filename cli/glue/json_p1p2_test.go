package main

// JSON coverage tests for the P1/P2 hardening batch:
// config get/set/unset/list, cache list/clear/gc/rebuild, hold/unhold, home, reset.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// --- config ---

func TestConfigJSON_listDefaults(t *testing.T) {
	root := t.TempDir()
	setJSONTestFlags(t, root)

	out := captureStdout(t, func() {
		if err := configListCmd.RunE(configListCmd, nil); err != nil {
			t.Fatalf("config list: %v", err)
		}
	})

	var res struct {
		Command     string `json:"command"`
		GitHubProxy string `json:"github_proxy"`
		ProxySet    bool   `json:"github_proxy_set"`
		Parallel    bool   `json:"parallel_download"`
		Color       bool   `json:"color"`
		Verbose     bool   `json:"verbose"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if res.GitHubProxy != "" || res.ProxySet {
		t.Fatalf("github_proxy = %q set=%v, want empty/false", res.GitHubProxy, res.ProxySet)
	}
	if !res.Parallel || !res.Color || res.Verbose {
		t.Fatalf("defaults: parallel=%v color=%v verbose=%v, want true/true/false", res.Parallel, res.Color, res.Verbose)
	}
}

func TestConfigJSON_setGetUnsetRoundtrip(t *testing.T) {
	root := t.TempDir()
	setJSONTestFlags(t, root)

	// set
	out := captureStdout(t, func() {
		if err := configSetCmd.RunE(configSetCmd, []string{"parallel_download", "false"}); err != nil {
			t.Fatalf("config set: %v", err)
		}
	})
	var setRes struct {
		Command string `json:"command"`
		OK      bool   `json:"ok"`
		Key     string `json:"key"`
		Value   bool   `json:"value"`
	}
	if err := json.Unmarshal([]byte(out), &setRes); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if !setRes.OK || setRes.Key != "parallel_download" || setRes.Value {
		t.Fatalf("set result: %+v", setRes)
	}

	// get (explicit set)
	out = captureStdout(t, func() {
		if err := configGetCmd.RunE(configGetCmd, []string{"parallel_download"}); err != nil {
			t.Fatalf("config get: %v", err)
		}
	})
	var getRes struct {
		Command string `json:"command"`
		Key     string `json:"key"`
		Value   bool   `json:"value"`
		Set     bool   `json:"set"`
	}
	if err := json.Unmarshal([]byte(out), &getRes); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if getRes.Value || !getRes.Set {
		t.Fatalf("get after set: %+v, want value=false set=true", getRes)
	}

	// unset (was set)
	out = captureStdout(t, func() {
		if err := configUnsetCmd.RunE(configUnsetCmd, []string{"parallel_download"}); err != nil {
			t.Fatalf("config unset: %v", err)
		}
	})
	var unsetRes struct {
		Command string `json:"command"`
		OK      bool   `json:"ok"`
		WasSet  bool   `json:"was_set"`
	}
	if err := json.Unmarshal([]byte(out), &unsetRes); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if !unsetRes.OK || !unsetRes.WasSet {
		t.Fatalf("unset result: %+v, want ok=true was_set=true", unsetRes)
	}

	// unset again (idempotent: was_set=false, still ok)
	out = captureStdout(t, func() {
		if err := configUnsetCmd.RunE(configUnsetCmd, []string{"parallel_download"}); err != nil {
			t.Fatalf("config unset: %v", err)
		}
	})
	if err := json.Unmarshal([]byte(out), &unsetRes); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if !unsetRes.OK || unsetRes.WasSet {
		t.Fatalf("second unset: %+v, want ok=true was_set=false", unsetRes)
	}
}

func TestConfigJSON_getUnknownKey(t *testing.T) {
	root := t.TempDir()
	setJSONTestFlags(t, root)

	out := captureStdout(t, func() {
		err := configGetCmd.RunE(configGetCmd, []string{"no_such_key"})
		if err == nil {
			t.Fatal("expected failure for unknown key")
		}
		if code := exitCode(err); code != 1 {
			t.Fatalf("exitCode = %d, want 1", code)
		}
	})

	var res struct {
		Command string `json:"command"`
		OK      bool   `json:"ok"`
		Key     string `json:"key"`
		Error   string `json:"error"`
		Code    string `json:"code"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if res.OK || res.Key != "no_such_key" || res.Error == "" || res.Code != "invalid_argument" {
		t.Fatalf("error envelope: %+v\n%s", res, out)
	}
}

// --- cache ---

func TestCacheJSON_listEmpty(t *testing.T) {
	root := t.TempDir()
	setJSONTestFlags(t, root)

	out := captureStdout(t, func() {
		if err := cacheListCmd.RunE(cacheListCmd, nil); err != nil {
			t.Fatalf("cache list: %v", err)
		}
	})

	var res struct {
		Packages []json.RawMessage `json:"packages"`
		Total    struct {
			PackageCount int `json:"packageCount"`
		} `json:"total"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if res.Packages == nil || len(res.Packages) != 0 {
		t.Fatalf("packages must be an empty array: %s", out)
	}
}

func TestCacheJSON_clearNotIndexed(t *testing.T) {
	root := t.TempDir()
	setJSONTestFlags(t, root)

	out := captureStdout(t, func() {
		err := cacheClearCmd.RunE(cacheClearCmd, []string{"missing-pkg"})
		if err == nil {
			t.Fatal("expected failure when nothing is cleared")
		}
		if code := exitCode(err); code != 1 {
			t.Fatalf("exitCode = %d, want 1", code)
		}
	})

	var res struct {
		Command    string   `json:"command"`
		OK         bool     `json:"ok"`
		Cleared    []string `json:"cleared"`
		NotInIndex []string `json:"not_in_index"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if res.OK || len(res.Cleared) != 0 || len(res.NotInIndex) != 1 || res.NotInIndex[0] != "missing-pkg" {
		t.Fatalf("clear result: %+v\n%s", res, out)
	}
}

func TestCacheJSON_clearAllEmptyGCRebuild(t *testing.T) {
	root := t.TempDir()
	setJSONTestFlags(t, root)

	// clear --all on empty index: ok=true, packages=0
	if err := cacheClearCmd.Flags().Set("all", "true"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cacheClearCmd.Flags().Set("all", "false") })

	out := captureStdout(t, func() {
		if err := cacheClearCmd.RunE(cacheClearCmd, nil); err != nil {
			t.Fatalf("cache clear --all: %v", err)
		}
	})
	var clearRes struct {
		OK      bool `json:"ok"`
		All     bool `json:"all"`
		Package int  `json:"packages"`
	}
	if err := json.Unmarshal([]byte(out), &clearRes); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if !clearRes.OK || !clearRes.All || clearRes.Package != 0 {
		t.Fatalf("clear --all result: %+v\n%s", clearRes, out)
	}

	// gc on empty root: ok=true, removed 0
	out = captureStdout(t, func() {
		if err := runCacheGC(cacheGCCmd, nil); err != nil {
			t.Fatalf("cache gc: %v", err)
		}
	})
	var gcRes struct {
		Command      string `json:"command"`
		OK           bool   `json:"ok"`
		RemovedBlobs int    `json:"removed_blobs"`
	}
	if err := json.Unmarshal([]byte(out), &gcRes); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if !gcRes.OK || gcRes.RemovedBlobs != 0 {
		t.Fatalf("gc result: %+v\n%s", gcRes, out)
	}

	// rebuild with no apps: indexed 0, ok=true
	out = captureStdout(t, func() {
		if err := cacheRebuildCmd.RunE(cacheRebuildCmd, nil); err != nil {
			t.Fatalf("cache rebuild: %v", err)
		}
	})
	var rebuildRes struct {
		Command string `json:"command"`
		OK      bool   `json:"ok"`
		Indexed int    `json:"indexed"`
	}
	if err := json.Unmarshal([]byte(out), &rebuildRes); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if !rebuildRes.OK || rebuildRes.Indexed != 0 {
		t.Fatalf("rebuild result: %+v\n%s", rebuildRes, out)
	}
}

// --- hold / reset error paths ---

func TestHoldJSON_missingPackage(t *testing.T) {
	root := t.TempDir()
	setJSONTestFlags(t, root)

	out := captureStdout(t, func() {
		err := runHold(holdCmd, []string{"definitely-missing"})
		if err == nil {
			t.Fatal("expected failure for missing package")
		}
		if code := exitCode(err); code != 1 {
			t.Fatalf("exitCode = %d, want 1", code)
		}
	})

	var res struct {
		Command string `json:"command"`
		OK      bool   `json:"ok"`
		Results []struct {
			Ref   string `json:"ref"`
			Error string `json:"error"`
			Code  string `json:"code"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if res.Command != "hold" || res.OK {
		t.Fatalf("hold result: command=%q ok=%v\n%s", res.Command, res.OK, out)
	}
	if len(res.Results) != 1 || res.Results[0].Error == "" || res.Results[0].Code == "" {
		t.Fatalf("hold results: %+v\n%s", res.Results, out)
	}
}

func TestResetJSON_missingPackage(t *testing.T) {
	root := t.TempDir()
	setJSONTestFlags(t, root)

	out := captureStdout(t, func() {
		err := runReset(resetCmd, []string{"definitely-missing"})
		if err == nil {
			t.Fatal("expected failure for missing package")
		}
		if code := exitCode(err); code != 1 {
			t.Fatalf("exitCode = %d, want 1", code)
		}
	})

	var res struct {
		Command string `json:"command"`
		OK      bool   `json:"ok"`
		Results []struct {
			Ref   string `json:"ref"`
			Error string `json:"error"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if res.Command != "reset" || res.OK {
		t.Fatalf("reset result: command=%q ok=%v\n%s", res.Command, res.OK, out)
	}
	if len(res.Results) != 1 || res.Results[0].Error == "" {
		t.Fatalf("reset results: %+v\n%s", res.Results, out)
	}
}

// --- home ---

func TestHomeJSON_returnsURLWithoutOpeningBrowser(t *testing.T) {
	root := t.TempDir()
	setJSONTestFlags(t, root)
	homeCmd.SetContext(nil)

	bucketDir := filepath.Join(root, "buckets", "demo", "bucket")
	if err := os.MkdirAll(bucketDir, 0755); err != nil {
		t.Fatal(err)
	}
	manifest := `{"version": "1.0.0", "url": "https://example.com/hello.zip", "hash": "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad", "homepage": "https://example.com/hello"}`
	if err := os.WriteFile(filepath.Join(bucketDir, "hello.json"), []byte(manifest), 0644); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		if err := runHome(homeCmd, []string{"demo/hello"}); err != nil {
			t.Fatalf("home: %v", err)
		}
	})

	var res struct {
		Command string `json:"command"`
		OK      bool   `json:"ok"`
		Results []struct {
			Ref    string `json:"ref"`
			URL    string `json:"url"`
			Opened bool   `json:"opened"`
			Error  string `json:"error"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if !res.OK || len(res.Results) != 1 {
		t.Fatalf("home result: ok=%v len=%d\n%s", res.OK, len(res.Results), out)
	}
	item := res.Results[0]
	if item.URL != "https://example.com/hello" || item.Opened {
		t.Fatalf("home item: %+v (want url set, opened=false)", item)
	}
}
