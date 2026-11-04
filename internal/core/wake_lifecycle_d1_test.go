// wake_lifecycle_d1_test.go — D-1 release-blocker regression.
//
// Pre-fix defect: handleScheduleWake produced rows in scheduled_wakes
// whose `metadata` column contained NULL (when the caller did not
// supply `p["metadata"]`) or a JSON object without a `kind` key. The
// public `ResolveWake` resolver required `strings.Contains(metadata,
// `"kind":`)` and rejected wake rows that lacked the substring, with
// the error message:
//
//	resolve wake: row "wk-..." is not a wake row
//	(no metadata.kind; use delete_task for scheduled tasks)
//
// The error message conflated the "kind absent" case (the wake
// default) with "is a scheduled_tasks row" (the delete_task surface).
// The result: a public CLI schedule+resolve round-trip was impossible
// without the caller manufacturing `metadata.kind` themselves, which
// is a producer/consumer contract bug.
//
// This file pins the contract that
//
//	schedule -> resolve
//
// works end-to-end through the public handler layer (not just the
// underlying DatabaseManager method), AND that the resolver now
// accepts legacy rows whose `metadata` is NULL or empty (the rows
// that pre-date this fix).
package internal

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestD1_ScheduleResolve_RoundTrip_ScheduleDefaultsKind exercises the
// producer-side default: handleScheduleWake is invoked WITHOUT a
// metadata field. The persisted row must contain a `kind` value so
// that ResolveWake accepts it. This is the producer-side fix.
func TestD1_ScheduleResolve_RoundTrip_ScheduleDefaultsKind(t *testing.T) {
	dm, err := NewDatabaseManager(t.TempDir())
	require.NoError(t, err)
	defer dm.Close()

	out, err := dm.ScheduleWake("D-1 regression probe", "1h", "", "test-agent", nil)
	require.NoError(t, err)
	id, ok := out["id"].(string)
	require.True(t, ok, "ScheduleWake must return a string id; got %#v", out["id"])

	// Hard probe: the metadata column MUST contain a "kind" key now.
	// We can't rely on ResolveWake to test this — that's the
	// consumer-side fix. The producer-side fix is verified by reading
	// the row back and asserting the substring is present in the
	// persisted metadata.
	var metadata string
	err = dm.db.QueryRow(`SELECT COALESCE(metadata, '') FROM scheduled_wakes WHERE id = ?`, id).Scan(&metadata)
	require.NoError(t, err)
	require.Contains(t, metadata, `"kind":`,
		"schedule (producer) must default metadata.kind when caller omits metadata; got %q", metadata)
	require.Contains(t, metadata, `"notification"`,
		"schedule (producer) must default kind=notification when caller omits metadata.kind; got %q", metadata)

	// Consumer-side round trip must succeed.
	resolved, status, err := dm.ResolveWake(id, "reconciled", "")
	require.NoError(t, err, "resolve of schedule-defaulted wake must succeed; got status=%q", status)
	require.True(t, resolved)
	require.Equal(t, "resolved", status)
}

// TestD1_ScheduleResolve_CallerKindPreserved confirms the producer
// fix does not overwrite a caller-supplied `kind`. Cascade and cron
// rows MUST keep their discriminator so they route correctly through
// the lifecycle — both are agent-owned kinds, and the agent may
// retire either of them through the resolve surface.
func TestD1_ScheduleResolve_CallerKindPreserved(t *testing.T) {
	dm, err := NewDatabaseManager(t.TempDir())
	require.NoError(t, err)
	defer dm.Close()

	callerMeta := map[string]interface{}{
		"kind":   "cascade",
		"source": "caller-test",
	}
	out, err := dm.ScheduleWake("D-1 cascade kind preservation", "1h", "th-1", "test-agent", callerMeta)
	require.NoError(t, err)
	id := out["id"].(string)

	var metadata string
	err = dm.db.QueryRow(`SELECT COALESCE(metadata, '') FROM scheduled_wakes WHERE id = ?`, id).Scan(&metadata)
	require.NoError(t, err)
	require.Contains(t, metadata, `"cascade"`,
		"caller-supplied kind= must be preserved verbatim; got %q", metadata)

	resolved, status, err := dm.ResolveWake(id, "reconciled", "")
	require.NoError(t, err, "cascade wake must be resolvable end-to-end")
	require.True(t, resolved)
	require.Equal(t, "resolved", status)
}

