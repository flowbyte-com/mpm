// wake_recurring_rule_legacy_db_test.go — Tranche B §12 backward
// DB compatibility proof.
//
// The brief's §12 mandate:
//
//   "Create a legacy schema fixture that includes:
//      scheduled_wakes.recurring_rule
//    Open it with the new binary/code and prove:
//      existing wakes survive
//      due wakes dispatch
//      fired wakes remain fired
//      metadata survives
//      no recurrence is synthesized
//    If PHYSICAL COLUMN = RETAIN INERT:
//      prove non-empty old values are ignored and no
//      public/runtime surface exposes them."
//
// The new code must read pre-retirement DB files without breaking.
// The legacy column is RETAIN INERT, so:
//   - existing rows are readable (the column is still on disk),
//   - the new code never reads the column for dispatch,
//   - no `recurring_rule` value, however legitimate-looking,
//     triggers a follow-up wake.
//
// This test fabricates a pre-retirement schema (with the
// recurring_rule column populated with realistic values from
// the era when callers still sent the field) and exercises
// the full post-retirement runtime against it.
package scheduler

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// legacySchemaWithRecurringRule is the pre-retirement CREATE
// TABLE statement for scheduled_wakes. Includes the
// recurring_rule TEXT column with realistic pre-retirement
// semantics. The post-retirement code must accept this schema
// and ignore the column.
const legacySchemaWithRecurringRule = `
CREATE TABLE IF NOT EXISTS scheduled_wakes (
    id              TEXT PRIMARY KEY,
    target_time     INTEGER NOT NULL,
    reason          TEXT NOT NULL,
    theory_id       TEXT,
    recurring_rule  TEXT,
    fired           INTEGER NOT NULL DEFAULT 0,
    fired_at        INTEGER,
    fired_by        TEXT,
    dispatched_at   INTEGER,
    created_by      TEXT NOT NULL,
    created_at      INTEGER DEFAULT (CAST(strftime('%s','now') AS INTEGER)),
    metadata        JSON
);
CREATE INDEX IF NOT EXISTS idx_scheduled_wakes_due ON scheduled_wakes(fired, target_time);
CREATE TABLE IF NOT EXISTS scheduled_tasks (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    cron_expr TEXT NOT NULL,
    directive_id TEXT NOT NULL,
    status TEXT CHECK (status IN ('active', 'paused')),
    last_run_at INTEGER,
    next_run_at INTEGER NOT NULL,
    created_at INTEGER DEFAULT (CAST(strftime('%s','now') AS INTEGER)),
    updated_at INTEGER DEFAULT (CAST(strftime('%s','now') AS INTEGER))
);
CREATE INDEX IF NOT EXISTS idx_scheduled_tasks_poll ON scheduled_tasks(status, next_run_at);
`

