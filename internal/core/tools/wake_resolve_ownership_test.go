// wake_resolve_ownership_test.go — the resolution side of the wake
// ownership boundary.
//
// `mpm_wakes resolve` is the EXPLICIT, audited retirement path: an
// agent (or operator tool) names one wake id and the resolver flips
// `fired=1`, writes a `wake-resolver` audit row, and the wake is
// terminal.
//
// That made it the last agent-facing surface that could retire a
// scheduler-owned wake. The resolver carried its OWN hand-written kind
// allowlist — `{cascade, cascade_summary, notification, ""}` — which
// disagreed with the canonical ownership set in two directions:
//
//   - `cascade_summary` is SCHEDULER-owned (it has a registered
//     handler), yet the resolver accepted it. An agent could therefore
//     retire a scheduler-owned wake before its handler ran, and the
//     scheduler's `QueryDueWakes` would never see the row again.
//   - `cron` is AGENT-owned (the fold delivers it; it carries the
//     `directive_id` the agent must consult), yet the resolver refused
//     it — with a message telling the agent to "use delete_task for
//     scheduled tasks", which is simply wrong: a cron wake is a
//     `scheduled_wakes` row, and `delete_task` does not apply to it.
//
// The resolver now uses the SAME predicate as the arming gate and the
// fold: `WakeKindAgentOwned`. One policy, one list, no drift.
//
// These tests drive the REAL handler path — `mpm_wakes` with
// `action: "resolve"` — because the defect was in a public surface,
// not in a helper. Every test uses a disposable database.
package tools

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/flowbyte-com/mpm-core"
	"github.com/stretchr/testify/require"
)

// schedulerOwnedResolveKinds are the kinds the scheduler dispatches
// through a registered handler. The agent may not retire any of them.
var schedulerOwnedResolveKinds = []string{
	"cascade_summary",
	"drill",
	"gc",
	"snapshot",
	"broadcast",
	"critic_audit",
}

// agentOwnedResolveKinds are the kinds the agent owns — and therefore
// may both arm and retire.
var agentOwnedResolveKinds = []string{"notification", "cascade", "cron"}

// seedResolveWake inserts a wake row with the given raw metadata and
// returns its id.
//
// The row is inserted rather than scheduled so the fixture does not
// depend on the producer's kind policy — the point of these tests is
// what the RESOLVER does with a row that already exists, including
// rows written by an older binary or by an internal producer.
func seedResolveWake(t *testing.T, dm *internal.DatabaseManager, id, metadata string) string {
	t.Helper()
	var metaVal interface{}
	if metadata != "<NULL>" {
		metaVal = metadata
	}
	_, err := dm.SQLDB().Exec(`
		INSERT INTO scheduled_wakes
			(id, target_time, reason, theory_id, recurring_rule, fired, fired_at, created_by, metadata)
		VALUES (?, ?, ?, NULL, NULL, 0, NULL, 'test-producer', ?)
	`, id, time.Now().Add(2*time.Hour).Unix(), "resolve-ownership fixture "+id, metaVal)
	require.NoError(t, err, "seed wake %s", id)
	return id
}

// resolveWakeViaTool drives the real public surface: the mpm_wakes
// tool registry handler, action "resolve".
func resolveWakeViaTool(t *testing.T, dm *internal.DatabaseManager, wakeID, reason string) map[string]interface{} {
	t.Helper()
	res, err := handleMpmWakes(dm, defaultACForPatch(), map[string]interface{}{
		"action": "resolve",
		"params": map[string]interface{}{
			"wake_id": wakeID,
			"reason":  reason,
		},
	})
	require.NoError(t, err, "a machine-readable refusal is not a Go error")
	m, ok := res.(map[string]interface{})
	require.True(t, ok, "the resolve envelope must be a map; got %T", res)
	return m
}

// wakeFired reads a wake's fired flag.
func wakeFired(t *testing.T, dm *internal.DatabaseManager, id string) int {
	t.Helper()
	var fired int
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT fired FROM scheduled_wakes WHERE id = ?`, id).Scan(&fired))
	return fired
}

// resolveAuditCount counts the audit rows the resolver wrote for a wake.
func resolveAuditCount(t *testing.T, dm *internal.DatabaseManager, wakeID string) int {
	t.Helper()
	var n int
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM system_audit_log
		 WHERE component = 'wake-resolver' AND message LIKE ?`,
		"%resolve_wake "+wakeID+"%",
	).Scan(&n))
	return n
}

