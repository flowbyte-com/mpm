// migrate_foundation.go — one-time backfill for the confidence foundation.
//
// Existing rows in memories and lessons have the new confidence column at
// 0.5 (the SQL default) but the per-type initial values are 0.5-0.8. This
// migrates the column to the correct per-type value for any row whose
// confidence is still at the default.
//
// Idempotent: re-running does not change rows that have already been
// backfilled.
package internal

import "fmt"

// BackfillInitialConfidence updates the confidence column on every artifact
// row to the per-type initial value if the row is still at the SQL default
// of 0.5. This is meant to be called once at startup (after SafeMigrations
// have run) or via an explicit `mpm migrate foundation` CLI command.
//
// Idempotent: rows that have already been backfilled are no longer at 0.5,
// so the WHERE clause is a no-op for them. Safe to re-run on every startup.
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
	cases := []struct {
		artifactType string
		initial      float64
		table        string
		collection   string // empty means no collection filter
	}{
		{"memory", InitialConfidence("memory"), "memories", "memories"},
		{"theory", InitialConfidence("theory"), "memories", "theories"},
		{"decision", InitialConfidence("decision"), "memories", "decisions"},
		{"lesson", InitialConfidence("lesson"), "lessons", ""},
	}
	for _, c := range cases {
		var err error
		if c.collection == "" {
			_, err = dm.ExecTracked(
				fmt.Sprintf(`UPDATE %s SET confidence = ? WHERE confidence = 0.5`, c.table),
				0, c.initial,
			)
		} else {
			_, err = dm.ExecTracked(
				fmt.Sprintf(`UPDATE %s SET confidence = ? WHERE collection = ? AND confidence = 0.5`, c.table),
				0, c.initial, c.collection,
			)
		}
		if err != nil {
			return fmt.Errorf("backfill %s: %w", c.artifactType, err)
		}
	}
	return nil
}
