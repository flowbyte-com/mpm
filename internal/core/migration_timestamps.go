package internal

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrTimestampsMigrationDeferred is returned (wrapped, with the offending
// column list) when MigrateAllTimestampsToUnixEpoch converted everything it
// could but at least one target column is still declared with TEXT affinity.
//
// Why that blocks the migration: the UPDATE rewrites the rows in place, but
// SQLite coerces the CAST result back to TEXT on store for a TEXT-affinity
// column. The `typeof(col) = 'text'` guard is therefore satisfied both before
// and after the UPDATE, so the migration would otherwise look like it had
// succeeded while leaving the column entirely unconverted (the C1 finding).
// The conversion is impossible without first changing the column's declared
// type in the schema DDL.
//
// The migration deliberately does NOT write its sentinel in this case: the
// work is incomplete, so the next boot must retry. Callers that run the
// migration as part of schema init should treat this as a warning and commit
// the partial (but individually correct) conversions — every column that could
// be converted was. Any other error from the migration is a genuine failure
// and must roll the transaction back.
//
// This is the expected state between the migration landing and the schema DDL
// flip that changes those columns to INTEGER; once the DDL flip lands, the
// deferral disappears and the sentinel is written on the next init.
var ErrTimestampsMigrationDeferred = errors.New("timestamps migration deferred: target columns are still declared with TEXT affinity; schema DDL flip required")

// timestampColumn identifies one (table, column) pair to migrate.
type timestampColumn struct {
	table string
	col   string
}

// allTimestampsToMigrate is the canonical list of 37 (table, column) pairs
// (14 distinct column names across 18 tables; some columns appear on multiple
// tables) that must convert from TEXT ISO 8601 to INTEGER Unix-epoch seconds.
//
// Scope: only the `main` schema. attached shared DBs (see attachShared)
// carry a copy of the same BaseTables, but this loop resolves unqualified
// names to main only; shared DB migration is filed as a follow-up because
// the shared.facts column coercion pattern is the same and a shared-side
// sweep would belong to a separate task.
//
// Mirrors the table in docs/superpowers/specs/2026-07-30-unix-epoch-timestamps-design.md.
var allTimestampsToMigrate = []timestampColumn{
	{"sessions", "created_at"},
	{"topics", "created_at"},
	{"topics", "updated_at"},
	{"topic_memberships", "created_at"},
	{"memories", "created_at"},
	{"memories", "updated_at"},
	{"memories", "last_accessed_at"},
	{"memories", "expires_at"},
	{"system_config", "updated_at"},
	{"retrieval_metadata", "created_at"},
	{"retrieval_metadata", "updated_at"},
	{"retrieval_metadata", "last_retrieved_at"},
	{"external_db_cursors", "updated_at"},
	{"reference_docs", "created_at"},
	{"reference_docs", "last_indexed"},
	{"system_audit_log", "created_at"},
	{"audit_cluster_proposals", "created_at"},
	{"audit_cluster_proposals", "updated_at"},
	{"audit_cluster_proposals", "first_seen"},
	{"audit_cluster_proposals", "last_seen"},
	{"audit_cluster_proposals", "snooze_until"},
	{"session_handoffs", "created_at"},
	{"session_handoffs", "ended_at"},
	{"session_handoffs", "read_at"},
	{"scheduled_wakes", "created_at"},
	{"scheduled_tasks", "created_at"},
	{"scheduled_tasks", "updated_at"},
	{"scheduled_tasks", "last_run_at"},
	{"scheduled_tasks", "next_run_at"},
	{"ephemeral_scratchpad", "created_at"},
	{"ephemeral_scratchpad", "updated_at"},
	{"ephemeral_scratchpad", "decay_at"},
	{"vector_clusters", "updated_at"},
	{"vector_assignments", "updated_at"},
	{"reference_interactions", "created_at"},
	{"admission_log", "created_at"},
	{"memory_revisions", "created_at"},
}

// columnExists reports whether the named column is present on the named table
// in the `main` schema. A missing table and a missing column are the same
// answer (false, nil): pragma_table_info on an unknown table returns zero rows
// rather than an error, so one probe covers both.
//
// Why the migration needs this: the (table, column) pairs in
// allTimestampsToMigrate span three different DDL slices in schema.go
// (BaseTables, ReferenceTables, CommonIndexes — the last of which carries
// eight CREATE TABLE statements despite its name). The migration therefore has
// a hard ordering dependency on *all* of those slices having executed. Rather
// than encode that dependency implicitly and re-break init the next time
// somebody reorders schema init, we skip what isn't there. Skipping is also
// the correct behaviour for on-disk databases created before a given table
// existed: there are no rows to convert, so there is nothing to lose.
func columnExists(tx *sql.Tx, table, col string) (bool, error) {
	var n int
	q := fmt.Sprintf(
		`SELECT COUNT(*) FROM pragma_table_info(%q) WHERE name = %q`,
		table, col,
	)
	if err := tx.QueryRow(q).Scan(&n); err != nil {
		return false, fmt.Errorf("pragma_table_info(%s, %s): %w", table, col, err)
	}
	return n > 0, nil
}

// columnHasTextAffinity reports whether the named column on the named table
// has declared TEXT affinity (and therefore cannot reliably store INTEGER
// values — the CAST AS INTEGER result is coerced back to TEXT on store).
//
// This is the structural root cause of the C1 finding: WHERE clauses with
// `typeof(col) = 'text'` are satisfied both before and after the UPDATE on
// TEXT-affinity columns, so the sentinel would otherwise be written without
// any actual conversion having occurred. Use pragma_table_info to ask SQLite
// directly; an empty / no-rows result means the column does not exist
// (caller decides whether that is fatal).
func columnHasTextAffinity(tx *sql.Tx, table, col string) (bool, error) {
	var affinity string
	q := fmt.Sprintf(
		`SELECT type FROM pragma_table_info(%q) WHERE name = %q`,
		table, col,
	)
	err := tx.QueryRow(q).Scan(&affinity)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("pragma_table_info(%s, %s): %w", table, col, err)
	}
	// SQLite type-name → affinity rules (https://www.sqlite.org/datatype3.html):
	//   INT/INTEGER/BIGINT/... → INTEGER (NUMERIC in our test)
	//   CHAR/CLOB/TEXT/VARCHAR → TEXT
	//   BLOB                   → BLOB
	//   REAL/FLOAT/DOUBLE      → REAL
	//   DATETIME/DATE          → NUMERIC
	switch strings.ToUpper(affinity) {
	case "TEXT", "CLOB":
		return true, nil
	}
	return false, nil
}

