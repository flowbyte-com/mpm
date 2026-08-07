// wake_context_audit_test.go — tests for the rich audit summary shape
// surfaced in wake_context.AuditSummary.
//
// Pins four contracts:
//   1. Empty surface: no errors, no clusters → "" (caller skips the line).
//   2. Headline shape: errors+warnings counts appear on the first body line.
//   3. Unknown clusters are surfaced rich (component, count, first_seen,
//      cluster_key) so the agent has actionable ID + triage context.
//   4. Known clusters (referenced by a pending theory, recent decision,
//      or resolved theory) are collapsed into a single low-noise line.
//   5. LIKE escape: a cluster_key containing % or _ is still matched
//      correctly against content (i.e. wildcards in cluster_key don't
//      break the dedup lookup).
//
// The tests use a fresh sqlite3 file (see newTestDMForWake in
// wake_context_test.go) so they never touch the workspace database.
package internal

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// seedAuditEvent inserts a raw audit_log row so AuditSummary's headline
// has something to count. level must be one of the documented levels
// (error, warn, fatal, info). createdAt is unix epoch seconds (INTEGER).
func seedAuditEvent(t *testing.T, dm *DatabaseManager, level, component, message string, createdAt int64) {
	t.Helper()
	_, err := dm.db.Exec(`
		INSERT INTO system_audit_log (level, component, message, created_at)
		VALUES (?, ?, ?, ?)`,
		level, component, message, createdAt)
	require.NoError(t, err)
}