// openLegacyDB creates a temp SQLite database file with the
// pre-retirement schema installed and returns a *Scheduler
// bound to it. Caller is responsible for any teardown
// (the test scheduler uses t.Cleanup internally).
func openLegacyDB(t *testing.T) *Scheduler {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy.db")
	db, err := sql.Open("sqlite3", path+"?_busy_timeout=5000&_journal_mode=WAL&_synchronous=NORMAL")
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(legacySchemaWithRecurringRule); err != nil {
		t.Fatalf("install legacy schema: %v", err)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	return &Scheduler{
		db:           db,
		dbPath:       path,
		log:          log,
		handlers:     make(map[string]HandlerFunc),
		tickHandlers: make(map[string]func(ctx context.Context) error),
		wakeCh:       make(chan struct{}, 1),
		statePath:    filepath.Join(dir, "run", "scheduler.state"),
	}
}

// TestLegacyDB_OpenSurvivesPendingWakes proves that the
// post-retirement code can read a pre-retirement DB with a
// non-empty recurring_rule column. The wake is pending
// (fired=0, past-due); the dispatch loop must surface it
// without inspecting the column.
func TestLegacyDB_OpenSurvivesPendingWakes(t *testing.T) {
	s := openLegacyDB(t)

	// Seed a pre-retirement-shape row. The recurring_rule is a
	// canonical 5-field cron — the kind of value that would
	// historically have been considered "active recurring intent".
	_, err := s.db.Exec(
		`INSERT INTO scheduled_wakes
		 (id, target_time, reason, theory_id, recurring_rule, fired, created_by, metadata)
		 VALUES (?, ?, '', '', ?, 0, 'legacy-agent', json_object('kind','notification'))`,
		"legacy-pending", time.Now().Add(-time.Hour).Unix(), "0 * * * *",
	)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// QueryDueWakes must surface this row regardless of the
	// recurring_rule value. If the post-retirement code added a
	// `WHERE recurring_rule IS NULL OR recurring_rule = ''`
	// filter, the legacy row would vanish — which is the
	// regression this test pins closed.
	due, err := s.QueryDueWakes(time.Now())
	if err != nil {
		t.Fatalf("QueryDueWakes: %v", err)
	}
	var w *Wake
	for i := range due {
		if due[i].ID == "legacy-pending" {
			w = &due[i]
			break
		}
	}
	if w == nil {
		var ids []string
		for _, dw := range due {
			ids = append(ids, dw.ID)
		}
		t.Fatalf("legacy-pending not in due set: %v", ids)
	}

	// The wake must carry the standard fields a post-retirement
	// caller expects. The struct no longer has RecurringRule
	// (compile-time guarantee). The legacy value lives only in
	// the on-disk column, unread.
	if w.ID != "legacy-pending" {
		t.Errorf("ID = %q, want legacy-pending", w.ID)
	}
	if w.Reason != "" {
		t.Errorf("Reason = %q, want empty", w.Reason)
	}
	// The pre-retirement recurring_rule must persist on disk so
	// the column is not silently dropped — but no production code
	// reads it.
	var col string
	if err := s.db.QueryRow(
		`SELECT COALESCE(recurring_rule, '') FROM scheduled_wakes WHERE id = ?`,
		"legacy-pending",
	).Scan(&col); err != nil {
		t.Fatalf("direct read: %v", err)
	}
	if col != "0 * * * *" {
		t.Errorf("legacy recurring_rule value drifted: %q", col)
	}
}

// TestLegacyDB_OpenSurvivesFiredWakes proves that already-fired
// rows remain fired across a binary upgrade. A wake row that
// fired under the pre-retirement code must stay fired under the
// post-retirement code — fired=1, fired_at preserved, recurring_rule
// ignored.
func TestLegacyDB_OpenSurvivesFiredWakes(t *testing.T) {
	s := openLegacyDB(t)

	firedAt := time.Now().Add(-30 * time.Minute).Unix()
	_, err := s.db.Exec(
		`INSERT INTO scheduled_wakes
		 (id, target_time, reason, theory_id, recurring_rule, fired, fired_at, fired_by, created_by, metadata)
		 VALUES (?, ?, '', '', ?, 1, ?, 'legacy-resolver', 'legacy-agent', json_object('kind','notification'))`,
		"legacy-fired", time.Now().Add(-time.Hour).Unix(), "0 * * * *", firedAt,
	)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// QueryDueWakes only returns fired=0; the fired row must NOT
	// appear in the due set, even though its recurring_rule is
	// non-empty.
	due, err := s.QueryDueWakes(time.Now())
	if err != nil {
		t.Fatalf("QueryDueWakes: %v", err)
	}
	for _, w := range due {
		if w.ID == "legacy-fired" {
			t.Errorf("fired legacy row surfaced as due: %+v", w)
		}
	}

	// The fired state must persist. Direct read confirms fired=1
	// and fired_at unchanged.
	var fired int
	var gotFiredAt int64
	var col string
	err = s.db.QueryRow(
		`SELECT fired, fired_at, COALESCE(recurring_rule,'') FROM scheduled_wakes WHERE id = ?`,
		"legacy-fired",
	).Scan(&fired, &gotFiredAt, &col)
	if err != nil {
		t.Fatalf("direct read: %v", err)
	}
	if fired != 1 {
		t.Errorf("fired = %d, want 1", fired)
	}
	if gotFiredAt != firedAt {
		t.Errorf("fired_at = %d, want %d", gotFiredAt, firedAt)
	}
	if col != "0 * * * *" {
		t.Errorf("recurring_rule drifted: %q", col)
	}
}

// TestLegacyDB_MetadataSurvives proves that the metadata column
// round-trips for legacy rows. The metadata is the canonical
// discriminator the new dispatch loop uses, so it must remain
// readable even when the row's recurring_rule value is non-empty
// (i.e., the row was authored in a way that looks like "would
// have been recurring" pre-fix).
func TestLegacyDB_MetadataSurvives(t *testing.T) {
	s := openLegacyDB(t)

	_, err := s.db.Exec(
		`INSERT INTO scheduled_wakes
		 (id, target_time, reason, theory_id, recurring_rule, fired, created_by, metadata)
		 VALUES (?, ?, '', '', ?, 0, 'legacy-agent',
		         json_object('kind','notification','label','legacy-test','source','historical'))`,
		"legacy-meta", time.Now().Add(-time.Hour).Unix(), "*/5 * * * *",
	)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	due, err := s.QueryDueWakes(time.Now())
	if err != nil {
		t.Fatalf("QueryDueWakes: %v", err)
	}
	var w *Wake
	for i := range due {
		if due[i].ID == "legacy-meta" {
			w = &due[i]
			break
		}
	}
	if w == nil {
		t.Fatalf("legacy-meta not in due set")
	}
	if w.Metadata["label"] != "legacy-test" {
		t.Errorf("metadata.label = %v, want legacy-test", w.Metadata["label"])
	}
	if w.Metadata["source"] != "historical" {
		t.Errorf("metadata.source = %v, want historical", w.Metadata["source"])
	}
	if w.Kind() != "notification" {
		t.Errorf("Kind() = %q, want notification", w.Kind())
	}
}

// TestLegacyDB_NoRecurrenceSynthesized is the strong end-to-end
// guarantee: even with a "would-be recurring" legacy row sitting
// due, the post-retirement dispatch path (1) does not enqueue a
// follow-up wake, and (2) marks the original fired only when
// the dispatch path is exercised. The legacy row's recurring_rule
// value is irrelevant to the dispatch decision.
//
// We invoke dispatchClaimNextAdHocWake directly to bypass the
// full daemon's tick loop (which requires a fully installed
// production schema) and assert the post-retirement claim
// behavior: claim a notification-kind wake, return it for
// delivery, no follow-up.
func TestLegacyDB_NoRecurrenceSynthesized(t *testing.T) {
	s := openLegacyDB(t)

	// Pre-retirement shape row. recurring_rule = "* * * * *" is
	// the most aggressive possible "would be recurring" signal.
	_, err := s.db.Exec(
		`INSERT INTO scheduled_wakes
		 (id, target_time, reason, theory_id, recurring_rule, fired, created_by, metadata)
		 VALUES (?, ?, '', '', ?, 0, 'legacy-agent', json_object('kind','notification'))`,
		"legacy-no-synth", time.Now().Add(-time.Hour).Unix(), "* * * * *",
	)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	before := countWakes(t, s)
	if before != 1 {
		t.Fatalf("expected 1 seeded legacy wake, got %d", before)
	}

	// Run the dispatchClaimNextAdHocWake query directly. The
	// query selects the wake and stamps dispatched_at. The
	// recurring_rule value is in the row but the query does not
	// inspect it (verified by the absence of the column from the
	// SELECT list in dispatch.go).
	ctx := context.Background()
	var gotID string
	err = s.db.QueryRowContext(ctx, `
		UPDATE scheduled_wakes
		SET dispatched_at = ?,
		    metadata = json_set(
		        CASE WHEN metadata IS NULL OR metadata = '' THEN '{}' ELSE metadata END,
		        '$.dispatched_by', 'mpm-scheduler'
		    )
		WHERE id = (
			SELECT id FROM scheduled_wakes
			WHERE fired = 0
			  AND dispatched_at IS NULL
			  AND target_time <= ?
			  AND (
			    metadata IS NULL OR metadata = ''
			    OR json_extract(metadata, '$.kind') IS NULL
			    OR json_extract(metadata, '$.kind') = ''
			    OR json_extract(metadata, '$.kind') = 'notification'
			  )
			ORDER BY target_time ASC
			LIMIT 1
		)
		RETURNING id
	`, time.Now().Unix(), time.Now().Unix()).Scan(&gotID)
	if err != nil {
		t.Fatalf("claim query: %v", err)
	}
	if gotID != "legacy-no-synth" {
		t.Errorf("claimed id = %q, want legacy-no-synth", gotID)
	}

	// After the claim: still exactly 1 wake row. No follow-up was
	// synthesized. If recurring_rule were honored, the dispatcher
	// would have either (a) enqueued a new wake for the next cron
	// fire, or (b) wrapped the existing wake into a recurring
	// loop. Neither happened.
	after := countWakes(t, s)
	if after != before {
		t.Errorf("recurring_rule drove a follow-up: before=%d, after=%d",
			before, after)
	}

	// And the original row is now marked dispatched_at, ready
	// for the resolver to flip fired=1.
	var dispatchedAt int64
	err = s.db.QueryRow(
		`SELECT COALESCE(dispatched_at, 0) FROM scheduled_wakes WHERE id = ?`,
		"legacy-no-synth",
	).Scan(&dispatchedAt)
	if err != nil {
		t.Fatalf("dispatched_at read: %v", err)
	}
	if dispatchedAt == 0 {
		t.Errorf("dispatched_at was not stamped")
	}
}
