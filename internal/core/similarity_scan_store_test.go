// similarity_scan_store_test.go — the scan must only ever compare vectors
// from the same embedding model.
//
// validEmbeddingPair checks that two vectors are the same LENGTH. Two
// 768-dim vectors from different models satisfy it and produce a confident
// cosine between unrelated spaces. Measured: a pair from
// `nomic-embed-text` and one from `mxbai-embed-large` compared as
// sim=0.995 and were proposed as a similarity candidate.
//
// That is survivable while a store uses one model. It is not survivable
// mid-migration, when a scan over a partially re-embedded store would compare
// all-minilm rows against nomic rows and enqueue pairs derived from a number
// that does not mean anything. Every retrieval path already filters on
// CurrentEmbeddingFingerprint().Model; the scan did not, and it is the last
// consumer left.

package internal

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// seedScanMemory inserts a memory with an explicit model stamp, bypassing
// seedNomutateMemory's habit of stamping the active fingerprint — the point
// here is to write rows from a model that is NOT the active one.
func seedScanMemory(t *testing.T, dm *DatabaseManager, id, content, model string, vec []float32) {
	t.Helper()
	emb, err := json.Marshal(vec)
	require.NoError(t, err)
	_, err = dm.SQLDB().Exec(`
		INSERT INTO memories (id, collection, content, metadata, weight, created_at,
		                       embedding, embedding_source, embedding_dimension, embedding_model)
		VALUES (?, 'memories', ?, '{}', 1, strftime('%s','now'), ?, 'provider', ?, ?)`,
		id, content, string(emb), len(vec), model)
	require.NoError(t, err)
}

// The active model wins: rows stamped with it are scanned, rows stamped with
// anything else are not.
func TestLoadSimilarityScanEntries_ExcludesForeignModels(t *testing.T) {
	dm := newTestDMWithShared(t)
	defer dm.Close()
	installStubProvider(t, map[string][]float32{"probe": {1, 0, 0}})

	active := CurrentEmbeddingFingerprint().Model
	require.NotEmpty(t, active, "the fixture must resolve an active fingerprint")

	seedScanMemory(t, dm, "mine", "same model", active, unitVec(1, 0, 0))
	seedScanMemory(t, dm, "theirs", "other model", "some-other-model", unitVec(1, 0, 0))

	entries, err := dm.LoadSimilarityScanEntries(0, "")
	require.NoError(t, err)

	ids := make([]string, 0, len(entries))
	for _, e := range entries {
		ids = append(ids, e.ID)
	}
	require.Contains(t, ids, "mine",
		"a row stamped with the active model must be scannable")
	require.NotContains(t, ids, "theirs",
		"a row stamped with a different model must be excluded: same-dimension "+
			"vectors from different spaces produce a meaningless cosine")
}

// A partially-migrated store is the case that matters: most rows on the new
// model, a few still on the old one. The stragglers must not join the scan,
// and — the reason this is worth a test — they must not be compared against
// the migrated majority.
func TestLoadSimilarityScanEntries_PartiallyMigratedStoreExcludesStragglers(t *testing.T) {
	dm := newTestDMWithShared(t)
	defer dm.Close()
	installStubProvider(t, map[string][]float32{"probe": {1, 0, 0}})

	active := CurrentEmbeddingFingerprint().Model

	// 4 rows on the active model, 3 stragglers from the previous one.
	for i := 0; i < 4; i++ {
		seedScanMemory(t, dm, "new-"+itoaForTest(i), "migrated", active, unitVec(1, 0, 0))
	}
	for i := 0; i < 3; i++ {
		seedScanMemory(t, dm, "old-"+itoaForTest(i), "not yet migrated",
			"ollama:all-minilm", unitVec(1, 0, 0))
	}

	entries, err := dm.LoadSimilarityScanEntries(0, "")
	require.NoError(t, err)
	require.Len(t, entries, 4,
		"only the migrated rows may be scanned; the stragglers are in a different vector space")
	for _, e := range entries {
		require.Contains(t, e.ID, "new-")
	}
}

// Rows with no model stamp at all are excluded too — an unstamped row cannot
// be shown to be in the active space.
func TestLoadSimilarityScanEntries_ExcludesUnstampedRows(t *testing.T) {
	dm := newTestDMWithShared(t)
	defer dm.Close()
	installStubProvider(t, map[string][]float32{"probe": {1, 0, 0}})

	active := CurrentEmbeddingFingerprint().Model
	seedScanMemory(t, dm, "stamped", "has a model", active, unitVec(1, 0, 0))
	seedScanMemory(t, dm, "unstamped", "no model", "", unitVec(1, 0, 0))

	entries, err := dm.LoadSimilarityScanEntries(0, "")
	require.NoError(t, err)

	ids := make([]string, 0, len(entries))
	for _, e := range entries {
		ids = append(ids, e.ID)
	}
	require.Equal(t, []string{"stamped"}, ids,
		"a row with no model stamp cannot be assumed to be in the active space")
}

// The pre-existing filters must survive the model filter: deleted rows,
// 'null' vectors and synthetic hash vectors stay out.
func TestLoadSimilarityScanEntries_KeepsExistingExclusions(t *testing.T) {
	dm := newTestDMWithShared(t)
	defer dm.Close()
	installStubProvider(t, map[string][]float32{"probe": {1, 0, 0}})

	active := CurrentEmbeddingFingerprint().Model
	emb, err := json.Marshal(unitVec(1, 0, 0))
	require.NoError(t, err)

	_, err = dm.SQLDB().Exec(`
		INSERT INTO memories (id, collection, content, metadata, weight, created_at,
		                       embedding, embedding_source, embedding_dimension, embedding_model, deleted_at)
		VALUES ('deleted-row', 'memories', 'soft deleted', '{}', 1, strftime('%s','now'),
		        ?, 'provider', 3, ?, strftime('%s','now'))`, emb, active)
	require.NoError(t, err)

	_, err = dm.SQLDB().Exec(`
		INSERT INTO memories (id, collection, content, metadata, weight, created_at,
		                       embedding, embedding_source, embedding_dimension, embedding_model)
		VALUES ('null-vec', 'memories', 'no vector', '{}', 1, strftime('%s','now'),
		        'null', 'provider', 3, ?)`, active)
	require.NoError(t, err)

	_, err = dm.SQLDB().Exec(`
		INSERT INTO memories (id, collection, content, metadata, weight, created_at,
		                       embedding, embedding_source, embedding_dimension, embedding_model)
		VALUES ('hashed', 'memories', 'synthetic', '{}', 1, strftime('%s','now'),
		        ?, 'hash', 3, ?)`, emb, active)
	require.NoError(t, err)

	seedScanMemory(t, dm, "good", "scannable", active, unitVec(1, 0, 0))

	entries, err := dm.LoadSimilarityScanEntries(0, "")
	require.NoError(t, err)

	ids := make([]string, 0, len(entries))
	for _, e := range entries {
		ids = append(ids, e.ID)
	}
	require.Equal(t, []string{"good"}, ids,
		"soft-deleted rows, 'null' vectors and hash vectors must all stay excluded")
}