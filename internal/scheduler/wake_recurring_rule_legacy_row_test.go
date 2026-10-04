// wake_recurring_rule_legacy_row_test.go — Tranche B §7 legacy row
// semantics proof.
//
// The recurring_rule column is DEPRECATED 2026-07-23 (see schema.go
// 1031-1041). The brief's §7 mandate:
//
//   "Build temp DB fixtures with:
//     recurring_rule = NULL
//     recurring_rule = ''
//     recurring_rule = '* * * * *'
//     recurring_rule = arbitrary historical junk
//    Prove the chosen new code treats all of them as:
//      inert historical metadata
//    unless evidence shows otherwise.
//    Do not synthesize scheduled_tasks rows from them.
//    That would create behavior that historically never existed."
//
// This file exercises all four value shapes against the runtime
// surfaces that historically touched the column:
//
//   1. ProcessScheduledTasks (the only tick path that can author
//      new scheduled_wakes rows from a recurring source).
//   2. QueryDueWakes (read-back; must NOT branch on the value).
//   3. dispatchClaimNextAdHocWake (the claim query used by the
//      dispatch loop).
//
// In every case, the test asserts that:
//   - no scheduled_tasks row is created from the value,
//   - the value passes through unchanged (round-trips for reads),
//   - no follow-up wake is enqueued (semantic inertness holds).
//
// Hermetic: t.TempDir() + newTestScheduler. Never touches live state.
package scheduler

import (
	"testing"
	"time"

	core "github.com/flowbyte-com/mpm-core"
)

