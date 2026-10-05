// drill_session_identity_reproducer_test.go — proves the drill-run
// session identity split that §3 of the session-identity fix is
// supposed to reproduce, and locks in the post-§6 invariant so a
// future regression of the "harness mints its own sessionID" bug
// cannot land silently.
//
// INVARIANT (§4, enforced by the assertions below + §9 + §16):
//
//	For every drill run, drill_runs.session_id == tool_invocations.session_id.
//
// The orchestrator (scheduler or CLI dispatcher) mints the canonical
// session_id S for each drill. Every component on the run path
// (ClaudeCodeHarness, SyntheticHarness, mpm-mcp, the fake claude
// shim) consumes S as-is and never mints its own. S is the sole
// join key between drill_runs and its tool_invocation evidence; any
// split is a provenance break.
//
// Historical bug (pre-§6):
//
//   - Scheduler / CLI orchestrator mints A and stores drill_runs.session_id=A.
//   - ClaudeCodeHarness.Launch mints its own B and sets MPM_SESSION_ID=B.
//   - mpm-mcp writes tool_invocations rows tagged with B.
//   - drill_runs.session_id = A; tool_invocations.session_id = B.
//   - An operator joining drill_runs.session_id = tool_invocations.session_id
//     to trace evidence back to its drill_run finds ZERO rows.
//   - Score() consumes rows under B; verdict cannot be back-traced.
//
// This test runs through the FULL path (orchestrator → harness →
// fake-claude → audit row) using the post-§6 API and asserts the
// correlation holds. It was originally written as a "demonstrate
// the bug" reproducer (asserts A != B); post-§6 its assertions flip
// to the positive contract (A == B) so the test name still maps to
// the historical bug it locks out. §16 reintroduces the bug to
// verify this test catches the regression.

