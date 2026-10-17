// challenge_restore_reporting_test.go — issue #11 regressions at the
// reporting surfaces.
//
// Issue #11: `mpm challenge restore` printed a fixed success line —
// "✓ Theory <id> resolved as disproven. Memory <id> cleared." — regardless
// of whether the theory UPDATE had matched a single row. An operator
// reading that line had no way to tell a real resolution from a no-op.
//
// These tests pin the CLI text AND the machine-readable result map. The
// machine surface matters independently: `mpm call`/MCP callers get the map
// directly, so a truthful map is what lets a script detect the partial case
// at all.
//
// Every test runs against an isolated in-memory database (internal.NewTestDM).
package main

import (
	"encoding/json"
	"strings"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// issue11Seed plants a memory and challenges it through the production
// handler path, returning the memory id.
func issue11Seed(t *testing.T, dm *mpminternal.DatabaseManager, content string, weight float64) string {
	t.Helper()
	id, err := dm.SaveMemory("memories", content, "", []string{"issue11"}, nil, nil, false, weight)
	require.NoError(t, err)
	_, err = runHandler(dm, "mpm_memory", map[string]interface{}{
		"action": "challenge",
		"params": map[string]interface{}{
			"memoryId": id,
			"evidence": "contradicted by newer measurement",
		},
	})
	require.NoError(t, err)
	return id
}

// TestIssue11_CLIRestoreResolved_ReportsResolved: the healthy path keeps its
// exact success wording and exit code. Requirement: preserve existing
// healthy behaviour.
func TestIssue11_CLIRestoreResolved_ReportsResolved(t *testing.T) {
	dm := newTestDMForCmd(t)
	id := issue11Seed(t, dm, "issue11 cli healthy path", 9)

	var code int
	out := captureStdout(t, func() { code = runChallengeRestore(dm, id) })

	assert.Equal(t, 0, code, "a fully successful restore must exit 0")
	assert.Contains(t, out, "resolved as disproven",
		"a real resolution must still say so")
	assert.Contains(t, out, id)
	assert.NotContains(t, out, "NOT resolved")

	_, meta := persistedWeight(t, dm, id)
	assert.Equal(t, true, meta["restored_from_challenge"], "the restore must stamp its audit marker")
}

// TestIssue11_CLIRestoreUnresolvedTheory_ReportsPartial: the defect. A
// theory reference that matches nothing must NOT print "resolved as
// disproven".
func TestIssue11_CLIRestoreUnresolvedTheory_ReportsPartial(t *testing.T) {
	dm := newTestDMForCmd(t)
	id := issue11Seed(t, dm, "issue11 cli missing theory", 9)

	// Break the reference the way a stale legacy row would be broken:
	// point it at an id no theory row carries.
	_, err := dm.SQLDB().Exec(
		`UPDATE memories SET metadata = json_set(COALESCE(metadata,'{}'), '$.challenged_theory_id', 'deadbeefdeadbeef') WHERE id = ?`, id)
	require.NoError(t, err)

	var code int
	out := captureStdout(t, func() { code = runChallengeRestore(dm, id) })

	assert.Equal(t, 0, code,
		"the memory half succeeded; a non-zero code would imply nothing was restored")
	assert.NotContains(t, out, "resolved as disproven",
		"ISSUE 11 REGRESSION: CLI claimed a theory resolution that never happened")
	assert.Contains(t, out, "NOT resolved",
		"the operator must be told the theory was not resolved")
	assert.Contains(t, out, "deadbeefdeadbeef",
		"the offending reference must be shown so it can be diagnosed")

	weight, meta := persistedWeight(t, dm, id)
	assert.Equal(t, 9, weight, "the memory half must genuinely have been restored")
	assert.NotContains(t, meta, "challenged_theory_id")
}

// TestIssue11_CLIRestoreFailure_ExitsNonZero: a genuine failure still
// exits 1 and reports the error, rather than being softened into the
// partial-success path.
func TestIssue11_CLIRestoreFailure_ExitsNonZero(t *testing.T) {
	dm := newTestDMForCmd(t)

	// respond() writes errors to stderr, so capture both streams.
	var code int
	out := captureBoth(t, func() { code = runChallengeRestore(dm, "no-such-memory-issue11") })

	assert.Equal(t, 1, code)
	assert.Contains(t, strings.ToLower(out), "not found")
}

// TestIssue11_CLIChallengeThenRestore_ResolvesTheory: the `mpm challenge`
// CLI handler is a SECOND producer of challenges, distinct from the
// mpm_memory/MCP surface. It writes the theory's back-link under
// `memory_id`, where ChallengeMemoryWithTheory writes
// `challenged_memory_id`. Both name the same thing — the memory the theory
// contests — and both must be accepted by the restore guard.
//
// This test exists because that asymmetry is invisible until it breaks: a
// guard written against only one key leaves every CLI-created theory
// unresolvable while the mpm_memory path keeps working, which looks like a
// data problem rather than a code one.
func TestIssue11_CLIChallengeThenRestore_ResolvesTheory(t *testing.T) {
	dm := newTestDMForCmd(t)
	id, err := dm.SaveMemory("memories", "issue11 cli producer round trip", "", []string{"issue11"}, nil, nil, false, 9)
	require.NoError(t, err)

	require.Equal(t, 0, runChallenge(dm, id, "contradicted by runbook"),
		"setup: `mpm challenge` must succeed")

	_, meta := persistedWeight(t, dm, id)
	theoryID, _ := meta["challenged_theory_id"].(string)
	require.NotEmpty(t, theoryID)

	// The CLI producer stamps `memory_id` on the theory, not
	// `challenged_memory_id`. Pin that fact — if it ever changes, this
	// assertion is what tells you the guard needs revisiting.
	var theoryMeta string
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT COALESCE(metadata,'{}') FROM memories WHERE id = ?`, theoryID).Scan(&theoryMeta))
	assert.Contains(t, theoryMeta, `"memory_id":"`+id+`"`,
		"setup: the CLI challenge path back-links via metadata.memory_id")
	assert.NotContains(t, theoryMeta, "challenged_memory_id",
		"setup: this is the key the CLI path does NOT use")

	var code int
	out := captureStdout(t, func() { code = runChallengeRestore(dm, id) })

	assert.Equal(t, 0, code)
	assert.Contains(t, out, "resolved as disproven",
		"a theory created by `mpm challenge` must still be resolved by `mpm challenge restore`")
	assert.NotContains(t, out, "NOT resolved")

	var status string
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT json_extract(metadata,'$.status') FROM memories WHERE id = ?`, theoryID).Scan(&status))
	assert.Equal(t, "disproven", status)

	weight, after := persistedWeight(t, dm, id)
	assert.Equal(t, 9, weight, "the pre-challenge weight must be restored")
	assert.NotContains(t, after, "challenged_theory_id")
}

