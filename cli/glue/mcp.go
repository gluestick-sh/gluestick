package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/gluestick-sh/cli/version"
	"github.com/gluestick-sh/core/bucket"
	"github.com/gluestick-sh/core/engine"
	"github.com/gluestick-sh/core/shim"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
)

// mcpCmd runs the agent-native MCP server over stdio (roadmap §4). Tool calls
// reuse the same core engine as the CLI, so both entry points share behavior,
// errors and the data root. All protocol traffic goes to stdout; logs go to
// stderr through the SDK.
var mcpCmd = &cobra.Command{
	Use:   "mcp",
	Short: "Run the MCP server (stdio) for agent clients",
	Args: func(cmd *cobra.Command, args []string) error {
		if len(args) > 0 {
			fmt.Fprintf(cmd.ErrOrStderr(), "glue mcp takes no arguments (got %q)\n", args[0])
			return wrapUsageError(fmt.Errorf("accepts 0 arg(s), received %d", len(args)))
		}
		return nil
	},
	RunE: runMCP,
}

func init() {
	rootCmd.AddCommand(mcpCmd)
	mcpCmd.SilenceUsage = true
	mcpCmd.SilenceErrors = true
}

// runMCP serves MCP over stdin/stdout until the client closes the pipe. The
// engine is created without verbose output so nothing can interleave with the
// JSON-RPC stream.
func runMCP(cmd *cobra.Command, _ []string) error {
	root := glueRoot()
	eng, err := engine.NewEngine(&engine.EngineConfig{RootDir: root})
	if err != nil {
		return fmt.Errorf("initialize engine: %w", err)
	}
	defer eng.Close()

	err = newGlueMCPServer(eng, root).Run(cmd.Context(), &mcp.StdioTransport{})
	if isMCPShutdown(err) {
		// A client closing stdin is a normal shutdown, not an operation failure.
		return nil
	}
	return err
}

// isMCPShutdown reports whether an MCP server error is just the client going
// away. The SDK surfaces that as the exported ErrConnectionClosed or as one of
// its internal jsonrpc2 errors ("server is closing" / "client is closing",
// carrying the underlying EOF), so the message is matched too — an agent
// closing stdin must not turn into exit code 1.
func isMCPShutdown(err error) bool {
	switch {
	case err == nil,
		errors.Is(err, io.EOF),
		errors.Is(err, context.Canceled),
		errors.Is(err, context.DeadlineExceeded),
		errors.Is(err, mcp.ErrConnectionClosed):
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "is closing") || strings.Contains(msg, "connection closed")
}

// newGlueMCPServer builds the server and registers the read tools. A fresh
// server per run keeps tests isolated; tool registration is centralized here so
// later write tools (install/uninstall with confirm) land in one place.
func newGlueMCPServer(eng *engine.Engine, root string) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "glue", Version: version.CLIVersion()}, nil)
	registerGlueReadTools(server, eng, root)
	registerGlueWriteTools(server, eng, root)
	return server
}

// MCP tool inputs. The SDK infers the JSON Schema from these structs and
// validates arguments before the handler runs.

type mcpSearchInput struct {
	Query string `json:"query" jsonschema:"substring to match package names and descriptions"`
	Limit int    `json:"limit,omitempty" jsonschema:"maximum number of results (0 = default)"`
}

type mcpListInput struct {
	Detailed bool `json:"detailed,omitempty" jsonschema:"include manifest details for each installed package"`
}

type mcpDoctorInput struct {
	Offline   bool `json:"offline,omitempty" jsonschema:"skip network probes"`
	ProbeShim bool `json:"probe_shim,omitempty" jsonschema:"launch one installed shim to prove it runs"`
}

type mcpInfoInput struct {
	Package string `json:"package" jsonschema:"installed package name (bucket/name is also accepted)"`
}

type mcpDependsInput struct {
	Package string `json:"package" jsonschema:"package reference to inspect (bucket/name or name)"`
}

// addGlueReadTool registers a read-only tool and audits every call.
func addGlueReadTool[In, Out any](server *mcp.Server, eng *engine.Engine, tool *mcp.Tool, h mcp.ToolHandlerFor[In, Out]) {
	mcp.AddTool(server, tool, mcpWrapTool(tool.Name, eng, true, h))
}

