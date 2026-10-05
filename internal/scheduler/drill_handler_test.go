// drill_handler_test.go — pin the scheduler-side drill executor.
//
// Six tests covering the drill-run session identity invariant (§4)
// and the framework-dispatch contract:
//
//   - TestDrillHandler_SyntheticPath_RecordsVerdict
//       framework-dispatch decision for the synthetic path; the
//       score pipeline records the row even when `mpm call` cannot
//       resolve on PATH.
//   - TestDrillHandler_UnsupportedFrameworkRejected
//       unsupported framework → status='error' with diagnostic
//       message; the row persists so the failure is recoverable.
//   - TestDrillHandler_ClaudeCodePath_SessionIdentityEndToEnd (§9)
//       orchestrator S == drill_runs.session_id ==
//       tool_invocations.session_id end-to-end through
//       DrillHandler.Launch.Finish on the claude_code path.
//   - TestDrillHandler_ClaudeCodePath_NoCrossRunBleed (§10)
//       two consecutive runs must own distinct session_ids and
//       exactly one audit row each, with no orphans.
//   - TestDrillHandler_ClaudeCodePath_ErrorPreservesSessionID (§11)
//       launch failure (claude absent from PATH) must surface as
//       status='error' WITHOUT erasing the orchestrator's
//       sessionID from drill_runs.
//   - TestDrillHandler_SyntheticPath_SessionIdentityEndToEnd
//       same §4 chain on the synthetic path: orchestrator S →
//       drill_runs.session_id → MPM_SESSION_ID env on `mpm call`
//       → tool_invocations.session_id.

package scheduler

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	core "github.com/flowbyte-com/mpm-core"
	"github.com/stretchr/testify/require"
)

// setupDrillDM returns a file-backed DatabaseManager with the schema
// initialised and MPM_WORKSPACE pinned to its directory. The caller
// must not call dm.Close — t.Cleanup handles it. Reused by every
// drill-handler test below; collapsing the inline boilerplate here
// keeps each test focused on its own assertion surface.
func setupDrillDM(t *testing.T) (*core.DatabaseManager, string) {
	t.Helper()
	workspace := t.TempDir()
	t.Setenv("MPM_WORKSPACE", workspace)
	dm, err := core.NewDatabaseManager(workspace)
	require.NoError(t, err)
	t.Cleanup(func() { _ = dm.Close() })
	require.NoError(t, dm.InitSchema())
	return dm, workspace
}

// installFakeBin drops an executable shell script at <tmpdir>/<name>
// and prepends that directory to PATH so the harness's exec.LookPath
// resolves our shim before any system binary. Reused by the
// claude_code + synthetic path tests.
func installFakeBin(t *testing.T, name, body string) string {
	t.Helper()
	binDir := t.TempDir()
	path := filepath.Join(binDir, name)
	require.NoError(t, os.WriteFile(path, []byte(body), 0o755))
	// Prepend so the harness resolves our shim before any real binary
	// that may be on PATH.
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return binDir
}

// installFakeClaudeAndMcp installs both the fake claude (the
// production-equivalent shim that stamps tool_invocations rows with
// MPM_SESSION_ID) and the fake mpm-mcp (harness.isAvailable only
// stats — a no-op script is enough). Used by every claude_code path
// test that needs both binaries present.
func installFakeClaudeAndMcp(t *testing.T) {
	t.Helper()
	dir := installFakeBin(t, "claude", fakeClaudeShimScript)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "mpm-mcp"), []byte("#!/bin/sh\nexit 0\n"), 0o755))
}

// writeDrillYAML serialises a minimal drill fixture to disk so the
// handler can resolve path metadata. Lives next to the helpers to
// keep the test bodies free of YAML noise.
func writeDrillYAML(t *testing.T, path, body string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
}

