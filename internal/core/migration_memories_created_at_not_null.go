// Package-internal migration: enforce NOT NULL on memories.created_at
// (and shared.memories.created_at when the shared schema is attached).
//
// Why this exists
// ────────────────
// Earlier code paths left the `memories.created_at` column nullable
// at the schema layer even though every read path (read_directives,
// HybridSearch, list_sessions, the wake-context catalogue) and every
// other write path assumes a non-NULL value. The default clause
// `DEFAULT (CAST(strftime('%s','now') AS INTEGER))` was always
// intended, but legacy installs that went through the d4cfbfa
// column-affinity rebuild lost the DEFAULT in the transition, and
// pre-d4cfbfa installs never had NOT NULL to begin with. The 2026-09-04
// fix (commit fa62c80) repaired the writer path and backfilled the
// one known NULL row; this migration closes the schema-level gap by
// physically enforcing the constraint.
//
// Physical enforcement vs application invariant
// ──────────────────────────────────────────────
// Up to this migration, "created_at is mandatory" was an application
// invariant — enforced by every writer providing a value, defended by
// the regression test TestApplyDirectives_FreshSeedPopulatesCreatedAt,
// and recoverable by the backfill migration
// (MigrateMemoriesCreatedAtBackfill). That's defense in depth for
// the *application* layer, but the *schema* still says the column is
// optional (notnull=0). The next writer who omits the column on a
// database that lost the DEFAULT, the next table-rebuild that strips
// the DEFAULT, or the next agent who trusts the schema doc would
// re-open the same failure mode. This migration makes the invariant
// physically real so future drift can only happen by active schema
// surgery, not by accident.
//
// Idempotency
// ───────────
// Gated on the `created_at_not_null_v1` sentinel (local) and
// `shared_memories_created_at_not_null_v1` (shared). Re-running on
// a database that already has NOT NULL on created_at is a no-op
// (schema-shape probe short-circuits before any rebuild).
//
// Substrate Defense Triad alignment
// ─────────────────────────────────
// Defends against:
//   - Silent-swallow: a NULL created_at reaching read_directives
//     crashes the agent's wake with `converting NULL to string is
//     unsupported`. The schema constraint makes the failure mode
//     impossible to introduce by writer omission.
//   - Edge-case promote: NULL was the edge case; promoting the
//     invariant to the schema layer removes the edge entirely.
//   - Column-write-shape (sister commit 627d0d8) and
//     column-affinity-rebuild (sister commit d4cfbfa): this
//     migration completes the timestamp-column discipline triad by
//     pinning notnull=1 on the one column where "missing" has no
//     defensible meaning.
package internal

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// memoriesCreatedAtNotNullSentinel is the schema_migrations row that
// gates the local `memories` rebuild.
const memoriesCreatedAtNotNullSentinel = "created_at_not_null_v1"

// sharedMemoriesCreatedAtNotNullSentinel is the same gate for the
// `shared.memories` table (when MPM_SHARED_DB is attached).
const sharedMemoriesCreatedAtNotNullSentinel = "shared_memories_created_at_not_null_v1"

