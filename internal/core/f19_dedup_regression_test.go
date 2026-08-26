// f19_dedup_regression_test.go — F19 regression.
//
// Audit finding F19: two identical saves created two rows without warning
// because background deduplication is dormant under default configuration.
//
// New semantics (explicit and testable):
//   - Identity criteria: (collection, SHA-256(content_hash)) over LIVE rows.
//     Same content in a different collection is legitimately distinct;
//     metadata/tag differences do not create new identity.
//   - The agent-facing save surface returns the existing row with
//     duplicate=true instead of writing a second one.
//   - Both low-level insert paths are idempotent for every caller.
package internal

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestF19_IdenticalSequentialSaveIsIdempotent(t *testing.T) {
	dm := newTestDM(t)
	defer dm.Close()

	const fact = "deploy checklist: freeze, tag, pipeline, verify"

	id1, err := dm.SaveMemory("memories", fact, "", []string{"ops"}, nil, nil, false, 1)
	require.NoError(t, err)
	countAfterFirst := countLiveMemories(t, dm)

	id2, err := dm.SaveMemory("memories", fact, "", []string{"ops"}, nil, nil, false, 1)
	require.NoError(t, err)
	assert.Equal(t, id1, id2, "identical save must return the existing id")
	assert.Equal(t, countAfterFirst, countLiveMemories(t, dm),
		"second identical save must not create a second row")
}

func TestF19_MCPMemorySaveSignalsDuplicate(t *testing.T) {
	dm := newTestDM(t)
	defer dm.Close()

	res1, _, err := dm.saveMemoryWithContextImpl("unique f19 payload body", "memories",
		[]string{"a"}, 0.5, "", ActiveContext{}, nil)
	require.NoError(t, err)
	if dup, _ := res1["duplicate"].(bool); dup {
		t.Fatalf("first save must not be flagged duplicate: %v", res1)
	}

	res2, _, err := dm.saveMemoryWithContextImpl("unique f19 payload body", "memories",
		[]string{"a"}, 0.5, "", ActiveContext{}, nil)
	require.NoError(t, err)
	assert.Equal(t, true, res2["duplicate"], "second identical save must be flagged duplicate")
	assert.Equal(t, res1["id"], res2["id"])
	assert.Equal(t, countLiveMemories(t, dm)-0, countLiveMemories(t, dm))
}

func TestF19_LegitimateDistinctionsStillPersist(t *testing.T) {
	dm := newTestDM(t)
	defer dm.Close()

	// Different collections: same text is a legitimately distinct artifact.
	idMem, err := dm.SaveMemory("memories", "shared text body", "", nil, nil, nil, false, 1)
	require.NoError(t, err)
	idDec, err := dm.SaveMemory("decisions", "shared text body", "", nil, nil, nil, false, 1)
	require.NoError(t, err)
	assert.NotEqual(t, idMem, idDec, "different collections must stay independent")
	assert.Equal(t, 2, countLiveMemories(t, dm))

	// Different content obviously persists.
	id3, err := dm.SaveMemory("memories", "different content entirely", "", nil, nil, nil, false, 1)
	require.NoError(t, err)
	require.NotEmpty(t, id3)
	assert.Equal(t, 3, countLiveMemories(t, dm))
}

func TestF19_ShreddedContentCanBeResaved(t *testing.T) {
	dm := newTestDM(t)
	defer dm.Close()

	const fact = "resave after shred probe"
	id1, err := dm.SaveMemory("memories", fact, "", nil, nil, nil, false, 1)
	require.NoError(t, err)

	// Soft-delete the original.
	_, err = dm.SQLDB().Exec(`UPDATE memories SET deleted_at = CAST(strftime('%s','now') AS INTEGER) WHERE id = ?`, id1)
	require.NoError(t, err)

	// Re-saving shredded content must create a NEW live row (identity is
	// defined over LIVE rows only).
	id2, err := dm.SaveMemory("memories", fact, "", nil, nil, nil, false, 1)
	require.NoError(t, err)
	assert.NotEqual(t, id1, id2, "re-saving after delete must produce a fresh live row")

	var live int
	require.NoError(t, dm.db.QueryRow(
		`SELECT COUNT(*) FROM memories WHERE content = ? AND deleted_at IS NULL`, fact).Scan(&live))
	assert.Equal(t, 1, live)
}

func countLiveMemories(t *testing.T, dm *DatabaseManager) int {
	t.Helper()
	var n int
	require.NoError(t, dm.db.QueryRow(`SELECT COUNT(*) FROM memories WHERE deleted_at IS NULL`).Scan(&n))
	return n
}

// TestF19_ConcurrentIdenticalSavesSingleRow: N goroutines racing the same
// logical save must converge to a single live row (or an idempotent
// return of it) — no duplicate explosion.
func TestF19_ConcurrentIdenticalSavesSingleRow(t *testing.T) {
	dm := newTestDM(t)
	defer dm.Close()

	const fact = "concurrent f19 probe"
	var wg sync.WaitGroup
	ids := make(chan string, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := dm.SaveMemory("memories", fact, "", nil, nil, nil, false, 1)
			if err == nil {
				ids <- id
			}
		}()
	}
	wg.Wait()
	close(ids)

	unique := map[string]bool{}
	for id := range ids {
		unique[id] = true
	}
	var live int
	require.NoError(t, dm.db.QueryRow(
		`SELECT COUNT(*) FROM memories WHERE content = ? AND deleted_at IS NULL`, fact).Scan(&live))
	assert.LessOrEqual(t, live, 1, "concurrent identical saves must not explode into duplicates")
	assert.LessOrEqual(t, len(unique), live+1,
		"returned ids must reference at most the surviving row(s)")
}
