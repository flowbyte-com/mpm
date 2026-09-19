// wakes_resolve_lifecycle_test.go — coverage for the explicit
// `mpm_wakes resolve` action surfaced in 2026-09-19.
//
// Closes lesson 9b9f286c (mpm_wakes lacks complete) by giving
// operators and agents an audited, idempotent path to retire a
// non-scheduled-task wake without raw SQL surgery.
//
// Lifecycle contract pinned here:
//
//   - Valid cascade wake + valid reason + missing wake_id → ERROR
//   - Valid cascade wake + valid reason + valid wake_id     → resolved (fired=1, fired_by="wake-resolver")
//   - Idempotent: re-running on an already-fired wake      → already_resolved (no new audit row)
//   - Wake not found                                       → wake_not_found
//   - Invalid reason enum                                  → invalid_reason
//   - Row has no metadata.kind                              → not_a_wake
//     (the row is a scheduled_tasks-owned row; the surface
//   refuses to retire it because scheduled tasks have their
//   own lifecycle (`delete_task` / `upsert_task`).)
//
// The reason taxonomy is canonical:
//
//   reconciled         — wake represents real work that has been
//                       completed out-of-band
//   obsolete           — wake no longer represents valid work (the
//                       underlying context has moved on)
//   superseded         — wake's target downstream was superseded
//                       by a successor before the wake could be
//                       acted on
//   already_satisfied  — wake was for tracking a condition that
//                       has already been met
//
// No other reason string is accepted; arbitrary text would let
// an operator silently encode data into the audit row.

package tools

import (
	"strings"
	"testing"
	"time"

	"github.com/flowbyte-com/mpm-core"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Use the existing convention: package mpm-core is `internal` per the
// `replace` directive. The alias `mpmcore` would shadow the package
// name in unexpected ways; just use the standard internal.X form.

// seedCascadeWake inserts a synthetic cascade wake row directly
// into scheduled_wakes so the test surface can exercise the
// resolve path without going through ScheduleWake. Returns the
// wake id. The row carries metadata.kind="cascade" (the discriminator
// handleResolveWake uses to refuse scheduled_tasks-owned rows).
func seedCascadeWake(t *testing.T, dm *internal.DatabaseManager, reason string) string {
	t.Helper()
	now := timeNowUnixForResolve()
	id := "wk-test-" + internal.GenerateID()
	_, err := dm.SQLDB().Exec(`
		INSERT INTO scheduled_wakes
			(id, target_time, reason, theory_id, recurring_rule, fired, fired_at, created_by, metadata)
		VALUES (?, ?, ?, NULL, NULL, 0, NULL, 'cascade-materializer',
		        '{"kind":"cascade","source":"cascade-materializer","theory_id":"test-theory","invalidation_event_id":"test-event"}')
	`, id, now, reason)
	require.NoError(t, err)
	return id
}

// seedScheduleTaskOwnedRow inserts a row whose metadata lacks the
// `kind` discriminator — the shape of a row owned by
// scheduled_tasks rather than the wakes lifecycle.
func seedScheduleTaskOwnedRow(t *testing.T, dm *internal.DatabaseManager) string {
	t.Helper()
	now := timeNowUnixForResolve()
	id := "task-row-" + internal.GenerateID()
	_, err := dm.SQLDB().Exec(`
		INSERT INTO scheduled_wakes
			(id, target_time, reason, theory_id, recurring_rule, fired, fired_at, created_by, metadata)
		VALUES (?, ?, 'cron:epistemic-compaction', NULL, NULL, 0, NULL, 'mpm-scheduler',
		        '{"directive_id":"mpm-seed-epistemic-compaction-policy","expired":{}}')
	`, id, now)
	require.NoError(t, err)
	return id
}

// TestWakesResolve_HappyPath_ResolvesAndAudits pins the canonical
// success path: a pending cascade wake + a canonical reason +
// fired_by sentinel + audit row recording the canonical reason.
func TestWakesResolve_HappyPath_ResolvesAndAudits(t *testing.T) {
	dm := newTestSharedDM(t)

	wakeID := seedCascadeWake(t, dm, "cascade: legacy ghost")

	res, err := handleResolveWake(dm, defaultACForPatch(), map[string]interface{}{
		"wake_id": wakeID,
		"reason":  "superseded",
		"result_reference": "downstream was superseded before materialization",
	})
	require.NoError(t, err)
	m := res.(map[string]interface{})
	assert.Equal(t, true, m["success"])
	assert.Equal(t, true, m["resolved"])
	assert.Equal(t, "resolved", m["status"])

	// Wake row must be fired=1 with fired_by="wake-resolver".
	var fired int
	var firedBy string
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT fired, COALESCE(fired_by, '') FROM scheduled_wakes WHERE id = ?`, wakeID,
	).Scan(&fired, &firedBy))
	assert.Equal(t, 1, fired)
	assert.Equal(t, "wake-resolver", firedBy,
		"fired_by must distinguish resolver firings from materializer / reconciler firings")

	// Audit row must be recorded with the canonical reason.
	var auditCount int
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM system_audit_log WHERE component = 'wake-resolver' AND message LIKE ?`,
		"%resolve_wake "+wakeID+" reason=superseded%",
	).Scan(&auditCount))
	assert.Equal(t, 1, auditCount,
		"every successful resolve must produce exactly one audit row")
}