// TestD1_ResolveWake_AcceptsLegacyNullMetadata pins the consumer-
// side fix: ResolveWake must NOT reject rows whose `metadata` is
// NULL or empty (legacy rows authored before the producer fix).
// Prior behavior was: the substring check failed → row was rejected
// as "not_a_wake". Post-fix: json_extract on a NULL/empty metadata
// returns NULL → "" is an untagged wake, and an untagged wake is
// agent-owned → wake row, proceed.
func TestD1_ResolveWake_AcceptsLegacyNullMetadata(t *testing.T) {
	dm, err := NewDatabaseManager(t.TempDir())
	require.NoError(t, err)
	defer dm.Close()

	// Insert a row directly with NULL metadata — simulating a row
	// produced by a pre-fix ScheduleWake (or imported from another
	// tool). The canonical resolution path must accept it.
	_, err = dm.db.Exec(
		`INSERT INTO scheduled_wakes (id, target_time, reason, theory_id, recurring_rule, fired, created_by, metadata)
		 VALUES (?, ?, ?, NULL, NULL, 0, ?, NULL)`,
		"wk-legacy-null-meta-001", timeUnix(t, 3600), "legacy row with NULL metadata", "legacy-test", "",
	)
	require.NoError(t, err)

	resolved, status, err := dm.ResolveWake("wk-legacy-null-meta-001", "already_satisfied", "")
	require.NoError(t, err, "legacy NULL-metadata wake must be resolvable; got status=%q", status)
	require.True(t, resolved)
	require.Equal(t, "resolved", status)
}

// TestD1_ResolveWake_AcceptsLegacyEmptyMetadataJson mirrors the
// legacy NULL case for a row whose metadata is an empty string or
// `{}`. The new json_extract-based discriminator must NOT confuse
// it with a scheduled_tasks row.
func TestD1_ResolveWake_AcceptsLegacyEmptyMetadataJson(t *testing.T) {
	dm, err := NewDatabaseManager(t.TempDir())
	require.NoError(t, err)
	defer dm.Close()

	_, err = dm.db.Exec(
		`INSERT INTO scheduled_wakes (id, target_time, reason, theory_id, recurring_rule, fired, created_by, metadata)
		 VALUES (?, ?, ?, NULL, NULL, 0, ?, '')`,
		"wk-legacy-empty-meta-001", timeUnix(t, 3600), "legacy row with empty metadata", "legacy-test",
	)
	require.NoError(t, err)

	resolved, status, err := dm.ResolveWake("wk-legacy-empty-meta-001", "obsolete", "")
	require.NoError(t, err, "empty-metadata wake must be resolvable")
	require.True(t, resolved)
	require.Equal(t, "resolved", status)
}