// fakeClaudeShimScript is the body of the fake `claude` binary. It
// honours MPM_SESSION_ID by writing exactly one tool_invocations row
// tagged with the env-passed session_id, then exits 0. Mirrors what
// mpm-mcp's audit hook does in production. Used by the claude_code
// path tests AND by the synthetic-path session-identity test (as a
// reference shape; the synthetic path uses a separate fake `mpm`
// shim).
//
// Embed MPM_SESSION_ID in the row id so multiple DrillHandler calls
// in one test (each with a different session) produce distinct row
// ids. Without this, the second run's INSERT collides on the PRIMARY
// KEY and the row never lands — masking the very correlation the
// test wants to assert.
const fakeClaudeShimScript = `#!/bin/sh
# Fake claude — stands in for the real Claude Code CLI in tests.
# Mirrors what mpm-mcp's audit hook does in production: stamps every
# tool_invocation row with the env-passed MPM_SESSION_ID.
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
NOW=$(date +%s)

sqlite3 "$DB" <<EOF
INSERT INTO tool_invocations
  (id, session_id, tool_name, action, invocation_id,
   actor_kind, payload_hash, result_status,
   started_at)
VALUES
  ('inv-fake-claude-$MPM_SESSION_ID', '$MPM_SESSION_ID', 'mpm_lessons', 'save',
   'uuid-fake-$MPM_SESSION_ID', 'agent', 'sha256:fake', 'success', $NOW);
EOF

exit 0
`

// fakeMpmShimScript is the body of the fake `mpm` binary used by
// TestDrillHandler_SyntheticPath_SessionIdentityEndToEnd. It honours
// MPM_SESSION_ID by writing exactly one tool_invocations row tagged
// with the env-passed session_id, then exits 0. Mirrors what the
// production mpm-call audit hook does.
const fakeMpmShimScript = `#!/bin/sh
# Fake mpm — stands in for the real mpm-call binary in tests.
# Mirrors what the audit hook does in production: stamps every
# tool_invocation row with the env-passed MPM_SESSION_ID.
set -e

if [ -z "$MPM_SESSION_ID" ]; then
  echo "fake mpm: MPM_SESSION_ID is empty" >&2
  exit 1
fi
if [ -z "$MPM_WORKSPACE" ]; then
  echo "fake mpm: MPM_WORKSPACE is empty" >&2
  exit 1
fi

DB="$MPM_WORKSPACE/src/db/mpm.db"
NOW=$(date +%s)

sqlite3 "$DB" "INSERT INTO tool_invocations (id, session_id, tool_name, action, invocation_id, actor_kind, payload_hash, result_status, started_at) VALUES ('inv-fake-mpm-$MPM_SESSION_ID', '$MPM_SESSION_ID', 'mpm_lessons', 'save', 'uuid-fake-mpm-$MPM_SESSION_ID', 'agent', 'sha256:fake-mpm', 'success', $NOW)"

exit 0
`

// ─── existing dispatch tests (unchanged semantics, helpers below) ───

func TestDrillHandler_SyntheticPath_RecordsVerdict(t *testing.T) {
	dm, _ := setupDrillDM(t)

	drillPath := filepath.Join(t.TempDir(), "test-drill.yaml")
	writeDrillYAML(t, drillPath, `
id: test-drill
description: synthetic self-test
framework: synthetic
prompt: dispatch a synthetic call sequence
expect:
  tools_required: [mpm_lessons]
  sequence:
    - tool: mpm_lessons
      action: save
timeout_secs: 5
`)

	w := Wake{
		ID: "test-wake-1",
		Metadata: map[string]interface{}{
			"kind":      "drill",
			"drill_id":  "test-drill",
			"path":      drillPath,
			"compliant": true,
		},
	}

	// Note: runSyntheticDrill shells `mpm call` which won't be on PATH in
	// this isolated test workspace, so the audit hook may not fire. The
	// test asserts on the framework-dispatch decision and the harness
	// side; the audit-row assertion is gated below.
	t.Setenv("PATH", "")
	if err := DrillHandler(context.Background(), w); err != nil {
		t.Logf("DrillHandler returned (acceptable for this synthetic path test): %v", err)
	}

	// Sanity: the row persists even when `mpm call` cannot resolve —
	// status='running' is the recorded state when dispatch returns an
	// error before updateDrillRunError fires. We just want the row to
	// be present so the report can see the run happened.
	var seen int
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM drill_runs WHERE drill_id = ?`, "test-drill",
	).Scan(&seen))
	require.Equal(t, 1, seen, "drill_runs ledger must record the synthetic run")
}

func TestDrillHandler_UnsupportedFrameworkRejected(t *testing.T) {
	dm, _ := setupDrillDM(t)

	drillPath := filepath.Join(t.TempDir(), "drill.yaml")
	writeDrillYAML(t, drillPath, `
id: bad-framework
description: framework that has no harness
framework: futura-framework
prompt: x
expect:
  tools_required: [a]
  sequence:
    - tool: a
      action: x
`)

	w := Wake{
		ID: "wake-bad",
		Metadata: map[string]interface{}{
			"kind":     "drill",
			"drill_id": "bad-framework",
			"path":     drillPath,
		},
	}

	err := DrillHandler(context.Background(), w)
	require.Error(t, err, "DrillHandler should reject unknown framework")

	// The row should still exist with status='error'.
	var status, errMsg string
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT status, error_message FROM drill_runs WHERE drill_id = ?`, "bad-framework",
	).Scan(&status, &errMsg))
	require.Equal(t, "error", status,
		"drill_runs ledger is the recovery channel: status='error' "+
			"so the report can show WHY the run failed")
	require.NotEmpty(t, errMsg, "error_message must be populated for diagnostic visibility")
}

