// Doctor environment checks for glue doctor and UI settings.
//
// Tool probes (git, 7z, dark, innounp) are implemented in deps_probe.go via CheckTool.
// Doctor only observes availability; it does not download bootstrap helpers.
package engine

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gluestick-sh/core/config"
	"github.com/gluestick-sh/core/git"
	"github.com/gluestick-sh/core/message"
	"github.com/gluestick-sh/core/shim"
)

// DoctorCheck is one environment check result.
// DetailKey/HintKey are i18n keys; DetailText/Hint are resolved English for CLI.
type DoctorCheck struct {
	ID         string `json:"id"`
	OK         bool   `json:"ok"`
	DetailKey  string `json:"detailKey,omitempty"`
	DetailText string `json:"detail,omitempty"`
	HintKey    string `json:"hintKey,omitempty"`
	Hint       string `json:"hint,omitempty"`
}

// DoctorReport is an environment diagnosis report.
type DoctorReport struct {
	Checks []DoctorCheck `json:"checks"`
	OK     bool          `json:"ok"`
}

// DoctorCheckNames returns environment check IDs in run order.
func DoctorCheckNames() []string {
	return []string{
		message.DoctorCheckGlueRoot,
		message.DoctorCheckGit,
		message.DoctorCheckSevenZip,
		message.DoctorCheckDark,
		message.DoctorCheckInnounp,
		message.DoctorCheckShimDir,
		message.DoctorCheckGitHub,
	}
}

// RunDoctor checks the Glue runtime environment without bootstrap or installs.
func (e *Engine) RunDoctor(ctx context.Context) DoctorReport {
	return e.RunDoctorProgress(ctx, nil, nil)
}

// RunDoctorProgress runs all checks concurrently and invokes callbacks as each finishes.
// onRunning is called once per check ID before work starts; onCheck after each result.
func (e *Engine) RunDoctorProgress(ctx context.Context, onRunning func(name string), onCheck func(DoctorCheck)) DoctorReport {
	if ctx == nil {
		ctx = context.Background()
	}
	root := e.Config.RootDir
	steps := []struct {
		id  string
		run func() DoctorCheck
	}{
		{message.DoctorCheckGlueRoot, func() DoctorCheck { return checkGlueRootWritable(root) }},
		{message.DoctorCheckGit, func() DoctorCheck { return checkGitAvailable(root) }},
		{message.DoctorCheckSevenZip, func() DoctorCheck { return check7zipAvailable(root) }},
		{message.DoctorCheckDark, func() DoctorCheck { return checkDarkAvailable(root) }},
		{message.DoctorCheckInnounp, func() DoctorCheck { return checkInnounpAvailable(root) }},
		{message.DoctorCheckShimDir, func() DoctorCheck { return checkShimInPath(root) }},
		{message.DoctorCheckGitHub, func() DoctorCheck { return checkGitHubReachable(ctx, root) }},
	}

	checks := make([]DoctorCheck, len(steps))
	var (
		mu    sync.Mutex
		allOK = true
		wg    sync.WaitGroup
	)

	if onRunning != nil {
		for _, step := range steps {
			onRunning(step.id)
		}
	}

	for i, step := range steps {
		wg.Add(1)
		go func(idx int, s struct {
			id  string
			run func() DoctorCheck
		}) {
			defer wg.Done()
			if ctx.Err() != nil {
				return
			}
			c := s.run()
			mu.Lock()
			checks[idx] = c
			if !c.OK {
				allOK = false
			}
			cb := onCheck
			mu.Unlock()
			if cb != nil {
				cb(c)
			}
		}(i, step)
	}
	wg.Wait()
	return DoctorReport{Checks: checks, OK: allOK}
}

func doctorHint(key string) string {
	if key == "" {
		return ""
	}
	return message.FormatEN(key, nil)
}

// checkGlueRootWritable ensures ~/.glue exists and accepts a short-lived write test.
func checkGlueRootWritable(root string) DoctorCheck {
	c := DoctorCheck{ID: message.DoctorCheckGlueRoot}
	if root == "" {
		c.DetailKey = message.DoctorGlueRootUnknown
		c.HintKey = message.DoctorHintGlueRootAccess
		c.Hint = doctorHint(c.HintKey)
		return c
	}
	if err := os.MkdirAll(root, 0755); err != nil {
		c.DetailKey = message.DoctorGlueRootNotWritable
		c.DetailText = fmt.Sprintf("%s: %v", root, err)
		c.HintKey = message.DoctorHintGlueRootAccess
		c.Hint = doctorHint(c.HintKey)
		return c
	}
	testFile := filepath.Join(root, ".doctor-write-test")
	if err := os.WriteFile(testFile, []byte("ok"), 0644); err != nil {
		c.DetailKey = message.DoctorGlueRootNotWritable
		c.DetailText = fmt.Sprintf("%s: %v", root, err)
		c.HintKey = message.DoctorHintGlueRootAccess
		c.Hint = doctorHint(c.HintKey)
		return c
	}
	_ = os.Remove(testFile)
	c.OK = true
	c.DetailText = root
	return c
}