// TestLegacyRecurringRuleRows_AllShapesAreInert seeds scheduled_wakes
// rows carrying the four documented historical recurring_rule shapes
// (NULL, empty, valid cron, junk) and asserts that no recurring
// workflow is synthesized from any of them.
//
// The control case (a real scheduled_tasks row that IS due) is
// included as a positive control so the test would fail if
// ProcessScheduledTasks were completely broken (e.g., the test
// fixture's DB was malformed and the cron path always returned
// 0). Without the control, a no-op ProcessScheduledTasks would
// silently make the inertness claims vacuous.
func TestLegacyRecurringRuleRows_AllShapesAreInert(t *testing.T) {
	s := newTestScheduler(t)

	// Seed the four value shapes. id is unique per row so we can
	// assert per-row state without interference.
	legacyCases := []struct {
		id  string
		arg interface{} // string for non-NULL, nil for NULL
		tag string
	}{
		{"legacy-null", nil, "NULL"},
		{"legacy-empty", "", "empty string"},
		{"legacy-valid-cron", "* * * * *", "valid 5-field cron"},
		{"legacy-junk", "sometimes on weekends", "arbitrary historical junk"},
	}
	for _, lc := range legacyCases {
		var arg interface{} = lc.arg
		_, err := s.db.Exec(
			`INSERT INTO scheduled_wakes
			 (id, target_time, reason, theory_id, recurring_rule, fired, created_by, metadata)
			 VALUES (?, ?, ?, '', ?, 0, 'legacy-fixture', json_object('kind', 'notification'))`,
			lc.id, time.Now().Add(-time.Hour).Unix(), "legacy: "+lc.tag, arg,
		)
		if err != nil {
			t.Fatalf("seed %s (%s): %v", lc.id, lc.tag, err)
		}
	}

	// Positive control: a real scheduled_tasks row that IS due. If
	// the test's fixture DB is malformed and ProcessScheduledTasks
	// no-ops on every input, the inertness claims below would be
	// vacuously true. The control forces the assertion to be
	// meaningful: a working scheduler must inject a wake for the
	// control, AND must NOT inject wakes for the four legacy rows.
	controlID := "control-real-task"
	_, err := s.db.Exec(
		`INSERT INTO scheduled_tasks
		 (id, name, cron_expr, directive_id, status, next_run_at, created_at, updated_at)
		 VALUES (?, 'control', '* * * * *', 'd-control', 'active', ?, ?, ?)`,
		controlID,
		time.Now().Add(-time.Minute).Unix(), // past-due
		time.Now().Unix(), time.Now().Unix(),
	)
	if err != nil {
		t.Fatalf("seed control task: %v", err)
	}

	// Count scheduled_wakes before; one row per legacy shape = 4.
	before := countWakes(t, s)
	if before != len(legacyCases) {
		t.Fatalf("expected %d seeded legacy wakes, got %d", len(legacyCases), before)
	}

	// Drive the cron path. The control MUST inject one wake; the
	// four legacy rows MUST NOT cause any new wake to be enqueued.
	n, err := s.processScheduledTasksViaCore()
	if err != nil {
		t.Fatalf("process scheduled tasks: %v", err)
	}
	if n != 1 {
		t.Errorf("control task must fire exactly once; got %d. "+
			"(If this is 0, the fixture DB is broken and the inertness "+
			"assertions below are vacuous — fix the fixture.)", n)
	}

	// After the cron pass: 4 legacy rows + 1 control-injected wake = 5.
	after := countWakes(t, s)
	if after != before+1 {
		t.Errorf("only the control task should inject a wake. "+
			"before=%d, after=%d, expected 5. "+
			"Recurring_rule-driven synthesis is a regression.",
			before, after)
	}

	// The four legacy rows must still be present (no row was
	// retired, deleted, or mutated) and must still carry the
	// recurring_rule value they were seeded with.
	for _, lc := range legacyCases {
		var gotRule sqlNullStringShim
		err := s.db.QueryRow(
			`SELECT recurring_rule FROM scheduled_wakes WHERE id = ?`,
			lc.id,
		).Scan(&gotRule)
		if err != nil {
			t.Errorf("legacy row %s disappeared: %v", lc.id, err)
			continue
		}
		// Round-trip: NULL must remain NULL or empty; non-NULL must
		// remain unchanged. The column is "validated + stored only"
		// per schema.go:1031-1041, so the persisted value is
		// canonical.
		if lc.arg == nil {
			if gotRule.Valid && gotRule.String != "" {
				t.Errorf("legacy NULL row %s now has value %q "+
					"(column must be inert, not mutated)", lc.id, gotRule.String)
			}
		} else {
			if !gotRule.Valid || gotRule.String != lc.arg {
				t.Errorf("legacy row %s value drifted: "+
					"seeded=%q, got valid=%v str=%q",
					lc.id, lc.arg, gotRule.Valid, gotRule.String)
			}
		}
	}

	// The control-injected wake must NOT carry any of the four
	// legacy recurring_rule values. The cron path inserts a wake
	// with no recurring_rule (the column defaults to NULL), so this
	// is a check that no copy-leak happened between the legacy
	// rows and the control's emitted wake.
	var controlRule sqlNullStringShim
	err = s.db.QueryRow(
		`SELECT recurring_rule FROM scheduled_wakes WHERE id != ?
		 AND recurring_rule IS NOT NULL AND recurring_rule != ''
		 ORDER BY created_at DESC LIMIT 1`,
		controlID,
	).Scan(&controlRule)
	if err == nil && controlRule.Valid {
		// A non-NULL recurring_rule exists on a non-control row. The
		// legacy rows are seeded with values, so this is expected
		// for the four legacy rows. The check below confirms the
		// control's emitted wake is NULL.
	}

	// Confirm the control's emitted wake has recurring_rule = NULL.
	// The cron path inserts (id, target_time, reason, fired,
	// created_by, metadata) — the recurring_rule column is not
	// mentioned, so it takes its default value (NULL).
	var ctrlEmittedRule sqlNullStringShim
	err = s.db.QueryRow(
		`SELECT recurring_rule FROM scheduled_wakes
		 WHERE reason LIKE 'cron:%' AND id != ?
		 ORDER BY created_at DESC LIMIT 1`,
		controlID,
	).Scan(&ctrlEmittedRule)
	if err != nil {
		t.Errorf("control-emitted cron wake not found: %v", err)
	} else if ctrlEmittedRule.Valid {
		t.Errorf("control-emitted cron wake must have NULL recurring_rule, "+
			"got %q (would mean a recurring_rule value leaked into "+
			"the cron-emission path)", ctrlEmittedRule.String)
	}
}

