// Package bucket manages Scoop-style manifest buckets (git repos under ~/.glue/buckets).
package bucket

// Bucket represents a Scoop bucket (git repository of manifests).
type Bucket struct {
	Name    string
	RepoURL string
	Root    string // Local directory path
	Updated bool   // Whether updates are available
}

// KnownBuckets returns a map of well-known Scoop buckets.
func KnownBuckets() map[string]string {
	return map[string]string{
		"main":         "https://github.com/ScoopInstaller/Main",
		"extras":       "https://github.com/ScoopInstaller/Extras",
		"versions":     "https://github.com/ScoopInstaller/Versions",
		"nightlies":    "https://github.com/ScoopInstaller/Nightlies",
		"nirsoft":      "https://github.com/kodybrown/scoop-nirsoft",
		"sysinternals": "https://github.com/asheroto/scoop-sysinternals",
		"java":         "https://github.com/ScoopInstaller/Java",
		"php":          "https://github.com/ScoopInstaller/PHP",
		"games":        "https://github.com/Calinou/scoop-games",
	}
}

// IsKnownBucket checks if a bucket name is a known bucket.
func IsKnownBucket(name string) bool {
	_, ok := KnownBuckets()[name]
	return ok
}

// GetKnownBucketURL returns the repository URL for a known bucket.
func GetKnownBucketURL(name string) (string, bool) {
	url, ok := KnownBuckets()[name]
	return url, ok
}

// KnownBucketDescriptions returns short descriptions for well-known buckets.
func KnownBucketDescriptions() map[string]string {
	return map[string]string{
		"main":         "Scoop official packages",
		"extras":       "Popular third-party apps not in main",
		"versions":     "Multiple versions / non-latest stable releases",
		"nightlies":    "Nightly builds and preview releases",
		"nirsoft":      "NirSoft utilities collection",
		"sysinternals": "Microsoft Sysinternals tools",
		"java":         "Java runtimes and development tools",
		"php":          "PHP and related tools",
		"games":        "Games",
	}
}

// GetKnownBucketDescription returns the description for a known bucket name.
func GetKnownBucketDescription(name string) string {
	desc, ok := KnownBucketDescriptions()[name]
	if !ok {
		return ""
	}
	return desc
}
