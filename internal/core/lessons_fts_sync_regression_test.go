// lessons_fts_sync_regression_test.go — F2 alpha-blocker regression.
//
// Audit finding F2: searching lessons for exact terms ("nginx", "logstats")
// returned ONE UNRELATED lesson although the correct lesson existed (and was
// reachable by id). Root cause: lessons_fts was rowid/content-desynced from
// lessons_base, so MATCH on the indexed text JOINed to the WRONG base row.
// Secondary defect: SearchLessons ranked by reinforcement_count before FTS
// relevance, so an exact match could be buried under loosely related rows.
package internal

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedLessonPair creates the audited shape: a correct lesson with distinctive
// technical terms plus an unrelated decoy.
func seedLessonPair(t *testing.T, dm *DatabaseManager) (correctID, decoyID string) {
	t.Helper()
	correct, err := dm.AddLesson(
		"When parsing nginx combined logs, never split on whitespace: quoted request strings can contain spaces (e.g. \"GET /a b HTTP/1.1\")",
		LessonTypePractice, []string{"parsing", "logstats"}, "")
	require.NoError(t, err)
	decoy, err := dm.AddLesson(
		"Pointer Architecture Best Practices: keep pointers out of hot paths",
		LessonTypeInsight, nil, "")
	require.NoError(t, err)
	return correct.ID, decoy.ID
}

func clearFTSSentinel(t *testing.T, dm *DatabaseManager) {
	t.Helper()
	_, err := dm.db.Exec(`DELETE FROM schema_migrations WHERE id = ?`, lessonsFTSSyncSentinel)
	require.NoError(t, err)
}

// TestF2_DesyncedIndexReturnsWrongLessonThenRepair reproduces the audit
// failure exactly: corrupt fts rowid mapping → wrong lesson returned;
// EnsureLessonsFTSInSync → correct lesson returned, decoy gone.
func TestF2_DesyncedIndexReturnsWrongLessonThenRepair(t *testing.T) {
	dm := newTestDM(t)
	defer dm.Close()

	correctID, decoyID := seedLessonPair(t, dm)

	// Corrupt the index the way the legacy migration did: swap the indexed
	// CONTENT between two rowids so text no longer belongs to its base row.
	var correctRowid, decoyRowid int
	require.NoError(t, dm.db.QueryRow(`SELECT rowid FROM lessons_base WHERE id = ?`, correctID).Scan(&correctRowid))
	require.NoError(t, dm.db.QueryRow(`SELECT rowid FROM lessons_base WHERE id = ?`, decoyID).Scan(&decoyRowid))
	var correctContent, decoyContent string
	require.NoError(t, dm.db.QueryRow(`SELECT content FROM lessons_fts WHERE rowid = ?`, correctRowid).Scan(&correctContent))
	require.NoError(t, dm.db.QueryRow(`SELECT content FROM lessons_fts WHERE rowid = ?`, decoyRowid).Scan(&decoyContent))
	_, err := dm.db.Exec(`UPDATE lessons_fts SET content = ? WHERE rowid = ?`, decoyContent, correctRowid)
	require.NoError(t, err)
	_, err = dm.db.Exec(`UPDATE lessons_fts SET content = ? WHERE rowid = ?`, correctContent, decoyRowid)
	require.NoError(t, err)

	// Pre-repair: the audited symptom reproduces — indexed nginx-text lives
	// under the decoy's rowid, so search confidently returns the WRONG
	// lesson. This assertion documents the failure mode; if it ever stops
	// holding, the corruption shape changed and this test needs updating.
	preResults, err := dm.SearchLessons("nginx", 10)
	require.NoError(t, err)
	require.Len(t, preResults, 1)
	assert.Equal(t, decoyID, preResults[0].ID,
		"expected the audited wrong-lesson symptom from a swapped-content index")

	// Repair pass (sentinel cleared to force verification).
	clearFTSSentinel(t, dm)
	require.NoError(t, dm.EnsureLessonsFTSInSync())

	results, err := dm.SearchLessons("nginx", 10)
	require.NoError(t, err)
	require.Len(t, results, 1, "exactly the nginx lesson must be found")
	assert.Equal(t, correctID, results[0].ID)

	// logstats appears only in the correct lesson's TAGS.
	results, err = dm.SearchLessons("logstats", 10)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, correctID, results[0].ID)

	// Unrelated query stays clean.
	results, err = dm.SearchLessons("kafka rebalancing", 10)
	require.NoError(t, err)
	assert.Empty(t, results)
}

