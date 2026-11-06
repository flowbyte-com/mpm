// migration_scheduled_wakes_delivery_state_test.go — the AT1–AT6
// acceptance tests for issue #178 Stage A (M1): the additive
// delivery-state migration on scheduled_wakes.
//
// Every test here is DETERMINISTIC. There is no sleep, no arbitrary
// wait and no timing-dependent assertion: the database is a disposable
// file-backed WAL database in a temporary workspace, and every
// observation is a read of committed state.
//
// The acceptance-test specifications are AT1–AT6 of
// docs/dev/issue-178-delivery-state-design-2026-11-04.md §5 (the design
// document lands with PR #219).
//
//	AT1  existing database migration
//	AT2  fresh database equivalence
//	AT3  whole-row preservation (byte-identical)
//	AT4  zero fabricated delivery history
//	AT5  idempotent migration
//	AT6  failed-migration rollback
//
// The specification's AT6 mechanism — "a trigger that aborts the third
// ALTER TABLE" — is not mechanically available: SQLite's ALTER TABLE
// ADD COLUMN does not fire triggers and exposes no trigger-visible
// hook, so no trigger can abort it. The injection used here is a
// trigger that aborts the migration's SENTINEL WRITE, which is later
// in the migration: the five ALTER TABLEs and the CREATE INDEX have
// already succeeded by then and must be undone. The rollback this
// exercises is therefore strictly STRONGER than the one the
// specification describes.
package internal

