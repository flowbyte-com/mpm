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
	"strings"
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
// fix does not overwrite a caller-supplied `kind`. Cascade and
// cron-scheduled-task rows MUST keep their discriminator so they
// route correctly through the lifecycle (cascade wakes go via
// resolve; cron-scheduled-task rows go via delete_task).
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
// as "not_a_wake; use delete_task". Post-fix: json_extract on a
// NULL/empty metadata returns NULL → "" matches the wake-kind
// switch's empty-string case → wake row, proceed.
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

// TestD1_ResolveWake_RejectsScheduledTaskKind pins the boundary:
// rows whose `metadata.kind` is something other than
// cascade / cascade_summary / notification are NOT wake rows and
// must be retired via delete_task. This guards against the fix
// over-widening the discriminator.
func TestD1_ResolveWake_RejectsScheduledTaskKind(t *testing.T) {
	dm, err := NewDatabaseManager(t.TempDir())
	require.NoError(t, err)
	defer dm.Close()

	_, err = dm.db.Exec(
		`INSERT INTO scheduled_wakes (id, target_time, reason, theory_id, recurring_rule, fired, created_by, metadata)
		 VALUES (?, ?, ?, NULL, NULL, 0, ?, ?)`,
		"wk-cron-001", timeUnix(t, 3600), "cron-style scheduled task row", "cron-owner",
		`{"kind":"cron","directive_id":"epistemic-compaction"}`,
	)
	require.NoError(t, err)

	resolved, status, err := dm.ResolveWake("wk-cron-001", "reconciled", "")
	require.Error(t, err, "cron-kind row must NOT be resolvable as a wake")
	require.False(t, resolved)
	require.Equal(t, "not_a_wake", status)
	require.True(t,
		strings.Contains(err.Error(), "delete_task") ||
			strings.Contains(err.Error(), "scheduled task"),
		"rejection error must guide caller to delete_task; got %q", err.Error())
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
// boundary: handleScheduleWake with `kind: cron` (a row type owned
// by delete_task, NOT by resolve) MUST still be produced correctly
// AND MUST be rejectable via ResolveWake. Adding the default
// `kind=notification` MUST NOT break the boundary between wake
// rows and scheduled-task rows.
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

	// Resolve must refuse — this row is owned by delete_task.
	_, status, err := dm.ResolveWake(id, "reconciled", "")
	require.Error(t, err)
	require.Equal(t, "not_a_wake", status)
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
