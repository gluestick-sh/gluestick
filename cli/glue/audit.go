package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

var (
	auditSource  string
	auditPackage string
	auditSince   string
	auditLimit   int
	auditJSONL   bool
)

// auditCmd queries the append-only audit trail (Phase 2 §4.6.1).
var auditCmd = &cobra.Command{
	Use:   "audit",
	Short: "Query the Glue audit trail",
}

var auditListCmd = &cobra.Command{
	Use:   "list",
	Short: "List audit entries (who did what, when)",
	Args:  cobra.NoArgs,
	RunE:  runAuditList,
}

var auditVerifyCmd = &cobra.Command{
	Use:   "verify",
	Short: "Verify the append-only audit.jsonl hash chain",
	Args:  cobra.NoArgs,
	RunE:  runAuditVerify,
}

func init() {
	rootCmd.AddCommand(auditCmd)
	auditCmd.AddCommand(auditListCmd)
	auditCmd.AddCommand(auditVerifyCmd)
	for _, c := range []*cobra.Command{auditListCmd, auditVerifyCmd} {
		c.SilenceUsage = true
		c.SilenceErrors = true
	}
	auditListCmd.Flags().StringVar(&auditSource, "source", "", "filter by source (cli|mcp)")
	auditListCmd.Flags().StringVar(&auditPackage, "package", "", "filter by package name")
	auditListCmd.Flags().StringVar(&auditSince, "since", "", "only entries at/after this RFC3339 timestamp")
	auditListCmd.Flags().IntVar(&auditLimit, "limit", 50, "maximum entries (0 = all)")
	auditListCmd.Flags().BoolVar(&auditJSONL, "jsonl", false, "read the append-only logs/audit.jsonl (rotated segments included) instead of SQLite")
}

// auditFormat labels the source of `audit list` output.
func auditFormat() string {
	if auditJSONL {
		return "jsonl"
	}
	return "sqlite"
}

func runAuditVerify(cmd *cobra.Command, _ []string) error {
	eng, err := openCLIEngine()
	if err != nil {
		return fmt.Errorf("initialize engine: %w", err)
	}
	defer eng.Close()

	result, err := eng.VerifyAuditLog()
	if err != nil {
		return fmt.Errorf("audit verify: %w", err)
	}
	if jsonOutputEnabled() {
		if err := emitJSON(result); err != nil {
			return err
		}
		if !result.OK {
			return reportedFail()
		}
		return nil
	}
	if result.OK {
		fmt.Printf("%s Audit chain OK (%d entries).\n", markSuccess, result.Entries)
		return nil
	}
	fmt.Printf("%s Audit chain broken at entry %d: %s\n", markFail, result.BrokenAt, result.Reason)
	return reportedFail()
}

func runAuditList(cmd *cobra.Command, _ []string) error {
	if auditSource != "" && auditSource != "cli" && auditSource != "mcp" {
		return wrapUsageError(fmt.Errorf("--source must be cli or mcp (got %q)", auditSource))
	}
	eng, err := openCLIEngine()
	if err != nil {
		return fmt.Errorf("initialize engine: %w", err)
	}
	defer eng.Close()

	entries, err := eng.QueryAuditLog(auditSource, auditPackage, auditSince, auditLimit)
	if auditJSONL {
		entries, err = eng.QueryAuditJSONL(auditSource, auditPackage, auditSince, auditLimit)
	}
	if err != nil {
		return fmt.Errorf("audit list: %w", err)
	}

	if jsonOutputEnabled() {
		return emitJSON(map[string]any{"entries": entries, "count": len(entries), "format": auditFormat()})
	}
	if len(entries) == 0 {
		fmt.Println("No audit entries.")
		return nil
	}
	fmt.Printf("%d audit entr(y/ies):\n\n", len(entries))
	for _, entry := range entries {
		fmt.Printf("  %-30v %-4v %-10v %-8v %-10v %v\n",
			entry["timestamp"], entry["source"], entry["operation"], entry["status"],
			entry["package_name"], entry["actor"])
	}
	return nil
}