package internal

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestClaudeCodeHarness_SessionIdentity_CallerOwned is the §3 / §9
// hermetic regression. It exercises the orchestrator → harness →
// fake-claude → audit-row path and asserts the drill-run session
// identity invariant (§4): drill_runs.session_id ==
// tool_invocations.session_id for the same run.
//
// The test MUST be hermetic: no real `claude`, no real `mpm-mcp`,
// no live DB, no network. It is allowed to depend on the `sqlite3`
// CLI for the fake binary's audit-row write — that CLI ships in
// the standard MPM dev environment and is present on every CI box.
//
// Failure mode this test locks out (pre-§6):
//
//   - caller (orchestrator) mints session_id A and stores drill_runs.session_id=A
//   - harness.Launch mints its own session_id B and sets MPM_SESSION_ID=B
//   - fake claude writes tool_invocations.session_id=B (because the env said so)
//   - drill_runs.session_id = A; tool_invocations.session_id = B
//   - A != B  →  assertions below fail
//
// The post-§6 API makes the harness reject an empty sessionID and
// thread the caller's id through MPM_SESSION_ID + writeMcpConfig +
// Finish scope. This test is the regression guard: if anyone
// reintroduces "harness mints its own id" (e.g. via a SetSessionID
// escape hatch or by skipping the new parameter), every assertion
// below trips. §16 documents this contract by temporarily
// reintroducing the old behaviour and verifying the test catches it.
func TestClaudeCodeHarness_SessionIdentity_CallerOwned(t *testing.T) {
	// Sanity: the test relies on the sqlite3 CLI being available to
	// the fake claude. Skip cleanly on hosts without it so the gate
	// isn't accidentally blamed on the harness code.
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skipf("sqlite3 CLI not on PATH — §3 reproducer requires it: %v", err)
	}

	workspace := t.TempDir()
	t.Setenv("MPM_WORKSPACE", workspace)

	// Minimal stubs so any code that introspects the tree (mode/,
	// persona/) does not panic. The harness itself does not need these
	// but mpm-mcp (called by real claude) does; the fake claude does
	// not invoke mpm-mcp, so they are documentation rather than
	// functional prerequisites here.
	require.NoError(t, os.MkdirAll(filepath.Join(workspace, "mode"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(workspace, "persona"), 0o755))

	// Real file-based DM so the fake claude's sqlite3 invocation
	// targets the same DB the test asserts against.
	dm, err := NewDatabaseManager(workspace)
	require.NoError(t, err)
	t.Cleanup(func() { _ = dm.Close() })
	require.NoError(t, dm.InitSchema())

	// Deterministic caller session_id A. The "A-fixed" suffix is
	// intentional: it lets the assertion message read like a proof
	// rather than a UUID soup.
	callerSessionID := "drill-session-A-fixed-" + t.Name()
	drillID := "reproduce-identity-mismatch"
	_, err = dm.SQLDB().Exec(`
		INSERT INTO drill_runs
		    (id, drill_id, framework, session_id, status, started_at)
		VALUES (?, ?, ?, ?, 'running', ?)`,
		"run-reproducer-1",
		drillID,
		"claude_code",
		callerSessionID,
		time.Now().Unix(),
	)
	require.NoError(t, err, "drill_runs seed")

	// Fake claude: a shell script that reads MPM_SESSION_ID and
	// MPM_WORKSPACE from the environment the harness sets, writes
	// exactly one tool_invocations row tagged with MPM_SESSION_ID, and
	// exits 0. This stands in for the production mpm-mcp audit hook
	// (cmd/mpm-mcp reads MPM_SESSION_ID from env and stamps every
	// tool_invocation row with it — see internal/core/provenance.go).
	fakeClaudePath := writeFakeClaudeShim(t)

	// mpmMcpPath must exist as a file (harness.isAvailable checks).
	// Reuse the fake claude path; isAvailable only stats, it does not
	// exec. The fake claude does not actually invoke mpm-mcp.
	h := NewClaudeCodeHarness(workspace, fakeClaudePath, fakeClaudePath)

	drill := DrillSpec{
		ID:          drillID,
		Framework:   "claude_code",
		Prompt:      "(fake prompt — fake claude ignores all arguments)",
		TimeoutSecs: 30,
		Expect: DrillExpect{
			ToolsRequired: []string{"mpm_lessons"},
			Sequence: []DrillStep{
				{Tool: "mpm_lessons", Action: "save"},
			},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// The orchestrator (this test stands in for it) supplies its
	// sessionID. Pre-§6 the harness would have minted its own and
	// silently broken the drill-run session identity invariant; the
	// §6 API rejects the implicit mint and threads the caller's id
	// through MPM_SESSION_ID + writeMcpConfig + Finish scope. This
	// test stands in for the §3 reproducer: under post-§6 code it
	// asserts the invariant holds. §16 reintroduces the bug
	// temporarily to verify the assertions catch the regression.
	harnessSessionID, err := h.Launch(ctx, drill, callerSessionID)
	require.NoError(t, err, "Launch")
	require.Equal(t, callerSessionID, harnessSessionID,
		"post-§6 invariant: harness returns the caller-owned sessionID "+
			"unchanged; it must not mint its own. Mismatch here means "+
			"§6 regressed — see this file's docstring for the historical "+
			"bug shape.")

	calls, err := h.Finish(ctx, dm.SQLDB())
	require.NoError(t, err, "Finish")
	require.Len(t, calls, 1, "fake claude wrote exactly 1 tool_invocations row")

	// The audit row must be tagged with the caller's session_id (A),
	// because the harness set MPM_SESSION_ID=A in the env and the fake
	// claude honours it. This is what mpm-mcp does in production.
	var writtenSessionID string
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT session_id FROM tool_invocations WHERE id = ?`,
		"inv-fake-claude-1",
	).Scan(&writtenSessionID))
	require.Equal(t, callerSessionID, writtenSessionID,
		"audit row must be tagged with the caller's session_id (A) — "+
			"the harness set MPM_SESSION_ID=A; mpm-mcp stamps every row "+
			"with it. A pre-fix regression (harness minting its own id) "+
			"would surface here.")

	// The drill-run session identity invariant (post-§6):
	// drill_runs.session_id == tool_invocations.session_id == A.
	var rowsUnderA int
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM tool_invocations WHERE session_id = ?`,
		callerSessionID,
	).Scan(&rowsUnderA))
	require.Equal(t, 1, rowsUnderA,
		"drill_runs.session_id = A must point at one audit row under A. "+
			"Pre-fix, this count was 0 (rows landed under the harness's "+
			"unrelated UUID) — see this file's docstring for the bug shape.")

	// No rows should exist under any other (harness-minted) UUID.
	var orphanRows int
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM tool_invocations WHERE session_id != ?`,
		callerSessionID,
	).Scan(&orphanRows))
	require.Equal(t, 0, orphanRows,
		"audit rows must all live under the caller's session_id. "+
			"Pre-fix the harness minted a second UUID; orphan rows here "+
			"indicate a regression of that pattern.")

	// The verdict was derived from rows under A. Finish already filtered
	// by h.sessionID (= A, caller-owned post-§6); Score sees only rows
	// under A. Verdict is now back-traceable to drill_runs via
	// session_id.
	verdict := Score(drill, calls, dm.SQLDB())
	require.True(t, verdict.Passed, "the synthetic call matches the spec")

	// What an operator sees when they try to trace evidence:
	// drill_runs ↔ tool_invocations join on session_id returns 1 row.
	var traceable int
	require.NoError(t, dm.SQLDB().QueryRow(`
		SELECT COUNT(*) FROM drill_runs dr
		JOIN tool_invocations ti ON ti.session_id = dr.session_id
		WHERE dr.id = ?`,
		"run-reproducer-1",
	).Scan(&traceable))
	require.Equal(t, 1, traceable,
		"drill_runs ↔ tool_invocations join on session_id must return "+
			"the run's evidence. Pre-fix this was 0 — the operator could "+
			"not trace 'where is the evidence for this drill_run?'")
}

// writeFakeClaudeShim drops an executable shell script at
// <tmpdir>/bin/claude-fake that simulates the contract mpm-mcp honours
// in production: read MPM_SESSION_ID and MPM_WORKSPACE, insert one
// tool_invocations row tagged with the env-passed session_id, exit 0.
// The script never speaks MCP and never invokes any other tool — it
// is the minimum a reproducer needs to demonstrate the audit-row
// scope.
func writeFakeClaudeShim(t *testing.T) string {
	t.Helper()

	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skipf("sqlite3 CLI required for fake claude shim: %v", err)
	}

	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	require.NoError(t, os.MkdirAll(binDir, 0o755))

	scriptPath := filepath.Join(binDir, "claude-fake")

	body := fmt.Sprintf(`#!/bin/sh
# Fake claude — stands in for the real Claude Code CLI in tests.
# Honours MPM_SESSION_ID by inserting exactly one tool_invocations
# row tagged with the env-passed session_id, then exits 0. Mirrors
# what mpm-mcp's audit hook does in cmd/mpm-mcp/main.go (stamps the
# session_id from MPM_SESSION_ID onto every tool_invocation row).
set -e

if [ -z "$MPM_SESSION_ID" ]; then
  echo "fake claude: MPM_SESSION_ID is empty" >&2
  exit 1
fi
if [ -z "$MPM_WORKSPACE" ]; then
  echo "fake claude: MPM_WORKSPACE is empty" >&2
  exit 1
fi

DB="$MPM_WORKSPACE/src/db/mpm.db"
NOW=$(date +%%s)

sqlite3 "$DB" <<EOF
INSERT INTO tool_invocations
  (id, session_id, tool_name, action, invocation_id,
   actor_kind, payload_hash, result_status,
   started_at)
VALUES
  ('inv-fake-claude-1', '$MPM_SESSION_ID', 'mpm_lessons', 'save',
   'uuid-fake-1', 'agent', 'sha256:fake', 'success', $NOW);
EOF

exit 0
`)

	require.NoError(t, os.WriteFile(scriptPath, []byte(body), 0o755))
	return scriptPath
}
