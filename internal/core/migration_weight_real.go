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
// load-bearing memory consolidation has been silently broken since the
// weight scale shifted to fractional values.
//
// Why the rename-recreate dance: SQLite does NOT support
// `ALTER TABLE ... ALTER COLUMN ... TYPE` natively. The supported ALTER
// TABLE subset is limited to ADD COLUMN, DROP COLUMN, RENAME COLUMN,
// and RENAME TO. To change a column type, we read the table's CREATE
// statement from sqlite_master, substitute "weight INTEGER" → "weight
// REAL", rename the original table aside, create the new one, copy
// data, recreate indexes, and drop the renamed copy. Universal across
// SQLite versions; the data migration is lossless because the actual
// stored values are already REAL (just under an INTEGER declaration).
//
// Idempotent via the schema_migrations sentinel `weight_real_v1`.
//
// Must run AFTER BaseTables and SafeMigrations have executed (memories
// and memory_revisions tables must exist with a weight column). Caller
// (DatabaseManager.init) wraps in a transaction:
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

	// 2. Migrate each table where weight is declared != REAL.
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

// migrateTableWeight probes <table>.weight via pragma_table_info. If
// declared as REAL (or the column doesn't exist), returns nil without
// touching the schema. Otherwise reads the original CREATE TABLE
// statement from sqlite_master, substitutes "weight INTEGER" →
// "weight REAL", and applies the rename-recreate dance. Indexes that
// referenced the renamed table are recreated against the new table.
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
		return fmt.Errorf("probe type: %w", err)
	}
	if strings.EqualFold(declType, "REAL") {
		return nil
	}

	// Read the original CREATE TABLE statement.
	var createSQL string
	err = tx.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = ?`,
		table,
	).Scan(&createSQL)
	if err != nil {
		return fmt.Errorf("read %s schema: %w", table, err)
	}
	if createSQL == "" {
		return fmt.Errorf("%s table not found in sqlite_master", table)
	}

	// Substitute "weight INTEGER" → "weight REAL" in the CREATE statement.
	// Replace N=1 so we only touch the first occurrence (defensive: the
	// schema has one weight column declaration per table).
	newCreate := strings.Replace(createSQL, "weight INTEGER", "weight REAL", 1)
	if newCreate == createSQL {
		return fmt.Errorf("%s CREATE statement does not contain 'weight INTEGER'; cannot auto-substitute — manual review required", table)
	}

	// Rename-recreate dance. We use a temporary table name; on rollback
	// we restore the original table name. Any error mid-dance leaves the
	// schema in an inconsistent state — the caller wraps in a transaction
	// that rolls back on error, so the partial state never escapes.
	oldName := table + "_weight_real_old"

	renameSQL := fmt.Sprintf(`ALTER TABLE %s RENAME TO %s`, table, oldName)
	if _, err := tx.Exec(renameSQL); err != nil {
		return fmt.Errorf("rename %s → %s: %w", table, oldName, err)
	}

	if _, err := tx.Exec(newCreate); err != nil {
		return fmt.Errorf("recreate %s with weight REAL: %w", table, err)
	}

	// Copy data. SELECT * preserves all columns including weight; the
	// actual stored values are already REAL (SQLite dynamic typing stored
	// fractional values as REAL despite the INTEGER declaration), so
	// casting is unnecessary — the new REAL column accepts the values as-is.
	copySQL := fmt.Sprintf(`INSERT INTO %s SELECT * FROM %s`, table, oldName)
	if _, err := tx.Exec(copySQL); err != nil {
		return fmt.Errorf("copy data %s → %s: %w", oldName, table, err)
	}

	// Recreate indexes that referenced the renamed old table. SQLite
	// automatically renamed indexes alongside the table; they sit on the
	// old table now and would be lost when we drop it. Walk sqlite_master
	// and replay each CREATE INDEX statement with the old name swapped for
	// the new.
	idxRows, err := tx.Query(`
		SELECT sql FROM sqlite_master
		WHERE type = 'index' AND tbl_name = ?
		  AND sql IS NOT NULL
		  AND name NOT LIKE 'sqlite_%'
	`, oldName)
	if err != nil {
		return fmt.Errorf("list indexes on %s: %w", oldName, err)
	}
	defer idxRows.Close()
	var indexSQLs []string
	for idxRows.Next() {
		var idxSQL string
		if err := idxRows.Scan(&idxSQL); err != nil {
			return fmt.Errorf("scan index sql: %w", err)
		}
		indexSQLs = append(indexSQLs, idxSQL)
	}
	if err := idxRows.Err(); err != nil {
		return fmt.Errorf("iterate indexes on %s: %w", oldName, err)
	}
	idxRows.Close()

	for _, idxSQL := range indexSQLs {
		newIdxSQL := strings.Replace(idxSQL, oldName, table, -1)
		if _, err := tx.Exec(newIdxSQL); err != nil {
			return fmt.Errorf("recreate index on %s: %w (sql: %s)", table, err, newIdxSQL)
		}
	}

	// Drop the renamed original.
	dropSQL := fmt.Sprintf(`DROP TABLE %s`, oldName)
	if _, err := tx.Exec(dropSQL); err != nil {
		return fmt.Errorf("drop %s: %w", oldName, err)
	}

	return nil
}
