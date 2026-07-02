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
	// Derive `now` once and pass the same string to both the raw
	// audit insert and the cluster upsert. Temporal alignment
	// matters: the cluster's last_seen must match the audit_log's
	// created_at so the 7d rolling-window reset (see
	// upsertClusterCounter) uses the same moment as the event.
	now := time.Now().UTC().Format("2006-01-02 15:04:05")
	if _, err := dm.db.Exec(
		`INSERT INTO system_audit_log (id, level, component, message, stack_trace, context, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		GenerateID(), string(level), component, message,
		sql.NullString{String: truncateStack(stack, 4000), Valid: stack != ""},
		ctxJSON, now,
	); err != nil {
		fmt.Fprintf(os.Stderr, "audit insert failed: %v (level=%s component=%s)\n", err, level, component)
		return
	}

	// SECONDARY: cluster counter. Best-effort aggregation. The raw
	// event is already durably stored above, so a failure here means
	// only that the cluster count lags by one — the next event in
	// the same cluster will retry the upsert. NEVER return error
	// from here: observation must not back-pressure the failing path.
	if err := dm.upsertClusterCounter(component, message, now); err != nil {
		fmt.Fprintf(os.Stderr, "audit cluster upsert failed: %v (component=%s) — raw event preserved; will retry on next event\n", err, component)
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

// AuditSummary returns the rich audit summary string for wake context.
// It surfaces:
//   - headline error/warning counts in the last 7 days
//   - rich per-cluster lines for UNKNOWN clusters (component, count,
//     first_seen, cluster_key) so the agent can act immediately
//   - a single low-noise line for the count of KNOWN clusters
//     (those whose cluster_key appears in a pending theory, recent
//     decision, or resolved theory — they're tracked, don't re-trigger
//
// Returns "" if no errors, warnings, or clusters were seen — caller
// should skip the line entirely.
//
// Backwards-compat shim: the pre-cluster era AuditSummary() lived here.
// The new version is in wake_context.go because it pulls from three
// tables (audit_cluster_proposals + theories + decisions) and belongs
// in the wake-context domain. This file still owns the raw audit_log
// primitives (LogAudit, QueryAuditLog, PruneAuditLog).
func (dm *DatabaseManager) AuditSummary() string {
	if dm == nil || dm.db == nil {
		return ""
	}
	return dm.auditSummaryRich()
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

// upsertClusterCounter is the write-side hook for the cluster detector.
// Called by LogAudit after the raw audit event is durably stored. This
// is best-effort aggregation: failure is logged to stderr but does NOT
// fail LogAudit (the raw event is already on disk; the next event in
// the same cluster will retry).
//
// The single SQL statement does five things atomically:
//   1. INSERT a new row on first occurrence (count=1).
//   2. ON CONFLICT (existing cluster_key), increment count.
//   3. If last_seen is older than ClusterWindowDays, reset count to 1
//      and first_seen to now (true rolling window — dormant clusters
//      decay naturally and re-cluster if they come back).
//   4. Update last_seen to the event's timestamp (passed in for
//      temporal alignment with system_audit_log.created_at).
//   5. Auto-reactivate snoozed clusters whose snooze_until has passed
//      (status='snoozed' → 'active'). Resolved clusters are NOT
//      auto-reactivated — that's an explicit agent decision that the
//      write-side must not override.
//
// Helpers (HashMessage, ClusterKey) live in internal/cluster_proposals.go
// as the canonical home for the cluster proposal API. Both the write
// path (here) and the future read path (wake_context.AuditSummary)
// share them so there's no copy-paste drift.
func (dm *DatabaseManager) upsertClusterCounter(component, message, now string) error {
	if dm == nil || dm.db == nil {
		return fmt.Errorf("db not initialized")
	}
	msgHash := HashMessage(message)
	clusterKey := ClusterKey(component, msgHash)
	windowClause := fmt.Sprintf("-%d days", ClusterWindowDays)

	_, err := dm.db.Exec(
		`INSERT INTO audit_cluster_proposals
		    (cluster_key, component, message_hash, count, first_seen, last_seen)
		VALUES (?, ?, ?, 1, ?, ?)
		ON CONFLICT(cluster_key) DO UPDATE SET
		    count = CASE
		        WHEN audit_cluster_proposals.last_seen < datetime('now', ?)
		        THEN 1
		        ELSE audit_cluster_proposals.count + 1
		    END,
		    first_seen = CASE
		        WHEN audit_cluster_proposals.last_seen < datetime('now', ?)
		        THEN excluded.last_seen
		        ELSE audit_cluster_proposals.first_seen
		    END,
		    last_seen = excluded.last_seen,
		    updated_at = CURRENT_TIMESTAMP,
		    status = CASE
		        WHEN audit_cluster_proposals.status = 'snoozed'
		             AND audit_cluster_proposals.snooze_until < datetime('now')
		        THEN 'active'
		        ELSE audit_cluster_proposals.status
		    END`,
		clusterKey, component, msgHash, now, now, windowClause, windowClause,
	)
	return err
}
