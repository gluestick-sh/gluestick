package engine

import (
	"context"
	"fmt"
	"strings"
)

// GetActivityLog returns activity_log rows (newest first), optionally filtered by package.
func (e *Engine) GetActivityLog(pkgName string, limit int) ([]map[string]any, error) {
	return e.Cache.GetActivityLog(pkgName, limit)
}

// QueryInstallHistory returns install_history rows (newest first), optionally filtered by package.
func (e *Engine) QueryInstallHistory(pkgName string, limit int) ([]map[string]any, error) {
	return e.Cache.QueryInstallHistory(pkgName, limit)
}

// QueryActivityLog returns paginated activity log rows since the given RFC3339 timestamp (empty = all).
func (e *Engine) QueryActivityLog(since string, limit, offset int) ([]map[string]any, error) {
	return e.Cache.QueryActivityLog(since, limit, offset)
}

// CountActivityLog returns the number of activity log rows since the given RFC3339 timestamp (empty = all).
func (e *Engine) CountActivityLog(since string) (int, error) {
	return e.Cache.CountActivityLog(since)
}

// ClearActivityLog removes all activity log entries.
func (e *Engine) ClearActivityLog() error {
	return e.Cache.ClearActivityLog()
}

// ClearActivityLogSince removes activity log entries since the given RFC3339 timestamp (empty = all).
func (e *Engine) ClearActivityLogSince(since string) (int64, error) {
	return e.Cache.ClearActivityLogSince(since)
}

// RecordDoctorActivity logs an environment diagnosis run to the activity log.
func (e *Engine) RecordDoctorActivity(report DoctorReport) error {
	return e.recordDoctorActivity("doctor", report)
}

// RecordEnvironmentActivity logs a `glue env` run. It shares the DoctorReport
// shape with doctor but is its own audit operation, so `glue audit list` can
// tell the two commands apart.
func (e *Engine) RecordEnvironmentActivity(report DoctorReport) error {
	return e.recordDoctorActivity("env", report)
}

// RecordAgentDoctorActivity logs a `glue doctor` agent-readiness run: the row
// carries the verdict (agentReady/score) plus the failing check ids, so the
// audit trail explains why a machine was reported not ready.
func (e *Engine) RecordAgentDoctorActivity(report AgentDoctorReport) error {
	details := map[string]interface{}{
		"ok":         report.OK,
		"agentReady": report.AgentReady,
		"score":      report.Score,
		"passed":     report.Summary.Passed,
		"total":      report.Summary.Total,
		"skipped":    report.Summary.Skipped,
	}
	if len(report.Summary.BlockingFailed) > 0 {
		details["blockingFailedChecks"] = report.Summary.BlockingFailed
	}
	if len(report.Summary.AdvisoryFailed) > 0 {
		details["advisoryFailedChecks"] = report.Summary.AdvisoryFailed
	}
	if len(report.Fixes) > 0 {
		applied := 0
		for _, fix := range report.Fixes {
			if fix.Applied {
				applied++
			}
		}
		details["fixes"] = len(report.Fixes)
		details["fixesApplied"] = applied
	}
	status := "success"
	if !report.AgentReady {
		status = "failed"
	}
	return e.RecordAudit(context.Background(), "doctor", "", "", status, details)
}

// recordDoctorActivity writes the shared DoctorReport row for a given operation
// name (doctor or env).
func (e *Engine) recordDoctorActivity(operation string, report DoctorReport) error {
	total := len(report.Checks)
	passed := 0
	var failed []string
	for _, c := range report.Checks {
		if c.OK {
			passed++
			continue
		}
		failed = append(failed, c.ID)
	}

	status := "success"
	if !report.OK {
		status = "failed"
	}

	details := map[string]interface{}{
		"passed": passed,
		"total":  total,
		"ok":     report.OK,
	}
	if len(failed) > 0 {
		details["failedChecks"] = failed
	}
	return e.RecordAudit(context.Background(), operation, "", "", status, details)
}

// RecordCheckUpdatesActivity logs a manual update-check result to the activity log.
func (e *Engine) RecordCheckUpdatesActivity(updatesCount int, summary string) error {
	details := map[string]interface{}{
		"updatesCount": updatesCount,
		"summary":      summary,
	}
	return e.RecordAudit(context.Background(), "check_updates", "", "", "success", details)
}

// RecordBucketUpdateActivity logs a bucket git pull update to the activity log.
func (e *Engine) RecordBucketUpdateActivity(taskName, status, errMsg string) error {
	return e.recordBucketActivity("bucket_update", bucketUpdateActivityLabel(taskName), status, errMsg)
}

// RecordBucketAddActivity logs adding a bucket to the activity log.
func (e *Engine) RecordBucketAddActivity(name, status, errMsg string) error {
	return e.recordBucketActivity("bucket_add", name, status, errMsg)
}

// RecordBucketRemoveActivity logs removing a bucket from the activity log.
func (e *Engine) RecordBucketRemoveActivity(name, status, errMsg string) error {
	return e.recordBucketActivity("bucket_remove", name, status, errMsg)
}

// RecordBucketCheckActivity logs a background bucket update-availability check.
func (e *Engine) RecordBucketCheckActivity(withUpdates int, names []string, status, errMsg string) error {
	details := map[string]interface{}{
		"withUpdates": withUpdates,
		"names":       names,
	}
	if errMsg != "" {
		details["error"] = errMsg
	}
	label := ""
	if len(names) == 1 {
		label = names[0]
	} else if len(names) > 1 {
		label = fmt.Sprintf("%s (+%d)", names[0], len(names)-1)
	}
	return e.RecordAudit(context.Background(), "bucket_check", label, "", status, details)
}

// recordBucketActivity is a helper that records a bucket-related operation to the
// activity log with optional error details.
func (e *Engine) recordBucketActivity(operation, label, status, errMsg string) error {
	details := map[string]any{}
	if errMsg != "" {
		details["error"] = errMsg
	}
	return e.RecordAudit(context.Background(), operation, label, "", status, details)
}

// bucketUpdateActivityLabel formats a bucket update task name for activity log display.
// Handles "*" (all buckets), single bucket names, and comma-separated lists.
func bucketUpdateActivityLabel(taskName string) string {
	switch taskName {
	case "", "*":
		return "*"
	default:
		if strings.Contains(taskName, ",") {
			names := strings.Split(taskName, ",")
			return fmt.Sprintf("%s (+%d)", names[0], len(names)-1)
		}
		return taskName
	}
}

// DeleteActivityLogByID removes a single activity log entry by id.
func (e *Engine) DeleteActivityLogByID(id int64) error {
	return e.Cache.DeleteActivityLogByID(id)
}
