// scheduled_tasks_test.go — round-trip tests for Agentic Cron (Phase 5b).
//
// Coverage:
//   - CalculateNextRun: standard 5-field parsing, invalid expressions,
//     next-occurrence math
//   - UpsertScheduledTask: insert, update preserves last_run_at and
//     created_at, validation (status enum, required fields, invalid cron)
//   - ProcessDueTasks: atomic injection + rollover; poison-pill cron
//     pauses the task; nothing is processed when no tasks are due
//   - ListScheduledTasks: ordering by next_run_at; nullable last_run_at
//
// Test isolation: tests share the live local DB at $HOME/.mpm/ (the
// projectRoot argument is ignored by NewDatabaseManager; only MPM_SHARED_DB
// is redirected to a tmpdir). Each test cleans both scheduled_wakes and
// scheduled_tasks in t.Cleanup so accumulated rows from prior runs don't
// trigger UNIQUE constraint conflicts on re-run.

package internal

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// newScheduledTaskDM returns a DM with scheduled_tasks + scheduled_wakes
// wiped before AND after the test. Use this instead of newTestWakeDM for
// any test that touches scheduled_tasks.
func newScheduledTaskDM(t *testing.T) *DatabaseManager {
	t.Helper()
	dm := newTestWakeDM(t)
	// Belt-and-braces: wipe scheduled_tasks at start AND register
	// cleanup at end. The first wipe handles accumulated state from
	// prior tests in this run; the cleanup handles the inverse case
	// (rows this test inserts and we want gone before the next test).
	_, _ = dm.db.Exec(`DELETE FROM scheduled_tasks`)
	t.Cleanup(func() {
		_, _ = dm.db.Exec(`DELETE FROM scheduled_tasks`)
	})
	return dm
}

// ── CalculateNextRun tests ─────────────────────────────────────────────

func TestCalculateNextRun_ValidExpressions(t *testing.T) {
	cases := []struct {
		expr string
		from time.Time
		want time.Time
	}{
		// Daily at 03:00 — next 03:00 after 2026-07-23 14:00 UTC is 2026-07-24 03:00
		{"0 3 * * *", time.Date(2026, 7, 23, 14, 0, 0, 0, time.UTC),
			time.Date(2026, 7, 24, 3, 0, 0, 0, time.UTC)},
		// Weekly Monday at midnight — 2026-07-23 is Thursday, next Monday is 2026-07-27
		{"0 0 * * 1", time.Date(2026, 7, 23, 14, 0, 0, 0, time.UTC),
			time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC)},
		// Every minute — should be from + 1 minute
		{"* * * * *", time.Date(2026, 7, 23, 14, 0, 0, 0, time.UTC),
			time.Date(2026, 7, 23, 14, 1, 0, 0, time.UTC)},
	}
	for _, tc := range cases {
		got, err := CalculateNextRun(tc.expr, tc.from)
		if err != nil {
			t.Errorf("CalculateNextRun(%q): unexpected error: %v", tc.expr, err)
			continue
		}
		if !got.Equal(tc.want) {
			t.Errorf("CalculateNextRun(%q) from %v: got %v, want %v",
				tc.expr, tc.from, got, tc.want)
		}
	}
}

func TestCalculateNextRun_InvalidExpressions(t *testing.T) {
	bad := []string{
		"",                  // empty
		"not a cron expr",   // garbage
		"60 0 * * *",        // minute out of range
		"0 25 * * *",        // hour out of range
		"0 0 32 * *",        // day-of-month out of range
		"* * *",             // too few fields
		"* * * * * * *",     // too many fields
	}
	from := time.Date(2026, 7, 23, 14, 0, 0, 0, time.UTC)
	for _, expr := range bad {
		_, err := CalculateNextRun(expr, from)
		if err == nil {
			t.Errorf("CalculateNextRun(%q): expected error, got nil", expr)
		}
	}
}

// ── UpsertScheduledTask tests ─────────────────────────────────────────

