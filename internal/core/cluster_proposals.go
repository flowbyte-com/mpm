// cluster_proposals.go — tunables + helpers for the audit cluster detector.
//
// A "cluster" is a (component, message) pair that has fired
// ClusterThreshold times within ClusterWindowDays. The detector runs at
// write-side (audit.go::LogAudit) and inserts/updates a row in
// audit_cluster_proposals. The agent surfaces active clusters via
// wake_context.AuditSummary and decides whether to propose a theory,
// snooze, or do nothing.
//
// Hash derivation: md5 of TrimSpace(message). Component is part of
// cluster_key but NOT part of the hash, so two subsystems throwing the
// same generic string (e.g. "connection timeout") become distinct
// clusters keyed by component. If false positives emerge from
// over-aggressive grouping, v2 can include component in the hash.
package internal

import (
	"crypto/md5"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ClusterThreshold is the minimum number of audit events for a
// (component, message) pair within ClusterWindowDays before a cluster
// proposal row is inserted. Tunable from one place.
const ClusterThreshold = 3

// ClusterWindowDays is the rolling lookback window for the cluster
// detector. Matches the AuditSummary wake-context window (7 days).
const ClusterWindowDays = 7

// ClusterSnoozeStatus values for audit_cluster_proposals.status.
// CHECK constraint enforces these at the DB layer.
const (
	ClusterStatusActive   = "active"
	ClusterStatusSnoozed  = "snoozed"
	ClusterStatusResolved = "resolved"
)

// HashMessage returns the stable cluster-hash for an audit message.
// TrimSpace collapses leading/trailing whitespace so log noise
// ("connection timeout" vs "connection timeout\n") lands in the same
// cluster. Internal whitespace and stack traces are preserved
// intentionally — different stack traces usually mean different code
// paths and should NOT cluster together.
func HashMessage(msg string) string {
	return fmt.Sprintf("%x", md5.Sum([]byte(strings.TrimSpace(msg))))
}

// ClusterKey returns the primary key for a cluster: "{component}:{hash}".
// Including component in the key (not just the hash) means two
// subsystems throwing identical messages get distinct clusters.
func ClusterKey(component, messageHash string) string {
	return component + ":" + messageHash
}
// ClusterProposal is the structured representation of one row from
// audit_cluster_proposals, enriched with the "known vs unknown"
// classification. Returned by ActiveClusters() and consumed by both
// the wake-context string formatter (AuditSummary) and the
// list_active_clusters MCP tool. Keeping a single struct prevents
// drift between the two consumers.
type ClusterProposal struct {
	Key       string `json:"key"`        // cluster_key — also the dedup primary key
	Component string `json:"component"`  // subsystem name (relay, security, etc.)
	Count     int    `json:"count"`      // events in the rolling window
	FirstSeen string `json:"first_seen"` // ISO timestamp, first event in window
	LastSeen  string `json:"last_seen"`  // ISO timestamp, most recent event
	Status    string `json:"status"`     // active / snoozed / resolved
	Known     bool   `json:"known"`      // true if cluster_key appears in pending theory / recent decision / resolved theory
}

// ActiveClusters returns the deduped cluster proposals above threshold,
// partitioned into known and unknown buckets. This is the shared read
// primitive — AuditSummary (string formatter) and list_active_clusters
// (structured tool) both call it. One source of truth for the
// (fetch + dedup) pipeline; the consumers only differ in output shape.
//
// Thresholds:
//   - count >= ClusterThreshold
//   - status='active' OR (status='snoozed' AND snooze_until < now)
//
// "Known" means cluster_key appears in:
//   - pending theories (json_extract(metadata,'$.status')='pending')
//   - resolved theories (status='proven' or 'disproven')
//   - recent decisions (last 30d, no status field — recency is the filter)
//
// On error, returns the error with nil slices — callers decide whether
// to degrade (AuditSummary returns "") or propagate (list_active_clusters
// surfaces the error to the agent).
func (dm *DatabaseManager) ActiveClusters() (known, unknown []ClusterProposal, err error) {
	if dm == nil || dm.db == nil {
		return nil, nil, fmt.Errorf("db not initialized")
	}

	// Fetch: rows above threshold that are active or have expired snooze.
	rows, err := dm.db.Query(`
		SELECT cluster_key, component, count, first_seen, last_seen, status
		FROM audit_cluster_proposals
		WHERE count >= ?
		  AND (status = 'active'
		       OR (status = 'snoozed' AND snooze_until < CAST(strftime('%s','now') AS INTEGER)))
		ORDER BY count DESC, component ASC`,
		ClusterThreshold)
	if err != nil {
		return nil, nil, fmt.Errorf("fetch active clusters: %w", err)
	}
	defer rows.Close()

	var clusters []ClusterProposal
	for rows.Next() {
		var c ClusterProposal
		if err := rows.Scan(&c.Key, &c.Component, &c.Count, &c.FirstSeen, &c.LastSeen, &c.Status); err != nil {
			continue
		}
		clusters = append(clusters, c)
	}

	// Dedup: classify each cluster as known or unknown. Lookup failures
	// are treated as 'unknown' — surfacing more is safer than hiding a
	// real cluster due to a query bug.
	for _, c := range clusters {
		matched, err := dm.clusterKeyKnownByEpistemology(c.Key)
		if err != nil {
			c.Known = false
			unknown = append(unknown, c)
			continue
		}
		c.Known = matched
		if matched {
			known = append(known, c)
		} else {
			unknown = append(unknown, c)
		}
	}
	return known, unknown, nil
}

// Allowed cluster status transitions enforced at the DM layer (the DB
// has a CHECK constraint on the status enum, but the transition graph
// is enforced here so callers get a clean error instead of a CHECK
// violation).
//
//	resolve():  active → resolved    (also idempotent: resolved → resolved)
//	snooze():   active → snoozed     (also: snoozed → snoozed resets timer)
//
// Both transitions are non-destructive — the row is preserved with
// status='resolved' or status='snoozed' so the watchdog.jsonl and
// audit-trail forensic history stay intact. Clusters never disappear
// from the audit_cluster_proposals table; they only transition state
// out of the active filter set.

// SetClusterStatus transitions an audit_cluster_proposals row to
// 'snoozed' or 'resolved' and writes a watchdog audit row recording
// the agent's decision.
//
// Parameters:
//   - clusterKey: the primary key (component:hash). If absent from the
//     table, returns an error (no silent auto-create — cluster rows
//     are detector-driven, not agent-driven).
//   - status: must be ClusterStatusSnoozed or ClusterStatusResolved.
//   - snoozeUntil: required when status='snoozed'; ignored otherwise.
//   - reason: optional free-form note (recorded in audit context).
//
// Audit row: LogAudit(AuditWarn, "cluster", "<verb> cluster by agent",
// ...). The watchdog surfaces these in AuditSummary, closing the
// observability loop: the agent's decision stream is captured at the
// same layer as the cluster-defining events.
//
// Idempotent: re-resolving an already-resolved cluster updates
// snooze_until / reason without error; re-snoozing resets the timer.
// Both transitions log a fresh audit row so the agent's reasoning is
// captured every time.
func (dm *DatabaseManager) SetClusterStatus(clusterKey, status, snoozeUntil, reason string) error {
	if dm == nil || dm.db == nil {
		return fmt.Errorf("db not initialized")
	}
	if clusterKey == "" {
		return fmt.Errorf("cluster_key required")
	}
	switch status {
	case ClusterStatusSnoozed:
		if snoozeUntil == "" {
			return fmt.Errorf("snooze_until required for status=%s", status)
		}
		// Validate and normalize the timestamp. The DB stores INTEGER
		// Unix-epoch seconds (`WHERE snooze_until < CAST(strftime('%s','now') AS INTEGER)`
		// only works on absolute timestamps), so the relative/either-form input
		// must be expanded to an absolute UTC time before the UPDATE.
		resolved, err := parseClusterSnoozeUntil(snoozeUntil)
		if err != nil {
			return fmt.Errorf("invalid snooze_until %q: %w", snoozeUntil, err)
		}
		// Overwrite the input with the normalized form so the UPDATE
		// below stores the same string the filter will compare against.
		snoozeUntil = resolved.UTC().Format(time.RFC3339)
	case ClusterStatusResolved:
		// snoozeUntil ignored. Resolved clusters don't auto-reactivate.
	default:
		return fmt.Errorf("status must be %q or %q, got %q",
			ClusterStatusSnoozed, ClusterStatusResolved, status)
	}

	// Verify the cluster exists before UPDATE. UPDATE-without-WHERE on
	// a missing row succeeds silently and produces no audit entry,
	// which masks typos.
	var existingStatus string
	var existingCount int
	if err := dm.db.QueryRow(
		`SELECT status, count FROM audit_cluster_proposals WHERE cluster_key = ?`,
		clusterKey,
	).Scan(&existingStatus, &existingCount); err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("cluster not found: %q", clusterKey)
		}
		return fmt.Errorf("lookup cluster: %w", err)
	}

	// Apply the transition. Snooze always updates snooze_until (reset
	// timer on re-snooze); resolve clears snooze_until (cleanup).
	var err error
	switch status {
	case ClusterStatusSnoozed:
		_, err = dm.db.Exec(
			`UPDATE audit_cluster_proposals
			 SET status = ?, snooze_until = ?
			 WHERE cluster_key = ?`,
			ClusterStatusSnoozed, snoozeUntil, clusterKey,
		)
	case ClusterStatusResolved:
		_, err = dm.db.Exec(
			`UPDATE audit_cluster_proposals
			 SET status = ?, snooze_until = NULL
			 WHERE cluster_key = ?`,
			ClusterStatusResolved, clusterKey,
		)
	}
	if err != nil {
		return fmt.Errorf("update cluster: %w", err)
	}

	// Audit row. The watchdog reads system_audit_log to surface recent
	// events; the audit row here is the agent's decision trail. Using
	// AuditWarn (not Error) because the action is deliberate, not
	// anomalous.
	var ctxJSON string
	if reason != "" {
		if b, jerr := json.Marshal(map[string]interface{}{
			"cluster_key":     clusterKey,
			"prior_status":    existingStatus,
			"prior_count":     existingCount,
			"new_status":      status,
			"snooze_until":    snoozeUntil,
			"reason":          reason,
		}); jerr == nil {
			ctxJSON = string(b)
		}
	} else {
		if b, jerr := json.Marshal(map[string]interface{}{
			"cluster_key":  clusterKey,
			"prior_status": existingStatus,
			"prior_count":  existingCount,
			"new_status":   status,
		}); jerr == nil {
			ctxJSON = string(b)
		}
	}

	// Log the audit through the monitored path so it's captured in
	// watchdog.jsonl and indexed by component='cluster'.
	if c := dm.LogAudit; c != nil {
		verb := "snoozed"
		if status == ClusterStatusResolved {
			verb = "resolved"
		}
		c(AuditWarn, "cluster", fmt.Sprintf("cluster %s by agent", verb),
			"", AuditContext{"cluster_key": clusterKey, "reason": reason, "ctx_json": ctxJSON})
	}

	return nil
}