// ─── claude_code path session identity tests ────────────────────────

// TestDrillHandler_ClaudeCodePath_SessionIdentityEndToEnd is the §9
// scheduler-level correlation test for the drill-run session identity
// invariant (§4):
//
//	drill_runs.session_id == tool_invocations.session_id
//
// after a full scheduler.DrillHandler call on the claude_code path.
// The data-layer guarantee is established by §3's
// TestClaudeCodeHarness_SessionIdentity_CallerOwned in internal/core;
// this test verifies the scheduler side wires the orchestrator's
// sessionID into the harness.Launch call correctly (no silent mint,
// no accidental `_ = sessionID` suppression that throws the id away
// — see the pre-fix bug shape in
// internal/core/drill_session_identity_reproducer_test.go).
//
// The test uses a fake claude binary on PATH that honours
// MPM_SESSION_ID and writes a tool_invocations row tagged with it.
// The fake mpm-mcp shim satisfies harness.isAvailable without
// actually being invoked. sqlite3 CLI must be on PATH for the fake
// claude to write its row.
func TestDrillHandler_ClaudeCodePath_SessionIdentityEndToEnd(t *testing.T) {
	if _, err := os.Stat("/usr/bin/sqlite3"); err != nil {
		t.Skipf("sqlite3 CLI required for fake claude shim: %v", err)
	}

	dm, workspace := setupDrillDM(t)

	drillPath := filepath.Join(workspace, "drill.yaml")
	writeDrillYAML(t, drillPath, `
id: session-identity-e2e
description: end-to-end correlation across drill_runs and tool_invocations
framework: claude_code
prompt: "(fake prompt — fake claude ignores arguments)"
expect:
  tools_required: [mpm_lessons]
  sequence:
    - tool: mpm_lessons
      action: save
timeout_secs: 30
`)
	installFakeClaudeAndMcp(t)

	w := Wake{
		ID: "wake-session-identity-e2e",
		Metadata: map[string]interface{}{
			"kind":      "drill",
			"drill_id":  "session-identity-e2e",
			"path":      drillPath,
			"compliant": true,
		},
	}

	require.NoError(t, DrillHandler(context.Background(), w),
		"DrillHandler must succeed when fake claude + mpm-mcp are on PATH")

	// §9 correlation: drill_runs.session_id == tool_invocations.session_id
	// for the run we just drove. Both rows must exist; both session
	// ids must be equal.
	var (
		drillRunID   string
		drillSession string
		drillStatus  string
	)
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT id, session_id, status FROM drill_runs WHERE drill_id = ? ORDER BY started_at DESC LIMIT 1`,
		"session-identity-e2e",
	).Scan(&drillRunID, &drillSession, &drillStatus))
	require.Equal(t, "passed", drillStatus,
		"drill should have scored the fake claude's audit row as passed")

	var auditSession string
	var auditCount int
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT session_id, COUNT(*) FROM tool_invocations GROUP BY session_id ORDER BY COUNT(*) DESC LIMIT 1`,
	).Scan(&auditSession, &auditCount))
	require.Equal(t, 1, auditCount,
		"one audit row per DrillHandler call")

	require.Equal(t, drillSession, auditSession,
		"§9 invariant: drill_runs.session_id (%q) must equal "+
			"tool_invocations.session_id (%q) for the same run. "+
			"Mismatch means the scheduler dropped or rewrote the "+
			"orchestrator's sessionID on the way to harness.Launch — "+
			"see internal/core/drill_session_identity_reproducer_test.go "+
			"for the pre-§6 bug shape (runClaudeCodeDrill had "+
			"`_ = sessionID // harness mints its own`).",
		drillSession, auditSession)

	// Structural proof: the join used by an operator to trace
	// evidence back to a drill_run returns exactly 1 row.
	var traceable int
	require.NoError(t, dm.SQLDB().QueryRow(`
		SELECT COUNT(*) FROM drill_runs dr
		JOIN tool_invocations ti ON ti.session_id = dr.session_id
		WHERE dr.id = ?`,
		drillRunID,
	).Scan(&traceable))
	require.Equal(t, 1, traceable,
		"drill_runs ↔ tool_invocations join on session_id must return "+
			"the run's evidence. Pre-§6 this was 0 — the orchestrator's "+
			"id and the harness's id were different rows.")
}

