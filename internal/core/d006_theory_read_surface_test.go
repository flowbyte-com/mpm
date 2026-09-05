// d006_theory_read_surface_test.go — regressions for audit finding D-006.
//
// D-006: `mpm_theories` (the `mpm call mpm_theories --payload ...` MCP
// surface) only exposed `propose` and `resolve` actions. Agents reading
// via MCP could not enumerate or look up theories even though the CLI had
// `mpm theories` since alpha-3. The fix adds `show`, `list`, and `query`
// actions mirroring the existing decision read surface.
//
// The new methods on DatabaseManager (GetTheory, ListTheories,
// QueryTheories) live alongside the decision read methods and follow the
// same storage shape: theories are memories with collection='theories',
// status is in metadata JSON, content is "<hypothesis>\n\nVALIDATION_CRITERIA: ...".
package internal

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestD006_GetTheoryReturnsSeededRow is the headline regression: a single
// theory id round-trips through GetTheory with hypothesis, validation
// criteria, and status surfaced as separate fields.
func TestD006_GetTheoryReturnsSeededRow(t *testing.T) {
	dm := newTestDM(t)

	proposed, err := dm.ProposeTheory(
		"alpha-4 audit D-006: theory MCP surface",
		"Run `mpm call mpm_theories show` and confirm it returns the seeded row",
		nil, nil, []string{"audit", "d006"})
	require.NoError(t, err)
	id, _ := proposed["id"].(string)
	require.NotEmpty(t, id)

	row, err := dm.GetTheory(id)
	require.NoError(t, err)
	assert.Equal(t, id, row["id"])
	assert.Equal(t, "pending", row["status"], "freshly-proposed theory must default to pending")
	assert.Equal(t, "alpha-4 audit D-006: theory MCP surface", row["hypothesis"])
	assert.Equal(t, "Run `mpm call mpm_theories show` and confirm it returns the seeded row", row["validation_criteria"])
	assert.Equal(t, "audit", row["tags"].([]string)[0])
	assert.Equal(t, "d006", row["tags"].([]string)[1])
}

// TestD006_GetTheoryRejectsEmptyID pins the input-validation contract.
func TestD006_GetTheoryRejectsEmptyID(t *testing.T) {
	dm := newTestDM(t)
	_, err := dm.GetTheory("")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "id is required")
}

// TestD006_GetTheoryRejectsNonTheoryID pins the collection guard:
// querying a memory from a different collection must not return it as a
// theory.
func TestD006_GetTheoryRejectsNonTheoryID(t *testing.T) {
	dm := newTestDM(t)

	// Insert a memory from a different collection.
	memID := "d006-not-a-theory"
	_, err := dm.ExecTracked(
		`INSERT INTO memories (id, collection, content) VALUES (?, 'memories', 'plain memory')`,
		0, memID)
	require.NoError(t, err)

	_, err = dm.GetTheory(memID)
	require.Error(t, err, "GetTheory must not return a row from a different collection")
}

// TestD006_ListTheoriesFiltersByStatus pins the headline filter contract:
// default returns only pending; "all" returns every row.
func TestD006_ListTheoriesFiltersByStatus(t *testing.T) {
	dm := newTestDM(t)

	// Two pending theories.
	id1, err := dm.ProposeTheory("pending-1", "vc-1", nil, nil, nil)
	require.NoError(t, err)
	id2, err := dm.ProposeTheory("pending-2", "vc-2", nil, nil, nil)
	require.NoError(t, err)
	// One proven theory.
	id3Map, err := dm.ProposeTheory("to-be-proven", "vc-3", nil, nil, nil)
	require.NoError(t, err)
	id3, _ := id3Map["id"].(string)
	require.NotEmpty(t, id3)
	_, err = dm.ResolveTheory(id3, "investigation confirms", "proven")
	require.NoError(t, err)

	// Default = pending only.
	// 2026-09-05 audit remediation pass 2: the DM-level
	// `if limit <= 0 { limit = 50 }` coercion was removed; callers
	// that want the historical default-50 behaviour pass 50
	// explicitly. The CLI does this in
	// cmd/mpm/handlers_epistemology.go.
	pending, err := dm.ListTheories(TheoryFilter{Limit: 50})
	require.NoError(t, err)
	assert.Len(t, pending, 2, "default filter must be pending only")
	for _, th := range pending {
		assert.Equal(t, "pending", th["status"])
	}

	// Status=pending explicit.
	pending2, err := dm.ListTheories(TheoryFilter{Status: "pending", Limit: 50})
	require.NoError(t, err)
	assert.Len(t, pending2, 2)

	// Status=all returns all three.
	all, err := dm.ListTheories(TheoryFilter{Status: "all", Limit: 50})
	require.NoError(t, err)
	assert.Len(t, all, 3)

	// Status=proven returns the resolved one.
	proven, err := dm.ListTheories(TheoryFilter{Status: "proven", Limit: 50})
	require.NoError(t, err)
	assert.Len(t, proven, 1)
	assert.Equal(t, id3, proven[0]["id"])
	assert.Equal(t, "proven", proven[0]["status"])

	// Sanity: the resolved one is NOT in the pending list.
	for _, th := range pending {
		assert.NotEqual(t, id3, th["id"], "resolved theory must not appear in pending list")
	}

	// Use the IDs to silence unused-var warnings if a refactor removes one.
	_ = id1
	_ = id2
}