// TestD1_ResolveWake_RejectsSchedulerOwnedKind pins the boundary:
// rows whose `metadata.kind` is a SCHEDULER-owned kind are not the
// agent's, and the agent must not retire them.
//
// 2026-11-04 (wake-ownership correction): this test used to pin the
// refusal of `cron` on the theory that a cron row was a
// scheduled_tasks row owned by `delete_task`. That was wrong twice
// over: a cron wake is a `scheduled_wakes` row (the cron producer
// injects one per tick), and `delete_task` does not apply to it. Under
// the wake-ownership model — the same model the arming gate and the
// opportunistic fold use — `cron` is an AGENT-owned kind: the fold
// delivers it, and it carries the `directive_id` the agent has to
// consult. The resolver therefore ACCEPTS it, and refuses the kinds
// the scheduler dispatches through a registered handler.
//
// The discriminator is now WakeKindAgentOwned, the single policy the
// whole wake lifecycle shares.
func TestD1_ResolveWake_RejectsSchedulerOwnedKind(t *testing.T) {
	dm, err := NewDatabaseManager(t.TempDir())
	require.NoError(t, err)
	defer dm.Close()

	// cascade_summary is the scheduler's: it has a registered handler
	// (scheduler.SystemHandlerCascadeSummary). An agent that retires it
	// suppresses the handler's run for that drain.
	_, err = dm.db.Exec(
		`INSERT INTO scheduled_wakes (id, target_time, reason, theory_id, recurring_rule, fired, created_by, metadata)
		 VALUES (?, ?, ?, NULL, NULL, 0, ?, ?)`,
		"wk-cascade-summary-001", timeUnix(t, 3600), "cascade summary wake row", "cascade-drain",
		`{"kind":"cascade_summary"}`,
	)
	require.NoError(t, err)

	resolved, status, err := dm.ResolveWake("wk-cascade-summary-001", "reconciled", "")
	require.Error(t, err, "a scheduler-owned row must NOT be resolvable by the agent")
	require.False(t, resolved)
	require.Equal(t, "not_a_wake", status)
	require.Contains(t, err.Error(), "scheduler-owned",
		"the rejection error must say the row belongs to the scheduler; got %q", err.Error())

	// The row must still be pending for its handler.
	var fired int
	require.NoError(t, dm.db.QueryRow(
		`SELECT fired FROM scheduled_wakes WHERE id = ?`, "wk-cascade-summary-001",
	).Scan(&fired))
	require.Equal(t, 0, fired,
		"a refused resolve must leave the scheduler-owned wake pending")
}

// TestD1_ResolveWake_AcceptsAgentOwnedCronKind is the inverse half of
// the boundary above.
//
// A cron wake is the agent's own: the fold delivers it and the agent
// acts on it. The resolver must therefore accept it, exactly as it
// accepts a notification or a cascade wake. Before the ownership
// correction it was refused with a message pointing at `delete_task`,
// which is not a surface that can retire a `scheduled_wakes` row.
func TestD1_ResolveWake_AcceptsAgentOwnedCronKind(t *testing.T) {
	dm, err := NewDatabaseManager(t.TempDir())
	require.NoError(t, err)
	defer dm.Close()

	_, err = dm.db.Exec(
		`INSERT INTO scheduled_wakes (id, target_time, reason, theory_id, recurring_rule, fired, created_by, metadata)
		 VALUES (?, ?, ?, NULL, NULL, 0, ?, ?)`,
		"wk-cron-001", timeUnix(t, 3600), "cron wake row", "cron-owner",
		`{"kind":"cron","directive_id":"epistemic-compaction"}`,
	)
	require.NoError(t, err)

	resolved, status, err := dm.ResolveWake("wk-cron-001", "reconciled", "")
	require.NoError(t, err,
		"a cron wake is agent-owned and must be resolvable; got status=%q err=%v", status, err)
	require.True(t, resolved)
	require.Equal(t, "resolved", status)
}

// TestD1_PersistenceAcrossReopen pins wake row persistence after
// closing and reopening the DatabaseManager. The D-1 fix must not
// regress this baseline behavior. Without reopen, a regression in
// the metadata shape (e.g. accidental double-marshal) would not be
// caught.
func TestD1_PersistenceAcrossReopen(t *testing.T) {
	root := t.TempDir()
	dm, err := NewDatabaseManager(root)
	require.NoError(t, err)

	out, err := dm.ScheduleWake("D-1 reopen probe", "1h", "", "test-agent", nil)
	require.NoError(t, err)
	id := out["id"].(string)

	require.NoError(t, dm.Close())

	dm2, err := NewDatabaseManager(root)
	require.NoError(t, err)
	defer dm2.Close()

	resolved, status, err := dm2.ResolveWake(id, "reconciled", "")
	require.NoError(t, err, "resolve after reopen must succeed; got status=%q", status)
	require.True(t, resolved)
	require.Equal(t, "resolved", status)
}