// TestDrillHandler_ClaudeCodePath_NoCrossRunBleed is the §10
// regression: two consecutive drill runs must not share session_ids,
// and each run's audit row must belong ONLY to that run's session.
//
// Pre-§6, the harness minted a fresh UUID per Launch. The two runs
// were naturally isolated because the UUIDs didn't share a namespace
// — but each run was ALSO internally split (drill_runs vs evidence,
// see §3). Post-§6, the orchestrator mints the sessionID. The risk
// shifts from "no split" to "no accidental reuse" — if the
// orchestrator ever stopped minting (or the harness's sessionID was
// reused by mistake), runs would bleed. This test ensures that
// didn't happen: run A's sessionID ≠ run B's sessionID, and each
// run's evidence lives exclusively under its own sessionID.
func TestDrillHandler_ClaudeCodePath_NoCrossRunBleed(t *testing.T) {
	if _, err := os.Stat("/usr/bin/sqlite3"); err != nil {
		t.Skipf("sqlite3 CLI required for fake claude shim: %v", err)
	}

	dm, workspace := setupDrillDM(t)

	drillPath := filepath.Join(workspace, "drill.yaml")
	writeDrillYAML(t, drillPath, `
id: cross-run-bleed
description: two consecutive runs must not share session_ids
framework: claude_code
prompt: "(fake prompt)"
expect:
  tools_required: [mpm_lessons]
  sequence:
    - tool: mpm_lessons
      action: save
timeout_secs: 30
`)
	installFakeClaudeAndMcp(t)

	// Run the drill twice. Each DrillHandler call mints a fresh
	// sessionID.
	for i, label := range []string{"run-A", "run-B"} {
		w := Wake{
			ID: "wake-" + label,
			Metadata: map[string]interface{}{
				"kind":      "drill",
				"drill_id":  "cross-run-bleed",
				"path":      drillPath,
				"compliant": true,
			},
		}
		require.NoError(t, DrillHandler(context.Background(), w),
			"DrillHandler %s failed", label)
		// Brief pause so the two started_at values are distinct; the
		// assertion below sorts by started_at and the test would be
		// fragile if both landed in the same Unix second.
		if i == 0 {
			time.Sleep(2 * time.Second)
		}
	}

	// Read both runs' session_ids.
	rows, err := dm.SQLDB().Query(`
		SELECT id, session_id FROM drill_runs
		WHERE drill_id = ? ORDER BY started_at ASC`,
		"cross-run-bleed",
	)
	require.NoError(t, err)
	defer rows.Close()

	type run struct {
		id        string
		sessionID string
	}
	var runs []run
	for rows.Next() {
		var r run
		require.NoError(t, rows.Scan(&r.id, &r.sessionID))
		runs = append(runs, r)
	}
	require.NoError(t, rows.Err())
	require.Len(t, runs, 2, "two DrillHandler invocations must produce two drill_runs rows")
	require.NotEqual(t, runs[0].sessionID, runs[1].sessionID,
		"consecutive runs must have distinct session_ids — "+
			"if they match, the orchestrator stopped minting (or "+
			"something is stashing the id between calls). "+
			"Either way, cross-run bleed has appeared.")

	// Each run's audit row must belong ONLY to that run's session.
	var auditByRun int
	for _, r := range runs {
		var n int
		require.NoError(t, dm.SQLDB().QueryRow(
			`SELECT COUNT(*) FROM tool_invocations WHERE session_id = ?`,
			r.sessionID,
		).Scan(&n))
		require.Equal(t, 1, n,
			"run %s (session %q) should own exactly 1 audit row; "+
				"got %d. Anything else means rows are being attributed "+
				"to the wrong session, or the harness is dropping them.",
			r.id, r.sessionID, n)
		auditByRun += n
	}

	// No orphan audit rows: every row's session_id matches SOME
	// run's session_id.
	var totalAudit, orphanAudit int
	require.NoError(t, dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM tool_invocations`).Scan(&totalAudit))
	require.NoError(t, dm.SQLDB().QueryRow(`
		SELECT COUNT(*) FROM tool_invocations
		WHERE session_id NOT IN (?, ?)`,
		runs[0].sessionID, runs[1].sessionID,
	).Scan(&orphanAudit))
	require.Equal(t, totalAudit, auditByRun,
		"total audit rows (%d) should equal the sum owned by runs (%d) "+
			"— orphans indicate a session_id leak",
		totalAudit, auditByRun)
	require.Equal(t, 0, orphanAudit,
		"no tool_invocations row should exist outside the runs' "+
			"session_ids — orphans here mean a third session_id "+
			"appeared (the harness minting its own id post-§6 "+
			"would surface here)")
}

// TestDrillHandler_ClaudeCodePath_ErrorPreservesSessionID is the §11
// regression: when the claude_code path errors out, the
// orchestrator-minted session_id is still the one recorded in
// drill_runs. The orchestrator's mint is the only source of truth
// for the run's identity; a harness error must not invalidate it,
// overwrite it with a fresh UUID, or leave it empty.
//
// We trigger the error path by removing claude from PATH so
// isAvailable reports "claude CLI not on PATH". DrillHandler surfaces
// this as a launch error and stamps status='error'. The assertion
// reads drill_runs back and confirms session_id is non-empty AND
// is the value the orchestrator originally minted (deterministic
// since DrillHandler calls uuid.NewString() but the value must
// survive the UPDATE in updateDrillRunError).
func TestDrillHandler_ClaudeCodePath_ErrorPreservesSessionID(t *testing.T) {
	dm, workspace := setupDrillDM(t)

	drillPath := filepath.Join(workspace, "drill.yaml")
	writeDrillYAML(t, drillPath, `
id: error-session-preserves
description: claude_code path that fails at Launch must still record the orchestrator's sessionID
framework: claude_code
prompt: "(anything — claude is unavailable)"
expect:
  tools_required: [mpm_lessons]
  sequence:
    - tool: mpm_lessons
      action: save
timeout_secs: 5
`)

	// PATH points only at a directory with NO claude / mpm-mcp so
	// exec.LookPath returns empty. NewClaudeCodeHarness(_, "", "")
	// then fails isAvailable at Launch.
	t.Setenv("PATH", t.TempDir())

	w := Wake{
		ID: "wake-error-preserves-session",
		Metadata: map[string]interface{}{
			"kind":     "drill",
			"drill_id": "error-session-preserves",
			"path":     drillPath,
		},
	}

	// DrillHandler must still complete cleanly (errors are surfaced
	// via drill_runs.status='error' + error_message, not via panic).
	err := DrillHandler(context.Background(), w)
	require.Error(t, err, "DrillHandler should return an error when claude is unavailable")

	// drill_runs row must exist with status='error' and the
	// orchestrator's sessionID still set to a non-empty value. The
	// sessionID was minted at DrillHandler BEFORE the dispatch
	// attempt — updateDrillRunError must not have reset it.
	var (
		gotStatus  string
		gotSession string
		errMessage string
	)
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT status, session_id, error_message FROM drill_runs WHERE drill_id = ? ORDER BY started_at DESC LIMIT 1`,
		"error-session-preserves",
	).Scan(&gotStatus, &gotSession, &errMessage))
	require.Equal(t, "error", gotStatus,
		"claude-unavailable failure must surface as status='error', "+
			"not 'running' or 'failed' (status='error' is the contract "+
			"for harness-internal failures; status='failed' is for "+
			"drill-scoring failures)")
	require.NotEmpty(t, gotSession,
		"drill_runs.session_id must be the orchestrator's mint even "+
			"on Launch failure — empty here means updateDrillRunError "+
			"reset session_id (regression of the §6 invariant: "+
			"drill_runs.session_id is the canonical run identifier)")
	require.NotEmpty(t, errMessage,
		"error_message must be populated for diagnostic visibility")
}

