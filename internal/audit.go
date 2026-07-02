// audit.go — System audit log: runtime anomaly capture and query.
//
// The system_audit_log table is the cognitive surface for runtime
// telemetry. It is the database-side counterpart to watchdog.jsonl and
// mirror.jsonl, but unlike those flat files it is queryable from the
// agent via the query_audit_log MCP tool and surfaceable in wake context.
//
// Design principles:
//
//   1. ALWAYS-ON. Subsystems that emit audit events should call LogAudit
//      unconditionally on the error path. We do not want a missing audit
//      call to be the difference between "the agent knew" and "the agent
//      was blind."
//
//   2. SAFE TO CALL FROM ANYWHERE. LogAudit must not panic on closed DB
//      or nil args. A failed audit insert is logged to stderr and
//      swallowed — the failing subsystem is what matters.
//
//   3. STRUCTURED CONTEXT. The context field is JSON (not free text) so
//      the agent can query it. Store the most useful 3-5 fields only;
//      this is not a log file replacement, it is a queryable ledger.
//
//   4. RETENTION IS POLICY, NOT INVARIANT. A 30-day TTL is enforced by
//      the gc sweep in runOpsMaintain. We do not use SQLite triggers
//      because retention is a tunable — different deployments may want
//      different windows.
package internal

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"runtime/debug"
	"time"
)

// AuditLevel classifies how serious a logged event is. The CHECK constraint
// in schema.go enforces these values at the database level.
type AuditLevel string

const (
	AuditWarn  AuditLevel = "warn"
	AuditError AuditLevel = "error"
	AuditFatal AuditLevel = "fatal"
)

// AuditContext is a free-form JSON blob passed to LogAudit. Keep it small —
// 3-5 fields, primitives only. The agent queries by level + component + days,
// not by deep context traversal.
type AuditContext map[string]interface{}

// LogAudit inserts a row into system_audit_log. Safe to call from any
// goroutine. A nil DatabaseManager or a closed DB causes the call to be
// silently swallowed (with a stderr note) so callers do not have to
// wrap every error path in error handling.
//
// The stack trace is captured automatically if not provided. Pass an
// empty string to skip capture (useful for hot-path warns where stack
// capture is too expensive).
func (dm *DatabaseManager) LogAudit(level AuditLevel, component, message, stack string, ctx AuditContext) {
	if dm == nil || dm.db == nil {
		return
	}
	if level != AuditWarn && level != AuditError && level != AuditFatal {
		fmt.Fprintf(os.Stderr, "audit: invalid level %q, skipping\n", level)
		return
	}
	if stack == "" {
		stack = string(debug.Stack())
	}
	var ctxJSON sql.NullString
	if ctx != nil {
		if b, err := json.Marshal(ctx); err == nil {
			ctxJSON = sql.NullString{String: string(b), Valid: true}
		}
	}
	if _, err := dm.db.Exec(
		`INSERT INTO system_audit_log (id, level, component, message, stack_trace, context, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		GenerateID(), string(level), component, message,
		sql.NullString{String: truncateStack(stack, 4000), Valid: stack != ""},
		ctxJSON, time.Now().UTC().Format("2006-01-02 15:04:05"),
	); err != nil {
		fmt.Fprintf(os.Stderr, "audit insert failed: %v (level=%s component=%s)\n", err, level, component)
	}
}

// QueryAuditLog returns recent audit rows filtered by the given criteria.
// Defaults: days=1, limit=20, level=any, component=any.
//
// The result is a slice of maps with stable keys so the agent can iterate
// over it via the JSON boundary. Order: newest first.
func (dm *DatabaseManager) QueryAuditLog(level AuditLevel, component string, days, limit int) ([]map[string]interface{}, error) {
	if dm == nil || dm.db == nil {
		return nil, fmt.Errorf("db not initialized")
	}
	if days <= 0 {
		days = 1
	}
	if limit <= 0 || limit > 500 {
		limit = 20
	}

	args := []interface{}{}
	q := `SELECT id, level, component, message, stack_trace, context, created_at
	      FROM system_audit_log
	      WHERE created_at >= datetime('now', ?)`
	args = append(args, fmt.Sprintf("-%d days", days))

	if level != "" {
		q += " AND level = ?"
		args = append(args, string(level))
	}
	if component != "" {
		q += " AND component = ?"
		args = append(args, component)
	}
	q += " ORDER BY created_at DESC LIMIT ?"
	args = append(args, limit)

	rows, err := dm.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("query audit log: %w", err)
	}
	defer rows.Close()

	out := []map[string]interface{}{}
	for rows.Next() {
		var (
			id, lvl, comp, msg, created string
			stack                       sql.NullString
			ctxRaw                      sql.NullString
		)
		if err := rows.Scan(&id, &lvl, &comp, &msg, &stack, &ctxRaw, &created); err != nil {
			continue
		}
		row := map[string]interface{}{
			"id":         id,
			"level":      lvl,
			"component":  comp,
			"message":    msg,
			"created_at": created,
		}
		if stack.Valid {
			row["stack_trace"] = stack.String
		}
		if ctxRaw.Valid && ctxRaw.String != "" {
			var ctxParsed map[string]interface{}
			if err := json.Unmarshal([]byte(ctxRaw.String), &ctxParsed); err == nil {
				row["context"] = ctxParsed
			}
		}
		out = append(out, row)
	}
	return out, nil
}

// AuditSummary returns a single-line summary of error/fatal activity in
// the last 7 days. Used by the wake context surface. Returns "" if no
// errors or fatals were logged — the caller should skip the line entirely.
//
// Window bumped from 24h to 7d on 2026-07-02: a 24h glance on a
// multi-day agent loop is functionally blind. 7d covers a full weekly
// cycle, catches error clusters between session boundaries, and stays
// within the 30-day audit-log retention (PruneAuditLog).
// See decision log: mpm-logs-diagnostic 2026-07-02.
func (dm *DatabaseManager) AuditSummary() string {
	if dm == nil || dm.db == nil {
		return ""
	}
	var errCount, fatalCount int
	row := dm.db.QueryRow(`
		SELECT
			SUM(CASE WHEN level = 'error' THEN 1 ELSE 0 END),
			SUM(CASE WHEN level = 'fatal' THEN 1 ELSE 0 END)
		FROM system_audit_log
		WHERE created_at >= datetime('now', '-7 days')`)
	if err := row.Scan(&errCount, &fatalCount); err != nil {
		return ""
	}
	if errCount == 0 && fatalCount == 0 {
		return ""
	}
	if fatalCount > 0 {
		return fmt.Sprintf("Audit note: %d error(s), %d fatal in the last 7 days. Run mpm call query_audit_log to investigate.", errCount, fatalCount)
	}
	return fmt.Sprintf("Audit note: %d error(s) in the last 7 days. Run mpm call query_audit_log to investigate.", errCount)
}

// PruneAuditLog deletes entries older than the given number of days.
// Returns the number of rows deleted. Called by the gc sweep.
func (dm *DatabaseManager) PruneAuditLog(retentionDays int) (int64, error) {
	if dm == nil || dm.db == nil {
		return 0, fmt.Errorf("db not initialized")
	}
	if retentionDays <= 0 {
		retentionDays = 30
	}
	res, err := dm.db.Exec(
		"DELETE FROM system_audit_log WHERE created_at < datetime('now', ?)",
		fmt.Sprintf("-%d days", retentionDays),
	)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// truncateStack caps a stack trace at max bytes. Long stacks are common
// in deeply recursive code paths; we want enough for diagnostics, not
// megabytes of goroutine trace.
func truncateStack(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "\n... (truncated)"
}
