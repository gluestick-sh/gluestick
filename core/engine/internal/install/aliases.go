package install

import etypes "github.com/gluestick-sh/core/engine/types"

// Install pipeline phase and status identifiers, re-exported from engine/types so code in
// the install package can reference them without importing etypes directly.
const (
	PhaseResolve   = etypes.PhaseResolve
	PhaseDownload  = etypes.PhaseDownload
	PhaseExtract   = etypes.PhaseExtract
	PhaseLink      = etypes.PhaseLink
	PhaseShim      = etypes.PhaseShim
	PhaseIndex     = etypes.PhaseIndex
	PhaseBootstrap = etypes.PhaseBootstrap
	PhaseComplete  = etypes.PhaseComplete
	PhaseError     = etypes.PhaseError

	StatusRunning = etypes.StatusRunning
	StatusSuccess = etypes.StatusSuccess
	StatusFailed  = etypes.StatusFailed
	StatusSkipped = etypes.StatusSkipped
	StatusWaiting = etypes.StatusWaiting
	StatusInfo    = etypes.StatusInfo
)
