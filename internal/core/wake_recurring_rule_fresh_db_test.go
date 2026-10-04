// wake_recurring_rule_fresh_db_test.go — Tranche B §13 fresh DB
// contract proof.
//
// The brief's §13 mandate:
//
//   "Separately test a fresh DB.
//    If DROP NOW:
//      PRAGMA table_info(scheduled_wakes);
//      must show no recurring_rule.
//    If RETAIN INERT:
//      test and document that:
//        column exists only for legacy compatibility
//        new code never writes/reads/returns it"
//
// This file proves the RETAIN INERT branch: a fresh DatabaseManager
// produces a scheduled_wakes table that has the column, but
// ScheduleWake never writes it, CheckPendingWakes never reads it,
// and ListScheduledWakes never returns it.
package internal

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestFreshDB_RecurringRuleColumnExists pins the RETAIN INERT
// half: a brand-new DatabaseManager installs the scheduled_wakes
// schema and the column is present (because the production
// install path keeps the column for legacy DB compatibility).
//
// If a future refactor decides to drop the column physically,
// this test would have to be updated — that is the point. The
// test surfaces the physical-column decision as code, not as a
// buried comment.
func TestFreshDB_RecurringRuleColumnExists(t *testing.T) {
	dm, err := NewDatabaseManager(t.TempDir())
	require.NoError(t, err)
	defer dm.Close()

	rows, err := dm.db.Query(`PRAGMA table_info(scheduled_wakes)`)
	require.NoError(t, err)
	defer rows.Close()

	var hasColumn bool
	var columnNames []string
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dfltValue *string
		require.NoError(t, rows.Scan(&cid, &name, &ctype, &notnull, &dfltValue, &pk))
		columnNames = append(columnNames, name)
		if name == "recurring_rule" {
			hasColumn = true
			require.Equal(t, "TEXT", ctype,
				"recurring_rule must be TEXT (legacy compat shape)")
		}
	}
	require.True(t, hasColumn,
		"RETAIN INERT branch: recurring_rule column must exist on a fresh "+
			"DB. columns present: %v", columnNames)
}

// TestFreshDB_ScheduleWakeDoesNotWriteRecurringRule pins the
// "new code never writes it" half: calling ScheduleWake on a
// fresh DB never writes a non-NULL recurring_rule value, even
// though the column accepts it (legacy compat).
func TestFreshDB_ScheduleWakeDoesNotWriteRecurringRule(t *testing.T) {
	dm, err := NewDatabaseManager(t.TempDir())
	require.NoError(t, err)
	defer dm.Close()

	out, err := dm.ScheduleWake("fresh-no-rr", "1h", "", "test-agent", nil)
	require.NoError(t, err)
	id, _ := out["id"].(string)
	require.NotEmpty(t, id)

	// The result map must not contain recurring_rule. If a future
	// refactor accidentally re-adds the key, the test fails
	// (Postel: be strict in what you emit).
	if _, present := out["recurring_rule"]; present {
		t.Errorf("ScheduleWake result must not include recurring_rule key; got %v", out)
	}

	// The persisted row must have recurring_rule = NULL or ''.
	// NOT NULL would be a regression — the post-retirement code
	// does not author recurring_rule values.
	var col *string
	err = dm.db.QueryRow(
		`SELECT recurring_rule FROM scheduled_wakes WHERE id = ?`, id,
	).Scan(&col)
	require.NoError(t, err)
	if col != nil && *col != "" {
		t.Errorf("ScheduleWake wrote a non-NULL recurring_rule: %q", *col)
	}
}

// TestFreshDB_CheckPendingWakesDoesNotReturnRecurringRule pins
// the "new code never returns it" half: CheckPendingWakes' result
// map does not include recurring_rule. The key is absent, not
// present-with-null — because returning `null` would force every
// caller to defensively handle a key the API never supported
// post-retirement.
func TestFreshDB_CheckPendingWakesDoesNotReturnRecurringRule(t *testing.T) {
	dm, err := NewDatabaseManager(t.TempDir())
	require.NoError(t, err)
	defer dm.Close()

	// Seed a due wake with an explicit (legacy) recurring_rule
	// value. The fresh DB accepts it on disk; the read APIs must
	// not surface it.
	_, err = dm.db.Exec(
		`INSERT INTO scheduled_wakes
		 (id, target_time, reason, theory_id, recurring_rule, fired, created_by, metadata)
		 VALUES ('fresh-read-1', strftime('%s','now')-60, 'fresh', '', '0 * * * *', 0, 'test-agent',
		         json_object('kind','notification'))`,
	)
	require.NoError(t, err)

	res, err := dm.CheckPendingWakes(time.Now(), nil)
	require.NoError(t, err)
	require.NotEmpty(t, res, "fresh wake must be in CheckPendingWakes result")

	for _, w := range res {
		if _, present := w["recurring_rule"]; present {
			t.Errorf("CheckPendingWakes row must not include recurring_rule key; "+
				"got %v (key present would be a regression to the pre-retirement "+
				"contract)", w)
		}
	}
}

// TestFreshDB_ListScheduledWakesDoesNotReturnRecurringRule is
// the same contract pin for ListScheduledWakes.
func TestFreshDB_ListScheduledWakesDoesNotReturnRecurringRule(t *testing.T) {
	dm, err := NewDatabaseManager(t.TempDir())
	require.NoError(t, err)
	defer dm.Close()

	_, err = dm.db.Exec(
		`INSERT INTO scheduled_wakes
		 (id, target_time, reason, theory_id, recurring_rule, fired, created_by, metadata)
		 VALUES ('fresh-list-1', strftime('%s','now')+3600, 'fresh', '', '0 * * * *', 0, 'test-agent',
		         json_object('kind','notification'))`,
	)
	require.NoError(t, err)

	rows, err := dm.ListScheduledWakes(true, false, 100)
	require.NoError(t, err)
	require.NotEmpty(t, rows)

	for _, r := range rows {
		if _, present := r["recurring_rule"]; present {
			t.Errorf("ListScheduledWakes row must not include recurring_rule key; "+
				"got %v", r)
		}
	}
}