// TestIssue11_MachineSurfaceReportsTheoryOutcome: `mpm call mpm_memory
// {"action":"restore_challenge"}` returns the core map verbatim. A script
// consuming it must be able to detect the partial case from the payload
// alone — this is the surface a CI gate or an agent would read.
func TestIssue11_MachineSurfaceReportsTheoryOutcome(t *testing.T) {
	dm := newTestDMForCmd(t)

	t.Run("resolved", func(t *testing.T) {
		id := issue11Seed(t, dm, "issue11 machine resolved path", 9)

		out, err := runHandler(dm, "mpm_memory", map[string]interface{}{
			"action": "restore_challenge",
			"params": map[string]interface{}{"memory_id": id},
		})
		require.NoError(t, err)
		res := out.(map[string]interface{})

		assert.Equal(t, true, res["theory_resolved"])
		assert.Equal(t, true, res["complete"])
		assert.Equal(t, "disproven", res["theory_status"])
		assert.Equal(t, "resolved", res["theory_resolution"])

		// The map must survive a JSON round trip with the same meaning:
		// a machine consumer reads the encoded form.
		blob, err := json.Marshal(res)
		require.NoError(t, err)
		var decoded map[string]interface{}
		require.NoError(t, json.Unmarshal(blob, &decoded))
		assert.Equal(t, true, decoded["theory_resolved"])
		assert.Equal(t, "disproven", decoded["theory_status"])
	})

	t.Run("not resolved", func(t *testing.T) {
		id := issue11Seed(t, dm, "issue11 machine partial path", 9)
		_, err := dm.SQLDB().Exec(
			`UPDATE memories SET metadata = json_set(COALESCE(metadata,'{}'), '$.challenged_theory_id', 'cafebabecafebabe') WHERE id = ?`, id)
		require.NoError(t, err)

		out, err := runHandler(dm, "mpm_memory", map[string]interface{}{
			"action": "restore_challenge",
			"params": map[string]interface{}{"memory_id": id},
		})
		require.NoError(t, err)
		res := out.(map[string]interface{})

		assert.Equal(t, false, res["theory_resolved"],
			"ISSUE 11 REGRESSION: machine surface claimed a resolution that matched no rows")
		assert.Equal(t, false, res["complete"])
		assert.Equal(t, "not_found", res["theory_resolution"])
		assert.Equal(t, "identifier", res["theory_reference"])
		assert.NotEmpty(t, res["theory_warning"])
	})
}
