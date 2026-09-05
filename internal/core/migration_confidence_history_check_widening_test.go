package internal

import (
	"database/sql"
	"sync/atomic"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

// migrationConfidenceCheckTestDBCounter ensures each test gets a unique
// shared-cache in-memory DB. Bare ":memory:" gives each pooled connection
// its own private DB and silently loses schema state.
var migrationConfidenceCheckTestDBCounter int64

// setupOldShapeConfidenceHistoryDB builds an in-memory DB whose
// confidence_history has the pre-widening CHECK constraint (6 values).
// Mirrors the deployed-DB shape the audit found at the time of writing:
//
//	('evidence_added','evidence_updated','evidence_deleted',
//	 'evidence_expired','decay_tick','manual_recompute')
//
// Missing: 'concept_drift','supersede','invalidate'.
//
// Returns the open *sql.DB and a teardown func.
func setupOldShapeConfidenceHistoryDB(t *testing.T) (*sql.DB, func()) {
	t.Helper()
	n := atomic.AddInt64(&migrationConfidenceCheckTestDBCounter, 1)
	dsn := "file:confidence-history-old-shape-" + testItoa(n) + "?mode=memory&cache=shared"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		t.Fatalf("sql.Open(%q): %v", dsn, err)
	}
	teardown := func() { _ = db.Close() }

	// Mirror production schema minus the CHECK-widening values.
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS schema_migrations (
			id TEXT PRIMARY KEY,
			applied_at INTEGER NOT NULL
		);
		CREATE TABLE IF NOT EXISTS confidence_history (
			id              TEXT PRIMARY KEY,
			artifact_id     TEXT NOT NULL,
			artifact_type   TEXT NOT NULL,
			confidence      REAL NOT NULL,
			computed_at     INTEGER NOT NULL,
			evidence_count  INTEGER NOT NULL,
			trigger         TEXT NOT NULL CHECK (trigger IN ('evidence_added','evidence_updated','evidence_deleted','evidence_expired','decay_tick','manual_recompute'))
		);
		CREATE INDEX IF NOT EXISTS idx_conf_history_artifact ON confidence_history(artifact_id, artifact_type, computed_at);
	`)
	if err != nil {
		teardown()
		t.Fatalf("create old-shape confidence_history: %v", err)
	}

	return db, teardown
}

// TestMigrateConfidenceHistoryCheckWidening_OnOldShapeDB is the audit's
// P0 regression: a DB built with the pre-widening CHECK must accept
// 'supersede' and 'invalidate' rows after the migration runs, and the
// existing history rows must survive.
func TestMigrateConfidenceHistoryCheckWidening_OnOldShapeDB(t *testing.T) {
	db, teardown := setupOldShapeConfidenceHistoryDB(t)
	defer teardown()

	// Seed one history row using a value that BOTH the old and new
	// CHECK accept — survives in either shape.
	_, err := db.Exec(`
		INSERT INTO confidence_history (id, artifact_id, artifact_type, confidence, computed_at, evidence_count, trigger)
		VALUES (?, ?, 'decision', ?, ?, 0, 'evidence_added')`,
		"row-pre-migration",
		"decision-pre",
		0.5,
		1700000000,
	)
	if err != nil {
		t.Fatalf("seed pre-migration row: %v", err)
	}

	// Run the migration in a transaction, mirroring the init path.
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if err := MigrateConfidenceHistoryCheckWidening(tx); err != nil {
		_ = tx.Rollback()
		t.Fatalf("migration: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// 1. Existing history row must survive unchanged.
	var preCount int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM confidence_history WHERE id = ?`,
		"row-pre-migration",
	).Scan(&preCount); err != nil {
		t.Fatalf("count pre-migration row: %v", err)
	}
	if preCount != 1 {
		t.Fatalf("expected 1 pre-migration row to survive, got %d", preCount)
	}

	// 2. The widened CHECK must now accept the canonical 'supersede'
	//    and 'invalidate' trigger values that the audit found blocked.
	if _, err := db.Exec(`
		INSERT INTO confidence_history (id, artifact_id, artifact_type, confidence, computed_at, evidence_count, trigger)
		VALUES (?, ?, 'decision', ?, ?, 0, 'supersede')`,
		"row-supersede",
		"decision-supersede",
		0.5,
		1700000100,
	); err != nil {
		t.Fatalf("insert 'supersede' row after migration: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO confidence_history (id, artifact_id, artifact_type, confidence, computed_at, evidence_count, trigger)
		VALUES (?, ?, 'decision', ?, ?, 0, 'invalidate')`,
		"row-invalidate",
		"decision-invalidate",
		0.5,
		1700000200,
	); err != nil {
		t.Fatalf("insert 'invalidate' row after migration: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO confidence_history (id, artifact_id, artifact_type, confidence, computed_at, evidence_count, trigger)
		VALUES (?, ?, 'decision', ?, ?, 0, 'concept_drift')`,
		"row-concept-drift",
		"decision-concept-drift",
		0.5,
		1700000300,
	); err != nil {
		t.Fatalf("insert 'concept_drift' row after migration: %v", err)
	}

	// 3. Total row count: 1 pre + 3 new = 4.
	var total int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM confidence_history`,
	).Scan(&total); err != nil {
		t.Fatalf("count total: %v", err)
	}
	if total != 4 {
		t.Fatalf("expected 4 history rows after migration, got %d", total)
	}

	// 4. Sentinel row must be present.
	var sentinelCount int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM schema_migrations WHERE id = ?`,
		"confidence_history_check_widening_v1",
	).Scan(&sentinelCount); err != nil {
		t.Fatalf("count sentinel: %v", err)
	}
	if sentinelCount != 1 {
		t.Fatalf("expected 1 sentinel row, got %d", sentinelCount)
	}
}

