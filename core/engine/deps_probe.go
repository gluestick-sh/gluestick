package engine

import (
	"fmt"
	"io"
	"runtime"

	"github.com/gluestick-sh/core/engine/internal/install"
	"github.com/gluestick-sh/core/message"
)

// ToolAvailability is the result of probing for a runtime tool without downloading.
type ToolAvailability = install.ToolAvailability

// ProbeGit reports whether git is on PATH or bootstrapped under glue root.
func ProbeGit(root string) ToolAvailability { return install.ProbeGit(root) }

// ProbeSevenZip reports whether 7-Zip exists under glue root or on PATH.
func ProbeSevenZip(root string) ToolAvailability { return install.ProbeSevenZip(root) }

// ProbeDark reports whether WiX dark/wix is available (bootstrap or glue install).
func ProbeDark(root string) ToolAvailability { return install.ProbeDark(root) }

// ProbeInnounp reports whether innounp is available under glue root.
func ProbeInnounp(root string) ToolAvailability { return install.ProbeInnounp(root) }

type toolCheckSpec struct {
	id                 string
	probe              func(string) ToolAvailability
	missingKey         string
	hintKey            string
	inPathKey          string // when set and probe is InSystemPath, use this detail key
	willBootstrapKey   string // doctor OK detail when missing but auto-bootstrap applies (Windows)
	bootstrapOnWindows bool
	startupNote        string // i18n key for CLI init Note line; empty = no startup note
}

var toolCheckSpecs = map[string]toolCheckSpec{
	message.DoctorCheckGit: {
		id:                 message.DoctorCheckGit,
		probe:              ProbeGit,
		missingKey:         message.DoctorGitMissing,
		hintKey:            message.DoctorHintGitInstall,
		inPathKey:          message.DoctorGitInPath,
		willBootstrapKey:   message.DoctorGitWillBootstrap,
		bootstrapOnWindows: true,
		startupNote:        message.DoctorStartupGitNote,
	},
	message.DoctorCheckSevenZip: {
		id:                 message.DoctorCheckSevenZip,
		probe:              ProbeSevenZip,
		missingKey:         message.DoctorSevenZipMissing,
		hintKey:            message.DoctorHintSevenZip,
		willBootstrapKey:   message.DoctorSevenZipWillBootstrap,
		bootstrapOnWindows: true,
		startupNote:        message.DoctorStartupSevenZipNote,
	},
	message.DoctorCheckDark: {
		id:                 message.DoctorCheckDark,
		probe:              ProbeDark,
		missingKey:         message.DoctorDarkMissing,
		hintKey:            message.DoctorHintDark,
		willBootstrapKey:   message.DoctorDarkWillBootstrap,
		bootstrapOnWindows: true,
	},
	message.DoctorCheckInnounp: {
		id:                 message.DoctorCheckInnounp,
		probe:              ProbeInnounp,
		missingKey:         message.DoctorInnounpMissing,
		hintKey:            message.DoctorHintInnounp,
		willBootstrapKey:   message.DoctorInnounpWillBootstrap,
		bootstrapOnWindows: true,
	},
}

// StartupToolCheckIDs returns tool check IDs shown as quiet CLI notes at startup.
func StartupToolCheckIDs() []string {
	return []string{message.DoctorCheckGit, message.DoctorCheckSevenZip}
}

// CheckTool runs a registered tool probe and returns a strict doctor-style result.
func CheckTool(id, root string) (DoctorCheck, bool) {
	return checkTool(id, root, false)
}

// CheckToolForDoctor treats Windows bootstrap-capable tools as OK when not yet installed.
func CheckToolForDoctor(id, root string) (DoctorCheck, bool) {
	return checkTool(id, root, true)
}

func checkTool(id, root string, allowBootstrapPending bool) (DoctorCheck, bool) {
	spec, ok := toolCheckSpecs[id]
	if !ok {
		return DoctorCheck{}, false
	}
	return doctorCheckFromProbe(spec, spec.probe(root), allowBootstrapPending), true
}

func doctorCheckFromProbe(spec toolCheckSpec, p ToolAvailability, allowBootstrapPending bool) DoctorCheck {
	c := DoctorCheck{ID: spec.id}
	if !p.OK {
		if allowBootstrapPending && spec.bootstrapOnWindows && runtime.GOOS == "windows" && spec.willBootstrapKey != "" {
			c.OK = true
			c.DetailKey = spec.willBootstrapKey
			return c
		}
		c.DetailKey = spec.missingKey
		c.HintKey = spec.hintKey
		c.Hint = doctorHint(spec.hintKey)
		return c
	}
	c.OK = true
	if spec.inPathKey != "" && p.InSystemPath {
		c.DetailKey = spec.inPathKey
		return c
	}
	c.DetailText = p.Path
	return c
}

// WriteStartupToolNotes prints one-line Notes for startup tools that are not yet available.
func WriteStartupToolNotes(w io.Writer, root string) {
	for _, id := range StartupToolCheckIDs() {
		spec := toolCheckSpecs[id]
		if spec.startupNote == "" || spec.probe(root).OK {
			continue
		}
		fmt.Fprintf(w, "Note: %s\n", message.FormatEN(spec.startupNote, nil))
	}
}