// TestHandleResolveWake_RefusesSchedulerOwnedKinds is the headline
// regression.
//
// A scheduler-owned wake must not be retirable by the agent. The
// refusal must leave the row completely untouched, so the scheduler
// still finds it and runs its handler.
func TestHandleResolveWake_RefusesSchedulerOwnedKinds(t *testing.T) {
	for _, kind := range schedulerOwnedResolveKinds {
		t.Run(kind, func(t *testing.T) {
			dm := newTestSharedDM(t)
			id := seedResolveWake(t, dm, "wk-resolve-owned-"+kind,
				`{"kind":"`+kind+`"}`)

			m := resolveWakeViaTool(t, dm, id, "superseded")

			require.Equal(t, false, m["success"],
				"the agent must not retire the scheduler-owned kind %q", kind)
			require.Equal(t, "not_a_wake", m["status"],
				"the refusal must keep the machine-readable status")
			errMsg, ok := m["error"].(string)
			require.True(t, ok,
				"a refusal envelope must carry a string \"error\" field; got %#v", m["error"])
			require.Contains(t, errMsg, kind,
				"the refusal must name the offending kind")
			require.Contains(t, strings.ToLower(errMsg), "scheduler-owned",
				"the refusal must say WHY the kind is refused; got %q", errMsg)

			require.Equal(t, 0, wakeFired(t, dm, id),
				"a refused resolve must not retire the scheduler-owned wake")
			require.Equal(t, 0, resolveAuditCount(t, dm, id),
				"a refused resolve must not write an audit row")
		})
	}
}

// TestHandleResolveWake_AllowsAgentOwnedKinds is the negative control.
//
// A resolver that refused everything would pass every refusal test
// above. This is the test that says the refusal is selective: the
// agent's own kinds — notification, cascade AND cron — still resolve.
func TestHandleResolveWake_AllowsAgentOwnedKinds(t *testing.T) {
	for _, kind := range agentOwnedResolveKinds {
		t.Run(kind, func(t *testing.T) {
			dm := newTestSharedDM(t)
			id := seedResolveWake(t, dm, "wk-resolve-agent-"+kind,
				`{"kind":"`+kind+`"}`)

			m := resolveWakeViaTool(t, dm, id, "reconciled")

			require.Equal(t, true, m["success"],
				"the agent must still be able to retire its own kind %q", kind)
			require.Equal(t, true, m["resolved"])
			require.Equal(t, "resolved", m["status"])
			require.Equal(t, 1, wakeFired(t, dm, id),
				"a resolved %q wake must be retired", kind)

			// The resolver's audit attribution is unchanged.
			var firedBy string
			require.NoError(t, dm.SQLDB().QueryRow(
				`SELECT COALESCE(fired_by,'') FROM scheduled_wakes WHERE id = ?`, id,
			).Scan(&firedBy))
			require.Equal(t, "wake-resolver", firedBy,
				"fired_by must still distinguish resolver firings")
			require.Equal(t, 1, resolveAuditCount(t, dm, id),
				"a successful resolve must write exactly one audit row")
		})
	}
}

// TestHandleResolveWake_UntaggedWakeStillResolves pins the legacy
// semantics.
//
// A row whose metadata carries no usable kind is an untagged
// notification wake — that is what the fold delivers and what the
// resolver has always accepted. The ownership change must not narrow
// it.
func TestHandleResolveWake_UntaggedWakeStillResolves(t *testing.T) {
	cases := []struct {
		name     string
		metadata string
	}{
		{"sql_null", "<NULL>"},
		{"empty_string", ""},
		{"empty_json_object", "{}"},
		{"json_without_kind", `{"source":"legacy-v1"}`},
		{"json_null_kind", `{"kind":null}`},
		{"empty_kind", `{"kind":""}`},
		{"malformed_json", `{not-actually-json{`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dm := newTestSharedDM(t)
			id := seedResolveWake(t, dm, "wk-resolve-untagged-"+tc.name, tc.metadata)

			m := resolveWakeViaTool(t, dm, id, "already_satisfied")

			require.Equal(t, true, m["success"],
				"an untagged wake is a notification wake and must still resolve")
			require.Equal(t, "resolved", m["status"])
			require.Equal(t, 1, wakeFired(t, dm, id))
		})
	}
}