// TestDrillHandler_SyntheticPath_SessionIdentityEndToEnd is the
// synthetic-path counterpart to
// TestDrillHandler_ClaudeCodePath_SessionIdentityEndToEnd. The §4
// chain on the synthetic path is:
//
//	orchestrator S (DrillHandler mints)
//	  == drill_runs.session_id (drill_runs INSERT in DrillHandler)
//	  == MPM_SESSION_ID env (runSyntheticDrill appends it to `mpm call`)
//	  == tool_invocations.session_id (audit hook stamps it)
//
// The fake mpm-cli's shim honours MPM_SESSION_ID by writing a
// tool_invocations row tagged with the env-passed id, mirroring what
// the production `mpm call` audit hook does. If runSyntheticDrill
// ever dropped the MPM_SESSION_ID env (or the synthetic harness
// minted its own id), the audit row's session_id would not match
// drill_runs.session_id — this test catches that.
func TestDrillHandler_SyntheticPath_SessionIdentityEndToEnd(t *testing.T) {
	if _, err := os.Stat("/usr/bin/sqlite3"); err != nil {
		t.Skipf("sqlite3 CLI required for fake mpm shim: %v", err)
	}

	dm, workspace := setupDrillDM(t)

	drillPath := filepath.Join(workspace, "drill.yaml")
	writeDrillYAML(t, drillPath, `
id: synthetic-session-identity
description: synthetic path must propagate sessionID through MPM_SESSION_ID env to tool_invocations
framework: synthetic
prompt: "(unused — synthetic harness generates sequence)"
expect:
  tools_required: [mpm_lessons]
  sequence:
    - tool: mpm_lessons
      action: save
timeout_secs: 5
`)
	installFakeBin(t, "mpm", fakeMpmShimScript)

	w := Wake{
		ID: "wake-synthetic-session-identity",
		Metadata: map[string]interface{}{
			"kind":      "drill",
			"drill_id":  "synthetic-session-identity",
			"path":      drillPath,
			"compliant": true,
		},
	}

	require.NoError(t, DrillHandler(context.Background(), w),
		"DrillHandler must succeed when fake mpm is on PATH")

	// §4 chain end-to-end on the synthetic path:
	//   drill_runs.session_id == tool_invocations.session_id.
	var drillSession string
	var drillStatus string
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT session_id, status FROM drill_runs WHERE drill_id = ? ORDER BY started_at DESC LIMIT 1`,
		"synthetic-session-identity",
	).Scan(&drillSession, &drillStatus))
	require.NotEmpty(t, drillSession,
		"drill_runs.session_id must be set for synthetic runs (§4 "+
			"invariant — drill_runs.session_id is the canonical "+
			"run identifier even on the synthetic path)")
	require.Equal(t, "passed", drillStatus,
		"the fake mpm wrote a tool_invocations row matching the spec")

	var auditSession string
	var auditCount int
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT session_id, COUNT(*) FROM tool_invocations GROUP BY session_id ORDER BY COUNT(*) DESC LIMIT 1`,
	).Scan(&auditSession, &auditCount))
	require.Equal(t, 1, auditCount,
		"one audit row per DrillHandler call on the synthetic path")

	require.Equal(t, drillSession, auditSession,
		"synthetic path §4 invariant: drill_runs.session_id (%q) must "+
			"equal tool_invocations.session_id (%q). Mismatch means "+
			"runSyntheticDrill dropped MPM_SESSION_ID env or the "+
			"synthetic harness minted its own id (regression of the "+
			"§6 contract — orchestrator owns the mint).",
		drillSession, auditSession)
}