// hasTextResidueInColumn returns true if at least one row in (table, col)
// still has storage class 'text' AND the column is not declared with TEXT
// affinity (so a residue is a migration failure, not a declaration). For
// columns declared TEXT-affinity, the residue is intrinsic and the
// pragma_table_info check is what matters instead.
func hasTextResidueInColumn(tx *sql.Tx, table, col string) (bool, error) {
	var n int
	q := fmt.Sprintf(
		`SELECT COUNT(*) FROM (SELECT 1 FROM %s WHERE typeof(%s) = 'text' LIMIT 1)`,
		table, col,
	)
	if err := tx.QueryRow(q).Scan(&n); err != nil {
		return false, fmt.Errorf("text-residue probe %s.%s: %w", table, col, err)
	}
	return n > 0, nil
}

// verifyTimestampsMigration runs after the UPDATE loop and before the
// sentinel write. For every (table, col) pair in allTimestampsToMigrate, it
// checks that the column is *not* declared with TEXT affinity — the only
// structural condition under which the typeof() guard is unreliable and the
// UPDATE silently no-ops (C1).
//
// Rationale: rows whose storage class is 'text' AND whose column has TEXT
// affinity are *expected* to remain text after the UPDATE — that is a
// property of the schema declaration, not a migration failure. But this
// migration exists to convert TEXT-shaped ISO 8601 strings to INTEGER
// unix-epoch seconds; on a TEXT-affinity column, that conversion is
// impossible without first changing the schema DDL. So the safe path is to
// surface the gap and refuse to write the sentinel, leaving the migration to
// retry once the DDL flip has landed.
//
// Two distinct failure shapes:
//
//   - TEXT-affinity columns → error wrapping ErrTimestampsMigrationDeferred.
//     Expected, recoverable, and the whole column list is reported at once so
//     one boot tells you everything the DDL flip still has to cover. Callers
//     running this during schema init should log and continue.
//   - TEXT residue on a NUMERIC-affinity column → plain error. This should be
//     unreachable and means the UPDATE didn't fire on a row that needed it;
//     callers must roll back.
//
// Either way the sentinel is not written, so no state is lost by continuing.
func verifyTimestampsMigration(tx *sql.Tx) error {
	var textAffinityCols []string
	for _, tc := range allTimestampsToMigrate {
		// Mirror the UPDATE loop's skip so verification never probes a
		// table/column the loop deliberately left alone.
		exists, err := columnExists(tx, tc.table, tc.col)
		if err != nil {
			return fmt.Errorf("verify %s.%s: %w", tc.table, tc.col, err)
		}
		if !exists {
			continue
		}
		textAff, err := columnHasTextAffinity(tx, tc.table, tc.col)
		if err != nil {
			return fmt.Errorf("verify %s.%s: %w", tc.table, tc.col, err)
		}
		if textAff {
			textAffinityCols = append(textAffinityCols, tc.table+"."+tc.col)
			continue
		}
		// Belt-and-braces: even on a NUMERIC-affinity column, a residue
		// would indicate the UPDATE didn't fire on a row that needed it.
		residue, err := hasTextResidueInColumn(tx, tc.table, tc.col)
		if err != nil {
			return fmt.Errorf("verify %s.%s: %w", tc.table, tc.col, err)
		}
		if residue {
			return fmt.Errorf(
				"migration verification: %s.%s still has TEXT rows after UPDATE "+
					"(this should be unreachable — investigate strftime results)",
				tc.table, tc.col,
			)
		}
	}
	if len(textAffinityCols) > 0 {
		return fmt.Errorf("%w (columns: %s)",
			ErrTimestampsMigrationDeferred, strings.Join(textAffinityCols, ", "))
	}
	return nil
}