// EnforceMemoriesCreatedAtNotNull is the public entry point. Safe to
// call on every initUnifiedSchema run — the sentinel guard + schema
// probe make it a no-op on databases that already have NOT NULL on
// created_at.
//
// Hooked into DatabaseManager.initUnifiedSchema AFTER the migration
// transaction commits, because the rebuild needs to manage its own
// PRAGMA foreign_keys envelope (which is a no-op inside a transaction,
// per SQLite's transaction-restrictions doc). Same wiring as
// RebuildMemoriesColumnAffinity.
//
// Caller contract: pass the production *sql.DB whose connections
// were opened with `_foreign_keys=1` (DSN-level FK enforcement). The
// rebuild operates on a single pinned connection so the envelope
// state is contained.
func EnforceMemoriesCreatedAtNotNull(db *sql.DB) error {
	if db == nil {
		return fmt.Errorf("EnforceMemoriesCreatedAtNotNull: nil db")
	}

	// Probe both tables first — cheap, no side effects. If neither
	// table exists or both already have NOT NULL on created_at, we
	// short-circuit before opening a connection.
	needLocal, err := memoriesCreatedAtNotNullNeeded(db, "memories", memoriesCreatedAtNotNullSentinel)
	if err != nil {
		return fmt.Errorf("probe memories.created_at nullability: %w", err)
	}
	needShared, err := memoriesCreatedAtNotNullNeeded(db, "shared.memories", sharedMemoriesCreatedAtNotNullSentinel)
	if err != nil {
		return fmt.Errorf("probe shared.memories.created_at nullability: %w", err)
	}
	if !needLocal && !needShared {
		return nil
	}

	// Defense-in-depth precondition check: refuse to start a rebuild
	// if any row still has NULL created_at. The backfill migration
	// (MigrateMemoriesCreatedAtBackfill) is the canonical repair; if
	// the operator jumped straight to this migration without running
	// the backfill first, the rebuild's INSERT...SELECT would fail
	// with a generic NOT NULL violation. The explicit precheck gives
	// them a precise error message instead.
	if needLocal {
		var n int
		if err := db.QueryRow(
			`SELECT COUNT(*) FROM memories WHERE created_at IS NULL`,
		).Scan(&n); err != nil {
			return fmt.Errorf("count memories NULL created_at: %w", err)
		}
		if n > 0 {
			return fmt.Errorf(
				"cannot enforce NOT NULL on memories.created_at: %d row(s) still have NULL "+
					"created_at. Run MigrateMemoriesCreatedAtBackfill first (auto-applied at boot "+
					"via initUnifiedSchema) or repair the rows manually, then re-run",
				n,
			)
		}
	}
	if needShared {
		var n int
		if err := db.QueryRow(
			`SELECT COUNT(*) FROM shared.memories WHERE created_at IS NULL`,
		).Scan(&n); err != nil {
			return fmt.Errorf("count shared.memories NULL created_at: %w", err)
		}
		if n > 0 {
			return fmt.Errorf(
				"cannot enforce NOT NULL on shared.memories.created_at: %d row(s) still have NULL "+
					"created_at. Run MigrateMemoriesCreatedAtBackfill first or repair manually.",
				n,
			)
		}
	}

	// Pin a single connection so the PRAGMA foreign_keys envelope
	// stays contained. PRAGMA foreign_keys is per-connection; without
	// pinning, db.Exec would pick a random pooled connection and the
	// OFF setting would be silently ignored on subsequent operations
	// that happen to use a different connection.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("pin connection: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys = OFF"); err != nil {
		return fmt.Errorf("PRAGMA foreign_keys=OFF: %w", err)
	}
	defer func() {
		_, _ = conn.ExecContext(context.Background(), "PRAGMA foreign_keys = ON")
	}()

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin rebuild tx: %w", err)
	}
	defer tx.Rollback()

	if needLocal {
		if err := enforceCreatedAtNotNullOneTable(ctx, tx, "memories", "new_memories", "memories_fts", memoriesCreatedAtNotNullSentinel); err != nil {
			return fmt.Errorf("rebuild memories: %w", err)
		}
	}
	if needShared {
		if err := enforceCreatedAtNotNullOneTable(ctx, tx, "shared.memories", "new_shared_memories", "shared.memories_fts", sharedMemoriesCreatedAtNotNullSentinel); err != nil {
			return fmt.Errorf("rebuild shared.memories: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	slog.Info("memories created_at NOT NULL enforcement complete",
		"local_enforced", needLocal, "shared_enforced", needShared)
	return nil
}

// memoriesCreatedAtNotNullNeeded returns true iff (a) the table
// exists, (b) the sentinel has NOT been written, and (c) the
// `created_at` column is currently declared nullable on the on-disk
// schema. All three must hold for the rebuild to be necessary.
//
// We check all three because:
//   - sentinel alone is a race-prone guard (would re-run if the
//     sentinel row were lost)
//   - schema-shape alone would re-run forever on a clean database
//     (the rebuild itself doesn't write the sentinel until commit)
//   - table-exists alone would re-run on a fresh install whose
//     BaseTables already declares NOT NULL
func memoriesCreatedAtNotNullNeeded(db *sql.DB, table, sentinel string) (bool, error) {
	// Table-exists probe.
	var tname string
	err := db.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table,
	).Scan(&tname)
	if err == sql.ErrNoRows {
		return false, nil // table doesn't exist on this install
	}
	if err != nil {
		return false, fmt.Errorf("probe %s existence: %w", table, err)
	}

	// Sentinel probe — bail early if already applied. Faster than
	// the schema scan on large databases.
	var applied int
	err = db.QueryRow(
		`SELECT COUNT(*) FROM schema_migrations WHERE id=?`, sentinel,
	).Scan(&applied)
	if err != nil {
		return false, fmt.Errorf("check %s sentinel: %w", sentinel, err)
	}
	if applied > 0 {
		return false, nil
	}

	// Schema probe — is created_at currently NOT NULL?
	var notNull int
	err = db.QueryRow(
		`SELECT "notnull" FROM pragma_table_info(?) WHERE name='created_at'`, table,
	).Scan(&notNull)
	if err != nil {
		return false, fmt.Errorf("probe %s.created_at notnull: %w", table, err)
	}
	return notNull == 0, nil
}

