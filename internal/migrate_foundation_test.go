package internal

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrateFoundation_BackfillsConfidenceByCollection(t *testing.T) {
	dm := newTestDM(t)

	// Insert rows that pre-date the foundation (simulated by inserting
	// after the foundation tables exist but not setting confidence).
	_, err := dm.ExecTracked(`INSERT INTO memories (id, collection, content, confidence) VALUES ('m1', 'memories', 'x', 0.5)`, 0)
	require.NoError(t, err)
	_, err = dm.ExecTracked(`INSERT INTO memories (id, collection, content, confidence) VALUES ('t1', 'theories', 'x', 0.5)`, 0)
	require.NoError(t, err)
	_, err = dm.ExecTracked(`INSERT INTO memories (id, collection, content, confidence) VALUES ('d1', 'decisions', 'x', 0.5)`, 0)
	require.NoError(t, err)
	_, err = dm.ExecTracked(`INSERT INTO lessons  (id, type, content, created, confidence) VALUES ('l1', 'insight', 'x', '2026-06-16T00:00:00Z', 0.5)`, 0)
	require.NoError(t, err)

	require.NoError(t, BackfillInitialConfidence(dm))

	cases := map[string]float64{
		"m1": 0.8,
		"t1": 0.5,
		"d1": 0.6,
		"l1": 0.7,
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
	_, err := dm.ExecTracked(`INSERT INTO memories (id, collection, content) VALUES ('m1', 'memories', 'x')`, 0)
	require.NoError(t, err)

	require.NoError(t, BackfillInitialConfidence(dm))
	require.NoError(t, BackfillInitialConfidence(dm)) // run twice

	var conf float64
	require.NoError(t, dm.QueryRowTracked(`SELECT confidence FROM memories WHERE id = 'm1'`).Scan(&conf))
	assert.InDelta(t, 0.8, conf, 1e-9)
}
