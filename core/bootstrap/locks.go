package bootstrap

import (
	"os/exec"
	"sync"

	"github.com/gluestick-sh/core/procutil"
)

var (
	rootLocksMu sync.Mutex
	rootGitMu   = map[string]*sync.Mutex{}
	root7zMu    = map[string]*sync.Mutex{}
	rootDarkMu  = map[string]*sync.Mutex{}
	rootInnoMu  = map[string]*sync.Mutex{}
)

func lockFor(root string, table map[string]*sync.Mutex) *sync.Mutex {
	rootLocksMu.Lock()
	defer rootLocksMu.Unlock()
	if m, ok := table[root]; ok {
		return m
	}
	m := &sync.Mutex{}
	table[root] = m
	return m
}

func gitLock(root string) *sync.Mutex   { return lockFor(root, rootGitMu) }
func sevenZipLock(root string) *sync.Mutex { return lockFor(root, root7zMu) }
func darkLock(root string) *sync.Mutex  { return lockFor(root, rootDarkMu) }
func innounpLock(root string) *sync.Mutex { return lockFor(root, rootInnoMu) }

// GitExecutableReady reports whether path runs `git --version` successfully.
func GitExecutableReady(path string) bool {
	if path == "" {
		return false
	}
	cmd := exec.Command(path, "--version")
	procutil.HideWindow(cmd)
	return cmd.Run() == nil
}
