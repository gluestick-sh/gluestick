package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Confirm tokens for MCP write operations. Tokens are persisted under
// <root>/logs/pending/<token>.json (0600) so a token issued before a server
// restart, or by another glue process, can still be confirmed. Consumption
// uses an atomic rename, so a token is single-use across processes.

const mcpConfirmTTL = 5 * time.Minute

// mcpPendingAction is the operation bound to a confirm token.
type mcpPendingAction struct {
	Op      string    `json:"op"`
	Package string    `json:"package"`
	Flag    bool      `json:"flag"` // install force / uninstall purge / update all
	Arg     string    `json:"arg,omitempty"`
	Expires time.Time `json:"expires"`
}

func mcpPendingDir(root string) string {
	return filepath.Join(root, "logs", "pending")
}

func mcpPendingPath(root, token string) string {
	return filepath.Join(mcpPendingDir(root), token+".json")
}

// mcpIssueConfirm persists a new pending action and returns its token.
func mcpIssueConfirm(root, op, pkg string, flag bool, arg string) (string, int, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", 0, fmt.Errorf("generate confirm token: %w", err)
	}
	token := hex.EncodeToString(buf)
	if err := os.MkdirAll(mcpPendingDir(root), 0700); err != nil {
		return "", 0, err
	}
	pruneMCPPending(root)
	action := mcpPendingAction{
		Op:      op,
		Package: pkg,
		Flag:    flag,
		Arg:     arg,
		Expires: time.Now().Add(mcpConfirmTTL),
	}
	data, err := json.Marshal(action)
	if err != nil {
		return "", 0, err
	}
	if err := os.WriteFile(mcpPendingPath(root, token), data, 0600); err != nil {
		return "", 0, err
	}
	return token, int(mcpConfirmTTL.Seconds()), nil
}

// mcpConsumeConfirm claims a token exactly once (atomic rename) and returns the
// bound operation. Unknown, used or expired tokens are rejected.
func mcpConsumeConfirm(root, token string) (mcpPendingAction, error) {
	token = strings.TrimSpace(token)
	if token == "" || strings.ContainsAny(token, `/\`) {
		return mcpPendingAction{}, mcpInvalidTokenError()
	}
	path := mcpPendingPath(root, token)
	claimed := path + ".used"
	if err := os.Rename(path, claimed); err != nil {
		return mcpPendingAction{}, mcpInvalidTokenError()
	}
	defer os.Remove(claimed)

	data, err := os.ReadFile(claimed)
	if err != nil {
		return mcpPendingAction{}, mcpInvalidTokenError()
	}
	var action mcpPendingAction
	if err := json.Unmarshal(data, &action); err != nil {
		return mcpPendingAction{}, mcpInvalidTokenError()
	}
	if time.Now().After(action.Expires) {
		return mcpPendingAction{}, mcpInvalidTokenError()
	}
	return action, nil
}

// pruneMCPPending removes expired pending files.
func pruneMCPPending(root string) {
	entries, err := os.ReadDir(mcpPendingDir(root))
	if err != nil {
		return
	}
	now := time.Now()
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(mcpPendingDir(root), entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var action mcpPendingAction
		if err := json.Unmarshal(data, &action); err != nil || now.After(action.Expires) {
			_ = os.Remove(path)
		}
	}
}

func mcpInvalidTokenError() error {
	return &mcpToolError{
		Code:    "invalid_confirm_token",
		Message: "confirm token is unknown, expired or already used",
		Hint:    "call the write tool again to get a fresh token",
	}
}