// AnnotateCluster appends a forensic annotation to the audit trail for
// an existing audit_cluster_proposals row. The annotation captures
// late-arriving insight, post-mortem context, or root-cause refinement
// without touching the cluster's status, snooze_until, count, or any
// other state field.
//
// Why a separate tool (not a flag on resolve_cluster):
//   - Resolves are intentionally final. Allowing resolve_with_note to
//     rewrite the resolution reason would invite flip-flopping.
//   - Annotations apply to ANY cluster state — active, snoozed, or
//     resolved. The agent often has more context a week later; the
//     tool must accept that without re-opening the cluster.
//   - Forensic append (audit log) is the right home — single source
//     of truth for the agent's decision stream, queryable via
//     query_audit_log with component='cluster'.
//
// Output target: a fresh row in system_audit_log with:
//   - level = AuditWarn (deliberate action, not anomaly)
//   - component = "cluster"
//   - message = "cluster annotated by agent: <first 80 chars of annotation>"
//   - context = {cluster_key, annotation, prior_status, prior_count, reason}
//
// Audit row identity (component='cluster' + prefix "cluster annotated
// by agent") is the queryable key. query_audit_log can pull a
// cluster's full annotation history by:
//
//	audit_logs := query_audit_log(component='cluster', days=30)
//	annotations := audit_logs.filter(l => strings.HasPrefix(l.message, 'cluster annotated by agent'))
//
// Annotations never mutate the cluster row. The SELECT-then-INSERT
// pattern is the same one SetClusterStatus uses; the annotation
// failure mode (audit row fails to write) returns an error but does
// not corrupt the cluster state — the cluster is independent of any
// annotation that refers to it.
//
// Re-annotating an existing cluster is permitted (no idempotency
// check) — each annotation is a distinct forensic event with its own
// timestamp. Two annotations for the same cluster on the same day
// each get their own audit row, in order.
func (dm *DatabaseManager) AnnotateCluster(clusterKey, annotation, reason string) error {
	if dm == nil || dm.db == nil {
		return fmt.Errorf("db not initialized")
	}
	if clusterKey == "" {
		return fmt.Errorf("cluster_key required")
	}
	if annotation == "" {
		return fmt.Errorf("annotation required")
	}

	// Verify the cluster exists. Annotations on phantom clusters would
	// clutter the audit log with no forensic value. Same defensive
	// check SetClusterStatus uses.
	var existingStatus string
	var existingCount int
	if err := dm.db.QueryRow(
		`SELECT status, count FROM audit_cluster_proposals WHERE cluster_key = ?`,
		clusterKey,
	).Scan(&existingStatus, &existingCount); err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("cluster not found: %q", clusterKey)
		}
		return fmt.Errorf("lookup cluster: %w", err)
	}

	// Build the audit message. Truncate annotation to 80 chars in the
	// message header for at-a-glance queryability; the full text lives
	// in context.annotation for full retrieval. 80 chars is enough to
	// convey "post-mortem", "week-later-refinement", etc. without
	// flooding query_audit_log's tail-render.
	msgPreview := annotation
	if len(msgPreview) > 80 {
		msgPreview = msgPreview[:77] + "..."
	}
	message := "cluster annotated by agent: " + msgPreview

	// Audit row. Same watchdog.jsonl capture path as SetClusterStatus;
	// annotation events get the same component='cluster' tag so a
	// single query_audit_log(component='cluster') returns the full
	// decision stream (snooze, resolve, annotate).
	if c := dm.LogAudit; c != nil {
		c(AuditWarn, "cluster", message, "",
			AuditContext{
				"cluster_key":   clusterKey,
				"annotation":    annotation,
				"prior_status":  existingStatus,
				"prior_count":   existingCount,
				"reason":        reason,
			})
	}

	return nil
}

