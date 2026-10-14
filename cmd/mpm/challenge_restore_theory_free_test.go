// challenge_restore_theory_free_test.go — issue #24 at the CLI surface.
//
// Issue #24: `applyResolution` and `applyArbitrationResolution` challenge a
// loser WITHOUT a `challenged_theory_id` — there is no theory to disprove,
// and fabricating one would send a restore hunting for a theory that was
// never created. Before this contract the two writers were refused outright
// ("is not challenged"), so this branch never rendered; the CLI had no
// wording for a complete restore that had no theory half.
//
// These tests pin that wording. The core contract itself is pinned in
// internal/core/challenge_recovery_contract_test.go, where the real Arc 1
// writers run against an attached shared DB; here the row is stamped in the
// shape those writers produce, because the CLI's job is to render an
// outcome, not to decide it.
//
// Every test runs against an isolated in-memory database.
package main

import (
	"testing"

	internal "github.com/flowbyte-com/mpm-core"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// issue24SeedTheoryFreeChallenge plants a memory in exactly the shape the
// two adjudication writers leave behind: challenged, both reduced columns
// with a recorded prior, and NO challenged_theory_id.
func issue24SeedTheoryFreeChallenge(t *testing.T, dm *internal.DatabaseManager, content string, weight float64, rp float64) string {
	t.Helper()
	id, err := dm.SaveMemory("memories", content, "", []string{"issue24"}, nil, nil, false, weight)
	require.NoError(t, err)

	_, err = dm.SQLDB().Exec(`
		UPDATE memories
		SET metadata = json_patch(COALESCE(metadata,'{}'), ?),
		    weight = MAX(1, weight - 2),
		    retrieval_priority = MAX(0.01, retrieval_priority - 0.02)
		WHERE id = ? AND deleted_at IS NULL`,
		`{"status":"challenged","challenged_via":"contradiction_resolution","challenged_at":1760000000,"challenged_prior_weight":9,"challenged_prior_retrieval_priority":0.8}`, id)
	require.NoError(t, err)
	return id
}

// TestIssue24_CLIRestoreTheoryFree_ReportsComplete: a challenge with no
// linked theory restores COMPLETELY. The CLI must say so plainly rather
// than reporting a missing theory, because there is no theory to be missing
// — an operator reading "theory NOT resolved" would go looking for one.
func TestIssue24_CLIRestoreTheoryFree_ReportsComplete(t *testing.T) {
	dm := newTestDMForCmd(t)
	id := issue24SeedTheoryFreeChallenge(t, dm, "issue24 cli theory free", 9, 0.8)

	var code int
	out := captureStdout(t, func() { code = runChallengeRestore(dm, id) })

	assert.Equal(t, 0, code, "a complete restore must exit 0")
	assert.Contains(t, out, "cleared")
	assert.Contains(t, out, "no linked theory",
		"the CLI must explain WHY there is no theory half")
	assert.NotContains(t, out, "NOT resolved",
		"a theory-free challenge must never render as a partial success")
	assert.NotContains(t, out, "resolved as disproven",
		"no theory was resolved, so the word must not appear")

	// Both reduced columns are named, because 'cleared' alone does not say
	// which ones were put back.
	assert.Contains(t, out, "restored: weight, retrieval_priority")

	w, meta := persistedWeight(t, dm, id)
	assert.Equal(t, 9, w, "the recorded pre-challenge weight must be reinstated")
	assert.NotContains(t, meta, "status", "the challenged flag must be cleared")
}

// TestIssue24_CLIRestoreUnrecoverable_ReportsPermanent: a challenged row
// with NO recorded baseline — the shape left by the two adjudication
// writers before #24 — must be refused with a message that distinguishes it
// from "is not challenged", and must say the slash cannot be reversed.
// Silently clearing the flag and leaving the slash in place would report a
// restore that only half happened.
func TestIssue24_CLIRestoreUnrecoverable_ReportsPermanent(t *testing.T) {
	dm := newTestDMForCmd(t)
	id, err := dm.SaveMemory("memories", "issue24 cli unrecoverable", "", []string{"issue24"}, nil, nil, false, 9)
	require.NoError(t, err)

	_, err = dm.SQLDB().Exec(`
		UPDATE memories
		SET metadata = json_patch(COALESCE(metadata,'{}'), ?),
		    weight = weight - 2
		WHERE id = ? AND deleted_at IS NULL`,
		`{"status":"challenged","challenged_via":"contradiction_resolution","challenged_at":1760000000}`, id)
	require.NoError(t, err)

	var code int
	// Errors go to stderr via respond(), so this assertion reads the
	// combined streams rather than stdout alone.
	out := captureBoth(t, func() { code = runChallengeRestore(dm, id) })

	assert.Equal(t, 1, code, "an unrecoverable challenge must exit 1")
	assert.Contains(t, out, "cannot be reversed")
	assert.Contains(t, out, "contradiction_resolution",
		"the message must name the writer that produced the challenge")
	assert.NotContains(t, out, "cleared", "nothing was cleared")

	w, meta := persistedWeight(t, dm, id)
	assert.Equal(t, 7, w, "a refused restore must not move anything")
	assert.Equal(t, "challenged", meta["status"], "the challenge must remain in force")
}
