package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gluestick-sh/core/bucket"
	"github.com/gluestick-sh/core/engine"
	"github.com/gluestick-sh/core/message"
	"github.com/gluestick-sh/core/shim"
)

// doctorFixStep is one remediation `glue doctor --fix` may run: idempotent,
// non-interactive, and either inside the data root, glue-scoped git config, or
// the user's own profile — always listed per item in the fix output. The
// default doctor stays read-only; only the explicit --fix applies anything.
// Agent CLIs are never installed (roadmap section 3.3).
type doctorFixStep struct {
	check  string
	action string
	title  string // human line of the fix list, e.g. "Configure UTF-8"
	// reportOnly marks steps that record a finding without changing the
	// machine (duplicate detection): they are listed as report-only and never
	// counted in the "safe fix(es)" footer.
	reportOnly bool
}

// doctorFixSteps is the ordered fix plan. Ordering matters: the data root and
// PATH come first, buckets exist before their git config is pinned, PowerShell
// 7 installs before profiles are written. Shared actions run once — path_setup
// repairs both shim_path (Repair PATH) and shell_git_bash (Configure Git Bash).
var doctorFixSteps = []doctorFixStep{
	{message.AgentCheckDataRoot, "mkdir_data_root", "Create data root", false},
	{message.AgentCheckShimPath, "path_setup", "Repair PATH", false},
	{message.AgentCheckShellGitBash, "path_setup", "Configure Git Bash", false},
	{message.AgentCheckBuckets, "bucket_add", "Add main bucket", false},
	{message.AgentCheckShellUTF8, "set_utf8", "Configure UTF-8", false},
	{message.AgentCheckShellPWSH, "install_pwsh", "Configure PowerShell 7", false},
	{message.AgentCheckShellExecPolicy, "set_exec_policy", "Configure PowerShell execution policy", false},
	{message.AgentCheckGitConfig, "git_config_fix", "Configure Git", false},
	{message.AgentCheckShellIntegration, "shell_profile", "Fix shell integration", false},
	{message.AgentCheckDuplicates, "report_duplicates", "Detect duplicate runtimes", true},
}

// doctorFixStepFor returns the static fix-plan entry for a check.
func doctorFixStepFor(id string) (doctorFixStep, bool) {
	for _, step := range doctorFixSteps {
		if step.check == id {
			return step, true
		}
	}
	return doctorFixStep{}, false
}

// doctorFixStepForCheck resolves the effective step for one failed check: the
// static plan entry, unless the check's reason changes the honest remediation.
// Buckets are the case that matters — they can be installed and still have
// nothing indexed, where "add main" is a no-op (fixAddMainBucket returns early
// when main exists) and would report a fix that never happened:
//
//   - index not ready → reindex the in-memory search index from the local
//     bucket dirs (offline, no network), which is what the stale state needs;
//   - no manifests at all → re-fetch the buckets (network), or skip under
//     --offline with a reason.
//
// One resolver feeds the fix plan, the JSON `fixes[]` and the human lines, so
// the three can never disagree about what ran.
func doctorFixStepForCheck(check engine.DoctorCheck) (doctorFixStep, bool) {
	step, ok := doctorFixStepFor(check.ID)
	if !ok {
		return doctorFixStep{}, false
	}
	if check.ID != message.AgentCheckBuckets {
		return step, true
	}
	switch check.DetailKey {
	case message.AgentBucketsIndexNotReady:
		return doctorFixStep{check: step.check, action: "reindex_buckets", title: "Rebuild bucket index"}, true
	case message.AgentBucketsNoManifests:
		return doctorFixStep{check: step.check, action: "bucket_pull", title: "Refresh bucket manifests"}, true
	}
	return step, true
}

// doctorFixTitle returns the human title of one fix line ("" when unknown).
func doctorFixTitle(check string) string {
	step, ok := doctorFixStepFor(check)
	if !ok {
		return check
	}
	return step.title
}

// doctorFixActionTitles names the reason-specific variants resolved by
// doctorFixStepForCheck (the static table holds the check's default action).
var doctorFixActionTitles = map[string]string{
	"reindex_buckets": "Rebuild bucket index",
	"bucket_pull":     "Refresh bucket manifests",
}