// seedCluster inserts an audit_cluster_proposals row directly. Use
// ClusterThreshold=3 in production, but tests can go lower to avoid
// having to insert 3 events per cluster.
func seedCluster(t *testing.T, dm *DatabaseManager, clusterKey, component, messageHash, status string, count int, firstSeen, lastSeen int64) {
	t.Helper()
	_, err := dm.db.Exec(`
		INSERT INTO audit_cluster_proposals
		    (cluster_key, component, message_hash, count, first_seen, last_seen, status)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		clusterKey, component, messageHash, count, firstSeen, lastSeen, status)
	require.NoError(t, err)
}

// seedTheory inserts a theories row with explicit metadata.status.
// content is the searchable text where cluster_key would appear.
// createdAt is unix epoch seconds (INTEGER).
func seedTheory(t *testing.T, dm *DatabaseManager, id, content, status string, createdAt int64) {
	t.Helper()
	meta := fmt.Sprintf(`{"status":"%s"}`, status)
	_, err := dm.db.Exec(`
		INSERT INTO memories (id, collection, content, tags, metadata, created_at)
		VALUES (?, 'theories', ?, '[]', ?, ?)`,
		id, content, meta, createdAt)
	require.NoError(t, err)
}

// seedDecision inserts a decisions row. Decisions have no status field;
// "recent" is implied by created_at (within last 30d).
// createdAt is unix epoch seconds (INTEGER).
func seedDecision(t *testing.T, dm *DatabaseManager, id, content string, createdAt int64) {
	t.Helper()
	_, err := dm.db.Exec(`
		INSERT INTO memories (id, collection, content, tags, created_at)
		VALUES (?, 'decisions', ?, '[]', ?)`,
		id, content, createdAt)
	require.NoError(t, err)
}

// ---------------------------------------------------------------------------
// Contract 1: empty surface
// ---------------------------------------------------------------------------

func TestAuditSummary_EmptyWhenNoEvents(t *testing.T) {
	dm := newTestDMForWake(t)
	out := dm.AuditSummary()
	require.Equal(t, "", out, "no events, no clusters → empty string (caller skips the line)")
}

// ---------------------------------------------------------------------------
// Contract 2: headline shape
// ---------------------------------------------------------------------------

func TestAuditSummary_HeadlineIncludesErrorAndWarningCounts(t *testing.T) {
	dm := newTestDMForWake(t)
	// Anchor the events to "now" so they fall inside the 7-day window
	// the production query checks against; a hardcoded date drifts
	// outside the window as the wall clock advances.
	now := time.Now().UTC().Unix()
	seedAuditEvent(t, dm, "error", "relay", "e1", now)
	seedAuditEvent(t, dm, "error", "relay", "e2", now)
	seedAuditEvent(t, dm, "warn", "relay", "w1", now)

	out := dm.AuditSummary()
	require.True(t, strings.HasPrefix(out, "Audit Summary (Last 7 Days):"),
		"expected the new headline prefix, got %q", out)
	require.Contains(t, out, "2 errors, 1 warnings logged.",
		"headline must report errors + warnings, got %q", out)
}

// ---------------------------------------------------------------------------
// Contract 3: unknown clusters surfaced rich
// ---------------------------------------------------------------------------

func TestAuditSummary_UnknownClusterRichSurface(t *testing.T) {
	dm := newTestDMForWake(t)
	now := int64(1783000800) // 2026-07-02 14:00:00 UTC, epoch seconds
	// Below the production threshold (3) but tests use ClusterThreshold
	// directly so we just need count >= ClusterThreshold. Since
	// ClusterThreshold is a package constant (default 3), seed at 4.
	seedCluster(t, dm, "relay:abc123def456abc123def456abc12345", "relay",
		"abc123def456abc123def456abc12345", "active", 4, now, now)
	// One headline event to trigger the Audit Summary line at all.
	seedAuditEvent(t, dm, "error", "relay", "some event", now)

	out := dm.AuditSummary()
	require.Contains(t, out, "- Active Clusters (Unknown):",
		"unknown clusters section header, got %q", out)
	require.Contains(t, out, "[relay] 4 events since 1783000800",
		"unknown cluster row format: [component] N events since <epoch>, got %q", out)
	require.Contains(t, out, "(ID: relay:abc123def456abc123def456abc12345)",
		"unknown cluster row must include the actionable cluster_key ID, got %q", out)
}

// ---------------------------------------------------------------------------
// Contract 4: known clusters collapsed to single low-noise line
// ---------------------------------------------------------------------------

func TestAuditSummary_KnownClusterCollapsedToSingleLine(t *testing.T) {
	dm := newTestDMForWake(t)
	now := int64(1783000800) // 2026-07-02 14:00:00 UTC
	knownKey := "relay:abc123def456abc123def456abc12345"
	unknownKey := "storage:def456abc123def456abc123def456ab"

	seedCluster(t, dm, knownKey, "relay", "abc123def456abc123def456abc12345", "active", 5, now, now)
	seedCluster(t, dm, unknownKey, "storage", "def456abc123def456abc123def456ab", "active", 4, now, now)

	// A pending theory whose content references the known cluster_key.
	// This is the contract: cluster_key appears in free text → known.
	seedTheory(t, dm, "th-1",
		fmt.Sprintf("HYPOTHESIS: relay cluster %s is a misconfigured retry loop", knownKey),
		"pending", now)

	out := dm.AuditSummary()
	// The known cluster's per-line detail should NOT appear.
	require.NotContains(t, out, "[relay] 5 events since",
		"known cluster must be collapsed, not surfaced in detail, got %q", out)
	require.NotContains(t, out, knownKey,
		"known cluster_key must NOT appear in detail (collapses to summary), got %q", out)
	// The unknown cluster's per-line detail SHOULD appear.
	require.Contains(t, out, "[storage] 4 events since",
		"unknown cluster must be surfaced in detail, got %q", out)
	require.Contains(t, out, unknownKey,
		"unknown cluster_key must appear as the actionable ID, got %q", out)
	// Single low-noise summary line for known.
	require.Contains(t, out, "(1 known cluster",
		"expected the (1 known cluster) summary line, got %q", out)
}

func TestAuditSummary_KnownClusterAlsoMatchesRecentDecision(t *testing.T) {
	dm := newTestDMForWake(t)
	now := int64(1783000800) // 2026-07-02 14:00:00 UTC
	knownKey := "relay:abc123def456abc123def456abc12345"

	seedCluster(t, dm, knownKey, "relay", "abc123def456abc123def456abc12345", "active", 4, now, now)
	// A recent decision (within last 30d) that mentions the cluster_key.
	// Use time.Now().Unix() rather than the hardcoded 1783000800 fixture
	// — the production filter is `created_at >= now() - 30d`, and a
	// July-2026 fixture is now older than 30 days in real wall-clock time.
	recent := time.Now().Unix()
	seedDecision(t, dm, "dec-1",
		fmt.Sprintf("CONTEXT: handling relay cluster %s in the publish path.\nCHOICE: continue investigating.", knownKey),
		recent)

	out := dm.AuditSummary()
	require.NotContains(t, out, knownKey,
		"recent decision match should collapse the cluster to a known-summary line, got %q", out)
	require.Contains(t, out, "(1 known cluster",
		"expected (1 known cluster) summary line from decision match, got %q", out)
}

func TestAuditSummary_KnownClusterAlsoMatchesResolvedTheory(t *testing.T) {
	dm := newTestDMForWake(t)
	now := int64(1783000800) // 2026-07-02 14:00:00 UTC
	resolvedKey := "relay:abc123def456abc123def456abc12345"

	seedCluster(t, dm, resolvedKey, "relay", "abc123def456abc123def456abc12345", "active", 4, now, now)
	// A resolved theory (status=proven) referencing the cluster — the
	// cluster is closed-loop, must NOT re-surface as unknown.
	seedTheory(t, dm, "th-1",
		fmt.Sprintf("HYPOTHESIS: relay cluster %s is a misconfigured retry loop", resolvedKey),
		"proven", now)

	out := dm.AuditSummary()
	require.NotContains(t, out, resolvedKey,
		"resolved theory must also count as known, got %q", out)
	require.Contains(t, out, "(1 known cluster",
		"expected (1 known cluster) summary line from resolved theory, got %q", out)
}

// ---------------------------------------------------------------------------
// Contract 5: snoozed clusters are filtered when snooze_until is in future
// ---------------------------------------------------------------------------

func TestAuditSummary_SnoozedClusterNotSurfacedWhenStillSnoozed(t *testing.T) {
	dm := newTestDMForWake(t)
	futureSnoozeKey := "relay:abc123def456abc123def456abc12345"

	seedCluster(t, dm, futureSnoozeKey, "relay", "abc123def456abc123def456abc12345",
		"snoozed", 5, int64(1782396000), int64(1782396000)) // 2026-06-25 14:00:00 UTC
	// Set snooze_until far in the future.
	_, err := dm.db.Exec(`UPDATE audit_cluster_proposals SET snooze_until = ? WHERE cluster_key = ?`,
		int64(1798675200), futureSnoozeKey) // 2026-12-31 00:00:00 UTC
	require.NoError(t, err)

	out := dm.AuditSummary()
	// Headline is also empty so AuditSummary returns "" entirely.
	require.Equal(t, "", out,
		"snoozed cluster with future snooze_until + no raw events → empty, got %q", out)
}

func TestAuditSummary_SnoozedClusterSurfacedWhenSnoozeExpired(t *testing.T) {
	dm := newTestDMForWake(t)
	expiredKey := "relay:abc123def456abc123def456abc12345"

	seedCluster(t, dm, expiredKey, "relay", "abc123def456abc123def456abc12345",
		"snoozed", 5, int64(1782396000), int64(1782396000)) // 2026-06-25 14:00:00 UTC
	// snooze_until in the past — auto-reactivated.
	_, err := dm.db.Exec(`UPDATE audit_cluster_proposals SET snooze_until = ? WHERE cluster_key = ?`,
		1767225600, expiredKey)
	require.NoError(t, err)

	out := dm.AuditSummary()
	require.Contains(t, out, "[relay] 5 events since",
		"expired-snooze cluster must surface as unknown, got %q", out)
}

// ---------------------------------------------------------------------------
// Contract 6: LIKE escape hardening
// ---------------------------------------------------------------------------

func TestClusterKeyKnownByEpistemology_EscapesLikeWildcards(t *testing.T) {
	dm := newTestDMForWake(t)
	now := int64(1783000800) // 2026-07-02 14:00:00 UTC

	// A cluster_key containing % — naive LIKE would match ANY content.
	// With ESCAPE '\' the % is literal, so it must NOT spuriously match.
	weirdKey := "weird%component:abc123def456abc123def456abc12345"
	seedCluster(t, dm, weirdKey, "weird%component",
		"abc123def456abc123def456abc12345", "active", 4, now, now)
	// A theory whose content does NOT contain the actual cluster_key
	// but DOES contain other text — must NOT be flagged as known.
	seedTheory(t, dm, "th-decoy",
		"HYPOTHESIS: completely unrelated, no cluster_key mention here.",
		"pending", now)

	// Lookup should return false — the theory's content has no literal
	// match for "weird%component:abc123def456abc123def456abc12345".
	matched, err := dm.clusterKeyKnownByEpistemology(weirdKey)
	require.NoError(t, err)
	require.False(t, matched,
		"cluster_key with %% must not match content via wildcard; got matched=true")

	// And: a theory whose content DOES contain the literal cluster_key
	// must match. This proves the escape didn't over-correct.
	seedTheory(t, dm, "th-real",
		fmt.Sprintf("HYPOTHESIS: investigating %s for retry-loop", weirdKey),
		"pending", now)
	matched, err = dm.clusterKeyKnownByEpistemology(weirdKey)
	require.NoError(t, err)
	require.True(t, matched,
		"literal cluster_key match must still work after escape; got matched=false")
}