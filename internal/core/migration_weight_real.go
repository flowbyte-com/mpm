package internal

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// MigrateWeightToReal converts memories.weight and memory_revisions.weight
// from INTEGER-declared columns to REAL.
//
// Why: SQLite's dynamic typing has been storing fractional weight values
// (82.5, 90.5, 88.5, 70.5, 10.5, …) in columns declared INTEGER. SQLite
// does not enforce declared types at write time — it stores the value
// with its natural type. The schema-vs-data drift only surfaces at read
// time: Go's database/sql Scan fails with
// `converting driver.Value type float64 ("82.5") to a int: invalid syntax`
// when reading those rows into an `int` target. ConsolidateMemories has
// been silently skipping affected rows via the silent-continue pattern —
// load-bearing memory consolidation has been silently broken.
//
// Strategy: ADD/UPDATE/DROP/RENAME column dance. Why not the rename-recreate
// table dance:
//
//   - Renaming the original table rewrites dependent views and triggers in
//     sqlite_master to reference the renamed copy. After DROP TABLE on that
//     renamed copy, views and triggers reference a non-existent table —
//     leaving them stale. The next migration attempt fails with
//     "error in view <name>: no such table" (2026-08-13 production hit).
//
//   - The column dance is in-place (ALTER TABLE ADD/DROP/RENAME COLUMN do
//     not require data copies), keeps views and triggers attached to the
//     same table, and only requires dropping+recreating the indexes,
//     triggers, and views that directly reference the `weight` column.
//
// DROP COLUMN refuses to operate while the column is referenced by:
//
//   - indexes ("error in index <name> after drop column") — production
//     hit on 2026-08-13 15:23 (idx_memories_session collision, addressed
//     by the first fix attempt)
//   - triggers ("error in trigger <name> after drop column: no such
//     column: NEW.weight") — production hit on 2026-08-13 18:37
//   - views ("error in view <name> after drop column: no such column:
//     m.weight") — production hit on 2026-08-13 18:40
//
// All three must be dropped first, then recreated after the RENAME COLUMN.
//
// Requires SQLite 3.35.0+ for DROP COLUMN; the bundled SQLite in
// mattn/go-sqlite3 v1.14.37 is 3.51.3.
//
// Idempotent via the schema_migrations sentinel `weight_real_v1`.
//
// Must run AFTER BaseTables has executed (memories and memory_revisions
// must exist). Caller (DatabaseManager.init) wraps in a transaction:
//
//	tx.Begin()
//	MigrateWeightToReal(tx)
//	tx.Commit()
func MigrateWeightToReal(tx *sql.Tx) error {
	// 1. Sentinel check — bail if already applied.
	var applied int
	err := tx.QueryRow(
		`SELECT COUNT(*) FROM schema_migrations WHERE id = ?`,
		"weight_real_v1",
	).Scan(&applied)
	if err != nil {
		return fmt.Errorf("check sentinel: %w", err)
	}
	if applied > 0 {
		return nil
	}

	// 2. Convert each table where weight is declared != REAL.
	for _, table := range []string{"memories", "memory_revisions"} {
		if err := migrateTableWeight(tx, table); err != nil {
			return fmt.Errorf("%s.weight: %w", table, err)
		}
	}

	// 3. Record sentinel so future runs short-circuit.
	if _, err := tx.Exec(
		`INSERT INTO schema_migrations (id, applied_at) VALUES (?, ?)`,
		"weight_real_v1",
		time.Now().Unix(),
	); err != nil {
		return fmt.Errorf("record sentinel: %w", err)
	}
	return nil
}

// sqlObject is one captured sqlite_master entry (index, trigger, or view)
// whose SQL references the `weight` column. Saved before DROP COLUMN so
// we can recreate the object after the column is back (as REAL).
type sqlObject struct {
	name string
	sql  string
}