import (
	"database/sql"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The sentinel id and the index name come from the production code
// (m1SentinelID, m1IndexName) rather than from literals here, so a
// rename in the migration cannot leave the tests asserting on a name
// the product does not use.

// --- helpers ---------------------------------------------------------------

// rawOpen opens the database file directly, bypassing the manager, so
// a test can inspect and shape the schema between opens. The production
// write DSN is used, so the pragmas match the ones the manager sets.
func rawOpen(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", sqliteWriteDSN(path))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}

// scheduledWakesColumns returns the ordered column names of
// scheduled_wakes.
func scheduledWakesColumns(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM pragma_table_info('scheduled_wakes') ORDER BY cid`)
	if err != nil {
		t.Fatalf("read scheduled_wakes columns: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// scheduledWakesIndexes returns the sorted names of the indexes on
// scheduled_wakes.
func scheduledWakesIndexes(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM pragma_index_list('scheduled_wakes') ORDER BY name`)
	if err != nil {
		t.Fatalf("read scheduled_wakes indexes: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// sentinelCount counts the rows of one schema_migrations sentinel.
func sentinelCount(t *testing.T, db *sql.DB, id string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE id = ?`, id).Scan(&n); err != nil {
		t.Fatalf("count sentinel %s: %v", id, err)
	}
	return n
}

// rowSnapshot renders every scheduled_wakes row as a canonical string,
// keyed by id.
//
// Every value goes through quote(), so NULL, the empty string, 0 and
// the text "0" are all distinguishable and a TEXT value is captured
// exactly as stored. The result is compared as a map keyed by id, so
// the assertion is about the whole row set, not about one column.
func m1RowSnapshot(t *testing.T, db *sql.DB, cols []string) map[string]string {
	t.Helper()
	if len(cols) == 0 {
		t.Fatal("a snapshot needs at least the id column")
	}
	quoted := make([]string, 0, len(cols))
	for _, c := range cols {
		quoted = append(quoted, fmt.Sprintf("quote(%s)", c))
	}
	q := fmt.Sprintf(`SELECT id, %s FROM scheduled_wakes ORDER BY id`,
		strings.Join(quoted, ", "))
	rows, err := db.Query(q)
	if err != nil {
		t.Fatalf("snapshot rows: %v", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id string
		vals := make([]sql.NullString, len(cols))
		dest := make([]any, len(cols))
		for i := range vals {
			dest[i] = &vals[i]
		}
		if err := rows.Scan(append([]any{&id}, dest...)...); err != nil {
			t.Fatal(err)
		}
		parts := make([]string, 0, len(cols))
		for _, v := range vals {
			if !v.Valid {
				parts = append(parts, "<nil>")
				continue
			}
			parts = append(parts, v.String)
		}
		out[id] = joinSnapshotParts(parts)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// joinSnapshotParts joins the rendered column values with a separator
// that cannot appear inside a quoted SQLite value.
func joinSnapshotParts(parts []string) string {
	return strings.Join(parts, "\x1f")
}

// m1ColumnNames returns the five columns M1 adds, in the order it adds
// them.
func m1ColumnNames() []string {
	out := make([]string, 0, len(deliveryStateColumns))
	for _, c := range deliveryStateColumns {
		out = append(out, c.name)
	}
	return out
}

// insertRepresentativeRows inserts rows shaped like the output of each
// of the five retirement paths, plus a never-deliverable cron row and
// a pending notification row.
//
// The rows are inserted directly rather than produced by the paths
// themselves, for two reasons: two of the five (the retention sweeps)
// live in internal/scheduler, which internal/core cannot import; and
// the subject of AT4 is what the MIGRATION does to a row, not how the
// row came to exist. The shapes are taken from the real writers:
//   - W-a the scheduler settlement (internal/core/wake_settlement.go)
//     writes fired=1, fired_at, and clears metadata.last_error;
//   - W-b the agent's fold (internal/core/wake_tools.go) writes
//     fired=1, fired_at, with `AND fired = 0`;
//   - W-c the explicit resolver writes fired=1, fired_at and fired_by;
//   - W-d and W-e the two retention sweeps write fired=1, fired_at and
//     an `expired` audit object in metadata.
func insertRepresentativeRows(t *testing.T, db *sql.DB) {
	t.Helper()
	now := time.Now().Unix()
	type row struct {
		id           string
		metadata     string
		fired        int
		firedBy      any
		dispatchedAt any
	}
	rows := []row{
		{
			id:       "wake-settled",
			metadata: fmt.Sprintf(`{"kind":"drill","last_error":null}`),
			fired:    1,
		},
		{
			id:           "wake-folded",
			metadata:     `{"kind":"notification"}`,
			fired:        1,
			dispatchedAt: now - 60,
		},
		{
			id:           "wake-resolved",
			metadata:     `{"kind":"cascade"}`,
			fired:        1,
			firedBy:      "wake-resolver",
			dispatchedAt: now - 120,
		},
		{
			id:       "wake-swept-7d",
			metadata: fmt.Sprintf(`{"kind":"notification","expired":{"reason":"notification_7d_retention","at":%d,"target_time":%d}}`, now, now-604800),
			fired:    1,
		},
		{
			id:       "wake-swept-1h",
			metadata: fmt.Sprintf(`{"kind":"cron","expired":{"reason":"cron1h_retention","at":%d,"target_time":%d}}`, now, now-3600),
			fired:    1,
		},
		{
			// A cron row that is still pending. kind='cron' is never
			// deliverable — no handler is registered for it — so it
			// must never acquire a delivery history either.
			id:       "wake-cron-pending",
			metadata: `{"kind":"cron","directive_id":"epistemic-compaction"}`,
			fired:    0,
		},
		{
			id:       "wake-pending",
			metadata: `{"kind":"notification"}`,
			fired:    0,
		},
	}
	for _, r := range rows {
		var firedAt any
		if r.fired == 1 {
			firedAt = now - 30
		}
		if _, err := db.Exec(`INSERT INTO scheduled_wakes
			(id, target_time, reason, theory_id, recurring_rule, fired, fired_at,
			 created_by, created_at, metadata, dispatched_at, fired_by)
			VALUES (?, ?, ?, NULL, NULL, ?, ?, ?, ?, ?, ?, ?)`,
			r.id, now-3600, "representative "+r.id, r.fired, firedAt,
			"at-test", now-7200, r.metadata, r.dispatchedAt, r.firedBy,
		); err != nil {
			t.Fatalf("insert %s: %v", r.id, err)
		}
	}
}

// preM1Fixture builds a disposable workspace whose scheduled_wakes
// table is in the PRE-M1 shape and carries representative rows. It
// returns the workspace root and the database path.
//
// The pre-M1 shape is produced by removing exactly what M1 adds from a
// fully-initialised database: the partial index, the five columns and
// the sentinel. Nothing else is touched, so the fixture IS the real
// post-M1 schema minus M1 — and the migration has to add exactly those
// five columns back, which is what AT1 asserts.
func preM1Fixture(t *testing.T) (root string, dbPath string) {
	t.Helper()
	root = t.TempDir()
	dm, err := NewDatabaseManager(root)
	if err != nil {
		t.Fatalf("bootstrap the fixture: %v", err)
	}
	dbPath = dm.DBPath()
	dm.Close()

	db := rawOpen(t, dbPath)
	// The index first: SQLite refuses to drop a column that an index
	// references, so the index has to go before the columns do.
	if _, err := db.Exec(`DROP INDEX IF EXISTS ` + m1IndexName); err != nil {
		t.Fatalf("drop %s: %v", m1IndexName, err)
	}
	// Each column is dropped only if it is there. The fixture must not
	// presuppose that the migration under test has already run: the
	// same fixture has to build a pre-M1 database whether M1 is present
	// in the binary or not, or the tests would fail at fixture
	// construction instead of on their assertions.
	for _, c := range deliveryStateColumns {
		var n int
		if err := db.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('scheduled_wakes') WHERE name = ?`, c.name,
		).Scan(&n); err != nil {
			t.Fatalf("probe column %s: %v", c.name, err)
		}
		if n == 0 {
			continue
		}
		if _, err := db.Exec(fmt.Sprintf(`ALTER TABLE scheduled_wakes DROP COLUMN %s`, c.name)); err != nil {
			t.Fatalf("drop column %s: %v", c.name, err)
		}
	}
	if _, err := db.Exec(`DELETE FROM schema_migrations WHERE id = ?`, m1SentinelID); err != nil {
		t.Fatalf("delete the M1 sentinel: %v", err)
	}
	insertRepresentativeRows(t, db)
	return root, dbPath
}

// postM1Columns returns the ordered column set of scheduled_wakes
// after M1: the pre-M1 set followed by the five M1 columns.
//
// It is derived from the fixture rather than written out, so the
// expectation can never drift from the production DDL.
func postM1Columns(t *testing.T) []string {
	t.Helper()
	_, dbPath := preM1Fixture(t)
	pre := scheduledWakesColumns(t, rawOpen(t, dbPath))
	return append(append([]string{}, pre...), m1ColumnNames()...)
}

// --- AT1 — existing database migration -------------------------------------

// TestDeliveryStateMigration_AT1_ExistingDatabaseMigration opens a
// pre-M1 database with the new binary and asserts that the migration
// added exactly the five columns, recorded the sentinel exactly once
// and created the partial index.
func TestDeliveryStateMigration_AT1_ExistingDatabaseMigration(t *testing.T) {
	root, dbPath := preM1Fixture(t)

	pre := scheduledWakesColumns(t, rawOpen(t, dbPath))
	for _, c := range deliveryStateColumns {
		if containsString(pre, c.name) {
			t.Fatalf("the fixture is not pre-M1: %s already exists", c.name)
		}
	}
	if n := sentinelCount(t, rawOpen(t, dbPath), m1SentinelID); n != 0 {
		t.Fatalf("the fixture is not pre-M1: the sentinel count is %d, want 0", n)
	}

	dm, err := NewDatabaseManager(root)
	if err != nil {
		t.Fatalf("the open must succeed: %v", err)
	}
	dm.Close()

	db := rawOpen(t, dbPath)
	post := scheduledWakesColumns(t, db)
	want := append(append([]string{}, pre...), m1ColumnNames()...)
	t.Logf("before: %v", pre)
	t.Logf("after:  %v", post)
	if !reflect.DeepEqual(post, want) {
		t.Fatalf("the migrated column set is wrong:\n got  %v\n want %v", post, want)
	}
	if n := sentinelCount(t, db, m1SentinelID); n != 1 {
		t.Fatalf("the sentinel count is %d, want exactly 1", n)
	}
	if !containsString(scheduledWakesIndexes(t, db), m1IndexName) {
		t.Fatalf("%s must exist after the migration", m1IndexName)
	}
}

// --- AT2 — fresh database equivalence ---------------------------------------

// TestDeliveryStateMigration_AT2_FreshDatabaseEquivalence asserts that a
// FRESH database and a MIGRATED one have identical schemas: the base
// DDL does not carry the five columns, so both paths produce them
// through the same migration.
func TestDeliveryStateMigration_AT2_FreshDatabaseEquivalence(t *testing.T) {
	root := t.TempDir()
	dm, err := NewDatabaseManager(root)
	if err != nil {
		t.Fatalf("the open must succeed: %v", err)
	}
	dbPath := dm.DBPath()
	dm.Close()

	db := rawOpen(t, dbPath)
	fresh := scheduledWakesColumns(t, db)
	migrated := postM1Columns(t)
	if !reflect.DeepEqual(fresh, migrated) {
		t.Fatalf("a fresh database and a migrated one must have identical schemas:\n fresh    %v\n migrated %v", fresh, migrated)
	}
	if n := sentinelCount(t, db, m1SentinelID); n != 1 {
		t.Fatalf("the sentinel count is %d, want exactly 1", n)
	}
	if !containsString(scheduledWakesIndexes(t, db), m1IndexName) {
		t.Fatalf("%s must exist on a fresh database too", m1IndexName)
	}
}

// --- AT3 — whole-row preservation ------------------------------------------

// TestDeliveryStateMigration_AT3_WholeRowPreservation asserts that
// every pre-existing row's every pre-existing column is byte-identical
// after the migration.
//
// The comparison is over the whole row set, keyed by id — not over one
// column, and not over `fired` alone.
func TestDeliveryStateMigration_AT3_WholeRowPreservation(t *testing.T) {
	root, dbPath := preM1Fixture(t)

	// The SAME columns are compared before and after: the five M1
	// columns are new and are covered by AT1 and AT4, not here.
	pre := scheduledWakesColumns(t, rawOpen(t, dbPath))
	before := m1RowSnapshot(t, rawOpen(t, dbPath), pre)
	if len(before) == 0 {
		t.Fatal("the fixture must carry rows")
	}

	dm, err := NewDatabaseManager(root)
	if err != nil {
		t.Fatalf("the open must succeed: %v", err)
	}
	dm.Close()

	after := m1RowSnapshot(t, rawOpen(t, dbPath), pre)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("the migration changed existing rows:\n before %v\n after  %v", before, after)
	}
}

// --- AT4 — zero fabricated delivery history --------------------------------

// TestDeliveryStateMigration_AT4_ZeroFabricatedDeliveryHistory asserts
// that the migration fabricates no delivery history.
//
// Every row must carry a NULL delivery_state and a NULL delivered_at —
// including the rows retired by each of the five retirement paths, and
// including the never-deliverable cron rows. A backfill would have
// claimed a delivery for four of those five paths, none of which ever
// ran a handler.
func TestDeliveryStateMigration_AT4_ZeroFabricatedDeliveryHistory(t *testing.T) {
	root, dbPath := preM1Fixture(t)

	dm, err := NewDatabaseManager(root)
	if err != nil {
		t.Fatalf("the open must succeed: %v", err)
	}
	dm.Close()

	db := rawOpen(t, dbPath)
	rows, err := db.Query(`SELECT id, fired, delivery_state, lease_expires_at,
		claimed_by, claimed_at, delivered_at FROM scheduled_wakes ORDER BY id`)
	if err != nil {
		t.Fatalf("read the delivery columns: %v", err)
	}
	defer rows.Close()

	seenFired, seenCron, total := 0, 0, 0
	for rows.Next() {
		var id string
		var fired int
		var state, claimedBy sql.NullString
		var lease, claimedAt, delivered sql.NullInt64
		if err := rows.Scan(&id, &fired, &state, &lease, &claimedBy, &claimedAt, &delivered); err != nil {
			t.Fatal(err)
		}
		total++
		if fired == 1 {
			seenFired++
		}
		if state.Valid || lease.Valid || claimedBy.Valid || claimedAt.Valid || delivered.Valid {
			t.Fatalf("row %q carries a fabricated delivery history: state=%v lease=%v claimed_by=%v claimed_at=%v delivered_at=%v",
				id, state, lease, claimedBy, claimedAt, delivered)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if total == 0 {
		t.Fatal("the fixture must carry rows")
	}
	if seenFired < 5 {
		t.Fatalf("the fixture must cover all five retirement paths, only %d retired rows are present", seenFired)
	}
	// The cron rows: one retired by the 1-hour sweep and one still
	// pending. Neither is deliverable, so neither may carry a
	// delivery history.
	var cronRows, cronWithHistory int
	if err := db.QueryRow(`SELECT COUNT(*) FROM scheduled_wakes
		WHERE json_extract(metadata, '$.kind') = 'cron'`).Scan(&cronRows); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM scheduled_wakes
		WHERE json_extract(metadata, '$.kind') = 'cron'
		  AND (delivery_state IS NOT NULL OR delivered_at IS NOT NULL)`).Scan(&cronWithHistory); err != nil {
		t.Fatal(err)
	}
	if cronRows < 2 {
		t.Fatalf("the fixture must carry both a retired and a pending cron row, found %d", cronRows)
	}
	if cronWithHistory != 0 {
		t.Fatalf("%d cron rows carry a delivery history; kind='cron' is never deliverable", cronWithHistory)
	}
	seenCron = cronRows
	_ = seenCron
}

// --- AT5 — idempotent migration --------------------------------------------

// TestDeliveryStateMigration_AT5_IdempotentMigration opens the same
// database three times and asserts that the second and third runs
// change nothing: not the column set, not the row set, not the index
// list, and not the sentinel count.
func TestDeliveryStateMigration_AT5_IdempotentMigration(t *testing.T) {
	root, dbPath := preM1Fixture(t)

	type state struct {
		columns  []string
		indexes  []string
		rows     map[string]string
		sentinel int
	}
	snapshot := func() state {
		db := rawOpen(t, dbPath)
		cols := scheduledWakesColumns(t, db)
		return state{
			columns:  cols,
			indexes:  scheduledWakesIndexes(t, db),
			rows:     m1RowSnapshot(t, db, cols),
			sentinel: sentinelCount(t, db, m1SentinelID),
		}
	}

	open := func() {
		dm, err := NewDatabaseManager(root)
		if err != nil {
			t.Fatalf("the open must succeed: %v", err)
		}
		dm.Close()
	}

	open() // run 1: the migration itself
	first := snapshot()
	if first.sentinel != 1 {
		t.Fatalf("after the first run the sentinel count is %d, want 1", first.sentinel)
	}

	for run := 2; run <= 3; run++ {
		open()
		got := snapshot()
		if !reflect.DeepEqual(got.columns, first.columns) {
			t.Fatalf("run %d changed the column set:\n before %v\n after  %v", run, first.columns, got.columns)
		}
		if !reflect.DeepEqual(got.indexes, first.indexes) {
			t.Fatalf("run %d changed the index list:\n before %v\n after  %v", run, first.indexes, got.indexes)
		}
		if !reflect.DeepEqual(got.rows, first.rows) {
			t.Fatalf("run %d changed the row set:\n before %v\n after  %v", run, first.rows, got.rows)
		}
		if got.sentinel != 1 {
			t.Fatalf("run %d recorded the sentinel %d times, want exactly 1", run, got.sentinel)
		}
	}
}

// --- AT6 — failed-migration rollback ----------------------------------------

// TestDeliveryStateMigration_AT6_FailedMigrationRollsBack injects a
// failure into the migration and asserts that the failure is atomic:
// the migration reports the error, the column set is unchanged, no
// sentinel is recorded, the row set is unchanged, and the index was
// not created.
//
// The injection is a trigger that aborts the sentinel write. By then
// the five ALTER TABLEs and the CREATE INDEX have SUCCEEDED, so the
// rollback has to undo real DDL — which is a stronger assertion than
// aborting the third ALTER TABLE (a mechanism SQLite does not offer:
// ALTER TABLE ADD COLUMN fires no triggers).
func TestDeliveryStateMigration_AT6_FailedMigrationRollsBack(t *testing.T) {
	root, dbPath := preM1Fixture(t)

	pre := scheduledWakesColumns(t, rawOpen(t, dbPath))
	before := m1RowSnapshot(t, rawOpen(t, dbPath), pre)

	// The injection.
	if _, err := rawOpen(t, dbPath).Exec(`CREATE TRIGGER block_m1_sentinel
		BEFORE INSERT ON schema_migrations
		WHEN NEW.id = '` + m1SentinelID + `'
		BEGIN SELECT RAISE(ABORT, 'injected M1 failure'); END`); err != nil {
		t.Fatalf("install the failure injection: %v", err)
	}

	dm, err := NewDatabaseManager(root)
	if err == nil {
		dm.Close()
		t.Fatal("the open must surface the injected migration failure")
	}
	t.Logf("the injected failure surfaced as: %v", err)

	db := rawOpen(t, dbPath)
	after := scheduledWakesColumns(t, db)
	if !reflect.DeepEqual(after, pre) {
		t.Fatalf("the failed migration changed the column set:\n before %v\n after  %v", pre, after)
	}
	if n := sentinelCount(t, db, m1SentinelID); n != 0 {
		t.Fatalf("the failed migration recorded the sentinel %d times, want 0", n)
	}
	if containsString(scheduledWakesIndexes(t, db), m1IndexName) {
		t.Fatalf("the failed migration left %s behind", m1IndexName)
	}
	rows := m1RowSnapshot(t, db, pre)
	if !reflect.DeepEqual(before, rows) {
		t.Fatalf("the failed migration changed the row set:\n before %v\n after  %v", before, rows)
	}
}

// --- the FTS shadow --------------------------------------------------------

// TestDeliveryStateMigration_TheFTSShadowStaysConsistent asserts that
// the migration leaves the FTS5 shadow of scheduled_wakes consistent
// with its base table.
//
// The shadow is maintained by the scheduled_wakes_ai/ad/au triggers.
// ADD COLUMN does not fire them, so the shadow must be exactly what it
// was — and the base table's row count must still match it.
func TestDeliveryStateMigration_TheFTSShadowStaysConsistent(t *testing.T) {
	root, dbPath := preM1Fixture(t)

	db := rawOpen(t, dbPath)
	var baseBefore, shadowBefore int
	if err := db.QueryRow(`SELECT COUNT(*) FROM scheduled_wakes`).Scan(&baseBefore); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM scheduled_wakes_fts`).Scan(&shadowBefore); err != nil {
		t.Fatal(err)
	}
	if baseBefore == 0 {
		t.Fatal("the fixture must carry rows")
	}
	if baseBefore != shadowBefore {
		t.Fatalf("the fixture's shadow is already inconsistent: %d base rows, %d shadow rows", baseBefore, shadowBefore)
	}

	dm, err := NewDatabaseManager(root)
	if err != nil {
		t.Fatalf("the open must succeed: %v", err)
	}
	dm.Close()

	db2 := rawOpen(t, dbPath)
	var baseAfter, shadowAfter int
	if err := db2.QueryRow(`SELECT COUNT(*) FROM scheduled_wakes`).Scan(&baseAfter); err != nil {
		t.Fatal(err)
	}
	if err := db2.QueryRow(`SELECT COUNT(*) FROM scheduled_wakes_fts`).Scan(&shadowAfter); err != nil {
		t.Fatal(err)
	}
	if baseAfter != baseBefore || shadowAfter != shadowBefore {
		t.Fatalf("the migration disturbed the FTS shadow: base %d -> %d, shadow %d -> %d",
			baseBefore, baseAfter, shadowBefore, shadowAfter)
	}
}

// TestDeliveryStateMigration_TheIndexIsForwardOnly asserts the
// forward-only rule the schema is supposed to enforce for itself:
// while the partial index exists, SQLite refuses to drop the column it
// references.
func TestDeliveryStateMigration_TheIndexIsForwardOnly(t *testing.T) {
	root, dbPath := preM1Fixture(t)
	dm, err := NewDatabaseManager(root)
	if err != nil {
		t.Fatal(err)
	}
	dm.Close()

	_, dropErr := rawOpen(t, dbPath).Exec(`ALTER TABLE scheduled_wakes DROP COLUMN delivery_state`)
	if dropErr == nil {
		t.Fatal("dropping delivery_state must be refused while the partial index exists")
	}
	// The refusal must come from the INDEX, not from the column simply
	// being absent: "no such column: delivery_state" on its own would
	// be the wrong reason and would pass for the wrong cause.
	if !strings.Contains(dropErr.Error(), m1IndexName) {
		t.Fatalf("the refusal must name %s, got: %v", m1IndexName, dropErr)
	}
	t.Logf("the refusal is: %v", dropErr)
}

// TestDeliveryStateMigration_TheIndexSurvivesTheDumpValidator asserts
// that `mpm restore-db` would accept a dump that carries M1's index.
//
// The restore validator is a security allow-list: a dump declaring a
// statement it does not recognise is REFUSED. The new index is a
// CREATE INDEX on scheduled_wakes, and scheduled_wakes is on the
// allow-list — but the assertion is made here rather than assumed,
// because a rejected dump would make the migration unrecoverable by
// restore.
func TestDeliveryStateMigration_TheIndexSurvivesTheDumpValidator(t *testing.T) {
	root, dbPath := preM1Fixture(t)
	dm, err := NewDatabaseManager(root)
	if err != nil {
		t.Fatal(err)
	}
	dm.Close()

	// The index's own SQL, exactly as sqlite_master records it and as
	// `sqlite3 .dump` would emit it.
	var indexSQL string
	if err := rawOpen(t, dbPath).QueryRow(
		`SELECT sql FROM sqlite_master WHERE type = 'index' AND name = ?`, m1IndexName,
	).Scan(&indexSQL); err != nil {
		t.Fatalf("read the index SQL: %v", err)
	}
	if indexSQL == "" {
		t.Fatalf("%s must exist", m1IndexName)
	}
	t.Logf("the index's SQL: %s", indexSQL)

	validator := NewCanonicalDumpValidator()
	if _, err := validator.Prepare(indexSQL + ";"); err != nil {
		t.Fatalf("the restore validator must accept a dump carrying %s: %v", m1IndexName, err)
	}
}

// --- helpers for the repair and recovery assertions ------------------------

// m1ColumnDefs renders every column of scheduled_wakes as a canonical
// definition string: name, declared type, NOT NULL, default and
// primary-key position, in column order.
//
// The names alone are not enough: a column that exists with the WRONG
// type is a different schema, and the migration's probe is by name. A
// definition snapshot makes "the column is there" and "the column is
// the column the contract specifies" two different assertions.
func m1ColumnDefs(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT name, type, "notnull", dflt_value, pk
		FROM pragma_table_info('scheduled_wakes') ORDER BY cid`)
	if err != nil {
		t.Fatalf("read scheduled_wakes definitions: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name, typ string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&name, &typ, &notnull, &dflt, &pk); err != nil {
			t.Fatal(err)
		}
		def := "<none>"
		if dflt.Valid {
			def = dflt.String
		}
		out = append(out, fmt.Sprintf("%s %s notnull=%d default=%s pk=%d", name, typ, notnull, def, pk))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// m1SchemaSnapshot renders the schema of scheduled_wakes and the
// delivery-state index as a canonical, comparable slice.
//
// It reads sqlite_master, so it captures the schema TEXT — the
// CREATE TABLE as SQLite rewrote it after the ALTERs, and the CREATE
// INDEX — not merely the names. Two databases that converged by
// different routes must produce the same text, which is a stronger
// statement than "both have the same tables".
func m1SchemaSnapshot(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT type, name, sql FROM sqlite_master
		WHERE (type = 'table' AND name = 'scheduled_wakes')
		   OR (type = 'index' AND name = ?)
		ORDER BY type, name`, m1IndexName)
	if err != nil {
		t.Fatalf("read the schema snapshot: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var typ, name, sqlText string
		if err := rows.Scan(&typ, &name, &sqlText); err != nil {
			t.Fatal(err)
		}
		out = append(out, typ+" "+name+": "+sqlText)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// m1ReferenceSchema returns the schema a correctly migrated database
// has: the pre-M1 fixture, opened once by the new binary.
//
// It is the reference the repair and recovery paths are compared
// against, so "the repaired database looks like a migrated one" is an
// assertion about two independently produced databases rather than a
// restatement of what the repair just did.
func m1ReferenceSchema(t *testing.T) []string {
	t.Helper()
	root, dbPath := preM1Fixture(t)
	dm, err := NewDatabaseManager(root)
	if err != nil {
		t.Fatalf("the reference database must open: %v", err)
	}
	dm.Close()
	return m1SchemaSnapshot(t, rawOpen(t, dbPath))
}

// m1AssertAxisIsComplete asserts the post-migration contract: the five
// columns are present with their specified definitions, the index is
// present, and the sentinel is recorded exactly once.
//
// The column definitions are compared against the PRODUCTION list
// (deliveryStateColumns), so the expectation cannot drift from the
// migration.
func m1AssertAxisIsComplete(t *testing.T, db *sql.DB) {
	t.Helper()
	defs := m1ColumnDefs(t, db)
	byName := map[string]string{}
	for _, d := range defs {
		byName[strings.SplitN(d, " ", 2)[0]] = d
	}
	for _, col := range deliveryStateColumns {
		got, ok := byName[col.name]
		if !ok {
			t.Fatalf("the column %s is missing: %v", col.name, defs)
		}
		want := fmt.Sprintf("%s %s notnull=0 default=<none> pk=0", col.name, col.decl)
		if got != want {
			t.Fatalf("the column %s has the wrong definition:\n got  %s\n want %s", col.name, got, want)
		}
	}
	if !containsString(scheduledWakesIndexes(t, db), m1IndexName) {
		t.Fatalf("%s must exist", m1IndexName)
	}
	if n := sentinelCount(t, db, m1SentinelID); n != 1 {
		t.Fatalf("the sentinel count is %d, want exactly 1", n)
	}
}

// --- R1: the index is self-healing ------------------------------------------

// TestDeliveryStateMigration_TheIndexIsRecreatedWhenAbsent is the
// regression test for R1: the index must be restored even when the
// sentinel is already present.
//
// The scenario is the one the review measured. The index has no reader
// in Stage A — its predicate is `delivery_state IS NOT NULL` and every
// row is NULL — so it matches zero rows and exists purely to enforce
// the forward-only rule. That makes it the most droppable object in
// the schema, and a sentinel-guarded migration never noticed the loss.
//
// The sequence is: migrate, drop the index by hand, reopen, and assert
// that the index is back and that `DROP COLUMN delivery_state` is
// refused again.
func TestDeliveryStateMigration_TheIndexIsRecreatedWhenAbsent(t *testing.T) {
	root, dbPath := preM1Fixture(t)

	// 1. Migrate.
	dm, err := NewDatabaseManager(root)
	if err != nil {
		t.Fatalf("the open must succeed: %v", err)
	}
	dm.Close()
	if !containsString(scheduledWakesIndexes(t, rawOpen(t, dbPath)), m1IndexName) {
		t.Fatalf("%s must exist after the migration", m1IndexName)
	}

	// 2. Drop the index, leaving the sentinel in place.
	if _, err := rawOpen(t, dbPath).Exec(`DROP INDEX ` + m1IndexName); err != nil {
		t.Fatalf("drop %s: %v", m1IndexName, err)
	}
	if containsString(scheduledWakesIndexes(t, rawOpen(t, dbPath)), m1IndexName) {
		t.Fatalf("the fixture is wrong: %s still exists", m1IndexName)
	}
	if n := sentinelCount(t, rawOpen(t, dbPath), m1SentinelID); n != 1 {
		t.Fatalf("the sentinel count is %d, want 1 — the scenario needs the sentinel in place", n)
	}

	// 3. Reopen. The index must be back.
	dm, err = NewDatabaseManager(root)
	if err != nil {
		t.Fatalf("the reopen must succeed: %v", err)
	}
	dm.Close()
	db := rawOpen(t, dbPath)
	if !containsString(scheduledWakesIndexes(t, db), m1IndexName) {
		t.Fatalf("%s must be recreated on the next open", m1IndexName)
	}

	// 4. And the forward-only rule must be back with it.
	_, dropErr := rawOpen(t, dbPath).Exec(`ALTER TABLE scheduled_wakes DROP COLUMN delivery_state`)
	if dropErr == nil {
		t.Fatal("dropping delivery_state must be refused while the partial index exists")
	}
	if !strings.Contains(dropErr.Error(), m1IndexName) {
		t.Fatalf("the refusal must name %s, got: %v", m1IndexName, dropErr)
	}
	t.Logf("the index was restored and the refusal is: %v", dropErr)
}

// TestDeliveryStateMigration_TheAxisIsRestoredWhenDropped extends R1 to
// the full loss: the index is dropped AND the column it protects is
// dropped, which is the only order in which the column can go.
//
// The repair has to restore the column too, because the index cannot
// be recreated without it — a `CREATE INDEX` over a missing column
// fails, so an index-only repair would leave the database unopenable.
//
// The restored column is NULL-defaulted and nothing reads or writes the
// axis yet, so restoring the shape loses no data. The row set must be
// byte-identical across the repair.
func TestDeliveryStateMigration_TheAxisIsRestoredWhenDropped(t *testing.T) {
	root, dbPath := preM1Fixture(t)

	dm, err := NewDatabaseManager(root)
	if err != nil {
		t.Fatalf("the open must succeed: %v", err)
	}
	dm.Close()

	before := m1RowSnapshot(t, rawOpen(t, dbPath), scheduledWakesColumns(t, rawOpen(t, dbPath)))

	// Drop the index, then the column it was protecting.
	if _, err := rawOpen(t, dbPath).Exec(`DROP INDEX ` + m1IndexName); err != nil {
		t.Fatalf("drop %s: %v", m1IndexName, err)
	}
	if _, err := rawOpen(t, dbPath).Exec(`ALTER TABLE scheduled_wakes DROP COLUMN delivery_state`); err != nil {
		t.Fatalf("drop the column: %v", err)
	}
	damaged := scheduledWakesColumns(t, rawOpen(t, dbPath))
	if containsString(damaged, "delivery_state") {
		t.Fatalf("the fixture is wrong: delivery_state still exists")
	}

	// Reopen: the axis must come back.
	dm, err = NewDatabaseManager(root)
	if err != nil {
		t.Fatalf("the reopen must succeed: %v", err)
	}
	dm.Close()

	db := rawOpen(t, dbPath)
	m1AssertAxisIsComplete(t, db)
	if !containsString(scheduledWakesColumns(t, db), "delivery_state") {
		t.Fatalf("delivery_state must be restored: %v", scheduledWakesColumns(t, db))
	}

	// The rows must be untouched by the repair.
	after := m1RowSnapshot(t, db, scheduledWakesColumns(t, db))
	for id, want := range before {
		// The five new columns are appended by the repair, so the
		// before-snapshot's columns are a prefix of the after-row.
		if !strings.HasPrefix(after[id], want) {
			t.Fatalf("the repair changed the existing columns of row %q:\n before %q\n after  %q", id, want, after[id])
		}
	}

	// And the forward-only rule must be enforced again.
	if _, err := rawOpen(t, dbPath).Exec(`ALTER TABLE scheduled_wakes DROP COLUMN delivery_state`); err == nil {
		t.Fatal("dropping delivery_state must be refused while the partial index exists")
	}
}

// TestDeliveryStateMigration_AFailingIndexCreationIsNotSwallowed asserts
// that a failure to create the index is a FATAL one.
//
// The index is created inside the fatal migration transaction, so an
// error there rolls the whole migration back and NewDatabaseManager
// returns an error. If the repair were best-effort — a warning, a
// swallowed error — a database whose index could not be created would
// open silently with the forward-only rule unenforced, which is
// exactly the silent failure the fatal transaction exists to prevent.
//
// The injection is an object that occupies the index's name: SQLite
// refuses to create an index whose name is already taken by a table,
// so `CREATE INDEX IF NOT EXISTS` fails with a name clash.
func TestDeliveryStateMigration_AFailingIndexCreationIsNotSwallowed(t *testing.T) {
	root, dbPath := preM1Fixture(t)

	dm, err := NewDatabaseManager(root)
	if err != nil {
		t.Fatalf("the open must succeed: %v", err)
	}
	dm.Close()

	// Drop the index and put a table in its place.
	if _, err := rawOpen(t, dbPath).Exec(`DROP INDEX ` + m1IndexName); err != nil {
		t.Fatalf("drop %s: %v", m1IndexName, err)
	}
	if _, err := rawOpen(t, dbPath).Exec(`CREATE TABLE ` + m1IndexName + ` (blocking TEXT)`); err != nil {
		t.Fatalf("install the conflicting object: %v", err)
	}

	// The open must FAIL, and the error must name the index.
	dm, err = NewDatabaseManager(root)
	if err == nil {
		dm.Close()
		t.Fatal("the open must surface the index-creation failure")
	}
	if !strings.Contains(err.Error(), m1IndexName) {
		t.Fatalf("the error must name %s, got: %v", m1IndexName, err)
	}
	t.Logf("the injected index failure surfaced as: %v", err)

	// The failed open must not have left the database broken: removing
	// the cause and opening again must succeed, with the axis complete.
	if _, err := rawOpen(t, dbPath).Exec(`DROP TABLE ` + m1IndexName); err != nil {
		t.Fatalf("remove the conflicting object: %v", err)
	}
	dm, err = NewDatabaseManager(root)
	if err != nil {
		t.Fatalf("the open must succeed once the cause is removed: %v", err)
	}
	dm.Close()
	m1AssertAxisIsComplete(t, rawOpen(t, dbPath))
}

// --- R2: convergence and recovery -------------------------------------------

// TestDeliveryStateMigration_ConvergesOnAPartiallyMigratedDatabase
// covers the first of the two paths a real interruption takes: a crash
// between the ALTERs and the sentinel.
//
// The fixture is a database carrying a SUBSET of the five columns and
// no sentinel — the state a process killed between the third and
// fourth ALTER leaves behind. The migration must converge on it: all
// five columns with their specified definitions, the index, the
// sentinel recorded exactly once, the original rows byte-identical, and
// a schema identical to a correctly migrated database's.
//
// The last assertion is the one that matters. A migration that
// converged on a *different* schema would still have five columns and
// would still pass a column-count check.
func TestDeliveryStateMigration_ConvergesOnAPartiallyMigratedDatabase(t *testing.T) {
	root, dbPath := preM1Fixture(t)

	// Apply the first two ALTERs and the index by hand, and leave the
	// sentinel absent — the interrupted state.
	partial := deliveryStateColumns[:2]
	db := rawOpen(t, dbPath)
	for _, col := range partial {
		if _, err := db.Exec(fmt.Sprintf(`ALTER TABLE scheduled_wakes ADD COLUMN %s %s`, col.name, col.decl)); err != nil {
			t.Fatalf("apply the partial migration: %v", err)
		}
	}
	if _, err := db.Exec(deliveryStateIndexDDL); err != nil {
		t.Fatalf("apply the partial index: %v", err)
	}
	if n := sentinelCount(t, rawOpen(t, dbPath), m1SentinelID); n != 0 {
		t.Fatalf("the sentinel count is %d, want 0 — the scenario needs it absent", n)
	}
	damaged := scheduledWakesColumns(t, rawOpen(t, dbPath))
	if len(damaged) == 0 || !containsString(damaged, "delivery_state") || !containsString(damaged, "lease_expires_at") {
		t.Fatalf("the fixture is wrong: %v", damaged)
	}
	if containsString(damaged, "claimed_by") {
		t.Fatalf("the fixture is wrong: claimed_by already exists: %v", damaged)
	}
	t.Logf("the partially-migrated database has columns %v", damaged)

	before := m1RowSnapshot(t, rawOpen(t, dbPath), scheduledWakesColumns(t, rawOpen(t, dbPath)))

	// Open: the migration must converge.
	dm, err := NewDatabaseManager(root)
	if err != nil {
		t.Fatalf("the open must succeed on a partially-migrated database: %v", err)
	}
	dm.Close()

	converged := rawOpen(t, dbPath)
	m1AssertAxisIsComplete(t, converged)

	// The rows must be byte-identical, and the pre-existing columns
	// must be a prefix of the post-migration columns.
	after := m1RowSnapshot(t, converged, scheduledWakesColumns(t, converged))
	if len(before) != len(after) {
		t.Fatalf("the row count changed: %d -> %d", len(before), len(after))
	}
	for id, want := range before {
		if !strings.HasPrefix(after[id], want) {
			t.Fatalf("the convergence changed the existing columns of row %q:\n before %q\n after  %q", id, want, after[id])
		}
	}

	// The converged schema must be identical to a correctly migrated
	// database's — not merely equivalent in column count.
	reference := m1ReferenceSchema(t)
	got := m1SchemaSnapshot(t, converged)
	if !reflect.DeepEqual(got, reference) {
		t.Fatalf("the converged schema differs from a migrated database's:\n got  %v\n want %v", got, reference)
	}
}

// TestDeliveryStateMigration_RecoversAfterAFailedMigration is the
// natural completion of AT6.
//
// AT6 proves the failure is ATOMIC — the failed migration leaves
// nothing behind. The missing half is that the system RECOVERS: once
// the cause is removed, the next open must complete the migration and
// leave a complete, correct axis.
//
// The sequence is: inject the failure exactly as AT6 does, assert the
// open fails and the state is unchanged, remove the cause, open again,
// and assert the axis is complete, the rows are untouched, and the
// schema is identical to a correctly migrated database's.
func TestDeliveryStateMigration_RecoversAfterAFailedMigration(t *testing.T) {
	root, dbPath := preM1Fixture(t)

	before := m1RowSnapshot(t, rawOpen(t, dbPath), scheduledWakesColumns(t, rawOpen(t, dbPath)))

	// The injection: a trigger that aborts the sentinel write.
	if _, err := rawOpen(t, dbPath).Exec(`CREATE TRIGGER block_m1_sentinel
		BEFORE INSERT ON schema_migrations
		WHEN NEW.id = '` + m1SentinelID + `'
		BEGIN SELECT RAISE(ABORT, 'injected M1 failure'); END`); err != nil {
		t.Fatalf("install the failure injection: %v", err)
	}

	// The open must fail.
	dm, err := NewDatabaseManager(root)
	if err == nil {
		dm.Close()
		t.Fatal("the open must surface the injected migration failure")
	}
	t.Logf("the injected failure surfaced as: %v", err)

	// And the failed migration must have left nothing behind.
	damaged := rawOpen(t, dbPath)
	if containsString(scheduledWakesColumns(t, damaged), "delivery_state") {
		t.Fatalf("the failed migration left a column behind: %v", scheduledWakesColumns(t, damaged))
	}
	if containsString(scheduledWakesIndexes(t, damaged), m1IndexName) {
		t.Fatalf("the failed migration left %s behind", m1IndexName)
	}
	if n := sentinelCount(t, damaged, m1SentinelID); n != 0 {
		t.Fatalf("the failed migration recorded the sentinel %d times, want 0", n)
	}

	// Remove the cause.
	if _, err := rawOpen(t, dbPath).Exec(`DROP TRIGGER block_m1_sentinel`); err != nil {
		t.Fatalf("remove the failure injection: %v", err)
	}

	// The next open must complete the migration.
	dm, err = NewDatabaseManager(root)
	if err != nil {
		t.Fatalf("the open must succeed once the cause is removed: %v", err)
	}
	dm.Close()

	recovered := rawOpen(t, dbPath)
	m1AssertAxisIsComplete(t, recovered)

	// The rows must be byte-identical across the failed attempt and the
	// successful one.
	after := m1RowSnapshot(t, recovered, scheduledWakesColumns(t, recovered))
	if len(before) != len(after) {
		t.Fatalf("the row count changed: %d -> %d", len(before), len(after))
	}
	for id, want := range before {
		if !strings.HasPrefix(after[id], want) {
			t.Fatalf("the recovery changed the existing columns of row %q:\n before %q\n after  %q", id, want, after[id])
		}
	}

	// And the recovered schema must be identical to a correctly
	// migrated database's.
	reference := m1ReferenceSchema(t)
	got := m1SchemaSnapshot(t, recovered)
	if !reflect.DeepEqual(got, reference) {
		t.Fatalf("the recovered schema differs from a migrated database's:\n got  %v\n want %v", got, reference)
	}
}