// doctorFixTitleForResult picks the title of a finished fix: the reason-specific
// variant when the applied action differs from the static plan entry.
func doctorFixTitleForResult(res engine.AgentFixResult) string {
	step, ok := doctorFixStepFor(res.Check)
	if !ok {
		return res.Check
	}
	if step.action != res.Action {
		if title, ok := doctorFixActionTitles[res.Action]; ok {
			return title
		}
	}
	return step.title
}

// fixSkipSet reads GLUE_DOCTOR_FIX_SKIP: a comma-separated list of check IDs
// whose fixes must not run (escape hatch to keep --fix away from PATH writes).
func fixSkipSet() map[string]bool {
	skip := map[string]bool{}
	for _, id := range strings.Split(os.Getenv("GLUE_DOCTOR_FIX_SKIP"), ",") {
		if id = strings.TrimSpace(id); id != "" {
			skip[id] = true
		}
	}
	return skip
}

// doctorFixableChecks lists failed checks --fix would actually change right
// now, preserving report order; drives the "Run glue doctor --fix" footer.
// Report-only steps (duplicate detection) are excluded: --fix only reports
// them, so counting them as safe fixes would overstate what will happen.
func doctorFixableChecks(report engine.AgentDoctorReport) []string {
	skip := fixSkipSet()
	ids := []string{}
	for _, check := range report.Checks {
		if check.Status != engine.AgentStatusFail {
			continue
		}
		step, ok := doctorFixStepForCheck(check)
		if !ok || step.reportOnly || skip[check.ID] {
			continue
		}
		ids = append(ids, check.ID)
	}
	return ids
}

// applyDoctorFixes runs the safe remediations justified by report, in fix-plan
// order; the caller re-runs the doctor afterwards so output always shows the
// post-fix state. A shared action executes once and records the same outcome
// for every check it repairs.
func applyDoctorFixes(ctx context.Context, eng *engine.Engine, report engine.AgentDoctorReport, offline bool) []engine.AgentFixResult {
	if ctx == nil {
		ctx = context.Background()
	}
	skip := fixSkipSet()
	checkByID := map[string]engine.DoctorCheck{}
	for _, check := range report.Checks {
		checkByID[check.ID] = check
	}
	results := []engine.AgentFixResult{}
	executed := map[string]engine.AgentFixResult{} // action → first outcome
	for _, step := range doctorFixSteps {
		check, ok := checkByID[step.check]
		if !ok || check.Status != engine.AgentStatusFail || skip[step.check] {
			continue
		}
		if resolved, ok := doctorFixStepForCheck(check); ok {
			step = resolved
		}
		res := engine.AgentFixResult{Check: step.check, Action: step.action}
		if prev, shared := executed[step.action]; shared {
			res.Applied, res.Error, res.Detail = prev.Applied, prev.Error, prev.Detail
			results = append(results, res)
			continue
		}
		res.Applied, res.Detail, res.Error = runDoctorFix(ctx, step, report, check, eng, offline)
		executed[step.action] = res
		results = append(results, res)
	}
	return results
}

// runDoctorFix dispatches one fix step. Every action returns (applied, detail,
// error): detail is the human/JSON summary, error only set when applied=false.
func runDoctorFix(ctx context.Context, step doctorFixStep, report engine.AgentDoctorReport, check engine.DoctorCheck, eng *engine.Engine, offline bool) (bool, string, string) {
	switch step.action {
	case "path_setup":
		applied, err := fixShimPath(report.DataRoot)
		return applied, "", err
	case "mkdir_data_root":
		if err := os.MkdirAll(report.DataRoot, 0755); err != nil {
			return false, "", err.Error()
		}
		return true, report.DataRoot, ""
	case "bucket_add":
		if offline {
			return false, "", "offline: skipped (re-run without --offline)"
		}
		applied, err := fixAddMainBucket(report.DataRoot, eng)
		return applied, "", err
	case "reindex_buckets":
		// Installed-but-unindexed: rebuild the in-memory search index from the
		// local bucket dirs. Offline-safe — nothing is downloaded.
		return fixReindexBuckets(eng)
	case "bucket_pull":
		if offline {
			return false, "", "offline: skipped (run glue bucket update without --offline)"
		}
		return fixPullBucketManifests(eng)
	case "set_utf8":
		return fixUTF8Codepage()
	case "install_pwsh":
		if offline {
			return false, "", "offline: skipped (install " + doctorPwshRef + " without --offline)"
		}
		return fixInstallPowerShell(ctx, eng)
	case "set_exec_policy":
		return fixExecutionPolicy()
	case "git_config_fix":
		return fixGitConfig(ctx, eng)
	case "shell_profile":
		return fixShellProfiles(report.DataRoot)
	case "report_duplicates":
		// Report-only: the detection itself is the deliverable — glue never
		// removes a tool.
		return true, duplicatesFixDetail(check), ""
	}
	return false, "", "unknown fix action: " + step.action
}

