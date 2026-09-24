package types

import (
	"encoding/json"
)

type resultJSON struct {
	Name        string              `json:"name"`
	Version     string              `json:"version,omitempty"`
	Status      Status              `json:"status"`
	Message     string              `json:"message,omitempty"`
	DurationMs  int64               `json:"durationMs"`
	Files       []string            `json:"files,omitempty"`
	Size        int64               `json:"size,omitempty"`
	Error       string              `json:"error,omitempty"`
	Manifest    *ManifestInfo       `json:"manifest,omitempty"`
	Suggestions []PackageSuggestion `json:"suggestions,omitempty"`
}

// MarshalJSON encodes Result for machine-readable CLI output.
func (r Result) MarshalJSON() ([]byte, error) {
	out := resultJSON{
		Name:        r.Name,
		Version:     r.Version,
		Status:      r.Status,
		Message:     r.Message,
		DurationMs:  r.Duration.Milliseconds(),
		Files:       r.Files,
		Size:        r.Size,
		Manifest:    r.Manifest,
		Suggestions: r.Suggestions,
	}
	if r.Error != nil {
		out.Error = r.Error.Error()
	}
	return json.Marshal(out)
}

type packageJSON struct {
	Name              string        `json:"name"`
	Version           string        `json:"version"`
	Description       string        `json:"description,omitempty"`
	Homepage          string        `json:"homepage,omitempty"`
	Bucket            string        `json:"bucket,omitempty"`
	Deprecated        bool          `json:"deprecated,omitempty"`
	InstalledAt       string        `json:"installedAt,omitempty"`
	InstalledSize     int64         `json:"installedSize,omitempty"`
	DetectedVersion   string        `json:"detectedVersion,omitempty"`
	VersionDetectSrc  string        `json:"versionDetectSrc,omitempty"`
	ExternallyUpdated bool          `json:"externallyUpdated,omitempty"`
	Manifest          *ManifestInfo `json:"manifest,omitempty"`
}

// MarshalJSON encodes Package for machine-readable CLI output.
func (p Package) MarshalJSON() ([]byte, error) {
	return json.Marshal(packageJSON{
		Name:              p.Name,
		Version:           p.Version,
		Description:       p.Description,
		Homepage:          p.Homepage,
		Bucket:            p.Bucket,
		Deprecated:        p.Deprecated,
		InstalledAt:       p.InstalledAt,
		InstalledSize:     p.InstalledSize,
		DetectedVersion:   p.DetectedVersion,
		VersionDetectSrc:  p.VersionDetectSrc,
		ExternallyUpdated: p.ExternallyUpdated,
		Manifest:          p.Manifest,
	})
}