// parseClusterSnoozeUntil accepts unix-epoch integers, ISO 8601 absolute
// timestamps, and Go-relative durations. Returns a normalized UTC time.Time.
//
// Accepted forms:
//
//	"1752422400"                  → unix-epoch seconds (cheapest to parse)
//	"24h"                         → now + 24 hours      (relative duration)
//	"7d"                          → now + 7 days        (custom extension; not Go standard)
//	"30m"                         → now + 30 minutes
//	"1h30m"                       → composite Go duration
//	"2026-07-12T12:00:00Z"        → absolute ISO 8601 UTC
//	"2026-07-12T12:00:00+02:00"   → absolute ISO 8601 with offset
//
// Rejected forms surface as a clean error so the handler can wrap it
// in a tool-level return. Relative durations like "24 hours" or "24"
// are rejected — sticking to Go's compact syntax prevents "is 7d seven
// days or December 7th?" ambiguity.
//
// Parsing order: integer → RFC3339 → Go-relative. The integer attempt
// is cheaper than RFC3339 parsing and is unambiguous (Go-relative forms
// like "24h"/"7d" contain letters and will never match ParseInt).
func parseClusterSnoozeUntil(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, fmt.Errorf("empty")
	}

	// Unix-epoch integer seconds. Cheapest parse — try first.
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return time.Unix(n, 0).UTC(), nil
	}

	// ISO 8601 absolute. Try RFC3339Nano first (covers the common
	// "Z" UTC suffix), then RFC3339 (covers offsets). Both
	// representations must work — the agent might emit either.
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}

	// Extension: Go's time.ParseDuration doesn't natively understand
	// 'd' (days). Common shorthand in operator ergonomics — handle it
	// before falling through to ParseDuration so the rest of the
	// grammar (h, m, s, ms, etc.) stays standard.
	if strings.HasSuffix(s, "d") {
		numPart := strings.TrimSuffix(s, "d")
		if d, err := time.ParseDuration(numPart + "h"); err == nil {
			// 1d = 24h (no DST semantics in snoozing).
			return time.Now().UTC().Add(d * 24), nil
		}
		// "d" suffix with non-numeric prefix is a real parse error.
		return time.Time{}, fmt.Errorf("invalid duration %q", s)
	}

	// Standard Go duration (24h, 1h30m, 30s, 500ms, ...).
	if d, err := time.ParseDuration(s); err == nil {
		return time.Now().UTC().Add(d), nil
	}

	return time.Time{}, fmt.Errorf("not a Go duration or RFC3339 timestamp")
}