// TestMigrateConfidenceHistoryCheckWidening_Idempotent proves the
// migration is a no-op on a second run: same DB, same sentinel row,
// no row loss, no CHECK regression.
func TestMigrateConfidenceHistoryCheckWidening_Idempotent(t *testing.T) {
	db, teardown := setupOldShapeConfidenceHistoryDB(t)
	defer teardown()

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin tx (run 1): %v", err)
	}
	if err := MigrateConfidenceHistoryCheckWidening(tx); err != nil {
		_ = tx.Rollback()
		t.Fatalf("migration run 1: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit run 1: %v", err)
	}

	var afterFirst int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM confidence_history`,
	).Scan(&afterFirst); err != nil {
		t.Fatalf("count after run 1: %v", err)
	}

	// Second run.
	tx, err = db.Begin()
	if err != nil {
		t.Fatalf("begin tx (run 2): %v", err)
	}
	if err := MigrateConfidenceHistoryCheckWidening(tx); err != nil {
		_ = tx.Rollback()
		t.Fatalf("migration run 2: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit run 2: %v", err)
	}

	var afterSecond int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM confidence_history`,
	).Scan(&afterSecond); err != nil {
		t.Fatalf("count after run 2: %v", err)
	}
	if afterSecond != afterFirst {
		t.Fatalf("idempotency broken: row count %d → %d", afterFirst, afterSecond)
	}

	// Sentinel must be exactly 1 (NOT 2).
	var sentinelCount int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM schema_migrations WHERE id = ?`,
		"confidence_history_check_widening_v1",
	).Scan(&sentinelCount); err != nil {
		t.Fatalf("count sentinel: %v", err)
	}
	if sentinelCount != 1 {
		t.Fatalf("idempotency broken: sentinel count = %d, want 1", sentinelCount)
	}

	// Sanity: supersede still inserts.
	if _, err := db.Exec(`
		INSERT INTO confidence_history (id, artifact_id, artifact_type, confidence, computed_at, evidence_count, trigger)
		VALUES (?, ?, 'decision', ?, ?, 0, 'supersede')`,
		"row-after-2nd-run",
		"decision-after-2nd-run",
		0.5,
		1700000999,
	); err != nil {
		t.Fatalf("insert 'supersede' after second run: %v", err)
	}
}

// TestMigrateConfidenceHistoryCheckWidening_AlreadyWidened proves the
// migration is a no-op on a fresh-DB shape that already has the 9-value
// CHECK (the schema.go declaration).
func TestMigrateConfidenceHistoryCheckWidening_AlreadyWidened(t *testing.T) {
	n := atomic.AddInt64(&migrationConfidenceCheckTestDBCounter, 1)
	dsn := "file:confidence-history-wide-" + testItoa(n) + "?mode=memory&cache=shared"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS schema_migrations (
			id TEXT PRIMARY KEY,
			applied_at INTEGER NOT NULL
		);
		CREATE TABLE IF NOT EXISTS confidence_history (
			id              TEXT PRIMARY KEY,
			artifact_id     TEXT NOT NULL,
			artifact_type   TEXT NOT NULL,
			confidence      REAL NOT NULL,
			computed_at     INTEGER NOT NULL,
			evidence_count  INTEGER NOT NULL,
			trigger         TEXT NOT NULL CHECK (trigger IN ('evidence_added','evidence_updated','evidence_deleted','evidence_expired','decay_tick','concept_drift','manual_recompute','supersede','invalidate'))
		);
	`)
	if err != nil {
		t.Fatalf("create wide-shape confidence_history: %v", err)
	}

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if err := MigrateConfidenceHistoryCheckWidening(tx); err != nil {
		_ = tx.Rollback()
		t.Fatalf("migration on already-widened: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Sentinel MUST be inserted even on a no-op widening so future
	// runs short-circuit. Without the sentinel, the next migration
	// call would re-probe the DDL and re-run the parser.
	var sentinelCount int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM schema_migrations WHERE id = ?`,
		"confidence_history_check_widening_v1",
	).Scan(&sentinelCount); err != nil {
		t.Fatalf("count sentinel: %v", err)
	}
	if sentinelCount != 1 {
		t.Fatalf("expected 1 sentinel row on no-op, got %d", sentinelCount)
	}

	// All 9 trigger values must be insertable.
	for _, trigger := range []string{
		"evidence_added", "evidence_updated", "evidence_deleted",
		"evidence_expired", "decay_tick", "concept_drift",
		"manual_recompute", "supersede", "invalidate",
	} {
		_, err := db.Exec(`
			INSERT INTO confidence_history (id, artifact_id, artifact_type, confidence, computed_at, evidence_count, trigger)
			VALUES (?, ?, 'decision', ?, ?, 0, ?)`,
			"row-"+trigger, "decision-"+trigger, 0.5, int64(1700000000), trigger,
		)
		if err != nil {
			t.Fatalf("insert %q on widened CHECK: %v", trigger, err)
		}
	}
}

// testItoa is defined in migration_deleted_at_test.go (shared test
// helper). Reusing it avoids a duplicate-declaration build error.