// fixReindexBuckets rescans every installed bucket into the in-memory search
// index. It is the honest remedy for "buckets installed, index not ready": the
// index is built in the background, so a stale or failed build leaves the check
// failing while the bucket dirs are perfectly fine. Nothing is downloaded, so
// this runs under --offline too.
func fixReindexBuckets(eng *engine.Engine) (bool, string, string) {
	if eng == nil || eng.BucketRegistry == nil {
		return false, "", "engine is not available"
	}
	names := []string{}
	for _, b := range eng.BucketRegistry.List() {
		eng.LoadSearchIndexBucket(b.Name)
		names = append(names, b.Name)
	}
	if len(names) == 0 {
		return false, "", "no buckets to reindex"
	}
	sort.Strings(names)
	return true, "reindexed " + strings.Join(names, ", "), ""
}

// fixPullBucketManifests re-fetches the installed buckets so an empty bucket dir
// gets its manifests back. Network-bound and therefore skipped under --offline,
// exactly like the add-main step.
func fixPullBucketManifests(eng *engine.Engine) (bool, string, string) {
	if eng == nil || eng.BucketRegistry == nil {
		return false, "", "engine is not available"
	}
	reg := eng.BucketRegistry
	if err := reg.UpdateSilent(nil); err != nil {
		return false, "", err.Error()
	}
	eng.ReloadBuckets(true)
	names := []string{}
	for _, b := range reg.List() {
		names = append(names, b.Name)
	}
	sort.Strings(names)
	return true, "pulled " + strings.Join(names, ", "), ""
}

// fixShimPath puts the data root's shims first on the user PATH — the same
// three steps as `glue path setup`, without its output wrapper.
func fixShimPath(root string) (bool, string) {
	mgr, err := shim.NewManager(root)
	if err != nil {
		return false, err.Error()
	}
	if _, err := ensureUserPathFront(mgr.BinDir()); err != nil {
		return false, err.Error()
	}
	prependDirToProcessPath(mgr.BinDir())
	disableWindowsPythonAliases()
	return true, ""
}

// fixAddMainBucket adds the known "main" bucket (git clone) and refreshes the
// engine index — the safe subset of `glue bucket add main`. An existing bucket
// is an idempotent no-op; git/network problems surface as the fix error.
func fixAddMainBucket(root string, eng *engine.Engine) (bool, string) {
	repoURL, ok := bucket.GetKnownBucketURL("main")
	if !ok {
		return false, "main is not a known bucket"
	}
	// Prefer the engine's own registry: the in-process re-run reads
	// e.BucketRegistry, so a second registry instance would leave it stale.
	var reg *bucket.Registry
	if eng != nil && eng.BucketRegistry != nil {
		reg = eng.BucketRegistry
	} else {
		var err error
		if reg, err = bucket.NewRegistry(root); err != nil {
			return false, err.Error()
		}
		reg.ReloadFromDisk()
	}
	if _, err := reg.Get("main"); err == nil {
		return true, ""
	}
	if _, err := reg.Add("main", repoURL); err != nil {
		return false, err.Error()
	}
	if eng != nil {
		syncEngineBucketsAfterAdd(eng, "main")
	}
	return true, ""
}

