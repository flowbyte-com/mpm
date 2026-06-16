package internal

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrateFoundation_BackfillsConfidenceByCollection(t *testing.T) {
	dm := newTestDM(t)

	// Insert rows that pre-date the foundation (simulated by inserting
	// after the foundation tables exist but at the per-table SQL default
	// — 0.8 for memories, 0.7 for lessons — which is what SafeMigrations
	// assigns to the column).
	_, err := dm.ExecTracked(`INSERT INTO memories (id, collection, content, confidence) VALUES ('m1', 'memories', 'x', 0.8)`, 0)
	require.NoError(t, err)
	_, err = dm.ExecTracked(`INSERT INTO memories (id, collection, content, confidence) VALUES ('t1', 'theories', 'x', 0.8)`, 0)
	require.NoError(t, err)
	_, err = dm.ExecTracked(`INSERT INTO memories (id, collection, content, confidence) VALUES ('d1', 'decisions', 'x', 0.8)`, 0)
	require.NoError(t, err)
	_, err = dm.ExecTracked(`INSERT INTO lessons  (id, type, content, created, confidence) VALUES ('l1', 'insight', 'x', '2026-06-16T00:00:00Z', 0.7)`, 0)
	require.NoError(t, err)

	require.NoError(t, BackfillInitialConfidence(dm))

	cases := map[string]float64{
		"m1": 0.8, // already at initial, no change
		"t1": 0.5, // 0.8 → 0.5
		"d1": 0.6, // 0.8 → 0.6
		"l1": 0.7, // already at initial, no change
	}
	for id, want := range cases {
		t.Run(id, func(t *testing.T) {
			var got float64
			require.NoError(t, dm.QueryRowTracked(`SELECT confidence FROM artifacts WHERE id = ?`, id).Scan(&got))
			assert.InDelta(t, want, got, 1e-9)
		})
	}
}

func TestMigrateFoundation_IsIdempotent(t *testing.T) {
	dm := newTestDM(t)
	// Insert a memory at the actual table default (0.8, not 0.5) so we
	// exercise the production-state path. After two backfill runs the
	// value must still be 0.8.
	_, err := dm.ExecTracked(`INSERT INTO memories (id, collection, content, confidence) VALUES ('m1', 'memories', 'x', 0.8)`, 0)
	require.NoError(t, err)

	require.NoError(t, BackfillInitialConfidence(dm))
	require.NoError(t, BackfillInitialConfidence(dm)) // run twice

	var conf float64
	require.NoError(t, dm.QueryRowTracked(`SELECT confidence FROM memories WHERE id = 'm1'`).Scan(&conf))
	assert.InDelta(t, 0.8, conf, 1e-9)
}

// TestMigrateFoundation_MigratesTheoriesAndDecisions is the production-state
// regression test: theory and decision rows inherit the memory table default
// of 0.8 from SafeMigrations, and the backfill must migrate them to their
// per-type initial (0.5 / 0.6). Without this test, the bug — a hardcoded
// `WHERE confidence = 0.5` — would have shipped.
func TestMigrateFoundation_MigratesTheoriesAndDecisions(t *testing.T) {
	dm := newTestDM(t)
	// Theory and decision rows inherit the memory default of 0.8 from
	// SafeMigrations. The backfill must migrate them to their per-type
	// initial (0.5 / 0.6).
	_, err := dm.ExecTracked(`INSERT INTO memories (id, collection, content, confidence) VALUES ('t1', 'theories', 'x', 0.8)`, 0)
	require.NoError(t, err)
	_, err = dm.ExecTracked(`INSERT INTO memories (id, collection, content, confidence) VALUES ('d1', 'decisions', 'x', 0.8)`, 0)
	require.NoError(t, err)

	require.NoError(t, BackfillInitialConfidence(dm))

	var conf float64
	require.NoError(t, dm.QueryRowTracked(`SELECT confidence FROM memories WHERE id = 't1'`).Scan(&conf))
	assert.InDelta(t, 0.5, conf, 1e-9, "theories should be migrated to 0.5")
	require.NoError(t, dm.QueryRowTracked(`SELECT confidence FROM memories WHERE id = 'd1'`).Scan(&conf))
	assert.InDelta(t, 0.6, conf, 1e-9, "decisions should be migrated to 0.6")
}
