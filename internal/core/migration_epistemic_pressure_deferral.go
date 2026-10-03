// migration_epistemic_pressure_deferral.go — adds the two deferral
// counts to epistemic_pressure_v.
//
// Why a migration rather than a BaseViews edit alone:
//
//	CREATE VIEW IF NOT EXISTS does not update an existing view
//	definition. On a database created before this change, the IF NOT
//	EXISTS clause makes the statement a silent no-op and the view keeps
//	its two-column shape forever. There is no other DROP VIEW machinery
//	in this repository.
//
// So the definition lives in exactly one place (epistemicPressureViewSQL)
// and both the base-schema path and this migration use it. A new
// database gets the current shape from BaseViews; an existing one gets
//	it from here.
//
// What changes and what does not:
//
//	raw_count        UNCHANGED — same name, same predicate, same
//	                 meaning (every un-compacted memory, deferred ones
//	                 included). Existing readers selecting
//	                 `raw_count, lesson_count` keep working unchanged.
//	deferred_count   NEW — rows that are simultaneously un-compacted
//	                 and deferred.
//	actionable_pending NEW — un-compacted rows an agent may act on now.
//
// The identity raw_count = actionable_pending + deferred_count holds by
// construction: all three count over the same set U, partitioned by
// whether compaction_deferred_at is NULL. See
// docs/archive/2026-09-30-compact-refusal-lifecycle.md §4.3.
//
// One deliberate deviation from §4.3, in mechanism only. The design
// states that json_extract "returns NULL for both" NULL and empty
// metadata, "so the new arm is satisfied and such rows land in A, not
// D. No special-casing of the empty case is needed, and none may be
// added."
//
// That is factually wrong about SQLite: json_extract('') raises
// "malformed JSON", it does not return NULL. Verified directly:
//
//	SELECT json_extract('', '$.x')   →  ERROR malformed JSON
//
// The EXISTING predicate survives only because OR short-circuits — for
// metadata='', the `metadata = ''` arm is true and json_extract is
// never reached. An AND-arm has no such luck: it always evaluates, so
// writing the new arm bare turns every empty-metadata row into a query
// error and takes the whole view down with it.
//
// The design's OUTCOME is nonetheless right and is what is implemented
// here: an empty-metadata row lands in A (actionable), not D, and is
// selected by the drain. Only the stated mechanism was wrong. The
// guard below repeats the same NULL-or-empty shape the existing
// predicate already uses — not a new special case, but the established
// one applied to a conjunct that cannot inherit the short-circuit.
//
// (Non-JSON non-empty metadata already errors on the pre-existing
// predicate; that is unchanged and out of scope here.)
//
// The lesson_count column is retained and keeps its meaning and
// position, because renaming or dropping it would break every existing
// SELECT that names it.

package internal

import (
	"database/sql"
	"fmt"
	"time"
)

// memoryIsUncompacted is the shared U predicate: the set of memories
// not yet rolled up into a lesson. NullOrEmptyMetadata guards the
// json_extract calls for the same short-circuit reason above.
//
// It is written once here and interpolated into both the view and
// extractRawBatch so the two cannot drift — a drift would mean the
// pressure gauge describes a different population than the drain works
// on, which is the failure this whole design is built to avoid.
const memoryIsUncompacted = `collection = 'memories'
     AND deleted_at IS NULL
     AND (metadata IS NULL OR metadata = ''
          OR json_extract(metadata, '$.compacted_into') IS NULL)`

// memoryIsDeferred is the D arm: un-compacted AND annotated deferred.
const memoryIsDeferred = memoryIsUncompacted + `
     AND (metadata IS NOT NULL AND metadata != ''
          AND json_extract(metadata, '$.compaction_deferred_at') IS NOT NULL)`

// memoryIsActionable is the A arm: un-compacted AND not deferred. The
// null/empty disjunction makes such rows satisfy this arm, so they land
// in A rather than D.
const memoryIsActionable = memoryIsUncompacted + `
     AND (metadata IS NULL OR metadata = ''
          OR json_extract(metadata, '$.compaction_deferred_at') IS NULL)`