// doctorPwshRef is the Glue-managed package installed when the shell_pwsh
// readiness check fails: PowerShell 7 from the main bucket, installed through
// Glue's own bucket → download → extract → shim pipeline. Glue never shells out
// to winget for it, so resolution, hashes, shims and the install journal stay
// under the engine's control.
const doctorPwshRef = "main/pwsh"

// doctorInstallReporter keeps doctor --fix installs silent. `doctor --json`
// must emit exactly one JSON document on stdout, and the per-item fix list
// already reports the outcome; human runs get the same fix line either way.
func doctorInstallReporter() engine.ProgressReporter {
	return engine.NewSilentReporter()
}

// doctorInstallPackage is the injectable install seam: tests replace it, so unit
// tests never download or mutate the machine. It runs only from the explicit
// `glue doctor --fix` path; agent CLIs are never installed by glue.
var doctorInstallPackage = func(ctx context.Context, eng *engine.Engine, ref string, reporter engine.ProgressReporter) (*engine.Result, error) {
	return eng.Install(ctx, &engine.InstallRequest{
		Request: engine.Request{Name: ref, Options: map[string]string{}},
	}, reporter)
}

// fixInstallPowerShell installs main/pwsh when the shell_pwsh check is failing.
// Everything the engine reports (version on success, structured error on
// failure) is folded into the fix item's detail/error.
func fixInstallPowerShell(ctx context.Context, eng *engine.Engine) (bool, string, string) {
	if eng == nil {
		return false, "", "no engine available to install " + doctorPwshRef
	}
	result, err := doctorInstallPackage(ctx, eng, doctorPwshRef, doctorInstallReporter())
	if failErr := installFailureError(err, result); failErr != nil {
		return false, "", "install " + doctorPwshRef + ": " + failErr.Error()
	}
	detail := "installed " + doctorPwshRef
	if result != nil && result.Version != "" {
		detail += " " + result.Version
	}
	return true, detail, ""
}