// MigrateAllTimestampsToUnixEpoch converts every legacy DATETIME TEXT column in
// allTimestampsToMigrate to INTEGER Unix-epoch seconds. Idempotent via the
// timestamps_unified_v1 sentinel.
//
// Order of operations:
//  1. Sentinel check — bail if already applied.
//  2. UPDATE loop — convert TEXT-stored values to INTEGER via
//     CAST(strftime('%s', col) AS INTEGER), guarded by both typeof() = 'text'
//     and strftime() IS NOT NULL so unparseable junk doesn't get NULL-ed
//     out (I3). (table, column) pairs absent from this database are skipped.
//  3. Verification — assert that no NUMERIC-affinity column still has any
//     TEXT-storing residue; this catches the C1 case where a TEXT-affinity
//     column silently bypasses the typeof guard.
//  4. Sentinel write — only reached if all three prior steps succeed.
//
// The sentinel uses INSERT OR IGNORE so two concurrent processes
// (e.g. CLI handling and detached watch daemon) that both run initUnifiedSchema
// against the same DB converge cleanly to a single sentinel row instead of
// failing on PRIMARY KEY.
//
// Error contract: an error wrapping ErrTimestampsMigrationDeferred means the
// migration did all the work it could and withheld the sentinel because some
// target columns are still declared TEXT-affinity. The transaction is safe to
// commit in that case. Any other error is a genuine failure — roll back.
//
// Must run AFTER MigrateDeletedAtToUnixEpoch (which establishes the
// integer-timestamp precedent on memories.deleted_at), AFTER every schema DDL
// slice has executed (BaseTables, ReferenceTables, and CommonIndexes — which
// despite its name declares eight of the tables this migration targets), and
// BEFORE any handler executes. Caller (DatabaseManager.initUnifiedSchema)
// wraps in a transaction:
//
//	tx.Begin()
//	MigrateDeletedAtToUnixEpoch(tx)
//	MigrateAllTimestampsToUnixEpoch(tx)
//	tx.Commit()
func MigrateAllTimestampsToUnixEpoch(tx *sql.Tx) error {
	// 1. Sentinel check — bail if already applied.
	var applied int
	err := tx.QueryRow(
		`SELECT COUNT(*) FROM schema_migrations WHERE id = ?`,
		"timestamps_unified_v1",
	).Scan(&applied)
	if err != nil {
		return fmt.Errorf("check timestamps_unified_v1 sentinel: %w", err)
	}
	if applied > 0 {
		return nil
	}

	// 2. Convert TEXT rows to INTEGER Unix epoch. Two guards:
	//   typeof(col) = 'text'           — skips already-integer rows
	//   strftime('%s', col) IS NOT NULL — skips rows whose string can't be
	//                                   parsed by SQLite (e.g. integer
	//                                   literals that have been
	//                                   TEXT-affinity-coerced to strings
	//                                   like '1721743500'), preventing the
	//                                   UPDATE from nulling them out and
	//                                   tripping a NOT NULL constraint.
	for _, tc := range allTimestampsToMigrate {
		// Skip (table, column) pairs that don't exist in this database.
		// See columnExists for why this is a skip rather than a failure.
		exists, err := columnExists(tx, tc.table, tc.col)
		if err != nil {
			return fmt.Errorf("probe %s.%s: %w", tc.table, tc.col, err)
		}
		if !exists {
			continue
		}
		query := fmt.Sprintf(
			`UPDATE %s SET %s = CAST(strftime('%%s', %s) AS INTEGER)
			   WHERE %s IS NOT NULL
			     AND typeof(%s) = 'text'
			     AND strftime('%%s', %s) IS NOT NULL`,
			tc.table, tc.col, tc.col, tc.col, tc.col, tc.col,
		)
		if _, err := tx.Exec(query); err != nil {
			return fmt.Errorf("migrate %s.%s to unix epoch: %w", tc.table, tc.col, err)
		}
	}

	// 3. Verify — refuse to write the sentinel if any NUMERIC-affinity
	// column still has TEXT residue. TEXT-affinity columns are
	// documented as out of scope (see verifyTimestampsMigration).
	if err := verifyTimestampsMigration(tx); err != nil {
		return err
	}

	// 4. Record sentinel so future runs short-circuit. INSERT OR IGNORE
	// makes this concurrency-safe: if another process already wrote the
	// sentinel between our COUNT and our INSERT, the row already exists
	// and we no-op.
	if _, err := tx.Exec(
		`INSERT OR IGNORE INTO schema_migrations (id, applied_at) VALUES (?, ?)`,
		"timestamps_unified_v1",
		time.Now().Unix(),
	); err != nil {
		return fmt.Errorf("record timestamps_unified_v1 sentinel: %w", err)
	}
	return nil
}
