package snapshot

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Diff compares current local core state against a target snapshot core state.
//
// install-missing (default):
//   - add missing buckets
//   - install package versions not present locally (same name, different version = missing version)
//   - switch active (current) version when the snapshot marks a different one
//   - apply config differences
//
// reconcile:
//   - also remove local packages/buckets not in target (caller must confirm)
func Diff(current CoreState, target CoreState, opts ApplyOptions) (*Plan, error) {
	mode := NormalizeMode(opts.Mode)
	plan := &Plan{}

	localBuckets := map[string]Bucket{}
	for _, b := range current.Buckets {
		name := strings.TrimSpace(b.Name)
		if name == "" {
			continue
		}
		localBuckets[strings.ToLower(name)] = b
	}
	for _, b := range target.Buckets {
		name := strings.TrimSpace(b.Name)
		if name == "" {
			continue
		}
		key := strings.ToLower(name)
		if _, ok := localBuckets[key]; ok {
			continue
		}
		plan.BucketsToAdd = append(plan.BucketsToAdd, Bucket{Name: name, URL: strings.TrimSpace(b.URL)})
	}

	localByKey := map[string]Package{}
	localByName := map[string]struct{}{}
	localCurrent := map[string]string{} // name -> active version
	for _, p := range current.Packages {
		name := strings.TrimSpace(p.Name)
		if name == "" {
			continue
		}
		nameKey := strings.ToLower(name)
		localByName[nameKey] = struct{}{}
		localByKey[packageKey(name, p.Version)] = p
		if p.Current {
			localCurrent[nameKey] = strings.TrimSpace(p.Version)
		}
	}
	for _, p := range target.Packages {
		name := strings.TrimSpace(p.Name)
		if name == "" {
			continue
		}
		version := strings.TrimSpace(p.Version)
		nameKey := strings.ToLower(name)
		if version == "" {
			// Legacy name-only intent: any local version satisfies.
			if _, ok := localByName[nameKey]; ok {
				continue
			}
		} else if _, ok := localByKey[packageKey(name, version)]; ok {
			continue
		}
		plan.PackagesToInstall = append(plan.PackagesToInstall, Package{
			Name:          name,
			Bucket:        strings.TrimSpace(p.Bucket),
			Version:       version,
			Current:       p.Current,
			VersionLocked: p.VersionLocked,
		})
	}

	// Restore active version when the snapshot records Current.
	for _, p := range target.Packages {
		if !p.Current {
			continue
		}
		name := strings.TrimSpace(p.Name)
		version := strings.TrimSpace(p.Version)
		if name == "" || version == "" {
			continue
		}
		nameKey := strings.ToLower(name)
		if localCurrent[nameKey] == version {
			continue
		}
		plan.PackagesToActivate = append(plan.PackagesToActivate, Package{
			Name:          name,
			Bucket:        strings.TrimSpace(p.Bucket),
			Version:       version,
			Current:       true,
			VersionLocked: p.VersionLocked,
		})
	}

	plan.ConfigChanges = diffConfig(current.Config, target.Config)

	if mode == ApplyModeReconcile {
		targetBuckets := map[string]struct{}{}
		for _, b := range target.Buckets {
			targetBuckets[strings.ToLower(strings.TrimSpace(b.Name))] = struct{}{}
		}
		for key, b := range localBuckets {
			if _, ok := targetBuckets[key]; !ok {
				plan.BucketsToRemove = append(plan.BucketsToRemove, b.Name)
			}
		}
		targetPkgs := map[string]struct{}{}
		for _, p := range target.Packages {
			targetPkgs[strings.ToLower(strings.TrimSpace(p.Name))] = struct{}{}
		}
		seenRemove := map[string]struct{}{}
		for nameKey := range localByName {
			if _, ok := targetPkgs[nameKey]; ok {
				continue
			}
			// Prefer a concrete local display name from any version entry.
			display := nameKey
			for _, p := range current.Packages {
				if strings.EqualFold(strings.TrimSpace(p.Name), nameKey) {
					display = strings.TrimSpace(p.Name)
					break
				}
			}
			if _, ok := seenRemove[strings.ToLower(display)]; ok {
				continue
			}
			seenRemove[strings.ToLower(display)] = struct{}{}
			plan.PackagesToRemove = append(plan.PackagesToRemove, display)
		}
		sort.Strings(plan.BucketsToRemove)
		sort.Strings(plan.PackagesToRemove)
	}

	sort.Slice(plan.BucketsToAdd, func(i, j int) bool {
		return strings.ToLower(plan.BucketsToAdd[i].Name) < strings.ToLower(plan.BucketsToAdd[j].Name)
	})
	sort.Slice(plan.PackagesToInstall, func(i, j int) bool {
		a, b := plan.PackagesToInstall[i], plan.PackagesToInstall[j]
		an, bn := strings.ToLower(a.Name), strings.ToLower(b.Name)
		if an != bn {
			return an < bn
		}
		return a.Version < b.Version
	})
	sort.Slice(plan.PackagesToActivate, func(i, j int) bool {
		return strings.ToLower(plan.PackagesToActivate[i].Name) < strings.ToLower(plan.PackagesToActivate[j].Name)
	})
	return plan, nil
}

func packageKey(name, version string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	version = strings.TrimSpace(version)
	if version == "" {
		return name
	}
	return name + "@" + version
}

func diffConfig(current, target Config) []ConfigChange {
	var out []ConfigChange
	if cur, want := strings.TrimSpace(current.GitHubProxy), strings.TrimSpace(target.GitHubProxy); cur != want {
		// Only propose a change when target explicitly sets a value, or current has one to clear.
		if want != "" || cur != "" {
			out = append(out, ConfigChange{Key: "githubProxy", From: cur, To: want})
		}
	}
	curWorkers, curOK := workersString(current.DownloadWorkers)
	wantWorkers, wantOK := workersString(target.DownloadWorkers)
	if wantOK && curWorkers != wantWorkers {
		from := ""
		if curOK {
			from = curWorkers
		}
		out = append(out, ConfigChange{Key: "downloadWorkers", From: from, To: wantWorkers})
	}
	if cur, want := strings.TrimSpace(current.BucketSyncMode), strings.TrimSpace(target.BucketSyncMode); want != "" && cur != want {
		out = append(out, ConfigChange{Key: "bucketSyncMode", From: cur, To: want})
	}
	return out
}

func workersString(v *int) (string, bool) {
	if v == nil {
		return "", false
	}
	return strconv.Itoa(*v), true
}

// InstallRef builds an engine InstallRequest name: [bucket/]name[@version].
func InstallRef(pkg Package) string {
	name := strings.TrimSpace(pkg.Name)
	bucket := strings.TrimSpace(pkg.Bucket)
	version := strings.TrimSpace(pkg.Version)
	ref := name
	if bucket != "" && !strings.EqualFold(bucket, "main") {
		ref = bucket + "/" + name
	}
	if version != "" {
		ref = ref + "@" + version
	}
	return ref
}

// ResolveBucketURL returns URL from snapshot entry or known buckets.
func ResolveBucketURL(name, url string, known func(string) (string, bool)) (string, error) {
	name = strings.TrimSpace(name)
	url = strings.TrimSpace(url)
	if url != "" {
		return url, nil
	}
	if known != nil {
		if u, ok := known(name); ok && strings.TrimSpace(u) != "" {
			return strings.TrimSpace(u), nil
		}
	}
	return "", fmt.Errorf("bucket %q has no URL and is not a known bucket", name)
}
