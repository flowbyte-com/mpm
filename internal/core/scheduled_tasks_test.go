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
	dm := newTestDM(t)
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
		nextRun                                    int64
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
	if nextRun < time.Now().Unix() {
		t.Errorf("next_run_at should be in the future, got %d", nextRun)
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
	var origCreated, origNextRun int64
	var origLastRun *int64
	err := dm.db.QueryRow(`
		SELECT created_at, last_run_at, next_run_at FROM scheduled_tasks WHERE id = ?
	`, original.ID).Scan(&origCreated, &origLastRun, &origNextRun)
	if err != nil {
		t.Fatalf("QueryRow: %v", err)
	}
	if origLastRun != nil {
		t.Errorf("fresh insert should have NULL last_run_at, got %d", *origLastRun)
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
	var newCreated, newNextRun int64
	var newLastRun *int64
	var newStatus, newName string
	err = dm.db.QueryRow(`
		SELECT created_at, last_run_at, next_run_at, status, name FROM scheduled_tasks WHERE id = ?
	`, original.ID).Scan(&newCreated, &newLastRun, &newNextRun, &newStatus, &newName)
	if err != nil {
		t.Fatalf("QueryRow after update: %v", err)
	}
	if newCreated != origCreated {
		t.Errorf("created_at should be preserved: got %d, want %d", newCreated, origCreated)
	}
	if newLastRun != nil {
		t.Errorf("last_run_at should remain NULL until first process: got %d", *newLastRun)
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
		past.Unix(), past.Unix(), past.Unix())
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
	// kind must also be set so check_wakes {kinds:["cron"]} matches.
	// CronSource and CronWakeKind share the literal value but are
	// semantically distinct (taxonomy tag vs provenance marker).
	if meta["kind"] != CronWakeKind {
		t.Errorf("metadata.kind should be %q, got %v", CronWakeKind, meta["kind"])
	}
	if meta["directive_id"] != "mpm-seed-test" {
		t.Errorf("metadata.directive_id should be mpm-seed-test, got %v", meta["directive_id"])
	}

	// next_run_at should now be in the future (rolled over to next 03:00)
	var newNextRun int64
	var lastRun *int64
	err = dm.db.QueryRow(`
		SELECT next_run_at, last_run_at FROM scheduled_tasks WHERE id = ?
	`, "test-cron").Scan(&newNextRun, &lastRun)
	if err != nil {
		t.Fatalf("QueryRow for task: %v", err)
	}
	if newNextRun <= time.Now().UTC().Unix() {
		t.Errorf("next_run_at should be in future after rollover, got %d", newNextRun)
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
		past.Unix(), past.Unix(), past.Unix())
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
	// Use .Unix() for INTEGER columns — passing time.Time directly
	// would round-trip through RFC3339 text and SQLite would coerce it
	// to 0/NaN on storage, defeating the test's intent.
	pastUnix := time.Now().UTC().Add(-1 * time.Minute).Unix()
	_, err := dm.db.Exec(`
		INSERT INTO scheduled_tasks
		(id, name, cron_expr, directive_id, status, next_run_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, "paused-task", "Paused", "0 3 * * *", "mpm-seed-pp", ScheduledTaskPaused,
		pastUnix, pastUnix, pastUnix)
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

// TestProcessDueTasks_RespectsFutureNextRun is the regression test for
// the SQL type-coercion bug discovered on 2026-09-07.
//
// Before the fix, ProcessScheduledTasks passed a time.Time value as the
// `next_run_at <= ?` query parameter. go-sqlite3 serialized it as
// RFC3339 TEXT; SQLite's INTEGER-affinity column compared against the
// unparseable TEXT evaluated TRUE regardless of the integer value, so
// the poll matched every active task on every tick. In production this
// caused cron tasks (epistemic-compaction, daily at 03:00 UTC) to fire
// every 60 seconds, accumulating 9407 cron wakes over six days.
//
// The regression test seeds a future-scheduled task (next_run_at one
// hour ahead) and a past-scheduled task (next_run_at one minute
// behind) into the same DB, calls ProcessDueTasks, and asserts the
// future task is NOT processed while the past task IS. The future-task
// half is what the bug breaks: with the bug, both are matched; with the
// fix, only the past one.
func TestProcessDueTasks_RespectsFutureNextRun(t *testing.T) {
	dm := newScheduledTaskDM(t)
	now := time.Now().UTC()
	futureUnix := now.Add(1 * time.Hour).Unix()
	pastUnix := now.Add(-1 * time.Minute).Unix()

	// Seed two tasks with explicit Unix-epoch next_run_at values so the
	// poll comparison is against real INTEGER column data, not freshly-
	// built schema with zero rows — that's the precondition the bug
	// specifically required to manifest.
	for _, row := range []struct {
		id        string
		nextRunAt int64
		cronExpr  string
	}{
		{"future-task", futureUnix, "0 3 * * *"},
		{"past-task", pastUnix, "0 3 * * *"},
	} {
		_, err := dm.db.Exec(`
			INSERT INTO scheduled_tasks
			(id, name, cron_expr, directive_id, status, next_run_at, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		`, row.id, row.id, row.cronExpr, "mpm-seed-reg", ScheduledTaskActive,
			row.nextRunAt, now.Unix(), now.Unix())
		if err != nil {
			t.Fatalf("seed %s: %v", row.id, err)
		}
	}

	beforeWakes := countWakes(t, dm)

	n, err := dm.ProcessDueTasks()
	if err != nil {
		t.Fatalf("ProcessDueTasks: %v", err)
	}

	// Only the past-due task should match — the future task is exactly
	// the case the bug broke. With the bug present, both matched and n
	// would be 2 (the regression failure mode).
	if n != 1 {
		t.Errorf("expected exactly 1 task processed (past only), got %d — future-scheduled task was incorrectly matched", n)
	}

	// Exactly one new wake should have been injected, and it must
	// reference the past task. The future task's row should have no
	// wake and its next_run_at should be unchanged.
	afterWakes := countWakes(t, dm)
	if afterWakes != beforeWakes+1 {
		t.Errorf("expected wake count delta +1, got before=%d after=%d", beforeWakes, afterWakes)
	}

	var reason string
	if err := dm.db.QueryRow(`
		SELECT reason FROM scheduled_wakes
		WHERE created_by = ? ORDER BY rowid DESC LIMIT 1
	`, CronCreatedBy).Scan(&reason); err != nil {
		t.Fatalf("QueryRow for newest wake: %v", err)
	}
	if !strings.Contains(reason, "past-task") {
		t.Errorf("newest wake should reference past-task, got %q", reason)
	}
	if strings.Contains(reason, "future-task") {
		t.Errorf("future-task should not have been processed; wake reason references it: %q", reason)
	}

	// future-task.next_run_at must be untouched.
	var futureNextRun int64
	if err := dm.db.QueryRow(`SELECT next_run_at FROM scheduled_tasks WHERE id = ?`,
		"future-task").Scan(&futureNextRun); err != nil {
		t.Fatalf("QueryRow for future-task: %v", err)
	}
	if futureNextRun != futureUnix {
		t.Errorf("future-task.next_run_at should be untouched: got %d, want %d", futureNextRun, futureUnix)
	}

	// past-task.next_run_at should have rolled forward to a future time
	// (the next 03:00 UTC occurrence from `now`).
	var pastNextRun int64
	if err := dm.db.QueryRow(`SELECT next_run_at FROM scheduled_tasks WHERE id = ?`,
		"past-task").Scan(&pastNextRun); err != nil {
		t.Fatalf("QueryRow for past-task: %v", err)
	}
	if pastNextRun <= now.Unix() {
		t.Errorf("past-task.next_run_at should have rolled to a future time, got %d (now=%d)", pastNextRun, now.Unix())
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
			r.nextRunAt.Unix(), now.Unix(), now.Unix()); err != nil {
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

// TestProcessDueTasks_CronWakeDiscoversByKind is the end-to-end proof
// that the cron-daemon's metadata.kind="cron" tag makes the injected
// wake discoverable via check_wakes {kinds:["cron"]}. Before this fix,
// cron wakes used source="cron" only and kinds=["cron"] silently
// matched nothing — a documentation/architecture trap documented in
// the 2026-07-24 handoff. This test fails if anyone reverts the kind
// tag in scheduled_tasks.go's metadata string.
func TestProcessDueTasks_CronWakeDiscoversByKind(t *testing.T) {
	dm := newScheduledTaskDM(t)

	// Seed a task due in the past so ProcessDueTasks will pick it up
	// on the very next call (no time mocking needed).
	past := time.Now().UTC().Add(-1 * time.Minute)
	if _, err := dm.db.Exec(`
		INSERT INTO scheduled_tasks
		(id, name, cron_expr, directive_id, status, next_run_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, "disc-cron", "discoverability cron", "0 3 * * *", "mpm-seed-disc",
		ScheduledTaskActive, past.Unix(), past.Unix(), past.Unix()); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if _, err := dm.ProcessDueTasks(); err != nil {
		t.Fatalf("ProcessDueTasks: %v", err)
	}

	// The wake is now in scheduled_wakes with target_time = past (already
	// due). CheckPendingWakes with kinds=["cron"] MUST return it.
	got, err := dm.CheckPendingWakes(time.Now(), []string{"cron"})
	if err != nil {
		t.Fatalf("CheckPendingWakes kinds=[cron]: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("kinds=[cron] should surface the cron-injected wake, got %d (the kinds taxonomy is broken if zero)", len(got))
	}
	if got[0]["reason"] != "cron:disc-cron" {
		t.Errorf("wake reason: got %q, want %q", got[0]["reason"], "cron:disc-cron")
	}

	// Conversely, kinds=["notification"] MUST NOT match the cron wake
	// (the kind field is the discriminator; without it, the default
	// filter would have included cron wakes, masking the bug).
	notif, err := dm.CheckPendingWakes(time.Now().Add(1*time.Hour), []string{"notification"})
	if err != nil {
		t.Fatalf("CheckPendingWakes kinds=[notification]: %v", err)
	}
	// The wake from the first CheckPendingWakes was already marked
	// fired=1 (above). So this second call with kinds=[notification]
	// against time.Now()+1h should see 0 cron-related wakes; any
	// notification-kind wakes would surface here but we didn't seed any.
	for _, w := range notif {
		if r, ok := w["reason"].(string); ok && strings.HasPrefix(r, CronWakePrefix) {
			t.Errorf("kinds=[notification] should not surface cron-injected wake, but got reason=%q", r)
		}
	}
}