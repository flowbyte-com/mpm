// theory_resolve_hook_test.go — tests for the auto-resolve hook in
// SaveMemoryWithContext. Companion to theory_resolve_hook.go.
//
// Coverage matrix:
//
//	Trigger shapes:
//	  - tag-based   (theory:<id> + outcome:proven|disproven)
//	  - content-based (Theory <id> resolved PROVEN|DISPROVEN)
//	  - mixed       (both tag and content)
//
//	Idempotency / safety:
//	  - no outcome → no resolution (we never infer)
//	  - already-resolved theory → no-op
//	  - non-existent theory ID → log + skip
//	  - non-theory collection → log + skip
//	  - atomicity: memory is saved even when theory resolve fails
package internal

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── Pure-function tests for detectTheoryResolutions ─────────────────────

func TestDetectTheoryResolutions_TagBased(t *testing.T) {
	res := detectTheoryResolutions(
		"unrelated content here",
		[]string{"theory:abcdef0123456789", "outcome:disproven"},
	)
	require.Len(t, res, 1, "tag-based trigger should produce one resolution")
	assert.Equal(t, "abcdef0123456789", res[0].theoryID)
	assert.Equal(t, "disproven", res[0].outcome)
}

func TestDetectTheoryResolutions_ContentBased(t *testing.T) {
	res := detectTheoryResolutions(
		"Result: Theory abcdef0123456789 resolved PROVEN on 2026-07-22.",
		nil,
	)
	require.Len(t, res, 1)
	assert.Equal(t, "abcdef0123456789", res[0].theoryID)
	assert.Equal(t, "proven", res[0].outcome)
}

func TestDetectTheoryResolutions_MixedSources(t *testing.T) {
	// Tag and content reference the same ID — dedup to one resolution.
	res := detectTheoryResolutions(
		"Theory abcdef0123456789 resolved DISPROVEN. Confirmed via tag also.",
		[]string{"theory:abcdef0123456789", "outcome:disproven"},
	)
	require.Len(t, res, 1)
	assert.Equal(t, "disproven", res[0].outcome)
}

func TestDetectTheoryResolutions_MultipleTheories(t *testing.T) {
	// Multiple theories with the same outcome. Both IDs must be
	// exactly 16 hex chars (the regex enforces this).
	res := detectTheoryResolutions(
		"Multiple: Theory aaaaaaaabbbbbbbb resolved PROVEN and Theory cccccccddddddddd resolved PROVEN.",
		[]string{"outcome:proven"},
	)
	require.Len(t, res, 2)
	ids := []string{res[0].theoryID, res[1].theoryID}
	assert.Contains(t, ids, "aaaaaaaabbbbbbbb")
	assert.Contains(t, ids, "cccccccddddddddd")
	assert.Equal(t, "proven", res[0].outcome)
	assert.Equal(t, "proven", res[1].outcome)
}

func TestDetectTheoryResolutions_RequiresOutcome(t *testing.T) {
	// Bare theory: tag with no outcome → no resolution (never infer).
	res := detectTheoryResolutions(
		"some content with Theory abcdef0123456789 mentioned but not resolved.",
		[]string{"theory:abcdef0123456789"},
	)
	assert.Nil(t, res, "bare theory ref without outcome must not produce a resolution")
}

func TestDetectTheoryResolutions_BadIDIgnored(t *testing.T) {
	// Non-hex or wrong-length IDs are silently dropped.
	res := detectTheoryResolutions(
		"Theory xyz123 resolved PROVEN. Theory abcdef0123456789 resolved PROVEN.",
		nil,
	)
	require.Len(t, res, 1)
	assert.Equal(t, "abcdef0123456789", res[0].theoryID,
		"only well-formed 16-hex IDs should pass")
}

func TestDetectTheoryResolutions_Dedup(t *testing.T) {
	// Same ID in tags and content → single resolution.
	res := detectTheoryResolutions(
		"Theory abcdef0123456789 resolved PROVEN.",
		[]string{"theory:abcdef0123456789", "outcome:proven"},
	)
	require.Len(t, res, 1)
}

func TestDetectTheoryResolutions_OutcomeCaseInsensitive(t *testing.T) {
	// "resolved PROVEN" vs "resolved proven" — both should produce the same outcome.
	for _, outcome := range []string{"PROVEN", "proven", "Proven"} {
		res := detectTheoryResolutions(
			"Theory abcdef0123456789 resolved "+outcome+".",
			nil,
		)
		require.Len(t, res, 1, "outcome=%q", outcome)
		assert.Equal(t, "proven", res[0].outcome, "outcome=%q", outcome)
	}
}

// ── lookupTheoryStatus preflight tests ──────────────────────────────────

func TestLookupTheoryStatus_Missing(t *testing.T) {
	dm := NewTestDM(t)
	status, found, err := dm.lookupTheoryStatus("0000000000000000")
	require.NoError(t, err)
	assert.False(t, found)
	assert.Equal(t, "", status)
}

func TestLookupTheoryStatus_WrongCollection(t *testing.T) {
	dm := NewTestDM(t)
	store, err := dm.getSharedStore()
	require.NoError(t, err)
	// Insert a memory that's NOT in the theories collection.
	mem, err := store.AddMemory("not a theory", "memories", nil, nil, "", "test")
	require.NoError(t, err)
	_, found, err := dm.lookupTheoryStatus(mem.ID)
	assert.Error(t, err, "should reject non-theory collection")
	assert.False(t, found)
}

// ── Integration tests for SaveMemoryWithContext + hook ─────────────────

