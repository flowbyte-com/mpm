// decision_no_duplication_test.go — alpha-4.1.2 D-007/W-006 regression test.
//
// Audit finding: the auditor flagged that `mpm decisions record-decision`
// produced a row whose content duplicated metadata.context/rationale/outcome
// (each structured field appeared twice — once in content with a "CHOICE: "
// style label, once in metadata).
//
// Fix (alpha-4.1.1): RecordDecision was rewritten so content holds the
// canonical body text (no labels) and metadata holds the structured fields.
// show/list reads the row as stored and never reconstructs the content
// from metadata.
//
// This test pins the post-fix contract: a decision row's content must NOT
// contain the legacy "CHOICE: ", "CONTEXT: ", "RATIONALE: ", "OUTCOME: "
// labels. The metadata should hold the structured fields cleanly. There
// is no projection of metadata → content at read time (no duplication).

package internal

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRecordDecision_NoContentMetadataDuplication(t *testing.T) {
	dm := newTestDM(t)

	const (
		ctx   = "context paragraph that should appear once"
		cho   = "decision choice that should appear once"
		rat   = "rationale paragraph that should appear once"
		out   = "outcome paragraph that should appear once"
	)
	res, err := dm.RecordDecision(ctx, cho, rat, out, []string{"alpha-4.1.2", "dedup"}, nil, ActiveContext{})
	require.NoError(t, err)
	id, _ := res["id"].(string)
	require.NotEmpty(t, id)

	// Pull the row as stored (no projection of metadata → content).
	var content string
	var metaJSON string
	require.NoError(t, dm.QueryRowTracked(
		`SELECT content, metadata FROM memories WHERE id = ?`, id,
	).Scan(&content, &metaJSON))

	// 1. None of the legacy labels survive in content.
	for _, label := range []string{"CHOICE:", "CONTEXT:", "RATIONALE:", "OUTCOME:"} {
		assert.NotContains(t, content, label,
			"content must not carry legacy %s label (would indicate double-write)", label)
	}

	// 2. Each structured field appears exactly once in content.
	assert.Equal(t, 1, strings.Count(content, ctx), "context appears once in content")
	assert.Equal(t, 1, strings.Count(content, cho), "choice appears once in content")
	assert.Equal(t, 1, strings.Count(content, rat), "rationale appears once in content")
	assert.Equal(t, 1, strings.Count(content, out), "outcome appears once in content")

	// 3. Metadata carries the structured fields (parsed JSON).
	assert.Contains(t, metaJSON, "context", "metadata should hold context")
	assert.Contains(t, metaJSON, "rationale", "metadata should hold rationale")
	assert.Contains(t, metaJSON, "outcome", "metadata should hold outcome")
	assert.Contains(t, metaJSON, "choice", "metadata should hold choice")
}

// TestRecordDecision_EmptyFields_NoPhantomLabels pins that a decision
// recorded with some empty fields (e.g. no outcome yet — common during
// initial recording) does not synthesise "OUTCOME: " labels for the
// missing parts. The pre-fix path always emitted all four labels even
// when the underlying values were empty, producing "OUTCOME: \n" or
// similar.
func TestRecordDecision_EmptyFields_NoPhantomLabels(t *testing.T) {
	dm := newTestDM(t)
	res, err := dm.RecordDecision("ctx", "chose X", "because", "", nil, nil, ActiveContext{})
	require.NoError(t, err)
	id, _ := res["id"].(string)
	require.NotEmpty(t, id)

	var content string
	require.NoError(t, dm.QueryRowTracked(
		`SELECT content FROM memories WHERE id = ?`, id,
	).Scan(&content))

	// No legacy labels even with outcome empty.
	for _, label := range []string{"CHOICE:", "CONTEXT:", "RATIONALE:", "OUTCOME:"} {
		assert.NotContains(t, content, label,
			"empty fields must not produce phantom %s labels", label)
	}
	// Content should still carry the non-empty fields.
	assert.Contains(t, content, "ctx")
	assert.Contains(t, content, "chose X")
}