// epistemicPressureViewSQL is the single source of truth for the view's
// definition. It is quoted by both the base-schema initializer and
// MigrateEpistemicPressureDeferral so the two can never drift.
//
// IF NOT EXISTS is required here and is not in tension with the
// migration: the base-schema path re-runs on every boot and must be
// idempotent, while the migration DROPs the view immediately before
// executing this, so the guard can never short-circuit there. The
// guard is what makes the statement safe in both places; the DROP is
// what makes it effective in the one place that needs it.
const epistemicPressureViewSQL = `CREATE VIEW IF NOT EXISTS epistemic_pressure_v AS
SELECT
  (SELECT COUNT(*) FROM memories
   WHERE ` + memoryIsUncompacted + `
  ) AS raw_count,
  (SELECT COUNT(*) FROM lessons) AS lesson_count,
  (SELECT COUNT(*) FROM memories
   WHERE ` + memoryIsDeferred + `
  ) AS deferred_count,
  (SELECT COUNT(*) FROM memories
   WHERE ` + memoryIsActionable + `
  ) AS actionable_pending`

// viewHasColumn reports whether the named view exposes a column. Used
// to decide whether a redefinition is needed without relying on the
// sentinel alone.
func viewHasColumn(tx *sql.Tx, view, column string) (bool, error) {
	rows, err := tx.Query(fmt.Sprintf("PRAGMA table_info(%s)", view))
	if err != nil {
		return false, fmt.Errorf("table_info %s: %w", view, err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid       int
			name      string
			declType  sql.NullString
			notNull   sql.NullInt64
			dfltValue sql.NullString
			pk        sql.NullInt64
		)
		if err := rows.Scan(&cid, &name, &declType, &notNull, &dfltValue, &pk); err != nil {
			return false, fmt.Errorf("scan table_info %s: %w", view, err)
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

// MigrateEpistemicPressureDeferral brings epistemic_pressure_v up to
// the four-column shape. Idempotent via the
// `epistemic_pressure_deferral_v1` sentinel in schema_migrations.
//
// The DROP+CREATE is safe here in a way it would not be for a table:
// the view holds no data of its own, it is a query, and the
// replacement is constructed in the same transaction. A crash between
// the two statements rolls back to the old view, which is the correct
// failure direction — an old-shaped view is a missing feature, a
// missing view is a broken substrate.
func MigrateEpistemicPressureDeferral(tx *sql.Tx) error {
	const sentinel = "epistemic_pressure_deferral_v1"

	// The column probe is authoritative, not the sentinel.
	//
	// The usual order is sentinel-first — it is cheaper and it is what
	// every other migration here does. It is the wrong order for this
	// one. A sentinel records that a migration RAN; it says nothing
	// about the resulting shape. A database can carry the sentinel
	// with a stale view (a restore that replayed migrations out of
	// order, a hand-built fixture, a partially-applied backup), and a
	// sentinel-first check would then short-circuit and leave the view
	// broken forever with no way out.
	//
	// Probing the actual shape costs one PRAGMA against a four-column
	// view — cheaper than the DROP/CREATE it replaces the need for —
	// and it repairs the inconsistent state rather than trusting it.
	hasActionable, err := viewHasColumn(tx, "epistemic_pressure_v", "actionable_pending")
	if err != nil {
		return fmt.Errorf("probe actionable_pending: %w", err)
	}
	if !hasActionable {
		if _, err := tx.Exec(`DROP VIEW IF EXISTS epistemic_pressure_v`); err != nil {
			return fmt.Errorf("drop epistemic_pressure_v: %w", err)
		}
		if _, err := tx.Exec(epistemicPressureViewSQL); err != nil {
			return fmt.Errorf("recreate epistemic_pressure_v: %w", err)
		}
	}

	// The sentinel is still recorded: it is the repo's migration
	// bookkeeping convention, and other tooling reads it.
	if _, err := tx.Exec(
		`INSERT OR IGNORE INTO schema_migrations (id, applied_at) VALUES (?, ?)`,
		sentinel, time.Now().Unix(),
	); err != nil {
		return fmt.Errorf("record %s sentinel: %w", sentinel, err)
	}
	return nil
}
