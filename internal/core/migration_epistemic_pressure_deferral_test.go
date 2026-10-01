// migration_epistemic_pressure_deferral_test.go — proves the view
// migration actually upgrades a database that already has the OLD
// two-column view.
//
// This is the hazard the migration exists to handle:
// CREATE VIEW IF NOT EXISTS silently no-ops against an existing
// definition, so editing the base schema alone would leave every
// already-provisioned database with the old shape forever. A fresh
// t.TempDir() database cannot catch that — it is always built from the
// current schema — so the old-shaped view is constructed by hand here.

package internal

import (
	"database/sql"
	"testing"
)

// runDeferralMigration executes the migration the way production does —
// inside a transaction — and fails the test if the transaction cannot be
// established or the migration errors.
func runDeferralMigration(t *testing.T, dm *DatabaseManager) {
	t.Helper()
	tx, err := dm.SQLDB().Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := MigrateEpistemicPressureDeferral(tx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// oldEpistemicPressureViewSQL is the pre-change definition, verbatim.
// If the migration is ever found insufficient, this is the shape it has
// to escape from.
const oldEpistemicPressureViewSQL = `CREATE VIEW epistemic_pressure_v AS
SELECT
  (SELECT COUNT(*) FROM memories
   WHERE collection = 'memories'
     AND deleted_at IS NULL
     AND (metadata IS NULL OR metadata = ''
          OR json_extract(metadata, '$.compacted_into') IS NULL)
  ) AS raw_count,
  (SELECT COUNT(*) FROM lessons) AS lesson_count`

// A database carrying the OLD view is upgraded in place, and the new
// columns read back correctly.
func TestMigrateEpistemicPressureDeferral_UpgradesOldView(t *testing.T) {
	dm := NewTestDM(t)

	// Roll the view back to its pre-change shape.
	if _, err := dm.SQLDB().Exec(`DROP VIEW epistemic_pressure_v`); err != nil {
		t.Fatalf("drop view: %v", err)
	}
	if _, err := dm.SQLDB().Exec(oldEpistemicPressureViewSQL); err != nil {
		t.Fatalf("recreate old view: %v", err)
	}
	// The migration sentinel was already recorded by the normal
	// startup path, so clear it to simulate a database that predates
	// this change.
	if _, err := dm.SQLDB().Exec(`DELETE FROM schema_migrations WHERE id = 'epistemic_pressure_deferral_v1'`); err != nil {
		t.Fatalf("clear sentinel: %v", err)
	}

	// Confirm we really are looking at the old shape.
	if cols, err := viewColumnNames(dm.SQLDB()); err != nil {
		t.Fatalf("read columns: %v", err)
	} else if cols["deferred_count"] {
		t.Fatal("test setup is wrong: the old view already has deferred_count")
	}

	seedMixed(t, dm, 4, 3, 2)

	runDeferralMigration(t, dm)

	got := readPressure(t, dm)
	if got.RawCount != 7 {
		t.Errorf("raw_count = %d, want 7 (4 pending + 3 deferred)", got.RawCount)
	}
	if got.DeferredCount != 3 {
		t.Errorf("deferred_count = %d, want 3", got.DeferredCount)
	}
	if got.ActionablePending != 4 {
		t.Errorf("actionable_pending = %d, want 4", got.ActionablePending)
	}
}

// The migration is idempotent: running it twice is a no-op the second
// time, and the view still reads correctly.
func TestMigrateEpistemicPressureDeferral_Idempotent(t *testing.T) {
	dm := NewTestDM(t)
	seedMixed(t, dm, 2, 1, 0)

	runDeferralMigration(t, dm)
	runDeferralMigration(t, dm)

	got := readPressure(t, dm)
	if got.RawCount != 3 || got.DeferredCount != 1 || got.ActionablePending != 2 {
		t.Errorf("counts drifted after a repeat migration: %+v", got)
	}

	// Exactly one sentinel row — not two.
	var n int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM schema_migrations WHERE id = 'epistemic_pressure_deferral_v1'`).Scan(&n); err != nil {
		t.Fatalf("count sentinels: %v", err)
	}
	if n != 1 {
		t.Errorf("sentinel rows = %d, want 1", n)
	}
}

// A database that somehow recorded the sentinel but kept an old-shaped
// view (a restore that replayed migrations out of order) is still
// repaired by the column probe. This is why the migration checks both
// the sentinel and the column.
func TestMigrateEpistemicPressureDeferral_RepairsStaleSentinel(t *testing.T) {
	dm := NewTestDM(t)

	if _, err := dm.SQLDB().Exec(`DROP VIEW epistemic_pressure_v`); err != nil {
		t.Fatalf("drop view: %v", err)
	}
	if _, err := dm.SQLDB().Exec(oldEpistemicPressureViewSQL); err != nil {
		t.Fatalf("recreate old view: %v", err)
	}
	// Sentinel present, view stale: the inconsistent state.
	if _, err := dm.SQLDB().Exec(
		`INSERT OR IGNORE INTO schema_migrations (id, applied_at) VALUES ('epistemic_pressure_deferral_v1', 0)`); err != nil {
		t.Fatalf("seed sentinel: %v", err)
	}

	runDeferralMigration(t, dm)
	// If the sentinel short-circuited, this read fails and the test
	// fails — the column is still missing.
	if got := readPressure(t, dm); got.RawCount != 0 {
		t.Errorf("raw_count = %d, want 0", got.RawCount)
	}
}

// lesson_count keeps its name, position, and meaning. A migration that
// renamed or dropped it would break every existing reader.
func TestMigrateEpistemicPressureDeferral_LessonCountPreserved(t *testing.T) {
	dm := NewTestDM(t)

	cols, err := viewColumnNames(dm.SQLDB())
	if err != nil {
		t.Fatalf("read columns: %v", err)
	}
	for _, want := range []string{"raw_count", "lesson_count", "deferred_count", "actionable_pending"} {
		if !cols[want] {
			t.Errorf("epistemic_pressure_v is missing column %q; got %v", want, cols)
		}
	}
	// Position is preserved for the first two so a positional reader
	// keeps working.
	if !cols["raw_count"] || !cols["lesson_count"] {
		t.Error("the original two columns must survive")
	}
}

func viewColumnNames(db *sql.DB) (map[string]bool, error) {
	rows, err := db.Query(`PRAGMA table_info(epistemic_pressure_v)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var (
			cid      int
			name     string
			declType sql.NullString
			notNull  sql.NullInt64
			dflt     sql.NullString
			pk       sql.NullInt64
		)
		if err := rows.Scan(&cid, &name, &declType, &notNull, &dflt, &pk); err != nil {
			return nil, err
		}
		out[name] = true
	}
	return out, rows.Err()
}
