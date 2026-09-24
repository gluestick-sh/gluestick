package launch

import (
	"errors"
	"path/filepath"
	"strings"

	"github.com/gluestick-sh/core/engine/internal/install"
	"github.com/gluestick-sh/core/engine/internal/runtime"
	"github.com/gluestick-sh/core/manifest"
	"github.com/gluestick-sh/core/message"
)

var errLaunchNotOpenable = errors.New(message.FormatEN(message.ErrLaunchNotOpenable, nil))

// LaunchKind describes how a runnable file is opened.
type LaunchKind string

const (
	// LaunchKindConsole opens the file in a console window.
	LaunchKindConsole LaunchKind = "console"
	// LaunchKindGUI opens the file as a GUI application.
	LaunchKindGUI LaunchKind = "gui"
	// LaunchKindSkip hides the file from the open-program menu.
	LaunchKindSkip LaunchKind = "skip"
)

// LaunchSource identifies where a launch target was discovered.
type LaunchSource int

const (
	// LaunchSourceShortcut is a target declared as a manifest shortcut.
	LaunchSourceShortcut LaunchSource = iota
	// LaunchSourceBin is a target declared as a manifest binary.
	LaunchSourceBin
	// LaunchSourceScan is a target found by scanning the install directory.
	LaunchSourceScan
)

type launchPathEntry struct {
	absPath string
	label   string
	source  LaunchSource
}

func (k LaunchKind) openable() bool {
	return k == LaunchKindConsole || k == LaunchKindGUI
}

// Openable reports whether this launch kind may be opened.
func (k LaunchKind) Openable() bool {
	return k.openable()
}

// EffectiveLaunchKind returns the launch kind for absPath, preferring a user
// override from the launch index before falling back to file-type detection.
func EffectiveLaunchKind(e *runtime.Engine, pkgName, installDir, absPath string, m *manifest.Manifest, source LaunchSource) LaunchKind {
	absPath = filepath.Clean(absPath)
	rel, err := filepath.Rel(installDir, absPath)
	if err != nil || strings.HasPrefix(rel, "..") {
		return LaunchKindSkip
	}
	if kind, ok := launchIndexKind(e, pkgName, filepath.ToSlash(rel)); ok {
		return kind
	}
	return autoLaunchKind(e, installDir, absPath, m, source)
}

// AutoLaunchKind picks a default open mode from file type only. Hidden (skip) is user-controlled.
func AutoLaunchKind(e *runtime.Engine, installDir, absPath string, m *manifest.Manifest, source LaunchSource) LaunchKind {
	return autoLaunchKind(e, installDir, absPath, m, source)
}

func autoLaunchKind(e *runtime.Engine, installDir, absPath string, m *manifest.Manifest, source LaunchSource) LaunchKind {
	_ = installDir
	_ = m
	_ = source
	lower := strings.ToLower(filepath.Clean(absPath))
	switch {
	case strings.HasSuffix(lower, ".bat"), strings.HasSuffix(lower, ".cmd"):
		return LaunchKindConsole
	case strings.HasSuffix(lower, ".jar"):
		return LaunchKindConsole
	case strings.HasSuffix(lower, ".exe"):
		return peLaunchKind(absPath)
	default:
		return LaunchKindSkip
	}
}

func shortcutTargetPath(installDir, target string) string {
	return filepath.Clean(filepath.Join(installDir, filepath.FromSlash(target)))
}

func shortcutLabel(entry manifest.ShortcutEntry, absPath string) string {
	if entry.Label != "" {
		return entry.Label
	}
	return launcherDisplayName(absPath)
}

func launchSourceForPath(installDir, absPath string, m *manifest.Manifest) LaunchSource {
	if m == nil {
		return LaunchSourceScan
	}
	clean := filepath.Clean(absPath)
	for _, sc := range m.ShortcutEntries() {
		if shortcutTargetPath(installDir, sc.Target) == clean {
			return LaunchSourceShortcut
		}
	}
	for _, pattern := range m.Binaries() {
		exe, _, _ := install.ParseBinPatternParts(pattern)
		if exe == "" {
			continue
		}
		for _, candidate := range install.ResolveBinCandidates(installDir, exe, "") {
			if filepath.Clean(candidate) == clean {
				return LaunchSourceBin
			}
		}
	}
	return LaunchSourceScan
}