func TestUpsertScheduledTask_NewInsert(t *testing.T) {
	dm := newScheduledTaskDM(t)
	task := ScheduledTask{
		ID:          "epistemic-compaction",
		Name:        "Nightly epistemic compaction",
		CronExpr:    "0 3 * * *",
		DirectiveID: "mpm-seed-epistemic-compaction",
		Status:      ScheduledTaskActive,
	}
	if err := dm.UpsertScheduledTask(task); err != nil {
		t.Fatalf("UpsertScheduledTask: %v", err)
	}

	// Verify row exists with correct fields
	var (
		gotName, gotCron, gotDirective, gotStatus string
		nextRun                                    time.Time
	)
	err := dm.db.QueryRow(`
		SELECT name, cron_expr, directive_id, status, next_run_at
		FROM scheduled_tasks WHERE id = ?
	`, task.ID).Scan(&gotName, &gotCron, &gotDirective, &gotStatus, &nextRun)
	if err != nil {
		t.Fatalf("QueryRow: %v", err)
	}
	if gotName != task.Name || gotCron != task.CronExpr || gotDirective != task.DirectiveID || gotStatus != task.Status {
		t.Errorf("row mismatch: got name=%q cron=%q directive=%q status=%q",
			gotName, gotCron, gotDirective, gotStatus)
	}
	if nextRun.Before(time.Now()) {
		t.Errorf("next_run_at should be in the future, got %v", nextRun)
	}
}

func TestUpsertScheduledTask_UpdatePreservesLastRunAndCreated(t *testing.T) {
	dm := newScheduledTaskDM(t)
	original := ScheduledTask{
		ID: "weekly-audit", Name: "Weekly audit", CronExpr: "0 0 * * 1",
		DirectiveID: "mpm-seed-audit", Status: ScheduledTaskActive,
	}
	if err := dm.UpsertScheduledTask(original); err != nil {
		t.Fatalf("initial upsert: %v", err)
	}

	// Capture the original created_at + last_run_at
	var origCreated, origNextRun time.Time
	var origLastRun *time.Time
	err := dm.db.QueryRow(`
		SELECT created_at, last_run_at, next_run_at FROM scheduled_tasks WHERE id = ?
	`, original.ID).Scan(&origCreated, &origLastRun, &origNextRun)
	if err != nil {
		t.Fatalf("QueryRow: %v", err)
	}
	if origLastRun != nil {
		t.Errorf("fresh insert should have NULL last_run_at, got %v", *origLastRun)
	}

	// Sleep to ensure timestamps would diverge if updated
	time.Sleep(50 * time.Millisecond)

	// Re-upsert with same id but different name and a paused status
	updated := original
	updated.Name = "Weekly audit (v2)"
	updated.Status = ScheduledTaskPaused
	if err := dm.UpsertScheduledTask(updated); err != nil {
		t.Fatalf("update upsert: %v", err)
	}

	// Verify created_at preserved, last_run_at still NULL, status updated
	var newCreated, newNextRun time.Time
	var newLastRun *time.Time
	var newStatus, newName string
	err = dm.db.QueryRow(`
		SELECT created_at, last_run_at, next_run_at, status, name FROM scheduled_tasks WHERE id = ?
	`, original.ID).Scan(&newCreated, &newLastRun, &newNextRun, &newStatus, &newName)
	if err != nil {
		t.Fatalf("QueryRow after update: %v", err)
	}
	if !newCreated.Equal(origCreated) {
		t.Errorf("created_at should be preserved: got %v, want %v", newCreated, origCreated)
	}
	if newLastRun != nil {
		t.Errorf("last_run_at should remain NULL until first process: got %v", *newLastRun)
	}
	if newStatus != ScheduledTaskPaused {
		t.Errorf("status should be updated to paused, got %q", newStatus)
	}
	if newName != updated.Name {
		t.Errorf("name should be updated to %q, got %q", updated.Name, newName)
	}
}

