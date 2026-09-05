// theories_pending_invariant_test.go — semantic invariants for the
// `theories_pending` health metric.
//
// Background (2026-09-02 OpenClaw report): the OpenClaw gateway logs
// `hc.theories_pending` and observed 92, while an attempted
// `mpm call mpm_theories list` returned a smaller number. The
// discrepancy traced to two independent issues:
//
//   1. NULL `tags` / `metadata` columns on legacy theory rows
//      panicked the `mpm_theories list` MCP read path with
//      "converting NULL to string is unsupported", so the operator
//      saw a partial / errored result rather than the real count.
//   2. The CLI `mpm theories list` defaults to filter="all" while
//      the MCP `mpm_theories list` defaults to filter="pending" with
//      a page-size cap (50). Different defaults → different counts
//      when either side is interpreted as "the pending population".
//
// This file pins the SEMANTIC INVARIANT: `theories_pending` in
// `mpm_system health_check` MUST equal the count of theories with
// metadata.status='pending' returned by `mpm_theories list status=pending`.
// Any drift between the two projections is a structural defect, not
// a presentation choice.
package internal

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pendingCountFromListTheories is the canonical "what does the read
// surface report as pending?" helper. Tests use this as the ground
// truth for the invariant assertion, not `len(theories)` from a raw
// SELECT, because the read surface is the contract the agent
// experiences.
func pendingCountFromListTheories(t *testing.T, dm *DatabaseManager) int {
	t.Helper()
	rows, err := dm.ListTheories(TheoryFilter{Status: "pending", Limit: 1_000_000})
	require.NoError(t, err)
	return len(rows)
}

// theoriesPendingFromHealthCheck returns the health-check metric value
// the agent runtime and OpenClaw gateway both consume.
func theoriesPendingFromHealthCheck(t *testing.T, dm *DatabaseManager) int64 {
	t.Helper()
	h, err := dm.HealthCheck()
	require.NoError(t, err)
	n, ok := h["theories_pending"].(int64)
	require.True(t, ok, "theories_pending must be present in health check; got %T", h["theories_pending"])
	return n
}

// TestTheoriesPending_HealthCheckMatchesListProjection is the headline
// invariant. After seeding a mix of pending, proven, disproven, and
// NULL-tags rows, the health-check count MUST equal the count
// returned by `ListTheories(status=pending)`. If this drifts, the
// OpenClaw discrepancy recurs.
func TestTheoriesPending_HealthCheckMatchesListProjection(t *testing.T) {
	dm := newTestDM(t)

	// 5 pending theories (canonical lifecycle: propose → pending).
	// Use unique hypotheses so ProposeTheory dedup doesn't collapse
	// them into a single id.
	for i := 0; i < 5; i++ {
		_, err := dm.ProposeTheory(
			"pending-t-"+time.Now().Format(time.RFC3339Nano)+"-"+string(rune('a'+i)),
			"vc",
			nil, nil, nil)
		require.NoError(t, err)
	}

	// 2 proven theories (must NOT inflate pending count).
	for i := 0; i < 2; i++ {
		m, err := dm.ProposeTheory(
			"to-prove-"+time.Now().Format(time.RFC3339Nano)+"-"+string(rune('a'+i)),
			"vc", nil, nil, nil)
		require.NoError(t, err)
		id, _ := m["id"].(string)
		_, err = dm.ResolveTheory(id, "yes", "proven")
		require.NoError(t, err)
	}

	// 3 disproven theories (must NOT inflate pending count).
	for i := 0; i < 3; i++ {
		m, err := dm.ProposeTheory(
			"to-disprove-"+time.Now().Format(time.RFC3339Nano)+"-"+string(rune('a'+i)),
			"vc", nil, nil, nil)
		require.NoError(t, err)
		id, _ := m["id"].(string)
		_, err = dm.ResolveTheory(id, "no", "disproven")
		require.NoError(t, err)
	}

	// 1 legacy "resolved" literal (pre-D-010 rows; must NOT inflate pending).
	_, err := dm.ExecTracked(`
		INSERT INTO memories (id, collection, content, metadata, created_at, weight)
		VALUES (?, 'theories', 'legacy resolved', ?, ?, 1)`,
		0,
		"t-legacy-resolved-"+time.Now().Format(time.RFC3339Nano),
		`{"status":"resolved"}`,
		time.Now().Unix(),
	)
	require.NoError(t, err)

	// 1 "challenged" memory (must NOT inflate pending — challenged is
	// its own state, not "pending awaiting validation").
	_, err = dm.ExecTracked(`
		INSERT INTO memories (id, collection, content, metadata, created_at, weight)
		VALUES (?, 'theories', 'challenged theory', ?, ?, 1)`,
		0,
		"t-challenged-"+time.Now().Format(time.RFC3339Nano),
		`{"status":"challenged"}`,
		time.Now().Unix(),
	)
	require.NoError(t, err)

	// Assert: only the 5 newly-proposed rows should count as pending.
	hc := theoriesPendingFromHealthCheck(t, dm)
	assert.Equal(t, int64(5), hc, "health_check.theories_pending must equal pending theories only")

	listCount := pendingCountFromListTheories(t, dm)
	assert.Equal(t, 5, listCount, "ListTheories(status=pending) must match the pending population")

	assert.Equal(t, hc, int64(listCount), "health_check.theories_pending MUST equal ListTheories(status=pending) count — this is the OpenClaw invariant")
}

