// Package device manages the stable identity of a glue data root (~/.glue/device.json).
//
// One RootDir maps to one deviceId. CLI and Desktop share this file; cloud sync
// uses deviceId as the device primary key. This file is not a credential store.
package device

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// SchemaVersion is the current device.json schema.
	SchemaVersion = 1

	// FileName is the device identity file under the glue root.
	FileName = "device.json"

	maxDisplayLen = 64
)

// Platform describes the host environment (refreshable; not the primary key).
type Platform struct {
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	Hostname string `json:"hostname,omitempty"`
}

// ClientInfo records the last time a client (cli/desktop) touched this root.
type ClientInfo struct {
	AppVersion string `json:"appVersion,omitempty"`
	LastSeenAt string `json:"lastSeenAt,omitempty"`
}

// Info is the on-disk device identity document.
type Info struct {
	SchemaVersion int                   `json:"schemaVersion"`
	DeviceID      string                `json:"deviceId"`
	CreatedAt     string                `json:"createdAt"`
	DisplayName   string                `json:"displayName,omitempty"`
	Platform      Platform              `json:"platform"`
	Clients       map[string]ClientInfo `json:"clients,omitempty"`
}

// Path returns ~/.glue/device.json for the given root.
func Path(rootDir string) string {
	return filepath.Join(rootDir, FileName)
}

// Ensure reads or creates device.json. DeviceID is stable once written.
func Ensure(rootDir string) (*Info, error) {
	if strings.TrimSpace(rootDir) == "" {
		return nil, fmt.Errorf("glue root directory unavailable")
	}
	if err := os.MkdirAll(rootDir, 0755); err != nil {
		return nil, fmt.Errorf("create glue root: %w", err)
	}

	info, err := readFile(Path(rootDir))
	if err == nil {
		refreshPlatform(info)
		if err := writeFile(Path(rootDir), info); err != nil {
			// Platform refresh is best-effort; identity is still usable.
			return info, nil
		}
		return info, nil
	}
	if !os.IsNotExist(err) && !isCorrupt(err) {
		return nil, err
	}
	if isCorrupt(err) {
		_ = quarantineCorrupt(Path(rootDir))
	}

	info, err = newInfo()
	if err != nil {
		return nil, err
	}
	if err := writeFileAtomic(Path(rootDir), info); err != nil {
		// Another process may have created the file first.
		if existing, readErr := readFile(Path(rootDir)); readErr == nil {
			return existing, nil
		}
		return nil, err
	}
	return info, nil
}

// Get returns the current device info without creating a new identity.
// If the file is missing, it returns os.ErrNotExist (via Ensure-style read).
func Get(rootDir string) (*Info, error) {
	if strings.TrimSpace(rootDir) == "" {
		return nil, fmt.Errorf("glue root directory unavailable")
	}
	return readFile(Path(rootDir))
}

// SetDisplayName updates the user-facing device name.
func SetDisplayName(rootDir, name string) error {
	name = strings.TrimSpace(name)
	if utf8.RuneCountInString(name) > maxDisplayLen {
		return fmt.Errorf("display name too long (max %d characters)", maxDisplayLen)
	}
	info, err := Ensure(rootDir)
	if err != nil {
		return err
	}
	info.DisplayName = name
	refreshPlatform(info)
	return writeFileAtomic(Path(rootDir), info)
}

// TouchClient updates last-seen metadata for a client such as "cli" or "desktop".
func TouchClient(rootDir, client, appVersion string) error {
	client = strings.ToLower(strings.TrimSpace(client))
	if client == "" {
		return fmt.Errorf("client name is required")
	}
	if strings.ContainsAny(client, "/\\") || strings.Contains(client, "..") {
		return fmt.Errorf("invalid client name")
	}
	info, err := Ensure(rootDir)
	if err != nil {
		return err
	}
	if info.Clients == nil {
		info.Clients = map[string]ClientInfo{}
	}
	info.Clients[client] = ClientInfo{
		AppVersion: strings.TrimSpace(appVersion),
		LastSeenAt: time.Now().UTC().Format(time.RFC3339),
	}
	refreshPlatform(info)
	return writeFileAtomic(Path(rootDir), info)
}

// DisplayLabel returns displayName, or hostname, or deviceId as fallback.
func DisplayLabel(info *Info) string {
	if info == nil {
		return ""
	}
	if name := strings.TrimSpace(info.DisplayName); name != "" {
		return name
	}
	if host := strings.TrimSpace(info.Platform.Hostname); host != "" {
		return host
	}
	return info.DeviceID
}

type corruptError struct {
	err error
}

func (e *corruptError) Error() string {
	return e.err.Error()
}

func (e *corruptError) Unwrap() error {
	return e.err
}

func isCorrupt(err error) bool {
	var c *corruptError
	return errors.As(err, &c)
}

func readFile(path string) (*Info, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var info Info
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, &corruptError{err: fmt.Errorf("parse device.json: %w", err)}
	}
	if err := validate(&info); err != nil {
		return nil, &corruptError{err: err}
	}
	return &info, nil
}

func validate(info *Info) error {
	if info == nil {
		return fmt.Errorf("device.json: empty document")
	}
	if info.SchemaVersion < 1 {
		return fmt.Errorf("device.json: unsupported schemaVersion %d", info.SchemaVersion)
	}
	id := strings.TrimSpace(info.DeviceID)
	if !validDeviceID(id) {
		return fmt.Errorf("device.json: invalid deviceId")
	}
	info.DeviceID = id
	if strings.TrimSpace(info.CreatedAt) == "" {
		return fmt.Errorf("device.json: missing createdAt")
	}
	return nil
}

func validDeviceID(id string) bool {
	if len(id) < 16 || len(id) > 64 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'f':
		case r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}

func newInfo() (*Info, error) {
	id, err := newDeviceID()
	if err != nil {
		return nil, err
	}
	info := &Info{
		SchemaVersion: SchemaVersion,
		DeviceID:      id,
		CreatedAt:     time.Now().UTC().Format(time.RFC3339),
		Clients:       map[string]ClientInfo{},
	}
	refreshPlatform(info)
	return info, nil
}

func newDeviceID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate device id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

func refreshPlatform(info *Info) {
	host, _ := os.Hostname()
	info.Platform = Platform{
		OS:       runtime.GOOS,
		Arch:     runtime.GOARCH,
		Hostname: host,
	}
}

func writeFile(path string, info *Info) error {
	return writeFileAtomic(path, info)
}

func writeFileAtomic(path string, info *Info) error {
	if info.Clients != nil && len(info.Clients) == 0 {
		info.Clients = nil
	}
	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal device.json: %w", err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create device dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, "device.json.*.tmp")
	if err != nil {
		return fmt.Errorf("create temp device.json: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp device.json: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temp device.json: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp device.json: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace device.json: %w", err)
	}
	return nil
}

func quarantineCorrupt(path string) error {
	stamp := time.Now().UTC().Format("20060102T150405Z")
	dest := path + ".corrupt." + stamp
	if err := os.Rename(path, dest); err != nil {
		return err
	}
	return nil
}