// addGlueWriteTool registers a write tool. The confirm/policy layer records its
// decisions, so the wrapper only emits the stderr call summary.
func addGlueWriteTool[In, Out any](server *mcp.Server, eng *engine.Engine, tool *mcp.Tool, h mcp.ToolHandlerFor[In, Out]) {
	mcp.AddTool(server, tool, mcpWrapTool(tool.Name, eng, false, h))
}

// mcpWrapTool is the universal MCP call logger: it tags the audit context with
// source=mcp + clientInfo actor, emits one stderr summary line per call, and
// (read tools only) records a tool_call audit row. Write tools already record
// their confirm/policy decisions, so they skip the extra row.
func mcpWrapTool[In, Out any](name string, eng *engine.Engine, auditCall bool, h mcp.ToolHandlerFor[In, Out]) mcp.ToolHandlerFor[In, Out] {
	return func(ctx context.Context, req *mcp.CallToolRequest, in In) (res *mcp.CallToolResult, out Out, err error) {
		start := time.Now()
		ctx = mcpAuditContext(ctx, req)
		res, out, err = h(ctx, req, in)
		status := "ok"
		if err != nil || (res != nil && res.IsError) {
			status = "error"
		}
		elapsed := time.Since(start).Milliseconds()
		fmt.Fprintf(os.Stderr, "[glue mcp] tool=%s status=%s duration_ms=%d\n", name, status, elapsed)
		if auditCall && eng != nil {
			_ = eng.RecordAudit(ctx, "tool_call", "", "", status, map[string]any{
				"tool":        name,
				"duration_ms": elapsed,
			})
		}
		return res, out, err
	}
}

// registerGlueReadTools adds the read-only tools (roadmap §4.2): they never
// modify the machine, so no confirmation is required.
func registerGlueReadTools(server *mcp.Server, eng *engine.Engine, root string) {
	addGlueReadTool(server, eng, &mcp.Tool{
		Name:        "glue_search",
		Description: "Search Glue bucket manifests for packages. Read-only.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in mcpSearchInput) (*mcp.CallToolResult, any, error) {
		out, err := runMCPSearch(ctx, eng, in)
		return nil, out, err
	})

	addGlueReadTool(server, eng, &mcp.Tool{
		Name:        "glue_list",
		Description: "List packages installed by Glue. Read-only.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in mcpListInput) (*mcp.CallToolResult, any, error) {
		out, err := runMCPList(ctx, eng, in)
		return nil, out, err
	})

	addGlueReadTool(server, eng, &mcp.Tool{
		Name:        "glue_info",
		Description: "Show install path, shims and manifest metadata for one installed package. Read-only.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in mcpInfoInput) (*mcp.CallToolResult, any, error) {
		out, err := runMCPInfo(eng, in)
		return nil, out, err
	})

	addGlueReadTool(server, eng, &mcp.Tool{
		Name:        "glue_depends",
		Description: "Show missing dependencies and optional suggestions for a package. Read-only.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in mcpDependsInput) (*mcp.CallToolResult, any, error) {
		out, err := runMCPDepends(ctx, eng, in)
		return nil, out, err
	})

	addGlueReadTool(server, eng, &mcp.Tool{
		Name:        "glue_path_check",
		Description: "Check that the Glue shim directory is on PATH and not shadowed by Store aliases. Read-only.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ any) (*mcp.CallToolResult, any, error) {
		out, err := runMCPPathCheck(root)
		return nil, out, err
	})

	addGlueReadTool(server, eng, &mcp.Tool{
		Name:        "glue_bucket_list",
		Description: "List installed Glue buckets. Read-only.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ any) (*mcp.CallToolResult, any, error) {
		out, err := runMCPBucketList(root)
		return nil, out, err
	})

	addGlueReadTool(server, eng, &mcp.Tool{
		Name:        "glue_doctor",
		Description: "Run the Glue agent-readiness report for this machine. Read-only.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in mcpDoctorInput) (*mcp.CallToolResult, any, error) {
		return nil, runMCPDoctor(ctx, eng, in), nil
	})
}