// TestTheoriesPending_NullLegacyColumnsDoNotBreakRead pins the
// NULL-safety fix: legacy rows written before tags/metadata were
// mandatory must not panic the read surface. This is the exact bug
// that caused OpenClaw to see a partial result for
// `mpm call mpm_theories list`.
func TestTheoriesPending_NullLegacyColumnsDoNotBreakRead(t *testing.T) {
	dm := newTestDM(t)

	// Seed a legacy-style row with NULL tags and NULL metadata — the
	// shape that pre-D-005 callers wrote when those columns were
	// treated as optional.
	_, err := dm.db.Exec(`
		INSERT INTO memories (id, collection, content, metadata, tags, created_at, weight)
		VALUES ('t-null-tags', 'theories', 'legacy null-tags theory', NULL, NULL, ?, 1)`,
		time.Now().Unix())
	require.NoError(t, err)

	// Also seed a row with status in metadata but NULL tags — the
	// combination that triggered the original OpenClaw panic.
	_, err = dm.db.Exec(`
		INSERT INTO memories (id, collection, content, metadata, tags, created_at, weight)
		VALUES ('t-null-tags-status', 'theories', 'legacy null-tags + status row',
		        '{"status":"pending"}', NULL, ?, 1)`,
		time.Now().Unix())
	require.NoError(t, err)

	// GetTheory must succeed on the NULL-tags row.
	row, err := dm.GetTheory("t-null-tags")
	require.NoError(t, err, "GetTheory must not panic on NULL tags/metadata")
	assert.Equal(t, "pending", row["status"], "missing metadata.status defaults to pending")

	// ListTheories(status=all) must succeed without panicking on the
	// NULL columns. The legacy null-tags row appears in the all-list
	// (read surface defaults missing status → "pending"), but is
	// EXCLUDED from the strict status='pending' filter (the SQL
	// extract returns NULL, not 'pending'). This is the documented
	// asymmetry pinned in TestTheoriesPending_DocumentedAsymmetry_…
	allRows, err := dm.ListTheories(TheoryFilter{Status: "all", Limit: 100})
	require.NoError(t, err, "ListTheories must not panic on NULL tags/metadata")

	var foundNullAll, foundStatusAll bool
	for _, r := range allRows {
		switch r["id"] {
		case "t-null-tags":
			foundNullAll = true
			// The null-tags row has no metadata → extractTheoryStatus
			// defaults to "pending" per the documented contract.
			assert.Equal(t, "pending", r["status"])
		case "t-null-tags-status":
			foundStatusAll = true
			assert.Equal(t, "pending", r["status"])
		}
	}
	assert.True(t, foundNullAll, "null-tags row must appear in status=all list")
	assert.True(t, foundStatusAll, "null-tags+status row must appear in status=all list")

	// The strict pending filter — same predicate as health_check —
	// includes the row whose metadata explicitly says "pending", and
	// excludes the row with NULL metadata (which is a documented
	// asymmetry pinned separately). Crucially, NEITHER row should
	// panic the scan.
	strictRows, err := dm.ListTheories(TheoryFilter{Status: "pending", Limit: 100})
	require.NoError(t, err, "ListTheories(status=pending) must not panic on NULL columns")

	var foundStatusStrict bool
	for _, r := range strictRows {
		if r["id"] == "t-null-tags-status" {
			foundStatusStrict = true
		}
	}
	assert.True(t, foundStatusStrict, "row with explicit status=pending must appear in the strict filter")
}

