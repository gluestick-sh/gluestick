package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gluestick-sh/core/config"
	"github.com/gluestick-sh/core/verbose"
)

// auditSettingsMemo caches the config.json audit thresholds per data root so
// appends do not re-read the file, while still noticing edits (modtime+size).
type auditSettingsMemo struct {
	settings config.AuditSettings
	modTime  time.Time
	size     int64
}

var auditSettingsCache sync.Map // data root -> auditSettingsMemo

// auditSettingsFor returns the rotation thresholds for root from config.json's
// "audit" section, falling back to the built-in defaults when unset or
// unreadable. Rotation is disabled only by an explicit negative max_bytes.
func auditSettingsFor(root string) config.AuditSettings {
	fallback := config.DefaultAuditSettings()
	if root == "" {
		return fallback
	}
	info, err := os.Stat(config.Path(root))
	if err != nil {
		return fallback
	}
	if cached, ok := auditSettingsCache.Load(root); ok {
		memo, _ := cached.(auditSettingsMemo)
		if memo.size == info.Size() && memo.modTime.Equal(info.ModTime()) {
			return memo.settings
		}
	}
	settings, err := config.ReadAudit(root)
	if err != nil {
		return fallback
	}
	auditSettingsCache.Store(root, auditSettingsMemo{settings: settings, modTime: info.ModTime(), size: info.Size()})
	return settings
}

// AuditInfo tags audit rows with who performed the operation (roadmap §4.6.1).
type AuditInfo struct {
	Source string // "cli" | "mcp"
	Actor  string // MCP clientInfo, e.g. "cline/3.7"
}

type auditContextKey struct{}

// WithAudit attaches audit provenance to ctx; engine operations read it when
// recording activity. Missing info defaults to the CLI.
func WithAudit(ctx context.Context, info AuditInfo) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if info.Source == "" {
		info.Source = "cli"
	}
	return context.WithValue(ctx, auditContextKey{}, info)
}

// AuditFromContext returns the audit provenance (CLI when unset).
func AuditFromContext(ctx context.Context) AuditInfo {
	if ctx != nil {
		if info, ok := ctx.Value(auditContextKey{}).(AuditInfo); ok && info.Source != "" {
			return info
		}
	}
	return AuditInfo{Source: "cli"}
}

// RecordAudit writes one audit row to SQLite and appends the same entry to
// <root>/logs/audit.jsonl (append-only, survives `glue cache clear`).
func (e *Engine) RecordAudit(ctx context.Context, operation, pkgName, version, status string, details map[string]any) error {
	if e == nil || e.Engine == nil || e.Cache == nil {
		return fmt.Errorf("engine not configured")
	}
	info := AuditFromContext(ctx)
	if err := e.Cache.RecordActivityWithSource(operation, pkgName, version, status, info.Source, info.Actor, details); err != nil {
		return err
	}
	return e.appendAuditJSONL(info, operation, pkgName, version, status, details)
}

// QueryAuditLog returns filtered audit rows for `glue audit list`.
func (e *Engine) QueryAuditLog(source, pkgName, since string, limit int) ([]map[string]any, error) {
	if e == nil || e.Engine == nil || e.Cache == nil {
		return nil, fmt.Errorf("engine not configured")
	}
	return e.Cache.QueryAuditLog(source, pkgName, since, limit)
}

// AuditVerifyResult is the outcome of `glue audit verify`.
type AuditVerifyResult struct {
	OK       bool   `json:"ok"`
	Entries  int    `json:"entries"`
	BrokenAt int    `json:"brokenAt,omitempty"`
	Reason   string `json:"reason,omitempty"`
	// Anchor is the retained chain head of already-pruned segments (empty when
	// the chain is verified from genesis).
	Anchor string `json:"anchor,omitempty"`
}

// recordAuditWarn writes the audit row and warns on stderr when it fails, so a
// broken audit trail cannot fail silently.
func (e *Engine) recordAuditWarn(ctx context.Context, operation, pkgName, version, status string, details map[string]any) {
	if err := e.RecordAudit(ctx, operation, pkgName, version, status, details); err != nil {
		verbose.Progressf("Warning: audit write failed: %v\n", err)
	}
}

