package main

// Registry setup for CLI commands that read bucket manifests directly.

import "github.com/gluestick-sh/core/bucket"

// loadBucketRegistry opens the bucket registry and registers local bucket dirs.
func loadBucketRegistry(root string) (*bucket.Registry, error) {
	registry, err := bucket.NewRegistry(root)
	if err != nil {
		return nil, err
	}
	if err := registry.ReloadFromDisk(); err != nil {
		return nil, err
	}
	return registry, nil
}
