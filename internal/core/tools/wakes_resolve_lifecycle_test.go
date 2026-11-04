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
//   - Row whose metadata.kind is a SCHEDULER-owned kind
//     (e.g. "cascade_summary")                            → not_a_wake
//     (the scheduler owns it and runs it through a registered
//     handler; the agent may not retire it.)
//
// 2026-09-29: the non-wake discriminator is metadata.kind, NOT the
// absence of it. Rows with `kind` absent (NULL, empty, or `{}`)
// are legitimate legacy wakes and MUST stay resolvable — see the D-1
// fix in internal/core/wake_tools.go (ScheduleWake defaults a missing
// kind to "notification", and ResolveWake accepts absent kind for
// rows authored before that default existed) and the canonical pins
// in internal/core/wake_lifecycle_d1_test.go. The fixture below
// previously inserted a kind-less row and expected a refusal, which
// contradicted that contract since D-1 landed; it now inserts the
// real non-agent-owned shape (an explicit scheduler-owned kind).
//
// 2026-11-04 (wake-ownership correction): the discriminator is now the
// shared wake-ownership policy (WakeKindAgentOwned) — the same one the
// arming gate and the opportunistic fold use. Two kinds moved: `cron`
// is AGENT-owned (the fold delivers it, and it carries the
// directive_id the agent must consult) and is therefore resolvable;
// `cascade_summary` is SCHEDULER-owned and is therefore NOT. The
// refusal message no longer points at `delete_task`, which never
// applied to a scheduled_wakes row.
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