// migrateTableWeight converts <table>.weight from INTEGER-declared to REAL
// using the ADD/UPDATE/DROP/RENAME column dance. Steps:
//
//  1. Capture indexes, triggers, and views whose SQL text references
//     "weight" — DROP COLUMN refuses otherwise. Production hit all
//     three failure modes over the course of 2026-08-13.
//  2. Drop the captured indexes.
//  3. Drop the captured triggers.
//  4. Drop the captured views.
//  5. ADD COLUMN weight_new REAL.
//  6. UPDATE weight_new = weight (the value already lives in REAL
//     storage per SQLite dynamic typing; this cast documents intent).
//  7. DROP COLUMN weight.
//  8. RENAME COLUMN weight_new TO weight.
//  9. Recreate the captured indexes (replay original SQL verbatim).
// 10. Recreate the captured triggers.
// 11. Recreate the captured views.
//
// Steps 1-4 and 9-11 keep the migration hermetic: nothing else about
// the table changes. Views stay attached and reference the same column
// name, so they don't need to be touched.
//
// Skips silently if the column doesn't exist (covers fresh DBs where
// BaseTables already declared REAL after the SafeMigrations update).
func migrateTableWeight(tx *sql.Tx, table string) error {
	// Probe declared type.
	var declType string
	err := tx.QueryRow(
		`SELECT type FROM pragma_table_info(?) WHERE name = 'weight'`,
		table,
	).Scan(&declType)
	if err == sql.ErrNoRows {
		// Table or column missing — nothing to migrate.
		return nil
	}
	if err != nil {
		return fmt.Errorf("probe %s.weight type: %w", table, err)
	}
	if strings.EqualFold(declType, "REAL") {
		return nil
	}

	// 1. Capture indexes, triggers, and views that reference `weight`.
	// The LIKE matches against the stored SQL text; false positives
	// (e.g., a view named `legacy_weight` whose body doesn't actually
	// reference the column) are harmless — drop+recreate is a no-op
	// semantically, just a few ms of work. Index matches also exclude
	// sqlite_* autoindexes (those are managed by SQLite itself).
	indexes, err := captureObjects(tx, "index",
		`type = 'index' AND tbl_name = ? AND sql IS NOT NULL
		 AND sql LIKE '%weight%' AND name NOT LIKE 'sqlite_%'`,
		table)
	if err != nil {
		return fmt.Errorf("list indexes on %s referencing weight: %w", table, err)
	}
	triggers, err := captureObjects(tx, "trigger",
		`type = 'trigger' AND tbl_name = ? AND sql IS NOT NULL
		 AND sql LIKE '%weight%'`,
		table)
	if err != nil {
		return fmt.Errorf("list triggers on %s referencing weight: %w", table, err)
	}
	views, err := captureObjects(tx, "view",
		`type = 'view' AND sql IS NOT NULL
		 AND sql LIKE '%weight%'
		 AND (sql LIKE '%FROM ' || ? || '%' OR sql LIKE '%JOIN ' || ? || '%' OR sql LIKE '%from ' || ? || '%' OR sql LIKE '%join ' || ? || '%')`,
		table, table, table, table)
	if err != nil {
		return fmt.Errorf("list views referencing %s.weight: %w", table, err)
	}

	// 2-4. Drop the captured objects.
	for _, ie := range indexes {
		dropIdx := fmt.Sprintf(`DROP INDEX IF EXISTS %q`, ie.name)
		if _, err := tx.Exec(dropIdx); err != nil {
			return fmt.Errorf("drop index %s on %s: %w", ie.name, table, err)
		}
	}
	for _, tr := range triggers {
		dropTr := fmt.Sprintf(`DROP TRIGGER IF EXISTS %q`, tr.name)
		if _, err := tx.Exec(dropTr); err != nil {
			return fmt.Errorf("drop trigger %s on %s: %w", tr.name, table, err)
		}
	}
	for _, vw := range views {
		dropV := fmt.Sprintf(`DROP VIEW IF EXISTS %q`, vw.name)
		if _, err := tx.Exec(dropV); err != nil {
			return fmt.Errorf("drop view %s (references %s.weight): %w", vw.name, table, err)
		}
	}

	// 5. ADD COLUMN weight_new REAL.
	if _, err := tx.Exec(fmt.Sprintf(
		`ALTER TABLE %s ADD COLUMN weight_new REAL`, table,
	)); err != nil {
		return fmt.Errorf("add weight_new REAL to %s: %w", table, err)
	}

	// 6. UPDATE weight_new = weight.
	if _, err := tx.Exec(fmt.Sprintf(
		`UPDATE %s SET weight_new = weight`, table,
	)); err != nil {
		return fmt.Errorf("copy weight → weight_new on %s: %w", table, err)
	}

	// 7. DROP COLUMN weight.
	if _, err := tx.Exec(fmt.Sprintf(
		`ALTER TABLE %s DROP COLUMN weight`, table,
	)); err != nil {
		return fmt.Errorf("drop weight from %s: %w", table, err)
	}

	// 8. RENAME COLUMN weight_new TO weight.
	if _, err := tx.Exec(fmt.Sprintf(
		`ALTER TABLE %s RENAME COLUMN weight_new TO weight`, table,
	)); err != nil {
		return fmt.Errorf("rename weight_new → weight on %s: %w", table, err)
	}

	// 9-11. Recreate the dropped objects.
	for _, ie := range indexes {
		if _, err := tx.Exec(ie.sql); err != nil {
			return fmt.Errorf("recreate index %s on %s: %w (sql: %s)", ie.name, table, err, ie.sql)
		}
	}
	for _, tr := range triggers {
		if _, err := tx.Exec(tr.sql); err != nil {
			return fmt.Errorf("recreate trigger %s on %s: %w (sql: %s)", tr.name, table, err, tr.sql)
		}
	}
	for _, vw := range views {
		if _, err := tx.Exec(vw.sql); err != nil {
			return fmt.Errorf("recreate view %s (references %s.weight): %w (sql: %s)", vw.name, table, err, vw.sql)
		}
	}

	return nil
}

// captureObjects reads sqlite_master and returns the entries (with their
// stored SQL) that match the given objectType ("index", "trigger", or
// "view") and the WHERE clause (parameterised by the trailing args).
// Caller is responsible for any further filtering (e.g., excluding
// sqlite_* autoindexes).
//
// The LIKE pattern '%weight%' used by callers is a substring match
// against the stored SQL — sufficient for column-name disambiguation
// when applied to a single table at a time. False positives are
// harmless: dropping and recreating an unaffected index costs
// milliseconds.
func captureObjects(tx *sql.Tx, objectType, where string, args ...any) ([]sqlObject, error) {
	query := fmt.Sprintf(`
		SELECT name, sql FROM sqlite_master
		WHERE %s
	`, where)
	rows, err := tx.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("query %s: %w", objectType, err)
	}
	defer rows.Close()
	var out []sqlObject
	for rows.Next() {
		var o sqlObject
		if err := rows.Scan(&o.name, &o.sql); err != nil {
			return nil, fmt.Errorf("scan %s: %w", objectType, err)
		}
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate %s: %w", objectType, err)
	}
	return out, nil
}