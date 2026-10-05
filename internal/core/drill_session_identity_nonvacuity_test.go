// drill_session_identity_nonvacuity_test.go — §16 non-vacuity guard for
// the drill-run session identity invariant (§4).
//
// §3 / §9 / §15 assert the post-fix invariant. Without a non-vacuity
// guard, those assertions could pass against an empty workspace
// (no rows, no split, no observed bug) and still green — leaving
// the regression shape undetected if the §6 fix were silently
// reverted.
//
// §16 reintroduces the bug shape explicitly so the §9 invariant
// assertions demonstrably fail when the bug is present, and §10's
// cross-run bleed test demonstrably fails when sessionIDs are
// accidentally reused. The fix is never reverted here — these tests
// model the pre-§6 harness behaviour with an in-test fake and
// assert that the invariant assertions correctly reject it.

package internal

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestClaudeCodeHarness_BuggyHarnessViolatesInvariant is the §16
// non-vacuity check for the §9 correlation test. It constructs a
// hermetic reproducer of the pre-§6 harness behaviour:
//
//   - caller (orchestrator) mints sessionID A
//   - caller stores drill_runs.session_id = A
//   - buggy harness IGNORES A and mints B
//   - buggy harness sets MPM_SESSION_ID = B
//   - mpm-mcp (modelled by a fake claude) writes tool_invocations under B
//
// Then asserts the §9 invariant is violated: A != B, drill_runs ↔
// tool_invocations join returns 0 rows. This is the OBSERVED state
// pre-§6; §9's green-on-the-fixed-code assertions are non-vacuous
// only if they would fail under THIS state.
//
// This test is NOT a regression test of the production code — the
// production harness has been fixed (§6) and no longer mints its own
// id. The buggy harness lives in this test file as a fixture; if
// anyone reverts §6 the §15 static guards fire first (cheaper
// tripwire), but §16 documents the historical bug shape and shows the
// correlation test's failure mode.
func TestClaudeCodeHarness_BuggyHarnessViolatesInvariant(t *testing.T) {
	if _, err := os.Stat("/usr/bin/sqlite3"); err != nil {
		t.Skipf("sqlite3 CLI required: %v", err)
	}

	workspace := t.TempDir()
	t.Setenv("MPM_WORKSPACE", workspace)
	require.NoError(t, os.MkdirAll(filepath.Join(workspace, "mode"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(workspace, "persona"), 0o755))

	dm, err := NewDatabaseManager(workspace)
	require.NoError(t, err)
	t.Cleanup(func() { _ = dm.Close() })
	require.NoError(t, dm.InitSchema())

	// Caller (orchestrator) mints A. The drill_runs row records it.
	callerSessionID := "drill-session-A-canonical-" + uuid.NewString()
	drillID := "nonvacuity-buggy-harness"
	_, err = dm.SQLDB().Exec(`
		INSERT INTO drill_runs
		    (id, drill_id, framework, session_id, status, started_at)
		VALUES (?, ?, ?, ?, 'running', ?)`,
		"run-buggy-fixture",
		drillID,
		"claude_code",
		callerSessionID,
		time.Now().Unix(),
	)
	require.NoError(t, err)

	// Buggy harness: IGNORES the caller's sessionID and mints B.
	// Mirrors the pre-§6 behaviour of drill_claude_code_harness.go's
	// Launch: `h.sessionID = uuid.NewString()` instead of
	// `h.sessionID = sessionID` (the supplied parameter).
	buggyHarnessSessionID := "drill-session-B-minted-" + uuid.NewString()

	// The harness sets the buggy harness's id as the env. The fake
	// claude reads MPM_SESSION_ID and stamps it onto audit rows —
	// exactly mirroring the production mpm-mcp audit hook. The
	// audit row therefore lands under B, while drill_runs points at A.
	dbPath := dm.DBPath()
	require.NoError(t, os.WriteFile(dbPath+".buggy-ok", []byte{}, 0o644)) // touch to verify path

	// Drop the fake claude shim in PATH so the simulation is
	// end-to-end through the env+audit-row path (not just a direct
	// INSERT).
	fakeBinDir := t.TempDir()
	fakeClaudePath := filepath.Join(fakeBinDir, "claude-fake")
	require.NoError(t, os.WriteFile(fakeClaudePath, []byte(`#!/bin/sh
set -e
if [ -z "$MPM_SESSION_ID" ] || [ -z "$MPM_WORKSPACE" ]; then
  echo "fake claude: env missing" >&2
  exit 1
fi
DB="$MPM_WORKSPACE/src/db/mpm.db"
NOW=$(date +%s)
INV_ID="inv-buggy-$MPM_SESSION_ID"
sqlite3 "$DB" "INSERT INTO tool_invocations (id, session_id, tool_name, action, invocation_id, actor_kind, payload_hash, result_status, started_at) VALUES ('$INV_ID', '$MPM_SESSION_ID', 'mpm_lessons', 'save', 'uuid-buggy-$MPM_SESSION_ID', 'agent', 'sha256:buggy', 'success', $NOW)"
exit 0
`), 0o755))

	// Run the fake claude with the BUGGY harness's sessionID —
	// simulating exactly what the pre-§6 Launch did.
	cmd := newCmd(fakeClaudePath, workspace, buggyHarnessSessionID)
	require.NoError(t, cmd.Run(), "fake claude must succeed")

	// ── §9 invariant violation ────────────────────────────────────
	//
	// The §9 correlation test asserts
	// `drill_runs.session_id == tool_invocations.session_id`. Under
	// the bug, A ≠ B, so the join returns 0 rows. This is the
	// failure mode the §9 test would observe if §6 were reverted.

	var rowsUnderA int
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM tool_invocations WHERE session_id = ?`,
		callerSessionID,
	).Scan(&rowsUnderA))
	require.Equal(t, 0, rowsUnderA,
		"§16 setup error: caller session A should have NO audit rows "+
			"under the buggy harness (it ignored A and used B)")

	var rowsUnderB int
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM tool_invocations WHERE session_id = ?`,
		buggyHarnessSessionID,
	).Scan(&rowsUnderB))
	require.Equal(t, 1, rowsUnderB,
		"§16 setup error: the audit row must be under the buggy harness's id B")

	require.NotEqual(t, callerSessionID, buggyHarnessSessionID,
		"§16 setup error: caller and buggy harness must have "+
			"different ids; if they happen to match, the bug "+
			"isn't being modelled and this test is vacuous")

	var traceable int
	require.NoError(t, dm.SQLDB().QueryRow(`
		SELECT COUNT(*) FROM drill_runs dr
		JOIN tool_invocations ti ON ti.session_id = dr.session_id
		WHERE dr.id = ?`,
		"run-buggy-fixture",
	).Scan(&traceable))
	require.Equal(t, 0, traceable,
		"§9 invariant is VIOLATED under the buggy harness: "+
			"drill_runs ↔ tool_invocations join returns 0 rows. "+
			"This is the observable shape pre-§6. §3's "+
			"TestClaudeCodeHarness_SessionIdentity_CallerOwned "+
			"asserts the inverse — it would FAIL if §6 regressed. "+
			"This test documents the failure mode so the inverse "+
			"test isn't accidentally vacuous (e.g. if the data layer "+
			"stops writing rows altogether, §3 might still pass).")

	// Also assert orphan rows: a buggy harness mints B but the
	// orchestrator's A is the canonical id, so any row under B is
	// an orphan from the report's point of view.
	var orphanRows int
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM tool_invocations WHERE session_id != ?`,
		callerSessionID,
	).Scan(&orphanRows))
	require.Equal(t, 1, orphanRows,
		"§10 invariant is VIOLATED under the buggy harness: 1 audit "+
			"row exists outside the orchestrator's session. "+
			"TestDrillHandler_ClaudeCodePath_NoCrossRunBleed asserts "+
			"orphanRows == 0; it would FAIL under this shape.")
}

// TestClaudeCodeHarness_BuggySessionReuseViolatesIsolation is the
// §16 non-vacuity check for the §10 cross-run bleed test. It models
// the failure mode the §10 test guards against: two DrillHandler
// calls accidentally reuse the same sessionID. Under that bug, two
// drill_runs rows point at the same session, and audit rows tagged
// with that session cannot be back-traced to a single run.
//
// This test uses the post-§6 fix path with a deliberate shared
// sessionID between two fake "runs" to confirm the §10 assertions
// would catch it.
func TestClaudeCodeHarness_BuggySessionReuseViolatesIsolation(t *testing.T) {
	if _, err := os.Stat("/usr/bin/sqlite3"); err != nil {
		t.Skipf("sqlite3 CLI required: %v", err)
	}

	workspace := t.TempDir()
	t.Setenv("MPM_WORKSPACE", workspace)

	dm, err := NewDatabaseManager(workspace)
	require.NoError(t, err)
	t.Cleanup(func() { _ = dm.Close() })
	require.NoError(t, dm.InitSchema())

	// Two runs accidentally reuse the same sessionID — the §10
	// bug shape. The orchestrator mints one shared id and stamps it
	// on both drill_runs rows.
	sharedSession := "drill-session-shared-buggy-" + uuid.NewString()
	for i, label := range []string{"run-A", "run-B"} {
		_, err := dm.SQLDB().Exec(`
			INSERT INTO drill_runs
			    (id, drill_id, framework, session_id, status, started_at)
			VALUES (?, ?, ?, ?, 'running', ?)`,
			label,
			"session-reuse-bug",
			"claude_code",
			sharedSession,
			int64(100+i), // distinct started_at so we can distinguish later
		)
		require.NoError(t, err)
	}

	// §10's invariant: each run's sessionID is unique. Under the
	// reuse bug, both rows share a sessionID — this assertion
	// fires. TestDrillHandler_ClaudeCodePath_NoCrossRunBleed asserts
	// the inverse.
	rows, err := dm.SQLDB().Query(`
		SELECT id, session_id FROM drill_runs
		WHERE drill_id = ? ORDER BY started_at ASC`,
		"session-reuse-bug",
	)
	require.NoError(t, err)
	defer rows.Close()

	var sessions []string
	for rows.Next() {
		var id, sid string
		require.NoError(t, rows.Scan(&id, &sid))
		sessions = append(sessions, sid)
	}
	require.Len(t, sessions, 2)
	require.Equal(t, sessions[0], sessions[1],
		"§16 setup error: two runs MUST share a session under this "+
			"bug shape; if they don't, §10's TestDrillHandler_"+
			"ClaudeCodePath_NoCrossRunBleed isn't being modelled")

	// Audit rows under the shared id cannot be back-traced to a
	// single run — the join returns both drill_runs.
	fakeBinDir := t.TempDir()
	fakeClaudePath := filepath.Join(fakeBinDir, "claude-fake")
	require.NoError(t, os.WriteFile(fakeClaudePath, []byte(`#!/bin/sh