// VerifyAuditLog recomputes the JSONL hash chain across rotated segments and
// reports the first break.
func (e *Engine) VerifyAuditLog() (AuditVerifyResult, error) {
	if e == nil || e.Config == nil || e.Config.RootDir == "" {
		return AuditVerifyResult{}, fmt.Errorf("engine not configured")
	}
	anchor, err := readAuditAnchor(filepath.Join(e.Config.RootDir, "logs"))
	if err != nil {
		return AuditVerifyResult{}, err
	}
	prev := anchor
	entries := 0
	for _, path := range auditSegmentFiles(e.Config.RootDir) {
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return AuditVerifyResult{}, err
		}
		for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
			if len(bytes.TrimSpace(line)) == 0 {
				continue
			}
			entries++
			var entry map[string]any
			if err := json.Unmarshal(line, &entry); err != nil {
				return AuditVerifyResult{Entries: entries - 1, BrokenAt: entries, Reason: "invalid JSON: " + err.Error()}, nil
			}
			claimed, _ := entry["hash"].(string)
			gotPrev, _ := entry["prev"].(string)
			if gotPrev != prev {
				return AuditVerifyResult{Entries: entries - 1, BrokenAt: entries, Reason: "previous hash mismatch"}, nil
			}
			delete(entry, "hash")
			recomputed, err := auditHash(entry)
			if err != nil {
				return AuditVerifyResult{}, err
			}
			if claimed != recomputed {
				return AuditVerifyResult{Entries: entries - 1, BrokenAt: entries, Reason: "hash mismatch"}, nil
			}
			prev = claimed
		}
	}
	return AuditVerifyResult{OK: true, Entries: entries, Anchor: anchor}, nil
}

// auditSegmentFiles returns rotated segments plus the active file, in write
// order (oldest first).
func auditSegmentFiles(root string) []string {
	dir := filepath.Join(root, "logs")
	dirEntries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	segments := []string{}
	for _, entry := range dirEntries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, "audit-") && strings.HasSuffix(name, ".jsonl") {
			segments = append(segments, filepath.Join(dir, name))
		}
	}
	sort.Strings(segments)
	return append(segments, auditLogPath(root))
}

// auditAnchorPath is the sidecar that keeps the chain head of the oldest
// segments already pruned, so `glue audit verify` still has a trusted starting
// hash after rotation dropped them.
func auditAnchorPath(dir string) string { return filepath.Join(dir, "audit.anchor") }

// auditAnchor is the persisted chain anchor: the last hash of the newest pruned
// segment.
type auditAnchor struct {
	Pruned string `json:"pruned"`
	Hash   string `json:"hash"`
}

// readAuditAnchor returns the retained chain head ("" when no anchor exists).
func readAuditAnchor(dir string) (string, error) {
	data, err := os.ReadFile(auditAnchorPath(dir))
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var anchor auditAnchor
	if err := json.Unmarshal(data, &anchor); err != nil {
		return "", fmt.Errorf("parse audit anchor: %w", err)
	}
	return anchor.Hash, nil
}

// writeAuditAnchor records the last hash of a pruned segment (atomic rewrite).
func writeAuditAnchor(dir, segment, hash string) error {
	if hash == "" {
		return nil
	}
	data, err := json.Marshal(auditAnchor{Pruned: segment, Hash: hash})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	tmp := auditAnchorPath(dir) + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, auditAnchorPath(dir))
}

// rotateAuditLog renames the active log to a timestamped segment and prunes the
// oldest segments beyond keepSegments (negative keeps every segment).
func rotateAuditLog(path string, keepSegments int) error {
	segment := filepath.Join(filepath.Dir(path),
		"audit-"+time.Now().UTC().Format("20060102T150405.000000000Z")+".jsonl")
	if err := os.Rename(path, segment); err != nil {
		return err
	}
	pruneAuditSegments(filepath.Dir(path), keepSegments)
	return nil
}

// pruneAuditSegments deletes the oldest rotated segments beyond keepSegments
// after anchoring their chain head. keepSegments < 0 disables pruning.
func pruneAuditSegments(dir string, keepSegments int) {
	if keepSegments < 0 {
		return
	}
	dirEntries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	segments := []string{}
	for _, entry := range dirEntries {
		name := entry.Name()
		if !entry.IsDir() && strings.HasPrefix(name, "audit-") && strings.HasSuffix(name, ".jsonl") {
			segments = append(segments, name)
		}
	}
	sort.Strings(segments)
	for len(segments) > keepSegments {
		oldest := segments[0]
		// Anchor the chain head before dropping the segment so verification can
		// still walk the surviving entries (and detect manual deletions).
		if hash, err := lastAuditHash(filepath.Join(dir, oldest)); err == nil {
			_ = writeAuditAnchor(dir, oldest, hash)
		}
		_ = os.Remove(filepath.Join(dir, oldest))
		segments = segments[1:]
	}
}

