package message

import (
	"fmt"
	"strings"
)

// FormatEN returns an English fallback string for CLI and logs.
func FormatEN(key string, args map[string]any) string {
	str := func(k string) string {
		if args == nil {
			return ""
		}
		v, _ := args[k].(string)
		return v
	}
	intArg := func(k string) int {
		if args == nil {
			return 0
		}
		switch v := args[k].(type) {
		case int:
			return v
		case int64:
			return int(v)
		case float64:
			return int(v)
		default:
			return 0
		}
	}

	switch key {
	case ProgressInstallStarting:
		return "Starting install..."
	case ProgressInstallComplete:
		return "Install complete"
	case ProgressInstallCancelled:
		return "Install cancelled"
	case ProgressUninstallStarting:
		return "Uninstalling..."
	case ProgressUninstallComplete:
		return "Uninstall complete"
	case ProgressResolvingManifest:
		return "Resolving package manifest..."
	case ProgressPreparingDownload:
		if f := str("file"); f != "" {
			return fmt.Sprintf("Preparing to download %s...", f)
		}
		return "Preparing download..."
	case ProgressDownloading:
		if f := str("file"); f != "" {
			return fmt.Sprintf("Downloading %s...", f)
		}
		return "Downloading..."
	case ProgressDownloadCached:
		return "Using cached download"
	case ProgressDownloadComplete:
		if s := str("size"); s != "" {
			return fmt.Sprintf("Downloaded %s", s)
		}
		return "Download complete"
	case ProgressLinkingFiles:
		return "Linking files..."
	case ProgressCreatingShims:
		return "Creating shims..."
	case ProgressUpdatingCache:
		return "Updating cache index..."
	case ProgressExtracting:
		return "Extracting and installing files..."
	case ProgressExtractDecompress:
		if intArg("percent") > 0 {
			return fmt.Sprintf("Decompressing archive (%d%%)", intArg("percent"))
		}
		return "Decompressing archive..."
	case ProgressExtractProcessing:
		return fmt.Sprintf("Processing files (%d/%d)", intArg("current"), intArg("total"))
	case ProgressExtractIndexing:
		if intArg("total") > 0 {
			return fmt.Sprintf("Indexing installed files (%d/%d)", intArg("current"), intArg("total"))
		}
		return fmt.Sprintf("Indexing installed files (%d)", intArg("current"))
	case ProgressPackageInstallComplete:
		if p := str("package"); p != "" {
			return fmt.Sprintf("%s installed", p)
		}
		return "Package installed"
	case GCPrepareStore:
		return "Preparing cache cleanup..."
	case GCClearingPartials:
		return "Clearing incomplete downloads..."
	case GCClearingSidecars:
		return "Clearing stale download indexes..."
	case GCReadingIndexRefs:
		return "Reading file references from cache index..."
	case GCIndexRefCount:
		return fmt.Sprintf("Cache index has %d file references", intArg("count"))
	case GCScanAppsStart:
		return "Scanning installed apps..."
	case GCScanAppsEmpty:
		if p := str("path"); p != "" {
			return fmt.Sprintf("Installed apps directory %s is empty; skipping scan", p)
		}
		return "No installed apps to scan"
	case GCScanAppsSkipped:
		return "Installed apps directory not found; skipping app scan"
	case GCAppsPendingScan:
		return fmt.Sprintf("%d installed packages to scan", intArg("count"))
	case GCBuildingStoreIndex:
		return fmt.Sprintf("Indexing store hardlinks… %d objects (%d/%d)", intArg("indexed"), intArg("current"), intArg("total"))
	case GCAppsEnumeratingDirs:
		return fmt.Sprintf("Planning scan for %d installed packages…", intArg("packages"))
	case GCAppsScanPlan:
		return fmt.Sprintf("Will scan %d files in %d directories across %d packages", intArg("fileTotal"), intArg("dirTotal"), intArg("packages"))
	case GCScanningApp:
		return fmt.Sprintf("Scanning %s (%d/%d)", str("package"), intArg("current"), intArg("total"))
	case GCScanningAppsMerged:
		return fmt.Sprintf("Scanning installed apps: %d/%d packages, %d/%d directories, %d/%d files", intArg("current"), intArg("total"), intArg("dirsDone"), intArg("dirTotal"), intArg("files"), intArg("fileTotal"))
	case GCAppsScanComplete:
		return fmt.Sprintf("App scan complete; %d file references", intArg("count"))
	case GCRefsCollected:
		return fmt.Sprintf("Collected %d valid file references", intArg("count"))
	case GCScanStoreStart:
		return "Scanning store objects..."
	case GCScanningStore:
		if intArg("total") > 0 {
			return fmt.Sprintf("Scanning store objects… checked %d (%d/%d)", intArg("scanned"), intArg("current"), intArg("total"))
		}
		return fmt.Sprintf("Scanning store objects… checked %d", intArg("scanned"))
	case GCScanStoreComplete:
		return fmt.Sprintf("Store scan complete: checked %d objects, found %d orphan blobs", intArg("scanned"), intArg("orphans"))
	case GCNoOrphans:
		if p := str("path"); p != "" {
			return fmt.Sprintf("Nothing to clean: no orphan blobs in %s", p)
		}
		return "Nothing to clean"
	case GCOrphansFound:
		return fmt.Sprintf("Found %d orphan blobs; deleting...", intArg("count"))
	case GCDeletingOrphan:
		return fmt.Sprintf("Deleting orphan %s (%d/%d, %s)", str("label"), intArg("current"), intArg("total"), str("size"))
	case GCDeletingOrphansBatch:
		return fmt.Sprintf("Deleting orphan blobs (%d/%d)", intArg("current"), intArg("total"))
	case GCCompleteFreed:
		return fmt.Sprintf("Removed %d junk items, freed %s", intArg("removed"), str("freed"))
	case GCCompleteNothing:
		return "No space to reclaim"
	case PurgePrepare:
		return fmt.Sprintf("Preparing to remove %s from cache index...", str("name"))
	case PurgeRemovingIndex:
		return fmt.Sprintf("Removing %s from cache index (%d files)...", str("name"), intArg("files"))
	case PurgeIndexRemoved:
		return fmt.Sprintf("Removed %s from cache index; checking file references...", str("name"))
	case PurgeScanningFiles:
		return fmt.Sprintf("Scanning installed files for %s (%d files checked)...", str("package"), intArg("scanned"))
	case PurgeScanningInstall:
		return fmt.Sprintf("Checking whether %s still uses these cache files...", str("name"))
	case PurgeScanningInstalls:
		return fmt.Sprintf("Checking whether %s or %d related packages still use these cache files...", str("name"), intArg("count"))
	case PurgeScanningOtherInstalls:
		return fmt.Sprintf("Checking other installed packages for remaining cache references (%d packages)...", intArg("count"))
	case PurgeNoInstallScan:
		return fmt.Sprintf("%s is not installed; skipping install directory scan", str("name"))
	case PurgeCheckingRefs:
		return "Checking remaining cache references..."
	case PurgeDeletingFile:
		if intArg("total") > 0 && intArg("current") == 0 {
			return fmt.Sprintf("Preparing to delete unreferenced cache files (%d total)...", intArg("total"))
		}
		return fmt.Sprintf("Deleting unreferenced cache files (%d/%d)...", intArg("current"), intArg("total"))
	case PurgeCompleteFreed:
		return fmt.Sprintf("Removed %d cache blobs, freed %s", intArg("removed"), str("freed"))
	case PurgeCompleteNothing:
		return "No unreferenced cache files to delete"
	case BucketCloning:
		return fmt.Sprintf("Cloning %s...", str("name"))
	case BucketUpdating:
		return fmt.Sprintf("Updating %s (%d/%d)...", str("name"), intArg("current"), intArg("total"))
	case BucketUpdateComplete:
		return "Bucket update complete"
	case BucketPrepareAdd:
		return "Preparing to add bucket..."
	case BucketIndexRefresh:
		return "Refreshing package index..."
	case BucketRemoving:
		return "Removing bucket..."
	case BucketRemoveComplete:
		return "Bucket removed"
	case BucketNoUpdates:
		return "No buckets need updating"
	case GitPulling:
		return "Pulling updates..."
	case BootstrapInnounpDetecting:
		return "Checking for innounp…"
	case BootstrapInnounpDiscovering:
		return "Scanning catalog for Inno Setup packages…"
	case BootstrapInnounpDownloading:
		return "Downloading innounp…"
	case BootstrapInnounpExtracting:
		return "Extracting innounp…"
	case BootstrapInnounpComplete:
		return "innounp is ready"
	case BootstrapGitDetecting:
		return "Checking for Git…"
	case BootstrapGitDownloading:
		return "Downloading MinGit…"
	case BootstrapGitExtracting:
		return "Extracting MinGit…"
	case BootstrapGitComplete:
		return "Git is ready"
	case BootstrapSevenZipDetecting:
		return "Checking for 7-Zip…"
	case BootstrapSevenZipDownloading:
		return "Downloading 7-Zip…"
	case BootstrapSevenZipExtracting:
		return "Extracting 7-Zip…"
	case BootstrapSevenZipComplete:
		return "7-Zip is ready"
	case BootstrapWixDetecting:
		return "Checking for WiX toolset…"
	case BootstrapWixDiscovering:
		return "Scanning catalog for WiX Burn packages…"
	case BootstrapWixDownloading:
		return "Downloading WiX toolset…"
	case BootstrapWixExtracting:
		return "Extracting WiX toolset…"
	case BootstrapWixComplete:
		return "WiX toolset is ready"
	case DoctorGlueRootUnknown:
		return "Cannot determine ~/.glue path"
	case DoctorGlueRootNotWritable:
		return "Glue data directory is not writable"
	case DoctorGitInPath:
		return "Git is available on PATH"
	case DoctorGitMissing:
		return "Git was not found"
	case DoctorGitWillBootstrap:
		return "Git not installed yet; downloads automatically on first bucket add"
	case DoctorSevenZipMissing:
		return "7-Zip was not found"
	case DoctorSevenZipWillBootstrap:
		return "7-Zip not installed yet; downloads automatically on first archive install"
	case DoctorDarkMissing:
		return "WiX dark was not found"
	case DoctorDarkWillBootstrap:
		return "WiX not installed yet; downloads automatically when a Burn package needs it"
	case DoctorInnounpMissing:
		return "innounp was not found"
	case DoctorInnounpWillBootstrap:
		return "innounp not installed yet; downloads automatically when an Inno Setup package needs it"
	case DoctorShimInPath:
		return "Shim directory is on PATH"
	case DoctorShimNotInPath:
		return "Shim directory is not on PATH"
	case DoctorGitHubDirectOK:
		return "Direct connection to GitHub is available"
	case DoctorGitHubMirrorOK:
		return "GitHub mirror is available"
	case DoctorGitHubGitOK:
		return "Git access to GitHub repositories works (bucket updates)"
	case DoctorGitHubDirectFailed:
		return "HTTPS access to github.com failed (package downloads may need a mirror)"
	case DoctorGitHubAllFailed:
		return "Git, HTTPS, and configured mirrors are all unavailable"
	case DoctorHintGlueRootAccess:
		return "Check directory permissions or run as administrator"
	case DoctorHintGitInstall:
		return "Bucket management requires Git; it is installed automatically on first bucket add, or install Git manually"
	case DoctorHintSevenZip:
		return "Most archive installs need 7-Zip; it is installed automatically on first extract, or run: glue install 7zip"
	case DoctorHintDark:
		return "WiX Burn packages need dark.exe; it is installed automatically on first use, or run: glue install dark"
	case DoctorHintInnounp:
		return "Inno Setup packages need innounp; it is installed automatically on first use, or run: glue install innounp"
	case DoctorHintShimPath:
		return "Run glue path to see how to add the shim directory to PATH"
	case DoctorHintGitHubProxy:
		return "Configure a GitHub mirror for package downloads; bucket updates use Git to reach GitHub"
	case DoctorHintGitHubMirror:
		return "Verify mirror URLs or try a different mirror"
	case DoctorStartupGitNote:
		return "git not available, will be bootstrapped when needed"
	case DoctorStartupSevenZipNote:
		return "7z not available, will be bootstrapped when needed"
	case ErrInvalidLaunchPath:
		return "Invalid executable path"
	case ErrLaunchUnsupportedType:
		return "Only exe, bat, cmd, and jar files are supported"
	case ErrLaunchFileMissing:
		return "File not found"
	case ErrLaunchNotOpenable:
		return "This program cannot be opened directly"
	case ErrLaunchInvalidRelPath:
		return "Invalid path"
	case ErrLaunchInvalidKind:
		if k := str("kind"); k != "" {
			return fmt.Sprintf("Invalid launch mode: %s", k)
		}
		return "Invalid launch mode"
	case ErrUninstallProcessesOpen:
		return fmt.Sprintf("Cannot uninstall %s@%s: processes still running", str("package"), str("version"))
	default:
		if strings.HasPrefix(key, "progress.") || strings.HasPrefix(key, "error.") || strings.HasPrefix(key, "doctor.") {
			return key
		}
		return key
	}
}