// seedSchedulerOwnedRow inserts a row carrying a SCHEDULER-owned
// `metadata.kind` — the shape of a row the scheduler dispatches
// through a registered handler, and which the agent may therefore
// NOT retire.
//
// 2026-09-29: this fixture previously inserted metadata with no
// `kind` at all and asserted that such a row is refused as
// not_a_wake. That contradicted the D-1 contract, which deliberately
// treats a kind-less row (NULL / empty / `{}`) as a legacy WAKE so
// rows authored before ScheduleWake defaulted kind=notification stay
// resolvable. The kind-less row resolved successfully as a wake, the
// test's success=false assertion failed, and the following
// `m["error"].(string)` type assertion then panicked on the absent
// error field. The fixture now writes the real discriminator the
// resolver keys on: an explicit non-agent-owned kind.
//
// 2026-11-04 (wake-ownership correction): the fixture's kind was
// "cron", on the theory that a cron row belongs to `delete_task`. A
// cron wake is a `scheduled_wakes` row and `delete_task` does not
// apply to it; under the wake-ownership model `cron` is AGENT-owned
// and IS resolvable. The fixture now uses `cascade_summary`, which
// the scheduler dispatches through a registered handler — the exact
// class of row the agent must not be able to retire. Matching
// TestD1_ResolveWake_RejectsSchedulerOwnedKind in
// internal/core/wake_lifecycle_d1_test.go.
func seedSchedulerOwnedRow(t *testing.T, dm *internal.DatabaseManager) string {
	t.Helper()
	now := timeNowUnixForResolve()
	id := "scheduler-row-" + internal.GenerateID()
	_, err := dm.SQLDB().Exec(`
		INSERT INTO scheduled_wakes
			(id, target_time, reason, theory_id, recurring_rule, fired, fired_at, created_by, metadata)
		VALUES (?, ?, ?, NULL, NULL, 0, NULL, 'mpm-scheduler',
		        '{"kind":"cascade_summary"}')
	`, id, now, "cascade summary wake row")
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
		"wake_id":          wakeID,
		"reason":           "superseded",
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

// TestWakesResolve_RejectsSchedulerOwnedRows pins the ownership
// boundary: a row whose metadata.kind is a SCHEDULER-owned kind (here
// "cascade_summary", which the scheduler dispatches through a
// registered handler) must NOT be retire-able from mpm_wakes resolve.
//
// 2026-11-04 (wake-ownership correction): this test used to use
// "cron" and to assert that the refusal redirected the caller to
// `delete_task`. Both halves were wrong: a cron wake is a
// `scheduled_wakes` row the agent owns and may retire, and
// `delete_task` is the surface for `scheduled_tasks` rows — a
// different table entirely. The refusal now says what is actually
// true: the row belongs to the scheduler, so the agent cannot retire
// it.
func TestWakesResolve_RejectsSchedulerOwnedRows(t *testing.T) {
	dm := newTestSharedDM(t)
	ownedID := seedSchedulerOwnedRow(t, dm)

	res, err := handleResolveWake(dm, defaultACForPatch(), map[string]interface{}{
		"wake_id": ownedID,
		"reason":  "superseded",
	})
	require.NoError(t, err)
	m := res.(map[string]interface{})
	assert.Equal(t, false, m["success"])
	assert.Equal(t, "not_a_wake", m["status"])
	// 2026-09-29: this was `strings.ToLower(m["error"].(string))`,
	// an unchecked type assertion. When the row was misclassified as
	// a wake the envelope carried no "error" key at all, so the
	// assertion panicked and destroyed the real diagnostic — the
	// earlier assert.Equal failures above were the useful signal.
	// Assert the key's presence and type first so a malformed
	// envelope reports a readable failure instead of a nil-interface
	// panic, and so a refusal can never be silently missing its
	// machine-readable cause.
	errMsg, ok := m["error"].(string)
	require.True(t, ok,
		"refusal envelope must carry a string \"error\" field; got %#v", m["error"])
	assert.Contains(t, strings.ToLower(errMsg), "scheduler-owned",
		"refusal message must say the row belongs to the scheduler")
	assert.NotContains(t, strings.ToLower(errMsg), "delete_task",
		"the refusal must not point at delete_task: a scheduler-owned wake is not a scheduled_tasks row")

	// The row must NOT have been flipped to fired=1.
	var fired int
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT fired FROM scheduled_wakes WHERE id = ?`, ownedID,
	).Scan(&fired))
	assert.Equal(t, 0, fired,
		"a scheduler-owned row must remain unfired after a refused resolve")
}

// seedKindlessWake inserts a legacy wake row whose metadata carries no
// `kind` discriminator. This is the shape of rows authored before
// ScheduleWake began defaulting kind=notification (2026-09-22 D-1), and
// of any row written directly by an operator or another tool.
//
// 2026-09-29: added alongside the fixture repair in
// seedScheduleTaskOwnedRow. The two fixtures are deliberately
// distinguished ONLY by the presence of `kind`, so a regression that
// collapsed the discriminator back to "absent means schedule task"
// would be caught from both sides.
func seedKindlessWake(t *testing.T, dm *internal.DatabaseManager, label, metadata string) string {
	t.Helper()
	now := timeNowUnixForResolve()
	id := "kindless-" + internal.GenerateID()
	var metaVal interface{}
	if metadata == "<NULL>" {
		metaVal = nil
	} else {
		metaVal = metadata
	}
	_, err := dm.SQLDB().Exec(`
		INSERT INTO scheduled_wakes
			(id, target_time, reason, theory_id, recurring_rule, fired, fired_at, created_by, metadata)
		VALUES (?, ?, ?, NULL, NULL, 0, NULL, 'legacy-test', ?)
	`, id, now, label, metaVal)
	require.NoError(t, err)
	return id
}

// TestWakesResolve_KindlessRowsRemainResolvable pins the OTHER side of
// the discriminator boundary: a row with no metadata.kind is a legacy
// WAKE, not a scheduled task, and must resolve successfully.
//
// 2026-09-29: this is the invariant the stale fixture violated. Per the
// D-1 fix, ScheduleWake defaults a missing kind to "notification" and
// ResolveWake accepts an absent kind so rows predating that default do
// not become unresolvable (and unretirable) forever. The canonical
// pins for this live in internal/core/wake_lifecycle_d1_test.go at the
// DatabaseManager layer; this test pins the same invariant at the MCP
// tool-handler layer, which is where the misclassification actually
// surfaced. All three legacy encodings are covered: SQL NULL, empty
// string, and empty JSON object.
func TestWakesResolve_KindlessRowsRemainResolvable(t *testing.T) {
	cases := []struct {
		name     string
		metadata string
	}{
		{"sql_null", "<NULL>"},
		{"empty_string", ""},
		{"empty_json_object", "{}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dm := newTestSharedDM(t)
			wakeID := seedKindlessWake(
				t, dm, "legacy kindless wake: "+tc.name, tc.metadata)

			res, err := handleResolveWake(dm, defaultACForPatch(), map[string]interface{}{
				"wake_id": wakeID,
				"reason":  "reconciled",
			})
			require.NoError(t, err)
			m, ok := res.(map[string]interface{})
			require.True(t, ok, "handler must return a result map; got %T", res)

			assert.Equal(t, true, m["success"],
				"a row with no metadata.kind is a legacy wake and must resolve")
			assert.Equal(t, "resolved", m["status"],
				"legacy kind-less row must reach the resolved status, not not_a_wake")
			_, hasErr := m["error"]
			assert.False(t, hasErr,
				"a successful resolve envelope must not carry an error field")

			var fired int
			require.NoError(t, dm.SQLDB().QueryRow(
				`SELECT fired FROM scheduled_wakes WHERE id = ?`, wakeID,
			).Scan(&fired))
			assert.Equal(t, 1, fired,
				"legacy wake must be retired exactly as any other wake")
		})
	}
}

// TestWakesResolve_RejectionEnvelopeAlwaysCarriesError pins that every
// refusal envelope is well-formed: a string "error" field naming the
// cause. A refusal that omits its cause is a malformed result that
// reads as success to any caller that only branches on "error".
//
// 2026-09-29: the panic this test's shape prevents came from an
// unchecked `m["error"].(string)` in TestWakesResolve_RejectsScheduleTaskRows.
func TestWakesResolve_RejectionEnvelopeAlwaysCarriesError(t *testing.T) {
	dm := newTestSharedDM(t)

	// wake_not_found — unknown id.
	res, err := handleResolveWake(dm, defaultACForPatch(), map[string]interface{}{
		"wake_id": "wk-does-not-exist-2026-09-29",
		"reason":  "reconciled",
	})
	require.NoError(t, err, "a machine-readable refusal is not a Go error")
	m, ok := res.(map[string]interface{})
	require.True(t, ok, "refusal must be a result map; got %T", res)
	assert.Equal(t, false, m["success"], "unknown wake must not report success")
	assert.Equal(t, "wake_not_found", m["status"])
	errMsg, ok := m["error"].(string)
	require.True(t, ok, "refusal envelope must carry a string error field; got %#v", m["error"])
	assert.NotEmpty(t, errMsg, "refusal error must not be empty")

	// not_a_wake — a scheduler-owned row.
	ownedID := seedSchedulerOwnedRow(t, dm)
	res, err = handleResolveWake(dm, defaultACForPatch(), map[string]interface{}{
		"wake_id": ownedID,
		"reason":  "superseded",
	})
	require.NoError(t, err)
	m, ok = res.(map[string]interface{})
	require.True(t, ok, "refusal must be a result map; got %T", res)
	assert.Equal(t, false, m["success"], "a scheduler-owned row must not report success")
	assert.Equal(t, "not_a_wake", m["status"])
	errMsg, ok = m["error"].(string)
	require.True(t, ok, "refusal envelope must carry a string error field; got %#v", m["error"])
	assert.NotEmpty(t, errMsg, "refusal error must not be empty")
	// The refusal must say who owns the row.
	assert.Contains(t, strings.ToLower(errMsg), "scheduler-owned",
		"refusal must say the row belongs to the scheduler")
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