// TestTheoriesPending_HealthCheckIgnoresExpiredAndDeleted pins the
// negative-space invariants: theories that are expired or soft-deleted
// MUST NOT inflate `theories_pending`. The OpenClaw discrepancy would
// recur if a tombstoned row accidentally surfaced in the count.
func TestTheoriesPending_HealthCheckIgnoresExpiredAndDeleted(t *testing.T) {
	dm := newTestDM(t)

	// Expired pending theory.
	_, err := dm.ExecTracked(`
		INSERT INTO memories (id, collection, content, metadata, created_at, weight, expires_at)
		VALUES (?, 'theories', 'expired', ?, ?, 1, ?)`,
		0,
		"t-expired",
		`{"status":"pending"}`,
		time.Now().Unix(),
		time.Now().Add(-time.Hour).Unix(),
	)
	require.NoError(t, err)

	// Soft-deleted pending theory.
	_, err = dm.ExecTracked(`
		INSERT INTO memories (id, collection, content, metadata, created_at, weight, deleted_at)
		VALUES (?, 'theories', 'deleted', ?, ?, 1, ?)`,
		0,
		"t-deleted",
		`{"status":"pending"}`,
		time.Now().Unix(),
		time.Now().Unix(),
	)
	require.NoError(t, err)

	// 1 live pending theory — only this should count.
	_, err = dm.ProposeTheory("live-pending", "vc", nil, nil, nil)
	require.NoError(t, err)

	hc := theoriesPendingFromHealthCheck(t, dm)
	assert.Equal(t, int64(1), hc, "expired and soft-deleted pending theories MUST NOT inflate theories_pending")

	listCount := pendingCountFromListTheories(t, dm)
	assert.Equal(t, 1, listCount, "ListTheories must agree with health_check on the same negative-space rules")
}

// TestTheoriesPending_LifecycleTransition verifies the count adjusts
// correctly as a theory moves through its lifecycle. propose → pending
// (count +1), resolve as proven (count -1). The same transition must
// be reflected on both the health-check side and the read surface.
func TestTheoriesPending_LifecycleTransition(t *testing.T) {
	dm := newTestDM(t)

	// Baseline: zero pending.
	require.Equal(t, int64(0), theoriesPendingFromHealthCheck(t, dm))

	// propose → count +1.
	m, err := dm.ProposeTheory("transition-test", "vc", nil, nil, nil)
	require.NoError(t, err)
	id, _ := m["id"].(string)
	assert.Equal(t, int64(1), theoriesPendingFromHealthCheck(t, dm))
	assert.Equal(t, 1, pendingCountFromListTheories(t, dm))

	// resolve as proven → count -1.
	_, err = dm.ResolveTheory(id, "transition confirmed", "proven")
	require.NoError(t, err)
	assert.Equal(t, int64(0), theoriesPendingFromHealthCheck(t, dm))
	assert.Equal(t, 0, pendingCountFromListTheories(t, dm))

	// Resolved row must NOT appear in the pending list.
	pending, err := dm.ListTheories(TheoryFilter{Status: "pending", Limit: 50})
	require.NoError(t, err)
	for _, th := range pending {
		assert.NotEqual(t, id, th["id"], "resolved theory must not appear in pending list")
	}

	// Resolved row IS reachable via status=all (historical preservation).
	// 2026-09-05 audit remediation pass 2: explicit Limit=50; the
	// DM-level `if limit <= 0 { limit = 50 }` coercion was removed
	// and callers that want the historical default must pass it
	// explicitly.
	all, err := dm.ListTheories(TheoryFilter{Status: "all", Limit: 50})
	require.NoError(t, err)
	var foundResolved bool
	for _, th := range all {
		if th["id"] == id {
			foundResolved = true
			assert.Equal(t, "proven", th["status"], "resolved theory surfaces with terminal status, not 'pending'")
		}
	}
	assert.True(t, foundResolved, "resolved theories must remain queryable through status=all (historical preservation)")
}

