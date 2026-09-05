package internal

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// MigrateConfidenceHistoryCheckWidening widens the confidence_history
// trigger CHECK constraint from the 6-value pre-alpha-final enum to the
// canonical 9-value enum declared by schema.go:205.
//
// Why: the 2026-09-05 full tool behavioural audit found the deployed DB
// has only 6 trigger values:
//
//	('evidence_added','evidence_updated','evidence_deleted',
//	 'evidence_expired','decay_tick','manual_recompute')
//
// but the source schema.go declaration includes 9, adding:
//
//	'concept_drift', 'supersede', 'invalidate'
//
// The handlers at epistemology_tools.go (SupersedeDecision and
// InvalidateDecision) INSERT rows with trigger='supersede' and
// trigger='invalidate' inside a WithTx. On the deployed DB, the CHECK
// rejects those inserts, the entire tx rolls back, and the original
// decision is left untouched — making both mpm_decisions actions
// non-functional.
//
// Strategy: rename-recreate the table. We checked for view/trigger
// dependencies first — confidence_history is a leaf table with no
// views, no triggers, no foreign keys referencing it. The only
// dependents are simple SELECTs in evidence_tools.go and
// evidence_store.go; they reference the table name only, not its
// columns or CHECK constraint, so a same-schema rename is invisible
// to readers.
//
// Procedure:
//   1. Probe whether the current CHECK already accepts the canonical
//      9-value vocabulary. If yes, no-op.
//   2. Otherwise, CREATE TABLE confidence_history__new with the wider
//      CHECK (schema.go declaration verbatim).
//   3. INSERT INTO confidence_history__new SELECT * FROM
//      confidence_history — preserves all history rows.
//   4. DROP TABLE confidence_history, RENAME confidence_history__new
//      TO confidence_history.
//   5. Recreate the idx_conf_history_artifact index (the original was
//      dropped implicitly when its table was dropped).
//   6. Record the schema_migrations sentinel.
//
// Idempotent via the schema_migrations sentinel
// `confidence_history_check_widening_v1`.
//
// Caller (DatabaseManager.init) wraps in a transaction:
//
//	tx.Begin()
//	MigrateConfidenceHistoryCheckWidening(tx)
//	tx.Commit()
//
// Caller must run AFTER BaseTables has executed — confidence_history
// must exist.
func MigrateConfidenceHistoryCheckWidening(tx *sql.Tx) error {
	// 1. Sentinel — bail if already applied.
	var applied int
	err := tx.QueryRow(
		`SELECT COUNT(*) FROM schema_migrations WHERE id = ?`,
		"confidence_history_check_widening_v1",
	).Scan(&applied)
	if err != nil {
		return fmt.Errorf("check sentinel: %w", err)
	}
	if applied > 0 {
		return nil
	}

	// 2. Probe the current CHECK constraint. Two failure modes the
	//    audit found need to be tolerated without panicking:
	//      a. confidence_history doesn't exist (caller forgot to run
	//         BaseTables first) — bail with a clear error.
	//      b. confidence_history exists but with a different CHECK
	//         shape — proceed to the table-recreate path.
	var sqlDDL string
	err = tx.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'confidence_history'`,
	).Scan(&sqlDDL)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("confidence_history table not found — run BaseTables first")
	}
	if err != nil {
		return fmt.Errorf("probe confidence_history DDL: %w", err)
	}

	// The 9 canonical trigger values per schema.go:205.
	canonical := []string{
		"evidence_added", "evidence_updated", "evidence_deleted",
		"evidence_expired", "decay_tick", "concept_drift",
		"manual_recompute", "supersede", "invalidate",
	}
	if hasAllTriggers(sqlDDL, canonical) {
		// Already widened — sentinel was missing for some reason
		// (e.g. manual DDL repair). Record the sentinel so a future
		// run is a no-op, but DO NOT recreate the table.
		if _, err := tx.Exec(
			`INSERT INTO schema_migrations (id, applied_at) VALUES (?, ?)`,
			"confidence_history_check_widening_v1",
			time.Now().Unix(),
		); err != nil {
			return fmt.Errorf("record sentinel (already-widened): %w", err)
		}
		return nil
	}

	// 3-5. Table-recreate dance.
	// The new table DDL mirrors schema.go:198-206 verbatim, including
	// the canonical 9-value CHECK.
	newDDL := `
		CREATE TABLE confidence_history__new (
			id              TEXT PRIMARY KEY,
			artifact_id     TEXT NOT NULL,
			artifact_type   TEXT NOT NULL,
			confidence      REAL NOT NULL,
			computed_at     INTEGER NOT NULL,
			evidence_count  INTEGER NOT NULL,
			trigger         TEXT NOT NULL CHECK (trigger IN ('evidence_added','evidence_updated','evidence_deleted','evidence_expired','decay_tick','concept_drift','manual_recompute','supersede','invalidate'))
		)
	`
	if _, err := tx.Exec(newDDL); err != nil {
		return fmt.Errorf("create confidence_history__new: %w", err)
	}

	// 4. Copy all existing rows verbatim. confidence_history has no
	//    FK columns referencing other tables, so a plain INSERT…SELECT
	//    is safe inside this tx.
	if _, err := tx.Exec(`
		INSERT INTO confidence_history__new (id, artifact_id, artifact_type, confidence, computed_at, evidence_count, trigger)
		SELECT id, artifact_id, artifact_type, confidence, computed_at, evidence_count, trigger
		FROM confidence_history
	`); err != nil {
		return fmt.Errorf("copy history rows: %w", err)
	}

	// 5. Drop + rename. ORDER MATTERS — rename first would orphan the
	//    new table if the old table's drop failed (FK constraints can
	//    hold it open). Drop the old first; the rename is then free.
	if _, err := tx.Exec(`DROP TABLE confidence_history`); err != nil {
		return fmt.Errorf("drop old confidence_history: %w", err)
	}
	if _, err := tx.Exec(`ALTER TABLE confidence_history__new RENAME TO confidence_history`); err != nil {
		return fmt.Errorf("rename confidence_history__new → confidence_history: %w", err)
	}

	// 6. Recreate the index dropped implicitly with the table.
	if _, err := tx.Exec(
		`CREATE INDEX IF NOT EXISTS idx_conf_history_artifact ON confidence_history(artifact_id, artifact_type, computed_at)`,
	); err != nil {
		return fmt.Errorf("recreate idx_conf_history_artifact: %w", err)
	}

	// 7. Sentinel.
	if _, err := tx.Exec(
		`INSERT INTO schema_migrations (id, applied_at) VALUES (?, ?)`,
		"confidence_history_check_widening_v1",
		time.Now().Unix(),
	); err != nil {
		return fmt.Errorf("record sentinel: %w", err)
	}

	return nil
}

// hasAllTriggers returns true iff every string in triggers appears as a
// single-quoted token in the CHECK constraint portion of a CREATE TABLE
// DDL.
//
// Parsing strategy: extract the substring inside CHECK (...), split on
// commas, trim, strip surrounding quotes. Avoids a regex dep and stays
// readable.
//
// Note: this is intentionally permissive — if a CHECK constraint has
// the canonical triggers PLUS extras, that's still a "no-op" match.
// The audit found the deployed DB has only 6; widening to the canonical
// 9 is the contract. If a future patch widens to 12, this migration
// stays a no-op for those DBs (which is correct).
func hasAllTriggers(ddl string, triggers []string) bool {
	lower := strings.ToLower(ddl)
	// Find the CHECK constraint.
	checkIdx := strings.Index(lower, "check")
	if checkIdx < 0 {
		return false
	}
	// Extract text between "CHECK (" and the matching ")".
	open := strings.Index(lower[checkIdx:], "(")
	if open < 0 {
		return false
	}
	open += checkIdx + 1
	close := strings.Index(lower[open:], ")")
	if close < 0 {
		return false
	}
	body := lower[open : open+close]
	// Body is " 'a','b','c' ". Split on commas, trim whitespace and
	// single quotes. Use a simple in-place scan to avoid allocations.
	for _, want := range triggers {
		wantToken := "'" + strings.ToLower(want) + "'"
		if !strings.Contains(body, wantToken) {
			return false
		}
	}
	return true
}
