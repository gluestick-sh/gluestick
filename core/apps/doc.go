// Package apps implements the on-disk layout under ~/.glue/apps
// (version directories, current link, install.json records).
//
// External integrators should use the engine package instead of importing apps
// directly (see engine.ParseInstallFilePath, engine.EnsureInstalledVersion,
// engine.ListInstalledAllVersions, and related helpers).
// This package remains exported for engine internals and test fixtures.
package apps