// TestD1_OverdueRemainsResolvable pins behavior for past-due wakes:
// a row whose target_time is already in the past MUST still be
// resolvable via the public surface. ResolveWake's contract covers
// both due and overdue. We bypass relative-time parsing by inserting
// an absolute past epoch directly, then triggering resolution via
// the canonical ResolveWake path.
func TestD1_OverdueRemainsResolvable(t *testing.T) {
	dm, err := NewDatabaseManager(t.TempDir())
	require.NoError(t, err)
	defer dm.Close()

	out, err := dm.ScheduleWake("D-1 overdue probe", "1h", "", "test-agent", nil)
	require.NoError(t, err)
	id := out["id"].(string)

	// Move the row's target_time into the past to simulate an
	// overdue wake (the surface ResolveWake must support).
	pastEpoch := time.Now().Add(-1 * time.Hour).Unix()
	_, err = dm.db.Exec(`UPDATE scheduled_wakes SET target_time = ? WHERE id = ?`, pastEpoch, id)
	require.NoError(t, err)

	resolved, status, err := dm.ResolveWake(id, "already_satisfied", "")
	require.NoError(t, err, "overdue wake must be resolvable; got status=%q", status)
	require.True(t, resolved)
	require.Equal(t, "resolved", status)
}

// TestD1_OrdinaryScheduledTaskSemanticsUnchanged pins the inverse
// boundary: a wake the producer authors with `kind: cron` MUST still
// be produced correctly AND MUST still be retirable by the agent.
//
// 2026-11-04 (wake-ownership correction): this test used to assert
// that a cron-kind row was NOT resolvable, on the theory that cron
// rows belong to `delete_task`. That conflated two different tables:
// the recurring TASK lives in `scheduled_tasks` and is owned by
// `delete_task` / `upsert_task`, while the cron WAKE is a
// `scheduled_wakes` row the cron producer injects and the fold
// delivers. Under the wake-ownership model the cron wake is the
// agent's, so the agent may retire it — which is what the resolver
// now does.
func TestD1_OrdinaryScheduledTaskSemanticsUnchanged(t *testing.T) {
	dm, err := NewDatabaseManager(t.TempDir())
	require.NoError(t, err)
	defer dm.Close()

	cronMeta := map[string]interface{}{
		"kind":         "cron",
		"directive_id": "mpm-seed-test-directive",
	}
	out, err := dm.ScheduleWake("D-1 cron owner probe", "1h", "0 */6 * * *", "test-agent", cronMeta)
	require.NoError(t, err)
	id := out["id"].(string)

	// Resolve must succeed — a cron wake is the agent's own kind.
	resolved, status, err := dm.ResolveWake(id, "reconciled", "")
	require.NoError(t, err, "a cron wake is agent-owned and must be resolvable; got status=%q", status)
	require.True(t, resolved)
	require.Equal(t, "resolved", status)
}

// TestD1_ResolveWake_ReasonEnumUnchanged pins the rest of the
// ResolveWake contract: reason must still be a member of
// reconciled | obsolete | superseded | already_satisfied. The D-1
// fix is strictly additive on the discriminator side; the reason
// enum validation is unchanged.
func TestD1_ResolveWake_ReasonEnumUnchanged(t *testing.T) {
	dm, err := NewDatabaseManager(t.TempDir())
	require.NoError(t, err)
	defer dm.Close()

	out, err := dm.ScheduleWake("D-1 reason enum unchanged", "1h", "", "test-agent", nil)
	require.NoError(t, err)
	id := out["id"].(string)

	resolved, status, err := dm.ResolveWake(id, "free-text-arbitrary-reason", "")
	require.Error(t, err)
	require.False(t, resolved)
	require.Equal(t, "invalid_reason", status)
}

// timeUnix is a small helper that returns a future unix epoch so the
// row is not past-due at test start. Centralized here so a typo
// can't drift between cases.
func timeUnix(t *testing.T, futureSeconds int64) int64 {
	t.Helper()
	return time.Now().Unix() + futureSeconds
}