func TestUpsertScheduledTask_Validation(t *testing.T) {
	dm := newScheduledTaskDM(t)
	base := ScheduledTask{
		ID: "x", Name: "x", CronExpr: "0 0 * * *",
		DirectiveID: "mpm-seed-x", Status: ScheduledTaskActive,
	}
	cases := []struct {
		name string
		task ScheduledTask
	}{
		{"empty id", ScheduledTask{Name: "x", CronExpr: "0 0 * * *", DirectiveID: "d", Status: ScheduledTaskActive}},
		{"empty name", ScheduledTask{ID: "x", CronExpr: "0 0 * * *", DirectiveID: "d", Status: ScheduledTaskActive}},
		{"empty cron", ScheduledTask{ID: "x", Name: "x", DirectiveID: "d", Status: ScheduledTaskActive}},
		{"empty directive", ScheduledTask{ID: "x", Name: "x", CronExpr: "0 0 * * *", Status: ScheduledTaskActive}},
		{"invalid status", ScheduledTask{ID: "x", Name: "x", CronExpr: "0 0 * * *", DirectiveID: "d", Status: "broken"}},
		{"invalid cron", ScheduledTask{ID: "x", Name: "x", CronExpr: "not a cron", DirectiveID: "d", Status: ScheduledTaskActive}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := dm.UpsertScheduledTask(tc.task); err == nil {
				t.Errorf("expected error for %s, got nil", tc.name)
			}
		})
	}
	_ = base
}

// ── ProcessDueTasks tests ─────────────────────────────────────────────

func TestProcessDueTasks_NothingDue(t *testing.T) {
	dm := newScheduledTaskDM(t)
	// No tasks at all → should process 0 with no error
	n, err := dm.ProcessDueTasks()
	if err != nil {
		t.Fatalf("ProcessDueTasks: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 processed, got %d", n)
	}
}