// TestTheoriesPending_SqlMatchesHealthCheck pins the underlying SQL:
// the literal SQL the health check runs MUST match the row set
// `ListTheories(status=pending)` returns. If anyone refactors the
// health check to a different aggregate (e.g. including `challenged`),
// this test catches the drift.
func TestTheoriesPending_SqlMatchesHealthCheck(t *testing.T) {
	dm := newTestDM(t)

	// Seed: 1 pending, 1 proven, 1 with NULL status (defaults to pending
	// in the read surface via extractTheoryStatus, but EXCLUDED by the
	// strict JSON-extract SQL — this asymmetry is the documented contract).
	m, err := dm.ProposeTheory("with-status", "vc", nil, nil, nil)
	require.NoError(t, err)
	idP, _ := m["id"].(string)

	m2, err := dm.ProposeTheory("to-prove-strict", "vc", nil, nil, nil)
	require.NoError(t, err)
	idPr, _ := m2["id"].(string)
	_, err = dm.ResolveTheory(idPr, "y", "proven")
	require.NoError(t, err)

	// Row with NULL metadata entirely (no status field at all).
	_, err = dm.db.Exec(`
		INSERT INTO memories (id, collection, content, metadata, tags, created_at, weight)
		VALUES ('t-no-status', 'theories', 'no status field', NULL, '[]', ?, 1)`,
		time.Now().Unix())
	require.NoError(t, err)

	// Sanity: 1 strictly-pending row (status='pending' literal in metadata).
	var strictCount int64
	require.NoError(t, dm.db.QueryRow(`
		SELECT COUNT(*) FROM memories
		WHERE collection = 'theories'
		  AND deleted_at IS NULL
		  AND (expires_at IS NULL OR expires_at > strftime('%s','now'))
		  AND json_extract(metadata, '$.status') = 'pending'
	`).Scan(&strictCount))
	assert.Equal(t, int64(1), strictCount)

	hc := theoriesPendingFromHealthCheck(t, dm)
	assert.Equal(t, strictCount, hc, "the health_check SQL must equal the canonical pending-population SQL")

	listRows, err := dm.ListTheories(TheoryFilter{Status: "pending", Limit: 50})
	require.NoError(t, err)
	assert.Len(t, listRows, 1, "ListTheories(status=pending) must use the same strict predicate as health_check")
	assert.Equal(t, idP, listRows[0]["id"])
}

// TestTheoriesPending_EmptyDatabaseIsZero pins the floor value. A
// fresh substrate with no theories must report 0 — a non-zero value
// here would indicate a SQL aggregation bug counting the empty set
// as non-zero (the NULL-aggregate class from Substrate Defense Triad #2).
func TestTheoriesPending_EmptyDatabaseIsZero(t *testing.T) {
	dm := newTestDM(t)
	assert.Equal(t, int64(0), theoriesPendingFromHealthCheck(t, dm))
	assert.Equal(t, 0, pendingCountFromListTheories(t, dm))
}

// guard against accidental drift between extractTheoryStatus default
// and the SQL filter (the read surface treats missing status as
// pending; the SQL filter does not). If these are intentionally
// different, the comment must explain why; if not, this test forces
// a decision.
func TestTheoriesPending_DocumentedAsymmetry_NullStatusIsPendingInReadButNotInSQL(t *testing.T) {
	dm := newTestDM(t)

	// Insert a row with status='pending' literal — both surfaces must
	// agree on it.
	m, err := dm.ProposeTheory("agree-pending", "vc", nil, nil, nil)
	require.NoError(t, err)
	idAgree, _ := m["id"].(string)

	// Insert a row with NULL metadata — read surface treats as pending,
	// SQL filter excludes.
	_, err = dm.db.Exec(`
		INSERT INTO memories (id, collection, content, metadata, tags, created_at, weight)
		VALUES ('t-asymmetry-no-meta', 'theories', 'no metadata at all', NULL, '[]', ?, 1)`,
		time.Now().Unix())
	require.NoError(t, err)

	// Read surface default-extracts status='pending' for NULL metadata.
	row, err := dm.GetTheory("t-asymmetry-no-meta")
	require.NoError(t, err)
	assert.Equal(t, "pending", row["status"], "read surface defaults missing status to pending")

	// SQL filter excludes it from the count.
	var hcAfter int64
	require.NoError(t, dm.db.QueryRow(`
		SELECT COUNT(*) FROM memories
		WHERE collection = 'theories'
		  AND deleted_at IS NULL
		  AND (expires_at IS NULL OR expires_at > strftime('%s','now'))
		  AND json_extract(metadata, '$.status') = 'pending'
	`).Scan(&hcAfter))
	assert.Equal(t, int64(1), hcAfter, "SQL filter excludes NULL-status rows; only the literal-status row counts")

	// The agreement: the read surface and the SQL filter must produce
	// matching counts for the SAME predicate (status='pending' literal).
	// The asymmetry is between "default-on-read" (permissive) and
	// "filter-on-SQL" (strict). Both behaviors are documented; this test
	// pins that ListTheories(status=pending) uses the strict filter,
	// matching the health_check metric — so the OpenClaw invariant
	// holds regardless of the read-surface default.
	list, err := dm.ListTheories(TheoryFilter{Status: "pending", Limit: 50})
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, idAgree, list[0]["id"], "ListTheories(status=pending) uses the strict SQL filter, matching health_check")
	assert.Equal(t, hcAfter, int64(len(list)), "ListTheories count MUST equal health_check.theories_pending — the OpenClaw invariant")
}