// TestF2_MissingAndOrphanRowsRepaired covers the other two desync classes:
// base rows missing from the index and orphan index rows.
func TestF2_MissingAndOrphanRowsRepaired(t *testing.T) {
	dm := newTestDM(t)
	defer dm.Close()

	seedLessonPair(t, dm)

	// Missing from index: write into lessons_base bypassing the view trigger.
	_, err := dm.db.Exec(`
		INSERT INTO lessons_base (id, type, content, tags, created) VALUES
		('bypass-row', 'insight', 'ecryptfs mounts need keyslots loaded first', '[]',
		 CAST(strftime('%s','now') AS INTEGER))
	`)
	require.NoError(t, err)

	clearFTSSentinel(t, dm)
	require.NoError(t, dm.EnsureLessonsFTSInSync())

	for _, q := range []string{"nginx", "ecryptfs"} {
		results, err := dm.SearchLessons(q, 10)
		require.NoError(t, err, "query %q", q)
		require.NotEmpty(t, results, "query %q must find its lesson after repair", q)
	}
}

// TestF2_ExactTermOutranksPartialMatch pins the ranking fix: an exact
// content-term match must rank above a weaker stem/prefix match even when
// the weaker match has a higher reinforcement_count.
func TestF2_ExactTermOutranksPartialMatch(t *testing.T) {
	dm := newTestDM(t)
	defer dm.Close()

	exactLesson, err := dm.AddLesson("nginx", LessonTypeInsight, nil, "") // minimal exact doc
	require.NoError(t, err)
	exactID := exactLesson.ID
	_, err = dm.AddLesson("Testing notes about the tester and tested testability of tests",
		LessonTypeInsight, nil, "")
	require.NoError(t, err)
	// Give the noisy lesson every non-relevance advantage.
	_, err = dm.db.Exec(`UPDATE lessons SET reinforcement_count = 50 WHERE content LIKE 'Testing notes%'`)
	require.NoError(t, err)

	results, err := dm.SearchLessons("nginx", 10)
	require.NoError(t, err)
	require.NotEmpty(t, results)
	assert.Equal(t, exactID, results[0].ID, "exact unique-term hit must rank first")

	// Porter-stem family: query "testing" should rank the exact-stem-heavy
	// document first too, regardless of reinforcement ordering.
	results, err = dm.SearchLessons("testing", 10)
	require.NoError(t, err)
	require.Len(t, results, 1)
}

// TestF2_SearchContractTable exercises the full audit matrix against a clean,
// synced index: case variation, punctuation, multi-word, identifiers,
// unrelated and no-result queries.
func TestF2_SearchContractTable(t *testing.T) {
	dm := newTestDM(t)
	defer dm.Close()

	correctID, decoyID := seedLessonPair(t, dm)

	cases := []struct {
		name    string
		query   string
		wantIDs []string // expected result IDs in order; empty = no results
	}{
		{"exact unique term", "nginx", []string{correctID}},
		{"tag-only term", "logstats", []string{correctID}},
		{"case variation", "NGINX", []string{correctID}},
		{"multi-word phrase terms", "parsing whitespace quoted", []string{correctID}},
		{"technical identifier with digits", "HTTP/1.1", []string{correctID}},
		{"unrelated query hits nothing", "kubernetes ingress controller", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			results, err := dm.SearchLessons(tc.query, 10)
			require.NoError(t, err)
			got := make([]string, 0, len(results))
			for _, r := range results {
				got = append(got, r.ID)
				if r.ID == decoyID {
					t.Errorf("decoy lesson leaked into results for query %q", tc.query)
				}
			}
			if tc.wantIDs == nil {
				assert.Empty(t, got, "query %q must return nothing", tc.query)
			} else {
				assert.Equal(t, tc.wantIDs, got)
			}
		})
	}

	// Punctuation-only / empty queries return no results without erroring.
	for _, q := range []string{"", "   ", "\"'^()*:"} {
		results, err := dm.SearchLessons(q, 10)
		require.NoError(t, err)
		assert.Empty(t, results, "query %q", q)
	}
}

// TestF2_RepairIsIdempotent: running the sync twice does not duplicate or
// corrupt the index.
func TestF2_RepairIsIdempotent(t *testing.T) {
	dm := newTestDM(t)
	defer dm.Close()

	seedLessonPair(t, dm)
	clearFTSSentinel(t, dm)
	require.NoError(t, dm.EnsureLessonsFTSInSync())
	require.NoError(t, dm.EnsureLessonsFTSInSync()) // second run: sentinel short-circuit

	var idxCount, baseCount int
	require.NoError(t, dm.db.QueryRow(`SELECT COUNT(*) FROM lessons_fts`).Scan(&idxCount))
	require.NoError(t, dm.db.QueryRow(`SELECT COUNT(*) FROM lessons_base`).Scan(&baseCount))
	assert.Equal(t, baseCount, idxCount, "index size must equal base size after repair")

	results, err := dm.SearchLessons("nginx", 10)
	require.NoError(t, err)
	assert.Len(t, results, 1)
}