// TestWakesResolve_Idempotent_NoDoubleAudit pins: re-running on an
// already-fired wake returns success without producing a second
// audit row. Operators retrying (e.g. after a network blip) must not
// create audit noise.
func TestWakesResolve_Idempotent_NoDoubleAudit(t *testing.T) {
	dm := newTestSharedDM(t)
	wakeID := seedCascadeWake(t, dm, "cascade: ghost")

	// First resolve — succeeds, audits.
	_, err := handleResolveWake(dm, defaultACForPatch(), map[string]interface{}{
		"wake_id": wakeID,
		"reason":  "obsolete",
	})
	require.NoError(t, err)

	// Second resolve — idempotent no-op.
	res, err := handleResolveWake(dm, defaultACForPatch(), map[string]interface{}{
		"wake_id": wakeID,
		"reason":  "obsolete",
	})
	require.NoError(t, err)
	m := res.(map[string]interface{})
	assert.Equal(t, true, m["success"])
	assert.Equal(t, "already_resolved", m["status"],
		"second resolve must report already_resolved, NOT resolved")

	// Audit count must remain 1 — the no-op path must not write
	// a duplicate audit row.
	var auditCount int
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM system_audit_log WHERE component = 'wake-resolver' AND message LIKE ?`,
		"%resolve_wake "+wakeID+"%",
	).Scan(&auditCount))
	assert.Equal(t, 1, auditCount,
		"idempotent resolve must NOT produce a duplicate audit row")
}

// TestWakesResolve_RejectsUnknownWake pins: an unknown wake id
// returns a wake_not_found status with success:false (machine-
// readable refusal). The error message names the cause.
func TestWakesResolve_RejectsUnknownWake(t *testing.T) {
	dm := newTestSharedDM(t)

	res, err := handleResolveWake(dm, defaultACForPatch(), map[string]interface{}{
		"wake_id": "wk-does-not-exist",
		"reason":  "superseded",
	})
	require.NoError(t, err)
	m := res.(map[string]interface{})
	assert.Equal(t, false, m["success"])
	assert.Equal(t, "wake_not_found", m["status"])
	assert.Contains(t, m["error"].(string), "not found")
}

// TestWakesResolve_RejectsInvalidReason pins the canonical reason
// taxonomy. An arbitrary string is refused with invalid_reason.
func TestWakesResolve_RejectsInvalidReason(t *testing.T) {
	dm := newTestSharedDM(t)
	wakeID := seedCascadeWake(t, dm, "cascade: ghost")

	res, err := handleResolveWake(dm, defaultACForPatch(), map[string]interface{}{
		"wake_id": wakeID,
		"reason":  "i-cleared-it-myself",
	})
	require.NoError(t, err)
	m := res.(map[string]interface{})
	assert.Equal(t, false, m["success"])
	assert.Equal(t, "invalid_reason", m["status"])
	assert.Contains(t, m["error"].(string), "must be one of")
}

// TestWakesResolve_RequiresWakeID pins the missing-wake_id error.
// Returns a hard Go error so callers see the failure as an
// exception, not as a success:false envelope (this is the contract
// for any "required parameter missing" — distinct from
// machine-readable refusals like wake_not_found).
func TestWakesResolve_RequiresWakeID(t *testing.T) {
	dm := newTestSharedDM(t)

	_, err := handleResolveWake(dm, defaultACForPatch(), map[string]interface{}{
		"reason": "superseded",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "wake_id is required")
}

// TestWakesResolve_RequiresReason pins the missing-reason error.
func TestWakesResolve_RequiresReason(t *testing.T) {
	dm := newTestSharedDM(t)
	wakeID := seedCascadeWake(t, dm, "cascade: ghost")

	_, err := handleResolveWake(dm, defaultACForPatch(), map[string]interface{}{
		"wake_id": wakeID,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reason is required")
}

// TestWakesResolve_RejectsScheduleTaskRows pins the safety
// boundary: a row with no metadata.kind is owned by the
// scheduled_tasks surface, NOT the wakes surface, and must NOT be
// retire-able from mpm_wakes resolve. Mixing the two surfaces
// would let an agent accidentally retire a recurring schedule.
func TestWakesResolve_RejectsScheduleTaskRows(t *testing.T) {
	dm := newTestSharedDM(t)
	taskID := seedScheduleTaskOwnedRow(t, dm)

	res, err := handleResolveWake(dm, defaultACForPatch(), map[string]interface{}{
		"wake_id": taskID,
		"reason":  "superseded",
	})
	require.NoError(t, err)
	m := res.(map[string]interface{})
	assert.Equal(t, false, m["success"])
	assert.Equal(t, "not_a_wake", m["status"])
	assert.Contains(t, strings.ToLower(m["error"].(string)),
		"scheduled task",
		"refusal message must redirect operators to the scheduled-task surface")

	// The schedule row must NOT have been flipped to fired=1.
	var fired int
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT fired FROM scheduled_wakes WHERE id = ?`, taskID,
	).Scan(&fired))
	assert.Equal(t, 0, fired,
		"schedule-task row must remain unfired after a refused resolve")
}

// TestWakesResolve_AllCanonicalReasonsAccepted pins the full reason
// taxonomy. Each canonical reason resolves successfully.
func TestWakesResolve_AllCanonicalReasonsAccepted(t *testing.T) {
	dm := newTestSharedDM(t)
	reasons := []string{"reconciled", "obsolete", "superseded", "already_satisfied"}
	for _, reason := range reasons {
		t.Run(reason, func(t *testing.T) {
			wakeID := seedCascadeWake(t, dm, "cascade: ghost for "+reason)
			res, err := handleResolveWake(dm, defaultACForPatch(), map[string]interface{}{
				"wake_id": wakeID,
				"reason":  reason,
			})
			require.NoError(t, err)
			m := res.(map[string]interface{})
			assert.Equal(t, true, m["success"], "reason %q must be accepted", reason)
			assert.Equal(t, "resolved", m["status"])
		})
	}
}

// timeNowUnixForResolve returns the current unix time. Test-only
// helper, named to avoid colliding with package-private symbols
// (e.g. internal.nowUnix) and to keep the test surface isolated.
func timeNowUnixForResolve() int64 {
	return time.Now().Unix()
}