// runMCPSearch mirrors `glue search --json`: same payload keys and engine.
func runMCPSearch(ctx context.Context, eng *engine.Engine, in mcpSearchInput) (any, error) {
	packages, err := eng.Search(ctx, &engine.SearchRequest{
		Query: in.Query,
		Limit: in.Limit,
	}, engine.NewSilentReporter())
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	return map[string]any{
		"query":   in.Query,
		"results": packages,
		"count":   len(packages),
	}, nil
}

// runMCPList mirrors `glue list --json` (sorted by package name).
func runMCPList(ctx context.Context, eng *engine.Engine, in mcpListInput) (any, error) {
	packages, err := eng.List(ctx, &engine.ListRequest{Details: in.Detailed}, engine.NewSilentReporter())
	if err != nil {
		return nil, fmt.Errorf("list: %w", err)
	}
	sort.Slice(packages, func(i, j int) bool { return packages[i].Name < packages[j].Name })
	if packages == nil {
		// JSON stability: an empty install reports [] rather than null.
		packages = []*engine.Package{}
	}
	return map[string]any{
		"packages": packages,
		"count":    len(packages),
	}, nil
}

// runMCPDoctor mirrors the `glue doctor --json` report; MCP is available now
// that this command exists, and Command stays "doctor" so clients see one schema.
func runMCPDoctor(ctx context.Context, eng *engine.Engine, in mcpDoctorInput) engine.AgentDoctorReport {
	report := eng.RunAgentDoctor(ctx, engine.AgentDoctorOptions{
		Offline:          in.Offline,
		ProbeShim:        in.ProbeShim,
		CLIVersion:       version.CLIVersion(),
		DataRootIsolated: dataRootIsolated(),
		MCPAvailable:     true,
	})
	report.Command = "doctor"
	return report
}

// runMCPInfo mirrors `glue info --json` for one package.
func runMCPInfo(eng *engine.Engine, in mcpInfoInput) (any, error) {
	detail, err := eng.GetInstalledPackageDetail(packageBaseName(in.Package))
	if err != nil {
		return nil, fmt.Errorf("info %s: %w", in.Package, err)
	}
	return map[string]any{
		"packages": []*engine.InstalledPackageDetail{detail},
		"count":    1,
	}, nil
}

// runMCPDepends mirrors `glue depends --json` for one package ref.
func runMCPDepends(ctx context.Context, eng *engine.Engine, in mcpDependsInput) (any, error) {
	plan, err := eng.PlanInstall(ctx, in.Package)
	if err != nil {
		return nil, fmt.Errorf("depends %s: %w", in.Package, err)
	}
	item := jsonDependsPlan{
		Ref:         in.Package,
		Package:     plan.Package,
		Depends:     plan.Depends,
		Suggestions: plan.Suggestions,
	}
	if item.Depends == nil {
		item.Depends = []engine.InstallPlanItem{}
	}
	if item.Suggestions == nil {
		item.Suggestions = []engine.InstallPlanItem{}
	}
	return jsonDependsResult{Command: "depends", OK: true, Plans: []jsonDependsPlan{item}}, nil
}

// runMCPPathCheck mirrors `glue path check --json`.
func runMCPPathCheck(root string) (any, error) {
	mgr, err := shim.NewManager(root)
	if err != nil {
		return nil, fmt.Errorf("path check: %w", err)
	}
	inPath := mgr.InPath()
	shadowed := engine.StoreAliasShadowsShims(mgr.BinDir())
	return map[string]any{
		"in_path":               inPath,
		"bin_dir":               mgr.BinDir(),
		"store_alias_shadowing": shadowed,
		"ok":                    inPath && !shadowed,
	}, nil
}

// runMCPBucketList mirrors `glue bucket list --json` (registry read only; no
// git bootstrap, so the tool stays side-effect free).
func runMCPBucketList(root string) (any, error) {
	br, err := bucket.NewRegistry(root)
	if err != nil {
		return nil, fmt.Errorf("bucket list: %w", err)
	}
	br.ReloadFromDisk()
	buckets := br.List()
	return map[string]any{
		"buckets": bucketJSONEntries(buckets),
		"count":   len(buckets),
	}, nil
}
