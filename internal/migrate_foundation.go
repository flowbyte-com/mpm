// migrate_foundation.go — one-time backfill for the confidence foundation.
//
// Existing rows in memories and lessons have the new confidence column at
// the per-table SQL default (0.8 for memories, 0.7 for lessons — see
// schema.go SafeMigrations). The per-type initial values, however, are
// 0.5-0.8 and can differ from the table default. This migrates the column
// to the correct per-type value for any row still at the table default.
//
// Why a per-table default and not a single sentinel (e.g. 0.5)?
//
//   - memory:   table default 0.8 = InitialConfidence("memory")   0.8 → no-op
//   - theory:   table default 0.8 ≠ InitialConfidence("theory")   0.8 → 0.5
//   - decision: table default 0.8 ≠ InitialConfidence("decision") 0.8 → 0.6
//   - lesson:   table default 0.7 = InitialConfidence("lesson")   0.7 → no-op
//
// Theory and decision rows live in the `memories` table but inherit the
// memory table default of 0.8 from SafeMigrations. The backfill must walk
// each collection explicitly and rewrite the rows whose confidence is
// still at the table default.
//
// Idempotent: re-running matches no rows, because the table default is no
// longer present after the first run. Safe to call on every startup.
package internal

import "fmt"

// BackfillInitialConfidence updates the confidence column on every artifact
// row to the per-type initial value if the row is still at the per-table
// SQL default. This is meant to be called once at startup (after
// SafeMigrations have run) or via an explicit `mpm migrate foundation` CLI
// command.
//
// Idempotent: rows that have already been backfilled are no longer at the
// table default, so the WHERE clause is a no-op for them. Safe to re-run
// on every startup.
//
// The per-type initial values come from InitialConfidence (confidence.go),
// the same source of truth used by MemoryStore.AddMemory, so the two stay
// in sync if the defaults ever change.
func BackfillInitialConfidence(dm *DatabaseManager) error {
	// The mapping from artifactType to (table, collection) is explicit so
	// the SQL strings are unambiguous:
	//   - memory   → memories, collection = 'memories'
	//   - theory   → memories, collection = 'theories'
	//   - decision → memories, collection = 'decisions'
	//   - lesson   → lessons (no collection discriminator — `lessons` is
	//     its own table)
	//
	// tableDefault is the SQL default SafeMigrations assigned to the
	// table (0.8 for memories, 0.7 for lessons). The WHERE clause
	// matches rows that have never been touched, so the backfill is
	// idempotent.
	cases := []struct {
		artifactType string
		initial      float64
		tableDefault float64 // SQL default SafeMigrations assigned to the table
		table        string
		collection   string // empty means no collection filter
	}{
		{"memory",   InitialConfidence("memory"),   0.8, "memories", "memories"},  // no-op (0.8 = 0.8)
		{"theory",   InitialConfidence("theory"),   0.8, "memories", "theories"},  // 0.8 → 0.5
		{"decision", InitialConfidence("decision"), 0.8, "memories", "decisions"}, // 0.8 → 0.6
		{"lesson",   InitialConfidence("lesson"),   0.7, "lessons",  ""},          // no-op (0.7 = 0.7)
	}
	for _, c := range cases {
		var err error
		if c.collection == "" {
			_, err = dm.ExecTracked(
				fmt.Sprintf(`UPDATE %s SET confidence = ? WHERE confidence = ?`, c.table),
				0, c.initial, c.tableDefault,
			)
		} else {
			_, err = dm.ExecTracked(
				fmt.Sprintf(`UPDATE %s SET confidence = ? WHERE collection = ? AND confidence = ?`, c.table),
				0, c.initial, c.collection, c.tableDefault,
			)
		}
		if err != nil {
			return fmt.Errorf("backfill %s: %w", c.artifactType, err)
		}
	}
	return nil
}
