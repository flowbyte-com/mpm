// migration_session_handoffs_optional_session_id.go — upgrade-in-place
// migration to make session_handoffs.session_id optional (nullable).
//
// Why
// ───
// The original schema declared:
//
//	session_id TEXT NOT NULL UNIQUE
//
// making the external session identifier a hard prerequisite for
// preserving any handoff. This excluded legitimate callers that do not
// have a stable session concept (e.g. Claude Code's tool-call surface,
// which boots without `MPM_SESSION_ID` and has no native UUID). The
// substrate's contract is that the durable identity of a handoff is
// the MPM-generated `id` (handoff_id) and `created_at`; `session_id`
// is opaque correlation metadata and must not gate persistence.
//
// After this migration the column is:
//
//	session_id TEXT UNIQUE     -- nullable, multiple NULLs allowed
//
// SQLite's UNIQUE-constraint semantics already permit multiple NULL
// values in a UNIQUE column (each NULL is considered distinct from
// every other value, including other NULLs — sqlite.org/lang_createtable
// §3, "For the purposes of unique indices, all NULL values are
// considered different from all other values, including other
// NULLs."). This means the existing UPSERT-on-collision semantics
// (`ON CONFLICT(session_id) DO UPDATE` in EndSession) are preserved
// for non-null session_ids while NULL session_id rows are simply
// appended on insert with no conflict possible.
//
// Migration shape
// ───────────────
// SQLite has no `ALTER COLUMN ... DROP NOT NULL`. The documented
// table-rebuild pattern is required:
//
//   1. CREATE TABLE session_handoffs_new with session_id TEXT (no NOT NULL)
//   2. INSERT INTO session_handoffs_new SELECT ... FROM session_handoffs
//   3. DROP TABLE session_handoffs            (drops indexes + UNIQUE auto-index)
//   4. ALTER TABLE session_handoffs_new RENAME TO session_handoffs
//   5. Recreate the three indexes (the UNIQUE auto-index recreates itself)
//
// session_handoffs has no FTS5 sync triggers, no incoming FKs, and no
// outgoing FKs — verified by grep across internal/core/schema.go and
// the migration_memories_affinity_rebuild FK-enumeration precedent.
// This lets us run the rebuild inside the existing initUnifiedSchema
// migration transaction without the PRAGMA foreign_keys envelope that
// RebuildMemoriesColumnAffinity needs for the memories table. If a
// future change introduces an FK to or from session_handoffs, this
// migration will need to be lifted out into its own envelope — see
// the comment block at the top of migration_memories_affinity_rebuild.go.
//
// Idempotency
// ───────────
// Gated on the `session_handoffs_session_id_optional_v1` sentinel.
// Re-running on a database that already has nullable session_id is a
// no-op: the pragma_table_info probe confirms the column is nullable
// and the early return triggers before any DDL fires.
//
// Does NOT touch existing rows. The bulk INSERT is a verbatim column-
// to-column copy; existing non-NULL session_ids stay non-NULL.
// "Do not rewrite historical session identifiers" (per the mission
// brief) is honored by the no-transformation SELECT.

package internal

import (
	"database/sql"
	"fmt"
	"time"
)

// sessionHandoffsOptionalSessionIDSentinel is the schema_migrations
// row that gates the rebuild.
const sessionHandoffsOptionalSessionIDSentinel = "session_handoffs_session_id_optional_v1"

