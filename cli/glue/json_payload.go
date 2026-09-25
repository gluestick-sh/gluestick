package main

import "github.com/gluestick-sh/core/engine"

// Shared JSON payload types for the read commands that both the CLI --json
// path and the MCP read tools emit. Defining the schema once as a typed struct
// (instead of ad-hoc map literals in each caller) keeps the two entry points
// marshalling the same shape from one definition; docs/json-schema.md documents
// these fields.

// jsonSearchResult is the payload for `glue search --json` / glue_search.
type jsonSearchResult struct {
	Query   string            `json:"query"`
	Results []*engine.Package `json:"results"`
	Count   int               `json:"count"`
}

// jsonListResult is the payload for `glue list --json` / glue_list.
type jsonListResult struct {
	Packages []*engine.Package `json:"packages"`
	Count    int               `json:"count"`
}

// jsonInfoResult is the payload for `glue info --json` / glue_info.
type jsonInfoResult struct {
	Packages []*engine.InstalledPackageDetail `json:"packages"`
	Count    int                              `json:"count"`
	Failed   []string                         `json:"failed,omitempty"`
}

// jsonPathCheckResult is the payload for `glue path check --json` / glue_path_check.
type jsonPathCheckResult struct {
	InPath              bool   `json:"in_path"`
	BinDir              string `json:"bin_dir"`
	StoreAliasShadowing bool   `json:"store_alias_shadowing"`
	OK                  bool   `json:"ok"`
}

// jsonBucketListResult is the payload for `glue bucket list --json` / glue_bucket_list.
type jsonBucketListResult struct {
	Buckets []jsonBucketEntry `json:"buckets"`
	Count   int               `json:"count"`
}