// enforceCreatedAtNotNullOneTable runs the table-rebuild dance for a
// single memories-shaped table, with created_at forced to NOT NULL.
// Caller passes the tx so the entire rebuild is atomic; the FK
// envelope is the caller's responsibility.
//
// Parameters table, newTable, ftsTable, and sentinel are passed as
// literals from the caller (EnforceMemoriesCreatedAtNotNull loops
// over the known table list). This keeps every dynamic-SQL
// interpolation inside the function provably safe at the audit-sql
// layer — `table` flows through fmt.Sprintf as an identifier that's
// been statically resolved to a literal at the call site, and
// `ftsTable` is a known companion FTS5 name. Hardcoding these
// (rather than computing from `table`) is the reason the mpm-lint
// gate stays at sql-built=0.
func enforceCreatedAtNotNullOneTable(
	ctx context.Context,
	tx *sql.Tx,
	table, newTable, ftsTable, sentinel string,
) error {
	if !rebuildMemoriesTableAllowlist[table] {
		return fmt.Errorf("enforceCreatedAtNotNullOneTable: refusing %q (not in allowlist)", table)
	}

	// Step 0: clear any leftover shadow table from an interrupted rebuild.
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("DROP TABLE IF EXISTS %s", newTable)); err != nil {
		return fmt.Errorf("drop stale %s: %w", newTable, err)
	}

	// Step 1: enumerate the existing column shapes. We re-use the
	// rebuild helpers from migration_memories_affinity_rebuild.go
	// (readColumnShapes, readIndexShapes, readTriggerSQL, readViewSQL,
	// readFKClause, clearStandaloneFTS, repopulateStandaloneFTS,
	// countMemoriesFKViolations, condFK, sanitizeIdent) so this
	// migration's behavior is structurally identical to the affinity
	// rebuild, modulo the column-shape override below.
	cols, err := readColumnShapes(ctx, tx, table)
	if err != nil {
		return fmt.Errorf("enumerate %s columns: %w", table, err)
	}

	indexes, err := readIndexShapes(ctx, tx, table)
	if err != nil {
		return fmt.Errorf("enumerate %s indexes: %w", table, err)
	}

	triggers, err := readTriggerSQL(ctx, tx, table)
	if err != nil {
		return fmt.Errorf("enumerate %s triggers: %w", table, err)
	}

	views, err := readViewSQL(ctx, tx, table)
	if err != nil {
		return fmt.Errorf("enumerate %s views: %w", table, err)
	}

	fkClause, err := readFKClause(ctx, tx, table)
	if err != nil {
		return fmt.Errorf("enumerate %s FK clause: %w", table, err)
	}

	// Step 2: composite pre-migration checksum on the OLD table.
	// Same shape as the affinity rebuild's — strftime-normalised for
	// the timestamp sum so the pre/post match even if any row is
	// still TEXT-shaped on disk.
	pre, err := checksumMemoriesTable(ctx, tx, table, true /* useStrftime */)
	if err != nil {
		return fmt.Errorf("pre-migration checksum: %w", err)
	}

	// Step 3: build the new column declarations. Every column
	// passes through the affinity rebuild's buildCreateColumnDecl
	// EXCEPT `created_at`, which we force to NOT NULL. The
	// override is the entire point of this migration — we leave
	// every other column's nullability alone, so a column that
	// was NULL yesterday stays NULL today.
	newCols := make([]string, 0, len(cols))
	for _, c := range cols {
		newCols = append(newCols, buildCreatedAtNotNullDecl(c))
	}
	createSQL := fmt.Sprintf(
		"CREATE TABLE %s (%s%s)",
		newTable,
		strings.Join(newCols, ", "),
		condFK(fkClause),
	)
	if _, err := tx.ExecContext(ctx, createSQL); err != nil {
		return fmt.Errorf("create %s: %w\nSQL: %s", newTable, err, createSQL)
	}

	// Step 4: bulk INSERT INTO new_<table> SELECT ... FROM <table>.
	// We use the affinity rebuild's strftime normalisation for
	// timestamp columns so the post-checksum matches the pre-checksum
	// byte-for-byte. COALESCE(..., 0) only kicks in for columns that
	// were NOT NULL on the legacy schema — wrapping a nullable
	// column's NULL value in COALESCE(..., 0) silently mutates
	// `deleted_at IS NULL` rows into `deleted_at = 0`, which the
	// legacy live sentinel (0 means live, NULL means live) handles
	// only AFTER MigrateDeletedAtZeroToNull runs. Doing it
	// unconditionally here would re-introduce the same D2 regression
	// (2026-08-25) the affinity rebuild fixed for itself.
	//
	// `created_at` will be NOT NULL in the new table; the
	// EnforceMemoriesCreatedAtNotNull precondition ensures no NULL
	// values reach this point, so the NotNull flag on the original
	// column doesn't matter for that specific column's safety.
	selectExprs := make([]string, 0, len(cols))
	notNullTimestampFallbacks := 0
	for _, c := range cols {
		if isTimestampColumn(c.Name) {
			expr := fmt.Sprintf(
				"CAST(CASE WHEN typeof(%s)='text' AND strftime('%%s', %s) IS NOT NULL "+
					"THEN strftime('%%s', %s) ELSE %s END AS INTEGER)",
				c.Name, c.Name, c.Name, c.Name,
			)
			if c.NotNull {
				expr = fmt.Sprintf("COALESCE(%s, 0)", expr)
				notNullTimestampFallbacks++
			}
			selectExprs = append(selectExprs, expr)
		} else if c.Name == "rowid" {
			selectExprs = append(selectExprs, "rowid")
		} else {
			selectExprs = append(selectExprs, c.Name)
		}
	}
	if notNullTimestampFallbacks > 0 {
		slog.Warn("created_at NOT NULL rebuild: NOT NULL timestamp columns use 0 fallback",
			"table", table, "columns", notNullTimestampFallbacks)
	}
	insertSQL := fmt.Sprintf(
		"INSERT INTO %s SELECT %s FROM %s",
		newTable,
		strings.Join(selectExprs, ", "),
		table,
	)
	if _, err := tx.ExecContext(ctx, insertSQL); err != nil {
		return fmt.Errorf("bulk copy into %s: %w (NOT NULL constraint violation here means a "+
			"NULL row slipped past the precondition check; surface the row count and re-run "+
			"MigrateMemoriesCreatedAtBackfill)", newTable, err)
	}

	// Step 5: composite post-migration checksum on the NEW table.
	post, err := checksumMemoriesTable(ctx, tx, newTable, false /* useStrftime */)
	if err != nil {
		return fmt.Errorf("post-migration checksum on %s: %w", newTable, err)
	}
	if pre != post {
		return fmt.Errorf(
			"checksum mismatch: pre=%+v post=%+v — rolling back",
			pre, post,
		)
	}

	// Step 6: DROP dependent views BEFORE the table DROP.
	for _, v := range views {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("DROP VIEW IF EXISTS %s", v.Name)); err != nil {
			return fmt.Errorf("drop view %s: %w", v.Name, err)
		}
	}

	// Step 7: clear the FTS5 shadow table.
	if err := clearStandaloneFTS(ctx, tx, ftsTable); err != nil {
		return fmt.Errorf("clear %s: %w", ftsTable, err)
	}

	// Step 8: DROP old + RENAME new.
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("DROP TABLE %s", table)); err != nil {
		return fmt.Errorf("drop %s: %w", table, err)
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("ALTER TABLE %s RENAME TO %s", newTable, table)); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", newTable, table, err)
	}

	// Step 9: recreate indexes verbatim.
	for _, ix := range indexes {
		if _, err := tx.ExecContext(ctx, ix.SQL); err != nil {
			return fmt.Errorf("recreate index %s: %w", ix.Name, err)
		}
	}

	// Step 10: repopulate FTS5 + recreate triggers.
	if err := repopulateStandaloneFTS(ctx, tx, ftsTable, table); err != nil {
		return fmt.Errorf("repopulate %s: %w", ftsTable, err)
	}
	for _, t := range triggers {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("DROP TRIGGER IF EXISTS %s", t.Name)); err != nil {
			return fmt.Errorf("drop trigger %s: %w", t.Name, err)
		}
		if _, err := tx.ExecContext(ctx, t.SQL); err != nil {
			return fmt.Errorf("recreate trigger %s: %w", t.Name, err)
		}
	}

	// Step 11: recreate views AFTER the RENAME.
	for _, v := range views {
		if _, err := tx.ExecContext(ctx, v.SQL); err != nil {
			return fmt.Errorf("recreate view %s: %w", v.Name, err)
		}
	}

	// Sentinel write.
	if _, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO schema_migrations (id, applied_at) VALUES (?, ?)`,
		sentinel, time.Now().Unix(),
	); err != nil {
		return fmt.Errorf("record %s sentinel: %w", sentinel, err)
	}

	slog.Info("created_at NOT NULL rebuild step complete",
		"table", table, "indexes_recreated", len(indexes),
		"triggers_recreated", len(triggers),
	)
	return nil
}

// buildCreatedAtNotNullDecl emits a single column declaration for
// CREATE TABLE, mirroring buildCreateColumnDecl from
// migration_memories_affinity_rebuild.go EXCEPT that `created_at`
// is forced to NOT NULL regardless of the legacy column's
// nullability. Every other column's shape (decltype, default, PK,
// NotNull) is preserved bit-for-bit.
//
// The base function preserves the original NotNull flag, so any other
// column that was already NOT NULL stays NOT NULL, and any column
// that was nullable stays nullable. This is intentional — the
// migration's contract is to enforce the ONE specific invariant
// the application already assumes, not to harden the entire
// timestamp vocabulary in a single shot (that's a follow-up if v
// wants it).
func buildCreatedAtNotNullDecl(c columnShape) string {
	parts := []string{c.Name}

	// Type clause. For timestamp columns, the affinity rebuild
	// forces INTEGER; we mirror that here so the column shape
	// stays consistent with what created_at already is on a
	// post-d4cfbfa install. The DEFAULT clause for created_at and
	// updated_at is preserved (canonical) — that keeps the
	// "writer can omit the column and still get a non-NULL value"
	// ergonomic intact.
	switch {
	case isTimestampColumn(c.Name):
		parts = append(parts, "INTEGER")
		if c.Name == "created_at" || c.Name == "updated_at" {
			parts = append(parts, "DEFAULT", `(CAST(strftime('%s','now') AS INTEGER))`)
		}
	default:
		parts = append(parts, c.DeclType)
		if c.Default.Valid {
			parts = append(parts, "DEFAULT", c.Default.String)
		}
	}

	// Nullability. For `created_at`, force NOT NULL regardless of
	// the legacy column's nullability. For every other column,
	// preserve the original shape (so we don't accidentally tighten
	// `deleted_at`, `expires_at`, or `last_accessed_at` — those
	// columns legitimately use NULL as the "never-set" sentinel).
	if c.Name == "created_at" {
		parts = append(parts, "NOT NULL")
	} else if c.PK > 0 {
		parts = append(parts, "PRIMARY KEY")
	} else if c.NotNull {
		parts = append(parts, "NOT NULL")
	}

	return strings.Join(parts, " ")
}