// QueryAuditJSONL reads the hash-chained JSONL files (rotated + active) with
// the same filters as QueryAuditLog, newest first.
func (e *Engine) QueryAuditJSONL(source, pkgName, since string, limit int) ([]map[string]any, error) {
	if e == nil || e.Config == nil || e.Config.RootDir == "" {
		return nil, fmt.Errorf("engine not configured")
	}
	entries := []map[string]any{}
	for _, path := range auditSegmentFiles(e.Config.RootDir) {
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
			if len(bytes.TrimSpace(line)) == 0 {
				continue
			}
			var entry map[string]any
			if err := json.Unmarshal(line, &entry); err != nil {
				continue
			}
			if source != "" && entry["source"] != source {
				continue
			}
			if pkgName != "" && entry["package"] != pkgName {
				continue
			}
			if since != "" {
				if ts, _ := entry["timestamp"].(string); ts < since {
					continue
				}
			}
			entries = append(entries, entry)
		}
	}
	for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
		entries[i], entries[j] = entries[j], entries[i]
	}
	if limit > 0 && len(entries) > limit {
		entries = entries[:limit]
	}
	return entries, nil
}

func auditLogPath(root string) string {
	return filepath.Join(root, "logs", "audit.jsonl")
}

func auditHash(entry map[string]any) (string, error) {
	line, err := json.Marshal(entry)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(line)
	return hex.EncodeToString(sum[:]), nil
}

// lastAuditHash returns the hash of the last JSONL line ("" when none), reading
// only the file tail so appends stay O(1).
func lastAuditHash(path string) (string, error) {
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	size := info.Size()
	if size == 0 {
		return "", nil
	}
	const tailBytes = 64 * 1024
	start := int64(0)
	if size > tailBytes {
		start = size - tailBytes
	}
	buf := make([]byte, size-start)
	if _, err := file.ReadAt(buf, start); err != nil && err != io.EOF {
		return "", err
	}
	trimmed := bytes.TrimRight(buf, "\r\n \t")
	line := trimmed
	if idx := bytes.LastIndexByte(trimmed, '\n'); idx >= 0 {
		line = trimmed[idx+1:]
	}
	var entry map[string]any
	if err := json.Unmarshal(line, &entry); err != nil {
		return "", fmt.Errorf("parse last audit entry: %w", err)
	}
	hash, _ := entry["hash"].(string)
	return hash, nil
}

// lastAuditChainHash returns the chain head: the active file's last hash, or the
// newest rotated segment's hash when the active file is empty (fresh process or
// just after rotation).
func lastAuditChainHash(root string) (string, error) {
	active := auditLogPath(root)
	if hash, err := lastAuditHash(active); err != nil || hash != "" {
		return hash, err
	}
	files := auditSegmentFiles(root)
	for i := len(files) - 1; i >= 0; i-- {
		if files[i] == active {
			continue
		}
		if hash, err := lastAuditHash(files[i]); err == nil && hash != "" {
			return hash, nil
		}
	}
	return "", nil
}

// acquireAuditLock serializes JSONL appends across processes via an exclusive
// lock on <root>/logs/audit.lock. Returns an unlock func.
func acquireAuditLock(dir string) (func(), error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(dir, "audit.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := lockAuditFile(file); err == nil {
			return func() {
				_ = unlockAuditFile(file)
				_ = file.Close()
			}, nil
		} else if time.Now().After(deadline) {
			_ = file.Close()
			return nil, fmt.Errorf("audit lock timeout: %w", err)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// appendAuditJSONL mirrors one audit row into the append-only, hash-chained
// JSONL log (each entry carries the previous entry's hash).
func (e *Engine) appendAuditJSONL(info AuditInfo, operation, pkgName, version, status string, details map[string]any) error {
	if e.Config == nil || e.Config.RootDir == "" {
		return nil
	}
	path := auditLogPath(e.Config.RootDir)
	unlock, err := acquireAuditLock(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer unlock()
	prev, err := lastAuditChainHash(e.Config.RootDir)
	if err != nil {
		return err
	}
	// Rotate before appending; prev carries the chain across segments.
	settings := auditSettingsFor(e.Config.RootDir)
	if info, statErr := os.Stat(path); statErr == nil && settings.RotationEnabled() && info.Size() >= settings.MaxBytes {
		if err := rotateAuditLog(path, settings.KeepSegments); err != nil {
			return err
		}
	}
	entry := map[string]any{
		"timestamp": time.Now().Format(time.RFC3339Nano),
		"operation": operation,
		"package":   pkgName,
		"version":   version,
		"status":    status,
		"source":    info.Source,
		"actor":     info.Actor,
		"details":   details,
		"prev":      prev,
	}
	hash, err := auditHash(entry)
	if err != nil {
		return err
	}
	entry["hash"] = hash
	line, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = file.Write(append(line, '\n'))
	return err
}