func TestProcessDueTasks_InjectsAndRollsOver(t *testing.T) {
	dm := newScheduledTaskDM(t)

	// Insert a task with next_run_at in the past so it's immediately due.
	// We bypass UpsertScheduledTask's cron-validation by writing directly,
	// since we want a controllable due-time independent of cron parsing.
	past := time.Now().UTC().Add(-1 * time.Minute)
	_, err := dm.db.Exec(`
		INSERT INTO scheduled_tasks
		(id, name, cron_expr, directive_id, status, next_run_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, "test-cron", "Test cron", "0 3 * * *", "mpm-seed-test", ScheduledTaskActive,
		past, past, past)
	if err != nil {
		t.Fatalf("seed task: %v", err)
	}

	beforeWakeCount := countWakes(t, dm)
	n, err := dm.ProcessDueTasks()
	if err != nil {
		t.Fatalf("ProcessDueTasks: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 task processed, got %d", n)
	}

	// A wake should have been injected
	afterWakeCount := countWakes(t, dm)
	if afterWakeCount != beforeWakeCount+1 {
		t.Errorf("expected wake count delta +1, got before=%d after=%d", beforeWakeCount, afterWakeCount)
	}

	// The wake should have reason="cron:test-cron" and metadata with directive_id
	var reason string
	var metadataJSON string
	err = dm.db.QueryRow(`
		SELECT reason, metadata FROM scheduled_wakes
		WHERE created_by = ? ORDER BY rowid DESC LIMIT 1
	`, CronCreatedBy).Scan(&reason, &metadataJSON)
	if err != nil {
		t.Fatalf("QueryRow for wake: %v", err)
	}
	if !strings.HasPrefix(reason, CronWakePrefix) || !strings.Contains(reason, "test-cron") {
		t.Errorf("wake reason should start with cron: and reference task id, got %q", reason)
	}
	var meta map[string]interface{}
	if err := json.Unmarshal([]byte(metadataJSON), &meta); err != nil {
		t.Fatalf("metadata JSON parse: %v", err)
	}
	if meta["source"] != CronSource {
		t.Errorf("metadata.source should be %q, got %v", CronSource, meta["source"])
	}
	if meta["directive_id"] != "mpm-seed-test" {
		t.Errorf("metadata.directive_id should be mpm-seed-test, got %v", meta["directive_id"])
	}

	// next_run_at should now be in the future (rolled over to next 03:00)
	var newNextRun time.Time
	var lastRun *time.Time
	err = dm.db.QueryRow(`
		SELECT next_run_at, last_run_at FROM scheduled_tasks WHERE id = ?
	`, "test-cron").Scan(&newNextRun, &lastRun)
	if err != nil {
		t.Fatalf("QueryRow for task: %v", err)
	}
	if !newNextRun.After(time.Now().UTC()) {
		t.Errorf("next_run_at should be in future after rollover, got %v", newNextRun)
	}
	if lastRun == nil {
		t.Errorf("last_run_at should be set after first process")
	}
}

func TestProcessDueTasks_PausesOnPoisonCron(t *testing.T) {
	dm := newScheduledTaskDM(t)
	// Insert with valid cron, then corrupt the cron_expr to something invalid.
	// ProcessDueTasks should pause the task rather than infinite-loop.
	past := time.Now().UTC().Add(-1 * time.Minute)
	_, err := dm.db.Exec(`
		INSERT INTO scheduled_tasks
		(id, name, cron_expr, directive_id, status, next_run_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, "poison", "Poison", "0 3 * * *", "mpm-seed-p", ScheduledTaskActive,
		past, past, past)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := dm.db.Exec(`UPDATE scheduled_tasks SET cron_expr = ? WHERE id = ?`,
		"this is not cron", "poison"); err != nil {
		t.Fatalf("corrupt cron: %v", err)
	}

	n, err := dm.ProcessDueTasks()
	if err != nil {
		t.Fatalf("ProcessDueTasks: %v", err)
	}
	if n != 0 {
		t.Errorf("poison task should not be counted as processed, got %d", n)
	}

	var status string
	err = dm.db.QueryRow(`SELECT status FROM scheduled_tasks WHERE id = ?`, "poison").Scan(&status)
	if err != nil {
		t.Fatalf("QueryRow: %v", err)
	}
	if status != ScheduledTaskPaused {
		t.Errorf("poison task should be paused, got %q", status)
	}
}

func TestProcessDueTasks_SkipsPaused(t *testing.T) {
	dm := newScheduledTaskDM(t)
	past := time.Now().UTC().Add(-1 * time.Minute)
	_, err := dm.db.Exec(`
		INSERT INTO scheduled_tasks
		(id, name, cron_expr, directive_id, status, next_run_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, "paused-task", "Paused", "0 3 * * *", "mpm-seed-pp", ScheduledTaskPaused,
		past, past, past)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	n, err := dm.ProcessDueTasks()
	if err != nil {
		t.Fatalf("ProcessDueTasks: %v", err)
	}
	if n != 0 {
		t.Errorf("paused task should not be processed, got %d", n)
	}
}

// ── ListScheduledTasks tests ──────────────────────────────────────────

func TestListScheduledTasks_OrdersByNextRun(t *testing.T) {
	dm := newScheduledTaskDM(t)
	now := time.Now().UTC()
	// Direct INSERT — we want explicit next_run_at values to test the
	// ORDER BY clause. UpsertScheduledTask always recomputes from cron,
	// so two tasks with the same cron would tie on next_run_at.
	rows := []struct {
		id        string
		nextRunAt time.Time
	}{
		{"later", now.Add(2 * time.Hour)},
		{"sooner", now.Add(1 * time.Hour)},
	}
	for _, r := range rows {
		if _, err := dm.db.Exec(`
			INSERT INTO scheduled_tasks
			(id, name, cron_expr, directive_id, status, next_run_at, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		`, r.id, r.id, "0 3 * * *", "d", ScheduledTaskActive,
			r.nextRunAt, now, now); err != nil {
			t.Fatalf("seed %s: %v", r.id, err)
		}
	}
	tasks, err := dm.ListScheduledTasks()
	if err != nil {
		t.Fatalf("ListScheduledTasks: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("expected 2 tasks, got %d", len(tasks))
	}
	if tasks[0].ID != "sooner" || tasks[1].ID != "later" {
		t.Errorf("ordering wrong: got [%s, %s], want [sooner, later]", tasks[0].ID, tasks[1].ID)
	}
}

// ── helpers ───────────────────────────────────────────────────────────

func countWakes(t *testing.T, dm *DatabaseManager) int {
	t.Helper()
	var n int
	if err := dm.db.QueryRow(`SELECT COUNT(*) FROM scheduled_wakes`).Scan(&n); err != nil {
		t.Fatalf("count wakes: %v", err)
	}
	return n
}