// TestD006_ListTheoriesResolvedFamily pins the CLI semantic parity:
// status="resolved" returns both proven and disproven rows (matches the
// legacy CLI's "resolved" filter which was the family of "no longer pending").
func TestD006_ListTheoriesResolvedFamily(t *testing.T) {
	dm := newTestDM(t)

	provenMap, err := dm.ProposeTheory("to-prove", "vc", nil, nil, nil)
	require.NoError(t, err)
	provenID, _ := provenMap["id"].(string)
	_, err = dm.ResolveTheory(provenID, "yes", "proven")
	require.NoError(t, err)

	disprovenMap, err := dm.ProposeTheory("to-disprove", "vc", nil, nil, nil)
	require.NoError(t, err)
	disprovenID, _ := disprovenMap["id"].(string)
	_, err = dm.ResolveTheory(disprovenID, "no", "disproven")
	require.NoError(t, err)

	// One pending row that must NOT appear in resolved.
	_, err = dm.ProposeTheory("still-pending", "vc", nil, nil, nil)
	require.NoError(t, err)

	resolved, err := dm.ListTheories(TheoryFilter{Status: "resolved", Limit: 50})
	require.NoError(t, err)
	assert.Len(t, resolved, 2, "resolved family must include both proven and disproven")

	gotIDs := map[string]bool{}
	for _, th := range resolved {
		gotIDs[th["id"].(string)] = true
	}
	assert.True(t, gotIDs[provenID])
	assert.True(t, gotIDs[disprovenID])
}

// TestD006_ListTheoriesRejectsUnknownStatus pins the input-validation
// contract: an unknown status filter is rejected loudly rather than
// silently returning everything or nothing.
func TestD006_ListTheoriesRejectsUnknownStatus(t *testing.T) {
	dm := newTestDM(t)
	_, err := dm.ListTheories(TheoryFilter{Status: "bogus"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown status filter")
}

// TestD006_QueryTheoriesRequiresQuery pins the input-validation contract.
func TestD006_QueryTheoriesRequiresQuery(t *testing.T) {
	dm := newTestDM(t)
	_, err := dm.QueryTheories("", 10)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "query is required")
}

// TestD006_QueryTheoriesFindsByHypothesis pins the FTS5 round-trip:
// seeding a theory with a distinctive keyword and querying it back must
// return the seed.
func TestD006_QueryTheoriesFindsByHypothesis(t *testing.T) {
	dm := newTestDM(t)

	const distinctive = "zxqyzbv-theory-d006"
	proposed, err := dm.ProposeTheory(distinctive+" hypothesis body", "vc", nil, nil, nil)
	require.NoError(t, err)
	id, _ := proposed["id"].(string)

	// Search for the distinctive token. BM25 should rank our row highly.
	rows, err := dm.QueryTheories(distinctive, 10)
	require.NoError(t, err)

	var found bool
	for _, r := range rows {
		if rid, _ := r["id"].(string); rid == id {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("QueryTheories(%q) did not return seeded theory %s; got %d rows", distinctive, id, len(rows))
	}
}

// TestD006_SplitHypothesisAndCriteria pins the structural contract that
// ListTheories / GetTheory decompose the storage concatenation into the
// two canonical fields. If a content row has no separator, the entire
// content is the hypothesis and the criteria is empty.
func TestD006_SplitHypothesisAndCriteria(t *testing.T) {
	cases := []struct {
		name              string
		content           string
		wantHypothesis    string
		wantCriteria      string
	}{
		{
			name:           "with separator",
			content:        "the hypothesis\n\nVALIDATION_CRITERIA: success criterion text",
			wantHypothesis: "the hypothesis",
			wantCriteria:   "success criterion text",
		},
		{
			name:           "no separator",
			content:        "single-line hypothesis without criteria",
			wantHypothesis: "single-line hypothesis without criteria",
			wantCriteria:   "",
		},
		{
			name:           "empty content",
			content:        "",
			wantHypothesis: "",
			wantCriteria:   "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hyp, crit := splitHypothesisAndCriteria(tc.content)
			assert.Equal(t, tc.wantHypothesis, hyp)
			assert.Equal(t, tc.wantCriteria, crit)
		})
	}
}

// TestD006_ExtractTheoryStatus pins the status-default contract: missing
// status key in metadata means "pending" (matches the ProposeTheory
// default).
func TestD006_ExtractTheoryStatus(t *testing.T) {
	cases := []struct {
		name   string
		meta   map[string]interface{}
		want   string
	}{
		{"nil meta", nil, "pending"},
		{"empty meta", map[string]interface{}{}, "pending"},
		{"pending status", map[string]interface{}{"status": "pending"}, "pending"},
		{"proven status", map[string]interface{}{"status": "proven"}, "proven"},
		{"empty string status", map[string]interface{}{"status": ""}, "pending"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, extractTheoryStatus(tc.meta))
		})
	}
}
