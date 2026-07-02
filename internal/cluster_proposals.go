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
	"fmt"
	"strings"
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
		       OR (status = 'snoozed' AND snooze_until < datetime('now')))
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