set -e
DB="$MPM_WORKSPACE/src/db/mpm.db"
NOW=$(date +%s)
INV_ID="inv-shared-$MPM_SESSION_ID"
sqlite3 "$DB" "INSERT INTO tool_invocations (id, session_id, tool_name, action, invocation_id, actor_kind, payload_hash, result_status, started_at) VALUES ('$INV_ID', '$MPM_SESSION_ID', 'mpm_lessons', 'save', 'uuid-shared-$MPM_SESSION_ID', 'agent', 'sha256:shared', 'success', $NOW)"
exit 0
`), 0o755))

	cmd := newCmd(fakeClaudePath, workspace, sharedSession)
	require.NoError(t, cmd.Run(), "fake claude must succeed")

	// The shared session means the join can't pin evidence to one
	// run — both rows are returned. §10's
	// TestDrillHandler_ClaudeCodePath_NoCrossRunBleed asserts each
	// run owns exactly one audit row; that fails here because the
	// shared id makes the audit ambiguous.
	var ambiguousJoin int
	require.NoError(t, dm.SQLDB().QueryRow(`
		SELECT COUNT(*) FROM drill_runs dr
		JOIN tool_invocations ti ON ti.session_id = dr.session_id`,
	).Scan(&ambiguousJoin))
	require.GreaterOrEqual(t, ambiguousJoin, 2,
		"§16 setup error: the join must return at least 2 rows "+
			"(both runs share the audit row); if it doesn't, §10's "+
			"isolation assumption isn't being modelled here")
}

// newCmd runs the fake claude binary with the buggy-harness
// environment. Wraps exec.CommandContext so the §16 tests don't
// duplicate the env construction. Returns the command so the test
// can call .Run() and inspect its result.
func newCmd(fakeClaudePath, workspace, sessionID string) *exec.Cmd {
	cmd := exec.Command(fakeClaudePath)
	cmd.Env = append(os.Environ(),
		"MPM_SESSION_ID="+sessionID,
		"MPM_WORKSPACE="+workspace,
	)
	return cmd
}