// doctorGitRun runs git with -C in dir, bounded so a hung helper cannot stall
// --fix.
func doctorGitRun(ctx context.Context, dir string, args ...string) (string, string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

// fixGitConfig repairs what agentCheckGitConfig found — strictly glue-scoped:
// pin each bucket's local core.autocrlf to its current effective value (so
// later global flips cannot dirty a checkout) and add safe.directory entries
// for glue's own bucket paths, but only where git reports dubious ownership.
// The user's global git config is never rewritten otherwise.
func fixGitConfig(ctx context.Context, eng *engine.Engine) (bool, string, string) {
	if eng == nil || eng.BucketRegistry == nil {
		return false, "", "no bucket registry"
	}
	buckets := eng.BucketRegistry.List()
	if len(buckets) == 0 {
		return false, "", "no buckets to fix"
	}
	if _, err := exec.LookPath("git"); err != nil {
		return false, "", "git not on PATH"
	}
	trusted, pinned := 0, 0
	summary := func() string { return fmt.Sprintf("trusted %d, pinned %d", trusted, pinned) }
	for _, b := range buckets {
		if b.Root == "" {
			continue
		}
		kind, detail := engine.ProbeGitBucket(ctx, b.Root)
		switch kind {
		case "":
			// healthy: already trusted and pinned
		case "ownership":
			_, stderr, err := doctorGitRun(ctx, b.Root, "config", "--global", "--add", "safe.directory", b.Root)
			if err != nil {
				return false, summary(), fmt.Sprintf("trust %s: %s", b.Name, doctorGitErr(err, stderr))
			}
			trusted++
			// Re-probe now that trust exists, so the pin lands in the same run.
			if kind, _ = engine.ProbeGitBucket(ctx, b.Root); kind == "unpinned" {
				if err := doctorPinBucket(ctx, b.Root); err != nil {
					return false, summary(), err.Error()
				}
				pinned++
			}
		case "unpinned":
			if err := doctorPinBucket(ctx, b.Root); err != nil {
				return false, summary(), err.Error()
			}
			pinned++
		case "error":
			return false, summary(), fmt.Sprintf("%s: %s", b.Name, detail)
		}
	}
	return true, summary(), ""
}

// doctorPinBucket pins the bucket's local core.autocrlf to the currently
// effective value (unset → "false", git's own default), preserving today's
// behavior against future global config flips.
func doctorPinBucket(ctx context.Context, dir string) error {
	out, _, err := doctorGitRun(ctx, dir, "config", "--get", "core.autocrlf")
	value := strings.TrimSpace(out)
	if err != nil || value == "" {
		value = "false"
	}
	if _, stderr, err := doctorGitRun(ctx, dir, "config", "--local", "core.autocrlf", value); err != nil {
		return fmt.Errorf("pin autocrlf: %s", doctorGitErr(err, stderr))
	}
	return nil
}

// doctorGitErr prefers git's first stderr line over the raw exit error.
func doctorGitErr(err error, stderr string) string {
	if line := strings.TrimSpace(strings.SplitN(strings.TrimSpace(stderr), "\n", 2)[0]); line != "" {
		return line
	}
	return err.Error()
}

// fixShellProfiles writes the glue block (PATH guard, UTF-8 console, UTF-8
// stdin) into every target PowerShell profile, backing up each file it touches
// first; profiles that already carry the block are left alone.
func fixShellProfiles(root string) (bool, string, string) {
	mgr, err := shim.NewManager(root)
	if err != nil {
		return false, "", err.Error()
	}
	targets := engine.GluePowerShellProfiles()
	if len(targets) == 0 {
		return false, "", "cannot resolve PowerShell profile paths"
	}
	block := engine.GlueProfileBlock(mgr.BinDir())
	written, ready := 0, 0
	progress := func() string { return fmt.Sprintf("%d of %d profile(s)", written+ready, len(targets)) }
	for _, target := range targets {
		if engine.GlueProfileHasBlock(target) {
			ready++
			continue
		}
		orig, _ := os.ReadFile(target) // missing file → nil: create new
		var next []byte
		if len(orig) > 0 {
			next = append(next, orig...)
			if !bytes.HasSuffix(orig, []byte("\n")) {
				next = append(next, '\r', '\n')
			}
			if err := os.WriteFile(target+".glue.bak", orig, 0644); err != nil {
				return false, progress(), err.Error()
			}
		}
		next = append(next, block...)
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return false, progress(), err.Error()
		}
		if err := os.WriteFile(target, next, 0644); err != nil {
			return false, progress(), err.Error()
		}
		written++
	}
	return true, fmt.Sprintf("%d written, %d already configured", written, ready), ""
}

// duplicatesFixDetail summarizes the report-only duplicate step for JSON.
func duplicatesFixDetail(check engine.DoctorCheck) string {
	dups, _ := check.Data["duplicates"].([]engine.DuplicateTool)
	if len(dups) == 0 {
		return "no duplicates"
	}
	names := make([]string, 0, len(dups))
	for _, d := range dups {
		names = append(names, fmt.Sprintf("%s (%d)", d.Name, len(d.Paths)))
	}
	return "reported: " + strings.Join(names, ", ")
}

// writeDoctorFixLines prints the fix list between the check sections and Result
// (human view only; --json carries the same data in report.fixes). Every change
// is listed individually — Glue never changes the machine silently: first
// `glue doctor` shows what is wrong, then `glue doctor --fix` lists each change
// it applied. Report-only findings use the skip mark and an explicit label.
func writeDoctorFixLines(results []engine.AgentFixResult) {
	if len(results) == 0 {
		fmt.Println("No safe fixes to apply.")
		fmt.Println()
		return
	}
	for _, res := range results {
		title := doctorFixTitleForResult(res)
		if step, ok := doctorFixStepFor(res.Check); ok && step.reportOnly {
			fmt.Printf("  %s %s — report only\n", markSkip, title)
			if res.Detail != "" {
				fmt.Printf("       %s\n", res.Detail)
			}
			continue
		}
		if res.Applied {
			fmt.Printf("  %s %s\n", markSuccess, title)
			continue
		}
		msg := res.Error
		if msg == "" {
			msg = "failed"
		}
		fmt.Printf("  %s %s — %s\n", markFail, title, msg)
	}
	fmt.Println()
}
