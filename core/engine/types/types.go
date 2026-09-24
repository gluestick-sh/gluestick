// Package types defines the shared data structures exchanged between the engine
// core and its callers: progress events, install/uninstall/search requests, and results.
package types

import (
	"context"
	"time"
)

// ProgressEvent represents a progress event during package operations.
type ProgressEvent struct {
	Phase       Phase                  `json:"phase"`
	Package     string                 `json:"package"`
	Item        string                 `json:"item,omitempty"`
	Status      Status                 `json:"status"`
	MessageKey  string                 `json:"messageKey,omitempty"`
	MessageArgs map[string]interface{} `json:"messageArgs,omitempty"`
	Message     string                 `json:"message"`
	Percentage  float64                `json:"percentage"`
	Bytes       int64                  `json:"bytes,omitempty"`
	TotalBytes  int64                  `json:"totalBytes,omitempty"`
	Error       error                  `json:"-"`
	Timestamp   time.Time              `json:"timestamp"`
}

// Phase represents different phases of package operations.
type Phase string

const (
	// PhaseResolve is the manifest resolution phase.
	PhaseResolve Phase = "resolve"
	// PhaseDownload is the download phase.
	PhaseDownload Phase = "download"
	// PhaseExtract is the archive extraction phase.
	PhaseExtract Phase = "extract"
	// PhaseLink is the file linking/deploy phase.
	PhaseLink Phase = "link"
	// PhaseShim is the shim creation phase.
	PhaseShim Phase = "shim"
	// PhaseIndex is the cache index update phase.
	PhaseIndex Phase = "index"
	// PhaseBootstrap is the tool bootstrap phase.
	PhaseBootstrap Phase = "bootstrap"
	// PhaseComplete marks successful completion.
	PhaseComplete Phase = "complete"
	// PhaseError marks a failed operation.
	PhaseError Phase = "error"
)

// Status represents the status of a progress event.
type Status string

const (
	// StatusRunning indicates the operation is in progress.
	StatusRunning Status = "running"
	// StatusSuccess indicates the operation succeeded.
	StatusSuccess Status = "success"
	// StatusFailed indicates the operation failed.
	StatusFailed Status = "failed"
	// StatusSkipped indicates the operation was skipped.
	StatusSkipped Status = "skipped"
	// StatusWaiting indicates the operation is waiting to start.
	StatusWaiting Status = "waiting"
	// StatusInfo indicates an informational event.
	StatusInfo Status = "info"
)

// Request represents a request for package operations.
type Request struct {
	Name       string
	Version    string
	Force      bool
	NoParallel bool
	Workers    int
	Options    map[string]string
	Context    context.Context
}

// InstallRequest represents a package installation request.
type InstallRequest struct {
	Request
	ManifestURL           string
	Buckets               []string
	DownloadURLOverrides  []string
	DownloadHashOverrides []string
}

// UninstallRequest represents a package uninstallation request.
type UninstallRequest struct {
	Request
	Purge bool
}

// SearchRequest represents a package search request.
type SearchRequest struct {
	Query   string
	Buckets []string
	Limit   int
}

// ListRequest represents a package listing request.
type ListRequest struct {
	Details    bool
	ShowHidden bool
}

// PackageSuggestion is a post-install soft recommendation exposed to callers.
type PackageSuggestion struct {
	Label string `json:"label"`
	Ref   string `json:"ref"`
}

// Result represents the result of a package operation.
type Result struct {
	Name        string
	Version     string
	Status      Status
	Message     string
	Duration    time.Duration
	Files       []string
	Size        int64
	Error       error
	Manifest    *ManifestInfo
	Suggestions []PackageSuggestion
}

// Package represents a package.
type Package struct {
	Name              string
	Version           string
	Description       string
	Homepage          string
	Bucket            string
	Deprecated        bool
	InstalledAt       string
	InstalledSize     int64
	DetectedVersion   string
	VersionDetectSrc  string
	ExternallyUpdated bool
	Manifest          *ManifestInfo
}

// ManifestInfo contains manifest information.
type ManifestInfo struct {
	URL          string       `json:"url,omitempty"`
	Hash         string       `json:"hash,omitempty"`
	Size         int64        `json:"size,omitempty"`
	ExtractDir   string       `json:"extractDir,omitempty"`
	Binaries     []BinaryInfo `json:"binaries,omitempty"`
	EnvPath      []string     `json:"envPath,omitempty"`
	Architecture string       `json:"architecture,omitempty"`
	Depends      []string     `json:"depends,omitempty"`
	PostInstall  string       `json:"postInstall,omitempty"`
	Description  string       `json:"description,omitempty"`
	Homepage     string       `json:"homepage,omitempty"`
}

// BinaryInfo represents binary/alias information.
type BinaryInfo struct {
	Name      string `json:"name"`
	Source    string `json:"source,omitempty"`
	Alias     string `json:"alias,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// ProgressReporter is an interface for reporting progress events.
type ProgressReporter interface {
	ReportProgress(event ProgressEvent)
}

// EngineConfig holds engine configuration.
type EngineConfig struct {
	RootDir  string
	Verbose  bool
	Workers  int
	Parallel bool
	Timeout  time.Duration
}

// EngineStats holds statistics about engine operations.
type EngineStats struct {
	TotalPackages   int64
	SuccessfulOps   int64
	FailedOps       int64
	TotalBytes      int64
	TotalDuration   time.Duration
	AverageDuration time.Duration
}