// checkGitAvailable delegates to CheckToolForDoctor (deps_probe.go).
func checkGitAvailable(root string) DoctorCheck {
	c, _ := CheckToolForDoctor(message.DoctorCheckGit, root)
	return c
}

func check7zipAvailable(root string) DoctorCheck {
	c, _ := CheckToolForDoctor(message.DoctorCheckSevenZip, root)
	return c
}

func checkDarkAvailable(root string) DoctorCheck {
	c, _ := CheckToolForDoctor(message.DoctorCheckDark, root)
	return c
}

func checkInnounpAvailable(root string) DoctorCheck {
	c, _ := CheckToolForDoctor(message.DoctorCheckInnounp, root)
	return c
}

// checkShimInPath reports whether the glue shim bin directory is on PATH.
func checkShimInPath(root string) DoctorCheck {
	c := DoctorCheck{ID: message.DoctorCheckShimDir}
	mgr, err := shim.NewManager(root)
	if err != nil {
		c.DetailText = err.Error()
		return c
	}
	binDir := mgr.BinDir()
	if mgr.InPath() {
		c.OK = true
		c.DetailKey = message.DoctorShimInPath
		c.DetailText = binDir
		return c
	}
	c.DetailKey = message.DoctorShimNotInPath
	c.DetailText = binDir
	c.HintKey = message.DoctorHintShimPath
	c.Hint = doctorHint(c.HintKey)
	return c
}

const doctorGitHubProbeURL = "https://github.com"
const doctorGitHubGitProbeRepo = "https://github.com/ScoopInstaller/Main.git"

// resolveGitForDoctor returns a git runner for connectivity probes, or nil when git is unavailable.
func resolveGitForDoctor(root string) *git.Runner {
	p := ProbeGit(root)
	if !p.OK {
		return nil
	}
	r := git.NewRunner()
	if p.FromBootstrap {
		r.SetGitPath(p.Path)
	}
	return r
}

// checkGitHubReachable tests bucket-update connectivity: git ls-remote first, then HTTPS, then mirrors.
func checkGitHubReachable(ctx context.Context, root string) DoctorCheck {
	c := DoctorCheck{ID: message.DoctorCheckGitHub}
	if ctx == nil {
		ctx = context.Background()
	}

	if gr := resolveGitForDoctor(root); gr != nil {
		if err := gr.LsRemote(ctx, doctorGitHubGitProbeRepo); err == nil {
			c.OK = true
			c.DetailKey = message.DoctorGitHubGitOK
			return c
		}
	}

	if code, err := probeURL(ctx, doctorGitHubProbeURL); err == nil && httpStatusOK(code) {
		c.OK = true
		c.DetailKey = message.DoctorGitHubDirectOK
		c.DetailText = fmt.Sprintf("HTTP %d", code)
		return c
	}

	proxies := config.LoadProxies(root)
	mirrorURLs := config.MirrorURLs(doctorGitHubProbeURL, proxies)
	for _, mirrorURL := range mirrorURLs {
		if mirrorURL == doctorGitHubProbeURL {
			continue
		}
		code, err := probeURL(ctx, mirrorURL)
		if err == nil && httpStatusOK(code) {
			c.OK = true
			c.DetailKey = message.DoctorGitHubMirrorOK
			c.DetailText = fmt.Sprintf("HTTP %d: %s", code, mirrorProbeLabel(mirrorURL))
			return c
		}
	}

	if len(proxies) == 0 {
		c.DetailKey = message.DoctorGitHubDirectFailed
		c.HintKey = message.DoctorHintGitHubProxy
		c.Hint = doctorHint(c.HintKey)
		return c
	}
	c.DetailKey = message.DoctorGitHubAllFailed
	c.HintKey = message.DoctorHintGitHubMirror
	c.Hint = doctorHint(c.HintKey)
	return c
}

// probeURL tries HEAD then GET; accepts 2xx–4xx as reachable (some hosts block HEAD).
func probeURL(ctx context.Context, url string) (int, error) {
	for _, method := range []string{http.MethodHead, http.MethodGet} {
		code, err := doHTTPProbe(ctx, url, method)
		if err == nil && httpStatusOK(code) {
			return code, nil
		}
		if err != nil && method == http.MethodHead {
			continue
		}
		if err != nil {
			return 0, err
		}
	}
	return 0, fmt.Errorf("probe failed")
}

// doHTTPProbe performs a single HTTP request with an 8s timeout.
func doHTTPProbe(ctx context.Context, url, method string) (int, error) {
	reqCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, method, url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", "Gluestick-Doctor/1.0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}

// httpStatusOK treats 4xx as reachable (server responded); 5xx is failure.
func httpStatusOK(code int) bool {
	return code >= 200 && code < 500
}

// mirrorProbeLabel shortens a mirror URL for doctor output (prefix before github.com).
func mirrorProbeLabel(mirrorURL string) string {
	const target = doctorGitHubProbeURL
	if strings.HasSuffix(mirrorURL, target) {
		prefix := strings.TrimSuffix(mirrorURL, target)
		return strings.TrimSuffix(prefix, "/")
	}
	if len(mirrorURL) > 80 {
		return mirrorURL[:77] + "..."
	}
	return mirrorURL
}