func TestSaveMemoryWithContext_HookFires_TagBased(t *testing.T) {
	dm := NewTestDM(t)
	// Create a pending theory.
	resp, err := dm.ProposeTheory(
		"Test theory: Swiatek loses Wimbledon",
		"Confirm: loses in R3",
		nil, nil,
		[]string{"test"},
	)
	require.NoError(t, err)
	theoryID, ok := resp["id"].(string)
	require.True(t, ok)
	require.NotEmpty(t, theoryID)

	// Save a memory with the trigger tags.
	result, _, err := dm.SaveMemoryWithContext(
		"Swiatek beaten in R3 2026.",
		"memories",
		[]string{"theory:" + theoryID, "outcome:disproven"},
		0.5, "",
		ActiveContext{},
	)
	require.NoError(t, err)

	applied, ok := result["theory_resolutions_applied"].([]string)
	require.True(t, ok, "result should report resolutions applied")
	assert.Equal(t, []string{theoryID}, applied)

	// Verify the theory is now marked disproven.
	status, found, err := dm.lookupTheoryStatus(theoryID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "disproven", status)
}

func TestSaveMemoryWithContext_HookFires_ContentBased(t *testing.T) {
	dm := NewTestDM(t)
	resp, err := dm.ProposeTheory(
		"Content-based test theory.",
		"v=42",
		nil, nil,
		[]string{"test"},
	)
	require.NoError(t, err)
	theoryID := resp["id"].(string)

	// Content contains "Theory <id> resolved PROVEN" with no tag triggers.
	content := "After observing matches, Theory " + theoryID + " resolved PROVEN. Confirmed."
	result, _, err := dm.SaveMemoryWithContext(
		content, "memories",
		[]string{},
		0.5, "",
		ActiveContext{},
	)
	require.NoError(t, err)
	applied, ok := result["theory_resolutions_applied"].([]string)
	require.True(t, ok)
	assert.Equal(t, []string{theoryID}, applied)

	status, found, err := dm.lookupTheoryStatus(theoryID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "proven", status)
}

func TestSaveMemoryWithContext_NoHookWithoutOutcome(t *testing.T) {
	dm := NewTestDM(t)
	resp, err := dm.ProposeTheory("Bare ref test.", "", nil, nil, []string{"test"})
	require.NoError(t, err)
	theoryID := resp["id"].(string)

	// theory: tag without outcome → hook MUST NOT fire.
	result, _, err := dm.SaveMemoryWithContext(
		"Just referencing Theory "+theoryID+" without resolution.",
		"memories",
		[]string{"theory:" + theoryID},
		0.5, "",
		ActiveContext{},
	)
	require.NoError(t, err)
	_, hasApplied := result["theory_resolutions_applied"]
	assert.False(t, hasApplied, "no outcome → no resolution applied")

	// Theory should still be pending.
	status, found, err := dm.lookupTheoryStatus(theoryID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "pending", status, "theory must remain pending when no outcome is given")
}

func TestSaveMemoryWithContext_IdempotentOnAlreadyResolved(t *testing.T) {
	dm := NewTestDM(t)
	resp, err := dm.ProposeTheory("Idempotency test.", "", nil, nil, []string{"test"})
	require.NoError(t, err)
	theoryID := resp["id"].(string)

	// Pre-resolve the theory manually.
	_, err = dm.ResolveTheory(theoryID, "manual pre-resolve for test", "disproven")
	require.NoError(t, err)

	// Now save a memory that references it as DISPROVEN. The hook should
	// observe status != pending and skip without error.
	result, _, err := dm.SaveMemoryWithContext(
		"Already-resolved theory test.",
		"memories",
		[]string{"theory:" + theoryID, "outcome:disproven"},
		0.5, "",
		ActiveContext{},
	)
	require.NoError(t, err)
	applied, hasApplied := result["theory_resolutions_applied"].([]string)
	if hasApplied {
		assert.Empty(t, applied, "already-resolved theory should not be re-applied")
	}

	// Status should still be "disproven" (manual), unchanged.
	status, _, err := dm.lookupTheoryStatus(theoryID)
	require.NoError(t, err)
	assert.Equal(t, "disproven", status)
}

func TestSaveMemoryWithContext_BogusTheoryIDTolerated(t *testing.T) {
	dm := NewTestDM(t)
	// Reference a non-existent theory — preflight fails, hook skipped,
	// but memory save succeeds.
	result, _, err := dm.SaveMemoryWithContext(
		"Referencing a phantom theory.",
		"memories",
		[]string{"theory:0000000000000000", "outcome:proven"},
		0.5, "",
		ActiveContext{},
	)
	require.NoError(t, err, "memory save must succeed even when theory ref is bogus")
	assert.NotEmpty(t, result["id"])
	// resolutions applied will be empty because preflight caught the missing theory.
	if applied, ok := result["theory_resolutions_applied"].([]string); ok {
		assert.Empty(t, applied)
	}
}

func TestSaveMemoryWithContext_FastPathUnchanged(t *testing.T) {
	dm := NewTestDM(t)
	// No theory refs at all → fast path, identical to pre-hook behavior.
	result, _, err := dm.SaveMemoryWithContext(
		"Just a regular memory.",
		"memories",
		[]string{"unrelated", "tags"},
		0.5, "",
		ActiveContext{},
	)
	require.NoError(t, err)
	assert.NotEmpty(t, result["id"])
	_, hasApplied := result["theory_resolutions_applied"]
	assert.False(t, hasApplied, "fast path should never carry theory_resolutions_applied")
}