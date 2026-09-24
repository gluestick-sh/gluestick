// Package snapshot defines the portable environment snapshot format and
// pure diff helpers used by Device Sync, Backup, and local export/import.
package snapshot

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// ErrInvalidFormat indicates the file is not a GlueStick environment snapshot.
var ErrInvalidFormat = errors.New("invalid environment snapshot format")

const (
	Kind          = "gluestick.environment-snapshot"
	SchemaVersion = 1

	SourceManual       = "manual"
	SourceAutoBackup   = "auto-backup"
	SourceSync         = "sync"
	SourceProfile      = "profile"
	SourceRecipe       = "recipe"
	SourceWorkspace    = "workspace" // legacy alias; prefer SourceRecipe
	SourceRestorePoint = "restore-point"

	ApplyModeInstallMissing = "install-missing"
	ApplyModeReconcile      = "reconcile"
)

// Meta is optional metadata supplied when exporting a snapshot.
type Meta struct {
	ID     string
	Source string
	Notes  string
}

// Snapshot is a portable environment description (device + core state).
// Desktop may attach a "desktop" segment outside this type when assembling
// a full sync document; core Import/Export only round-trips this shape.
type Snapshot struct {
	SchemaVersion int     `json:"schemaVersion"`
	Kind          string  `json:"kind"`
	ID            string  `json:"id"`
	CreatedAt     string  `json:"createdAt"`
	Source        string  `json:"source,omitempty"`
	Notes         string  `json:"notes,omitempty"`
	Device        Device  `json:"device"`
	Core          CoreState `json:"core"`
}

// Device is the snapshot header copied from device.Ensure (not applied as identity).
type Device struct {
	DeviceID    string `json:"deviceId"`
	DisplayName string `json:"displayName,omitempty"`
	Hostname    string `json:"hostname,omitempty"`
	OS          string `json:"os"`
	Arch        string `json:"arch"`
}

// CoreState is the engine-owned environment intent.
type CoreState struct {
	Packages []Package `json:"packages"`
	Buckets  []Bucket  `json:"buckets"`
	Config   Config    `json:"config"`
}

// Package is one installed package version. Multi-version installs are stored
// as multiple entries sharing Name with different Version values.
// Current marks the active version (apps/<name>/current); at most one entry
// per Name should set Current.
type Package struct {
	Name          string `json:"name"`
	Bucket        string `json:"bucket,omitempty"`
	Version       string `json:"version,omitempty"`
	Current       bool   `json:"current,omitempty"`
	VersionLocked bool   `json:"versionLocked,omitempty"`
}

// Bucket is a bucket source intent.
type Bucket struct {
	Name string `json:"name"`
	URL  string `json:"url,omitempty"`
}

// Config is the syncable subset of ~/.glue/config.json.
type Config struct {
	GitHubProxy     string `json:"githubProxy,omitempty"`
	DownloadWorkers *int   `json:"downloadWorkers,omitempty"`
	BucketSyncMode  string `json:"bucketSyncMode,omitempty"`
}

// ApplyOptions controls Diff/Apply behaviour.
type ApplyOptions struct {
	Mode   string // install-missing (default) | reconcile (reserved)
	DryRun bool
}

// Plan is the result of Diff / dry-run Apply.
type Plan struct {
	BucketsToAdd       []Bucket        `json:"bucketsToAdd,omitempty"`
	PackagesToInstall  []Package       `json:"packagesToInstall,omitempty"`
	PackagesToActivate []Package       `json:"packagesToActivate,omitempty"`
	ConfigChanges      []ConfigChange  `json:"configChanges,omitempty"`
	// Reserved for reconcile mode (empty in install-missing MVP).
	BucketsToRemove  []string `json:"bucketsToRemove,omitempty"`
	PackagesToRemove []string `json:"packagesToRemove,omitempty"`
}

// ConfigChange describes one config key change.
type ConfigChange struct {
	Key  string `json:"key"`
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
}

// NewID returns a random snapshot id (32 hex chars).
func NewID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate snapshot id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// Validate checks required snapshot fields.
func Validate(s *Snapshot) error {
	if s == nil {
		return fmt.Errorf("snapshot is nil")
	}
	if s.Kind != "" && s.Kind != Kind {
		return fmt.Errorf("%w: unsupported kind %q", ErrInvalidFormat, s.Kind)
	}
	if s.SchemaVersion != 0 && s.SchemaVersion != SchemaVersion {
		return fmt.Errorf("%w: unsupported schemaVersion %d", ErrInvalidFormat, s.SchemaVersion)
	}
	if strings.TrimSpace(s.Device.DeviceID) == "" {
		return fmt.Errorf("%w", ErrInvalidFormat)
	}
	for i, pkg := range s.Core.Packages {
		if strings.TrimSpace(pkg.Name) == "" {
			return fmt.Errorf("%w: core.packages[%d] name is required", ErrInvalidFormat, i)
		}
	}
	for i, b := range s.Core.Buckets {
		if strings.TrimSpace(b.Name) == "" {
			return fmt.Errorf("%w: core.buckets[%d] name is required", ErrInvalidFormat, i)
		}
	}
	return nil
}

// NormalizeMode returns a supported apply mode.
func NormalizeMode(mode string) string {
	switch strings.TrimSpace(strings.ToLower(mode)) {
	case ApplyModeReconcile:
		return ApplyModeReconcile
	default:
		return ApplyModeInstallMissing
	}
}

// Empty reports whether the plan has no actions.
func (p *Plan) Empty() bool {
	if p == nil {
		return true
	}
	return len(p.BucketsToAdd) == 0 &&
		len(p.PackagesToInstall) == 0 &&
		len(p.PackagesToActivate) == 0 &&
		len(p.ConfigChanges) == 0 &&
		len(p.BucketsToRemove) == 0 &&
		len(p.PackagesToRemove) == 0
}

// WriteFile writes snapshot JSON atomically.
func WriteFile(path string, s *Snapshot) error {
	if s == nil {
		return fmt.Errorf("snapshot is nil")
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal snapshot: %w", err)
	}
	data = append(data, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return fmt.Errorf("write temp snapshot: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("replace snapshot: %w", err)
	}
	return nil
}

// ReadFile loads and validates a snapshot JSON file.
func ReadFile(path string) (*Snapshot, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s Snapshot
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidFormat, err)
	}
	// Do not invent a kind for arbitrary JSON: only fill defaults when the
	// document already looks like a snapshot (has a device id).
	if strings.TrimSpace(s.Kind) == "" {
		if strings.TrimSpace(s.Device.DeviceID) == "" {
			return nil, fmt.Errorf("%w", ErrInvalidFormat)
		}
		s.Kind = Kind
	}
	if s.SchemaVersion == 0 {
		s.SchemaVersion = SchemaVersion
	}
	if err := Validate(&s); err != nil {
		return nil, err
	}
	return &s, nil
}

// NowRFC3339 returns UTC RFC3339 timestamp.
func NowRFC3339() string {
	return time.Now().UTC().Format(time.RFC3339)
}
