package internal

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHybridSearch_ConceptDriftBanner verifies that when HybridSearch retrieves
// a memory with concept_drift: true in its metadata, the result's Content is
// prepended with the concept drift quarantine banner and IsConceptDrift is true.
func TestHybridSearch_ConceptDriftBanner(t *testing.T) {
	dm := freshDB(t)

	memID := "retrieval-drift-test-1"
	_, err := dm.ExecTracked(`
		INSERT INTO memories (id, collection, content, confidence, metadata, deleted_at)
		VALUES (?, 'memories', 'Redis is the exclusive caching layer for the widget pipeline', 0.55, ?, NULL)
	`, 0, memID, `{"concept_drift":true,"drift_theory_id":"test-theory-001"}`)
	require.NoError(t, err)

	results, err := HybridSearch(dm, "Redis is the exclusive caching layer for the widget pipeline", "", DefaultHybridConfig())
	require.NoError(t, err)
	require.Len(t, results, 1, "should return the drifted memory")

	r := results[0]
	assert.True(t, r.IsConceptDrift, "IsConceptDrift should be true")
	assert.True(t, r.IsChallenged, "IsChallenged should also be true for a drifted memory")
	assert.Contains(t, r.Content, conceptDriftWarning,
		"Content should be prepended with the concept drift quarantine banner")
	assert.Contains(t, r.Content, "Redis is the exclusive caching layer",
		"original content should still be present after the banner")
}

// TestHybridSearch_ChallengedBanner verifies that a manually challenged memory
// (status=challenged, no concept_drift flag) still gets the original banner.
func TestHybridSearch_ChallengedBanner(t *testing.T) {
	dm := freshDB(t)

	memID := "retrieval-challenged-test-1"
	_, err := dm.ExecTracked(`
		INSERT INTO memories (id, collection, content, confidence, metadata, deleted_at)
		VALUES (?, 'memories', 'Some older claim that was challenged', 0.40, ?, NULL)
	`, 0, memID, `{"status":"challenged","challenged_theory_id":"test-theory-002"}`)
	require.NoError(t, err)

	results, err := HybridSearch(dm, "Some older claim that was challenged", "", DefaultHybridConfig())
	require.NoError(t, err)
	require.Len(t, results, 1)

	r := results[0]
	assert.False(t, r.IsConceptDrift, "IsConceptDrift should be false for manual challenge")
	assert.True(t, r.IsChallenged, "IsChallenged should be true")
	assert.Contains(t, r.Content, challengeWarning,
		"Content should be prepended with the original challenge banner")
}

// TestHybridSearch_NoBannerForHealthy verifies that a healthy memory with no
// flags does NOT get any banner prepended.
func TestHybridSearch_NoBannerForHealthy(t *testing.T) {
	dm := freshDB(t)

	memID := "retrieval-healthy-test-1"
	_, err := dm.ExecTracked(`
		INSERT INTO memories (id, collection, content, confidence, deleted_at)
		VALUES (?, 'memories', 'A well-established fact confirmed by many tests', 0.90, NULL)
	`, 0, memID)
	require.NoError(t, err)

	results, err := HybridSearch(dm, "A well-established fact confirmed by many tests", "", DefaultHybridConfig())
	require.NoError(t, err)
	require.Len(t, results, 1)

	r := results[0]
	assert.False(t, r.IsConceptDrift, "IsConceptDrift should be false")
	assert.False(t, r.IsChallenged, "IsChallenged should be false")
	assert.NotContains(t, r.Content, challengeWarning, "no banner should be prepended")
	assert.NotContains(t, r.Content, conceptDriftWarning, "no concept drift banner")
	assert.Contains(t, r.Content, "A well-established fact confirmed by many tests",
		"original content should be intact")
}
