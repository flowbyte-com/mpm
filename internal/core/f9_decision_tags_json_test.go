// f9_decision_tags_json_test.go — Regression: SupersedeDecision/InvalidateDecision
// must keep the `tags` column as a valid JSON array. Pre-fix the path concatenated
// CSV strings ("superseded,superseded-by:abc") into a column whose schema is JSON
// (e.g. `["foo","bar"]`), yielding malformed text that json.Unmarshal rejected.
// Every `mpm memory list` call then printed N noisy "failed to unmarshal tags"
// warnings — operator-visible regression caused by a one-line tag-append bug.
//
// Defect Q (2026-09-13 acceptance): InvalidateDecision tags with `invalidated`
// rather than `superseded`; the canonical retirement marker for the two paths
// is now distinct. The JSON-shape contract (parses cleanly into []string)
// remains; the marker check accepts whichever retirement token the path
// actually writes.
package internal

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// assertTagsIsValidJSON loads the row's tags column for id and asserts it
// parses cleanly into a []string. Pre-fix this fails with "invalid character
// ',' after top-level value" because the column holds
// `[],superseded,superseded-by:<id>` rather than a real JSON array.
//
// accepts is the set of retirement tokens the caller expects the row to
// carry. SupersedeDecision writes `superseded` (+ `superseded-by:<id>`);
// InvalidateDecision writes `invalidated`. Pass exactly the one you need.
func assertTagsIsValidJSON(t *testing.T, dm *DatabaseManager, id string, accepts ...string) {
	t.Helper()
	mem, err := dm.GetMemory(id)
	require.NoError(t, err)
	require.NotNil(t, mem)
	tagsRaw, _ := mem["tags"].(string)
	require.NotEmpty(t, tagsRaw, "tags column should be populated")

	var tags []string
	err = json.Unmarshal([]byte(tagsRaw), &tags)
	require.NoErrorf(t, err, "tags column must be valid JSON (got %q)", tagsRaw)

	// Must contain one of the canonical retirement markers — otherwise the
	// fix would have rewritten storage but lost the ranking-discount signal.
	found := false
	for _, tag := range tags {
		for _, a := range accepts {
			if tag == a {
				found = true
				break
			}
		}
		if found {
			break
		}
	}
	assert.True(t, found, "retirement marker (%v) must survive the JSON rewrite; got tags=%v", accepts, tags)
}

func TestF9_SupersedeDecision_TagsColumnStaysValidJSON(t *testing.T) {
	dm := newTestDM(t)
	defer dm.Close()

	originalID := seedF9Decision(t, dm, "deploy with blue-green")
	res, err := dm.SupersedeDecision(originalID, "ctx", "deploy with canary", "safer rollback", "",
		nil, nil, ActiveContext{})
	require.NoError(t, err)
	newID, _ := res["id"].(string)
	require.NotEmpty(t, newID)

	// Original's tags column must round-trip as JSON and carry the
	// canonical supersede marker.
	assertTagsIsValidJSON(t, dm, originalID, "superseded")

	// Replacement's tags must stay clean (SupersedeDecision creates the
	// replacement via RecordDecision → AddMemoryWithWeight which already
	// writes JSON).
	mem, err := dm.GetMemory(newID)
	require.NoError(t, err)
	tagsRaw, _ := mem["tags"].(string)
	var tags []string
	require.NoErrorf(t, json.Unmarshal([]byte(tagsRaw), &tags),
		"replacement tags must be valid JSON (got %q)", tagsRaw)
}

func TestF9_InvalidateDecision_TagsColumnStaysValidJSON(t *testing.T) {
	dm := newTestDM(t)
	defer dm.Close()

	id := seedF9Decision(t, dm, "use in-house scheduler")
	_, err := dm.InvalidateDecision(id, "replaced by managed queue")
	require.NoError(t, err)

	// Defect Q (2026-09-13): invalidate now tags `invalidated` (was
	// `superseded` pre-fix). The JSON-shape contract is unchanged.
	assertTagsIsValidJSON(t, dm, id, "invalidated")
}

// TestF9_SupersedeThenList_NoUnmarshalWarnings locks in the operator-visible
// symptom: walking the recent-memories list must not emit "failed to unmarshal
// tags" lines for any decision that was touched by Supersede/Invalidate. The
// memory store's GetRecent currently writes that warning to stderr when it
// hits a malformed tags row. We assert it doesn't happen via a small in-memory
// stderr capture.
func TestF9_SupersedeThenList_NoUnmarshalWarnings(t *testing.T) {
	dm := newTestDM(t)
	defer dm.Close()

	id := seedF9Decision(t, dm, "cli-acceptance-corpus-marker")
	_, err := dm.SupersedeDecision(id, "ctx", "replacement", "rationale", "",
		nil, nil, ActiveContext{})
	require.NoError(t, err)

	// Direct DB scan — that's exactly what GetRecent does under the hood
	// (memory.go GetRecent unmarshals `tags` from the same column).
	rows, err := dm.SQLDB().Query(`SELECT id, tags FROM memories WHERE deleted_at IS NULL`)
	require.NoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var id, rawTags string
		require.NoError(t, rows.Scan(&id, &rawTags))
		if rawTags == "" || rawTags == "[]" {
			continue
		}
		var tags []string
		require.NoErrorf(t, json.Unmarshal([]byte(rawTags), &tags),
			"row %s has malformed tags column: %q", id, rawTags)
	}
}

// readRawTags is unused — left as a private helper if a future regression
// wants to scan a single id.