// TestHandleResolveWake_RefusesUnknownAndMalformedKinds pins the
// fail-closed direction.
//
// A kind this tree does not know is not the agent's. It may be a
// system kind a newer binary introduces, a typo, or an attempt to
// smuggle a wake past the boundary. A kind that is present but not a
// string is malformed and is likewise not the agent's.
func TestHandleResolveWake_RefusesUnknownAndMalformedKinds(t *testing.T) {
	cases := []struct {
		name     string
		metadata string
	}{
		{"unknown_kind", `{"kind":"kind_this_tree_does_not_know"}`},
		{"number_kind", `{"kind":42}`},
		{"bool_kind", `{"kind":true}`},
		{"array_kind", `{"kind":["notification"]}`},
		{"object_kind", `{"kind":{"kind":"notification"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dm := newTestSharedDM(t)
			id := seedResolveWake(t, dm, "wk-resolve-bad-"+tc.name, tc.metadata)

			m := resolveWakeViaTool(t, dm, id, "obsolete")

			require.Equal(t, false, m["success"],
				"an unknown or malformed kind must not be retirable")
			require.Equal(t, "not_a_wake", m["status"])
			require.Equal(t, 0, wakeFired(t, dm, id),
				"a refused resolve must not retire the wake")
			require.Equal(t, 0, resolveAuditCount(t, dm, id),
				"a refused resolve must not write an audit row")
		})
	}
}

// TestHandleResolveWake_SchedulerOwnedWakeSurvivesTheRefusal pins that
// a refusal changes NOTHING about the row.
//
// "Existing scheduler-owned rows must not be silently reclassified,
// deleted or retired." A refused resolve must not flip fired, must
// not stamp fired_at or fired_by, must not rewrite the kind, and must
// not rewrite the creator.
func TestHandleResolveWake_SchedulerOwnedWakeSurvivesTheRefusal(t *testing.T) {
	dm := newTestSharedDM(t)
	id := seedResolveWake(t, dm, "wk-resolve-survives", `{"kind":"cascade_summary"}`)

	m := resolveWakeViaTool(t, dm, id, "reconciled")
	require.Equal(t, false, m["success"])

	var fired int
	var firedAt sql.NullInt64
	var firedBy sql.NullString
	var createdBy string
	var metadata string
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT fired, fired_at, fired_by, created_by, COALESCE(metadata,'') FROM scheduled_wakes WHERE id = ?`,
		id,
	).Scan(&fired, &firedAt, &firedBy, &createdBy, &metadata))

	require.Equal(t, 0, fired, "a refused resolve must not retire the wake")
	require.False(t, firedAt.Valid, "a refused resolve must not stamp fired_at")
	require.False(t, firedBy.Valid, "a refused resolve must not stamp fired_by")
	require.Equal(t, "test-producer", createdBy,
		"a refused resolve must not rewrite the wake's creator")
	require.Equal(t, `{"kind":"cascade_summary"}`, metadata,
		"a refused resolve must not reclassify the wake")
}

// TestHandleResolveWake_CronResolutionIsIdempotent pins that the
// newly-resolvable cron kind behaves exactly like every other
// agent-owned kind.
//
// Idempotency is part of the resolver's existing contract: a second
// resolve returns `already_resolved` and writes NO second audit row.
func TestHandleResolveWake_CronResolutionIsIdempotent(t *testing.T) {
	dm := newTestSharedDM(t)
	id := seedResolveWake(t, dm, "wk-resolve-cron-idempotent",
		`{"kind":"cron","directive_id":"mpm-seed-epistemic-compaction-policy"}`)

	first := resolveWakeViaTool(t, dm, id, "reconciled")
	require.Equal(t, true, first["success"])
	require.Equal(t, "resolved", first["status"])

	second := resolveWakeViaTool(t, dm, id, "reconciled")
	require.Equal(t, true, second["success"])
	require.Equal(t, "already_resolved", second["status"],
		"the second resolve must report already_resolved, not resolved")

	require.Equal(t, 1, resolveAuditCount(t, dm, id),
		"an idempotent resolve must NOT produce a duplicate audit row")
}

// TestHandleResolveWake_ReasonTaxonomyUnchanged pins that the
// ownership change did not touch the reason enum.
func TestHandleResolveWake_ReasonTaxonomyUnchanged(t *testing.T) {
	dm := newTestSharedDM(t)

	// Every canonical reason is accepted.
	for _, reason := range []string{"reconciled", "obsolete", "superseded", "already_satisfied"} {
		id := seedResolveWake(t, dm, "wk-resolve-reason-"+reason, `{"kind":"notification"}`)
		m := resolveWakeViaTool(t, dm, id, reason)
		require.Equal(t, true, m["success"], "reason %q must be accepted", reason)
		require.Equal(t, "resolved", m["status"])
	}

	// An arbitrary reason is refused, and the refusal is checked
	// BEFORE the kind policy — an invalid reason on a scheduler-owned
	// row must still report invalid_reason.
	schedulerOwned := seedResolveWake(t, dm, "wk-resolve-reason-owned", `{"kind":"drill"}`)
	m := resolveWakeViaTool(t, dm, schedulerOwned, "i-cleared-it-myself")
	require.Equal(t, false, m["success"])
	require.Equal(t, "invalid_reason", m["status"])

	agentOwned := seedResolveWake(t, dm, "wk-resolve-reason-agent", `{"kind":"notification"}`)
	m = resolveWakeViaTool(t, dm, agentOwned, "i-cleared-it-myself")
	require.Equal(t, false, m["success"])
	require.Equal(t, "invalid_reason", m["status"])
}

// TestHandleResolveWake_UnknownWakeStillRefused pins that the
// ownership change did not disturb the not-found path.
func TestHandleResolveWake_UnknownWakeStillRefused(t *testing.T) {
	dm := newTestSharedDM(t)

	m := resolveWakeViaTool(t, dm, "wk-does-not-exist", "reconciled")
	require.Equal(t, false, m["success"])
	require.Equal(t, "wake_not_found", m["status"])
	errMsg, ok := m["error"].(string)
	require.True(t, ok, "the refusal envelope must carry a string error field")
	require.Contains(t, errMsg, "not found")
}
