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