// MigrateSessionHandoffsOptionalSessionID rebuilds session_handoffs
// with `session_id TEXT UNIQUE` (nullable). Idempotent via sentinel.
//
// Caller (DatabaseManager.initUnifiedSchema) wraps in the same
// migration transaction that runs MigrateAllTimestampsToUnixEpoch,
// MigrateCascadeWakeScheduled, etc. The rebuild is atomic with those
// — if any earlier migration in the tx fails and rolls back, this
// migration never commits; if this migration fails, all earlier work
// rolls back too.
func MigrateSessionHandoffsOptionalSessionID(tx *sql.Tx) error {
	// 1. Sentinel check — bail if already applied.
	var applied int
	err := tx.QueryRow(
		`SELECT COUNT(*) FROM schema_migrations WHERE id = ?`,
		sessionHandoffsOptionalSessionIDSentinel,
	).Scan(&applied)
	if err != nil {
		return fmt.Errorf("check %s sentinel: %w", sessionHandoffsOptionalSessionIDSentinel, err)
	}
	if applied > 0 {
		return nil
	}

	// 2. Schema-shape probe. A missing table is a no-op (the table
	// is created later in initUnifiedSchema's CommonIndexes loop —
	// and even if it weren't, the canonical DDL now declares the
	// column nullable, so no migration is needed).
	var tname string
	err = tx.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, "session_handoffs",
	).Scan(&tname)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return fmt.Errorf("probe session_handoffs existence: %w", err)
	}

	// 3. Idempotency probe via pragma_table_info. The "notnull"
	// column is 1 if the column is NOT NULL, 0 if nullable. If
	// already 0, the rebuild is unnecessary; bail before any DDL.
	// (The sentinel is the authoritative gate; this probe is a
	// belt-and-suspenders short-circuit for databases that were
	// created after the canonical DDL flip but before the sentinel
	// was written — should be unreachable but cheap.)
	var sessionIDNotNull int
	err = tx.QueryRow(
		`SELECT "notnull" FROM pragma_table_info('session_handoffs') WHERE name = 'session_id'`,
	).Scan(&sessionIDNotNull)
	if err == sql.ErrNoRows {
		return fmt.Errorf("session_handoffs.session_id column not found: %w", err)
	}
	if err != nil {
		return fmt.Errorf("probe session_id notnull: %w", err)
	}
	if sessionIDNotNull == 0 {
		// Already nullable — write sentinel and return.
		if _, err := tx.Exec(
			`INSERT OR IGNORE INTO schema_migrations (id, applied_at) VALUES (?, ?)`,
			sessionHandoffsOptionalSessionIDSentinel, time.Now().Unix(),
		); err != nil {
			return fmt.Errorf("record %s sentinel (already-nullable path): %w", sessionHandoffsOptionalSessionIDSentinel, err)
		}
		return nil
	}

	// 4. Step 0: clear any leftover new-table from an interrupted
	// rebuild. Atomic-tx rebuilds can never leave new_session_handoffs
	// behind, but ancient non-atomic state would wedge every
	// subsequent boot on "table new_session_handoffs already exists".
	// The authoritative data lives in session_handoffs; dropping the
	// half-built copy is always safe.
	if _, err := tx.Exec(`DROP TABLE IF EXISTS new_session_handoffs`); err != nil {
		return fmt.Errorf("drop stale new_session_handoffs: %w", err)
	}

	// 5. Step 1: CREATE TABLE new_session_handoffs with the desired
	// schema. Column-by-column mirror of the canonical DDL in
	// schema.go (CommonIndexes), with the single change:
	// `session_id TEXT NOT NULL UNIQUE` → `session_id TEXT UNIQUE`.
	//
	// Defensive note: this CREATE statement is hardcoded so the
	// audit-sql isAllowlistGuardedParam check stays clean. If
	// schema.go's canonical DDL ever changes again, this CREATE
	// must be updated in lockstep — see the structural test in
	// migration_session_handoffs_optional_session_id_test.go
	// (TestSessionHandoffsSchemaShape_MatchesCanonicalDDL) which
	// fails noisily if the two diverge.
	if _, err := tx.Exec(`
		CREATE TABLE new_session_handoffs (
			id            TEXT PRIMARY KEY,
			session_id    TEXT UNIQUE,
			ended_at      INTEGER NOT NULL,
			ended_state   TEXT NOT NULL CHECK (ended_state IN ('clean','crashed','interrupted','force_end')),
			summary       TEXT NOT NULL,
			commitments   JSON NOT NULL DEFAULT '[]',
			open_questions JSON NOT NULL DEFAULT '[]',
			read_at       INTEGER,
			read_by       TEXT,
			created_at    INTEGER DEFAULT (CAST(strftime('%s','now') AS INTEGER))
		)
	`); err != nil {
		return fmt.Errorf("create new_session_handoffs: %w", err)
	}

	// 6. Step 2: bulk copy verbatim. No CASE/COALESCE — every
	// existing row has a non-NULL session_id (the NOT NULL constraint
	// guaranteed that on the legacy schema), and we want them to
	// stay non-NULL on the new schema. NULL-pass-through is correct
	// for the read_at column (already nullable); the columns with
	// NOT NULL on the legacy schema (id, ended_at, ended_state,
	// summary) would have rejected NULLs at insert time, so the
	// SELECT returns no NULLs for those columns either.
	//
	// The pragma_table_info probe (`notnull=1` on the legacy column)
	// is the structural guarantee — re-asserted here by direct
	// SELECT verification (rowsAffected must match COUNT(*) of the
	// source table).
	preCount, err := countSessionHandoffs(tx, "session_handoffs")
	if err != nil {
		return fmt.Errorf("count pre-rebuild: %w", err)
	}
	res, err := tx.Exec(`
		INSERT INTO new_session_handoffs
			(id, session_id, ended_at, ended_state, summary, commitments, open_questions, read_at, read_by, created_at)
		SELECT id, session_id, ended_at, ended_state, summary, commitments, open_questions, read_at, read_by, created_at
		FROM session_handoffs
	`)
	if err != nil {
		return fmt.Errorf("bulk copy session_handoffs: %w", err)
	}
	copied, _ := res.RowsAffected()
	postCount, err := countSessionHandoffs(tx, "new_session_handoffs")
	if err != nil {
		return fmt.Errorf("count post-rebuild: %w", err)
	}
	if preCount != postCount {
		return fmt.Errorf(
			"session_handoffs rebuild row-count drift: pre=%d post=%d copied=%d — rolling back",
			preCount, postCount, copied,
		)
	}

	// 7. Step 3: DROP old. Triggers, indexes, and the UNIQUE auto-index
	// drop with the table. session_handoffs has no FTS5 sync, no views,
	// no incoming FKs (verified by grep across internal/core/schema.go
	// — see the file-level comment block). A safe DROP.
	if _, err := tx.Exec(`DROP TABLE session_handoffs`); err != nil {
		return fmt.Errorf("drop session_handoffs: %w", err)
	}

	// 8. Step 4: RENAME new → old name. After this point readers see
	// the new schema.
	if _, err := tx.Exec(`ALTER TABLE new_session_handoffs RENAME TO session_handoffs`); err != nil {
		return fmt.Errorf("rename new_session_handoffs: %w", err)
	}

	// 9. Step 5: recreate the three indexes. The UNIQUE auto-index on
	// session_id was re-declared inline in the new CREATE TABLE
	// statement above (via the column-level UNIQUE clause) — SQLite
	// manages that index automatically. Only the three explicit
	// `CREATE INDEX` statements need to be re-issued.
	if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_handoffs_unread ON session_handoffs(read_at, ended_at DESC)`); err != nil {
		return fmt.Errorf("recreate idx_handoffs_unread: %w", err)
	}
	if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_handoffs_ended ON session_handoffs(ended_at)`); err != nil {
		return fmt.Errorf("recreate idx_handoffs_ended: %w", err)
	}
	if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_handoffs_session ON session_handoffs(session_id)`); err != nil {
		return fmt.Errorf("recreate idx_handoffs_session: %w", err)
	}

	// 10. Sentinel write — INSERT OR IGNORE so two concurrent boots
	// racing on this migration converge to one sentinel row.
	if _, err := tx.Exec(
		`INSERT OR IGNORE INTO schema_migrations (id, applied_at) VALUES (?, ?)`,
		sessionHandoffsOptionalSessionIDSentinel, time.Now().Unix(),
	); err != nil {
		return fmt.Errorf("record %s sentinel: %w", sessionHandoffsOptionalSessionIDSentinel, err)
	}

	return nil
}

// countSessionHandoffs returns COUNT(*) for the named table. Verifies
// the bulk copy preserved every row.
func countSessionHandoffs(tx *sql.Tx, table string) (int, error) {
	var n int
	// table is a static literal at every call site; safe by
	// allowlist convention (rebuildMemoriesTableAllowlist precedent).
	q := fmt.Sprintf(`SELECT COUNT(*) FROM %s`, table)
	if err := tx.QueryRow(q).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}