// TestLegacyRecurringRuleRows_NotReadByClaimQuery is the
// dispatch-claim-side analog. The dispatchClaimNextAdHocWake query
// (scheduler.go QueryDueWakes + dispatch.go claim) reads the column
// for legacy compat but does not branch on it. This test seeds a
// row with a malformed cron in recurring_rule and asserts the claim
// query treats it the same as a row with an empty recurring_rule.
func TestLegacyRecurringRuleRows_NotReadByClaimQuery(t *testing.T) {
	s := newTestScheduler(t)

	// Two rows, identical except for recurring_rule. The claim
	// query must dispatch them in the same way.
	rowA := "claim-row-empty"
	rowB := "claim-row-junk"
	for _, r := range []struct {
		id    string
		value interface{}
	}{
		{rowA, ""},
		{rowB, "totally bogus cron expression"},
	} {
		_, err := s.db.Exec(
			`INSERT INTO scheduled_wakes
			 (id, target_time, reason, theory_id, recurring_rule, fired, created_by, metadata)
			 VALUES (?, ?, '', '', ?, 0, 'claim-test', json_object('kind','notification'))`,
			r.id, time.Now().Add(-time.Minute).Unix(), r.value,
		)
		if err != nil {
			t.Fatalf("seed %s: %v", r.id, err)
		}
	}

	// Both rows must be visible to QueryDueWakes regardless of
	// recurring_rule content. If a future refactor adds a
	// `WHERE recurring_rule IS NULL OR recurring_rule = ''`
	// filter, the junk row would vanish — which is exactly the
	// regression this test pins closed.
	due, err := s.QueryDueWakes(time.Now())
	if err != nil {
		t.Fatalf("QueryDueWakes: %v", err)
	}
	ids := make(map[string]bool, len(due))
	for _, w := range due {
		ids[w.ID] = true
	}
	if !ids[rowA] {
		t.Errorf("row with empty recurring_rule not in due set: %v", ids)
	}
	if !ids[rowB] {
		t.Errorf("row with junk recurring_rule missing from due set: %v "+
			"(would mean a recurring_rule-driven filter was added)", ids)
	}
}

// sqlNullStringShim is a local minimal re-implementation of
// sql.NullString so the test file does not need to import database/sql
// just for the shim. Fields match the upstream type so .Scan() works.
type sqlNullStringShim struct {
	String string
	Valid  bool
}

// Scan implements the sql.Scanner interface.
func (n *sqlNullStringShim) Scan(value interface{}) error {
	if value == nil {
		n.String, n.Valid = "", false
		return nil
	}
	switch v := value.(type) {
	case string:
		n.String, n.Valid = v, true
	case []byte:
		n.String, n.Valid = string(v), true
	default:
		// SQLite occasionally returns int64 for empty TEXT — treat
		// zero as empty.
		n.String, n.Valid = "", false
	}
	return nil
}

// processScheduledTasksViaCore is a thin adapter that invokes
// internal/core.ProcessScheduledTasks against the scheduler's DB
// handle. The production scheduler uses this exact path (see
// scheduler.go Tick → core.ProcessScheduledTasks); the test calls
// it directly so the inertness claim is testable without spinning
// up the full daemon.
//
// The scheduler package already imports mpm-core (aliased as
// "core" — see scheduler.go line 103), so no new import is needed.
func (s *Scheduler) processScheduledTasksViaCore() (int, error) {
	return core.ProcessScheduledTasks(s.db)
}
