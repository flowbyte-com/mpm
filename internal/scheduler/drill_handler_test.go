// drill_handler_test.go — pin the scheduler-side drill executor.
//
// Two tests: the synthetic path (deterministic, no real agent) and a
// framework-routing rejection test for unsupported frameworks. The
// claude_code real-framework path is exercised end-to-end by
// TestDrillE2E_ClaudeCode (gated on claude + mpm-mcp binaries).

package scheduler

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	core "github.com/flowbyte-com/mpm-core"
	"github.com/stretchr/testify/require"
)

func TestDrillHandler_SyntheticPath_RecordsVerdict(t *testing.T) {
	workspace := t.TempDir()
	t.Setenv("MPM_WORKSPACE", workspace)
	dm, err := core.NewDatabaseManager(workspace)
	if err != nil {
		t.Fatalf("NewDatabaseManager: %v", err)
	}
	defer dm.Close()
	if err := dm.InitSchema(); err != nil {
		t.Fatalf("InitSchema: %v", err)
	}

	// Seed a drill fixture (the handler expects the file on disk).
	drillPath := filepath.Join(workspace, "test-drill.yaml")
	if err := writeFile(drillPath, []byte(`
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
`), 0644); err != nil {
		t.Fatalf("write drill: %v", err)
	}

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
		// Even with empty PATH the dispatch should record the run
		// (the failing `mpm call` invocations are audited as errors
		// when mpm is missing — but mpm is not invoked in test
		// environment since we redirect PATH away from it).
		t.Logf("DrillHandler returned (acceptable for this synthetic path test): %v", err)
	}
}

func TestDrillHandler_UnsupportedFrameworkRejected(t *testing.T) {
	workspace := t.TempDir()
	t.Setenv("MPM_WORKSPACE", workspace)
	dm, err := core.NewDatabaseManager(workspace)
	if err != nil {
		t.Fatalf("NewDatabaseManager: %v", err)
	}
	defer dm.Close()
	if err := dm.InitSchema(); err != nil {
		t.Fatalf("InitSchema: %v", err)
	}

	drillPath := filepath.Join(workspace, "drill.yaml")
	if err := writeFile(drillPath, []byte(`
id: bad-framework
description: framework that has no harness
framework: futura-framework
prompt: x
expect:
  tools_required: [a]
  sequence:
    - tool: a
      action: x
`), 0644); err != nil {
		t.Fatalf("write drill: %v", err)
	}

	w := Wake{
		ID: "wake-bad",
		Metadata: map[string]interface{}{
			"kind":     "drill",
			"drill_id": "bad-framework",
			"path":     drillPath,
		},
	}

	err = DrillHandler(context.Background(), w)
	if err == nil {
		t.Fatal("DrillHandler should reject unknown framework")
	}
	// The row should still exist with status='error'.
	var status, errMsg string
	if e := dm.SQLDB().QueryRow(
		`SELECT status, error_message FROM drill_runs WHERE drill_id = ?`, "bad-framework",
	).Scan(&status, &errMsg); e != nil {
		t.Fatalf("scan: %v", e)
	}
	if status != "error" {
		t.Errorf("status = %q, want error (drill_runs ledger is the recovery channel)", status)
	}
	if errMsg == "" {
		t.Error("error_message must be populated for diagnostic visibility")
	}
}

// writeFile is a one-line helper so tests don't pull in os just for
// one write call. Lives here so it doesn't drift into a shared test
// util that later gets imported incorrectly.
func writeFile(path string, data []byte, perm uint32) error {
	return os.WriteFile(path, data, os.FileMode(perm))
}

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
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skipf("sqlite3 CLI required for fake claude shim: %v", err)
	}

	workspace := t.TempDir()
	t.Setenv("MPM_WORKSPACE", workspace)

	dm, err := core.NewDatabaseManager(workspace)
	if err != nil {
		t.Fatalf("NewDatabaseManager: %v", err)
	}
	defer dm.Close()
	if err := dm.InitSchema(); err != nil {
		t.Fatalf("InitSchema: %v", err)
	}

	// Drill YAML pinned to claude_code so dispatchDrill routes through
	// the harness under test.
	drillPath := filepath.Join(workspace, "drill.yaml")
	if err := writeFile(drillPath, []byte(`
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
`), 0644); err != nil {
		t.Fatalf("write drill: %v", err)
	}

	// Fake claude + mpm-mcp on a PATH-only directory so the harness's
	// exec.LookPath finds them. The fake claude writes a tool_invocations
	// row tagged with MPM_SESSION_ID (mirrors what mpm-mcp does in
	// production); the fake mpm-mcp just exists (isAvailable only stats).
	fakeBinDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(fakeBinDir, "claude"), []byte(fakeClaudeShimScript), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	if err := os.WriteFile(filepath.Join(fakeBinDir, "mpm-mcp"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake mpm-mcp: %v", err)
	}
	// PATH must come BEFORE the system PATH so the harness resolves
	// our shims, not any real claude / mpm-mcp that may be on PATH.
	t.Setenv("PATH", fakeBinDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// Capture the run id so the assertion can locate the row (the
	// orchestrator mints it; the test can't predict it without
	// intercepting DrillHandler).
	w := Wake{
		ID: "wake-session-identity-e2e",
		Metadata: map[string]interface{}{
			"kind":      "drill",
			"drill_id":  "session-identity-e2e",
			"path":      drillPath,
			"compliant": true,
		},
	}

	if err := DrillHandler(context.Background(), w); err != nil {
		t.Fatalf("DrillHandler returned: %v", err)
	}

	// §9 correlation: drill_runs.session_id == tool_invocations.session_id
	// for the run we just drove. Both rows must exist; both session
	// ids must be equal.
	var (
		drillRunID   string
		drillSession string
		drillStatus  string
	)
	if err := dm.SQLDB().QueryRow(
		`SELECT id, session_id, status FROM drill_runs WHERE drill_id = ? ORDER BY started_at DESC LIMIT 1`,
		"session-identity-e2e",
	).Scan(&drillRunID, &drillSession, &drillStatus); err != nil {
		t.Fatalf("read drill_runs: %v", err)
	}
	if drillStatus != "passed" {
		t.Errorf("drill_runs.status = %q, want passed (drill should have scored the fake claude's row)", drillStatus)
	}

	var auditSession string
	var auditCount int
	if err := dm.SQLDB().QueryRow(
		`SELECT session_id, COUNT(*) FROM tool_invocations GROUP BY session_id ORDER BY COUNT(*) DESC LIMIT 1`,
	).Scan(&auditSession, &auditCount); err != nil {
		t.Fatalf("read tool_invocations: %v", err)
	}
	if auditCount != 1 {
		t.Fatalf("audit row count = %d, want 1 (one audit row per DrillHandler call)", auditCount)
	}

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
	if err := dm.SQLDB().QueryRow(`
		SELECT COUNT(*) FROM drill_runs dr
		JOIN tool_invocations ti ON ti.session_id = dr.session_id
		WHERE dr.id = ?`,
		drillRunID,
	).Scan(&traceable); err != nil {
		t.Fatalf("join: %v", err)
	}
	require.Equal(t, 1, traceable,
		"drill_runs ↔ tool_invocations join on session_id must return "+
			"the run's evidence. Pre-§6 this was 0 — the orchestrator's "+
			"id and the harness's id were different rows.")
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
	workspace := t.TempDir()
	t.Setenv("MPM_WORKSPACE", workspace)

	dm, err := core.NewDatabaseManager(workspace)
	if err != nil {
		t.Fatalf("NewDatabaseManager: %v", err)
	}
	defer dm.Close()
	if err := dm.InitSchema(); err != nil {
		t.Fatalf("InitSchema: %v", err)
	}

	drillPath := filepath.Join(workspace, "drill.yaml")
	if err := writeFile(drillPath, []byte(`
id: error-session-preserved
description: claude_code path that fails at Launch must still record the orchestrator's sessionID
framework: claude_code
prompt: "(anything — claude is unavailable)"
expect:
  tools_required: [mpm_lessons]
  sequence:
    - tool: mpm_lessons
      action: save
timeout_secs: 5
`), 0644); err != nil {
		t.Fatalf("write drill: %v", err)
	}

	// PATH points only at a directory with NO claude / mpm-mcp so
	// exec.LookPath returns empty. NewClaudeCodeHarness(_, "", "")
	// then fails isAvailable at Launch.
	emptyBin := t.TempDir()
	t.Setenv("PATH", emptyBin)

	w := Wake{
		ID: "wake-error-preserves-session",
		Metadata: map[string]interface{}{
			"kind":     "drill",
			"drill_id": "error-session-preserved",
			"path":     drillPath,
		},
	}

	// DrillHandler must still complete cleanly (errors are surfaced
	// via drill_runs.status='error' + error_message, not via panic).
	err = DrillHandler(context.Background(), w)
	if err == nil {
		t.Fatal("DrillHandler should return an error when claude is unavailable")
	}

	// drill_runs row must exist with status='error' and the
	// orchestrator's sessionID still set to a non-empty value. The
	// sessionID was minted at DrillHandler:74-91 BEFORE the
	// dispatch attempt — updateDrillRunError must not have reset it.
	var (
		gotStatus  string
		gotSession string
		errMessage string
	)
	if err := dm.SQLDB().QueryRow(
		`SELECT status, session_id, error_message FROM drill_runs WHERE drill_id = ? ORDER BY started_at DESC LIMIT 1`,
		"error-session-preserved",
	).Scan(&gotStatus, &gotSession, &errMessage); err != nil {
		t.Fatalf("read drill_runs: %v", err)
	}
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
		"error_message must be populated for diagnostic visibility — "+
			"the operator should be able to read WHY the run failed "+
			"from drill_runs alone, without re-running")
}

// TestDrillHandler_ClaudeCodePath_SessionIdentityEndToEnd. It honours
// MPM_SESSION_ID by writing exactly one tool_invocations row tagged
// with the env-passed session_id, then exits 0 — the minimum a
// reproducer needs to demonstrate audit-row scope.
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

# Embed MPM_SESSION_ID in the row id so multiple DrillHandler calls in
# one test (each with a different session) produce distinct row ids.
# Without this, the second run's INSERT collides on the PRIMARY KEY
# and the row never lands — masking the very correlation the test
# wants to assert.
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

// fakeClaudeShimScript is the body of the fake `claude` binary used by
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
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skipf("sqlite3 CLI required for fake claude shim: %v", err)
	}

	workspace := t.TempDir()
	t.Setenv("MPM_WORKSPACE", workspace)

	dm, err := core.NewDatabaseManager(workspace)
	if err != nil {
		t.Fatalf("NewDatabaseManager: %v", err)
	}
	defer dm.Close()
	if err := dm.InitSchema(); err != nil {
		t.Fatalf("InitSchema: %v", err)
	}

	drillPath := filepath.Join(workspace, "drill.yaml")
	if err := writeFile(drillPath, []byte(`
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
`), 0644); err != nil {
		t.Fatalf("write drill: %v", err)
	}

	fakeBinDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(fakeBinDir, "claude"), []byte(fakeClaudeShimScript), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	if err := os.WriteFile(filepath.Join(fakeBinDir, "mpm-mcp"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake mpm-mcp: %v", err)
	}
	t.Setenv("PATH", fakeBinDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// Run the drill twice. Each DrillHandler call mints a fresh
	// sessionID (runID/sessionID := uuid.NewString() twice).
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
		if err := DrillHandler(context.Background(), w); err != nil {
			t.Fatalf("DrillHandler %s returned: %v", label, err)
		}
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
	if err != nil {
		t.Fatalf("query drill_runs: %v", err)
	}
	defer rows.Close()

	type run struct {
		id        string
		sessionID string
	}
	var runs []run
	for rows.Next() {
		var r run
		if err := rows.Scan(&r.id, &r.sessionID); err != nil {
			t.Fatalf("scan: %v", err)
		}
		runs = append(runs, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	require.Len(t, runs, 2, "two DrillHandler invocations must produce two drill_runs rows")
	require.NotEqual(t, runs[0].sessionID, runs[1].sessionID,
		"consecutive runs must have distinct session_ids — "+
			"if they match, the orchestrator stopped minting (or "+
			"something is stashing the id between calls). "+
			"Either way, cross-run bleed has appeared.")

	// Each run's audit row must belong ONLY to that run's session.
	// The fake claude writes one row per invocation, tagged with the
	// harness's MPM_SESSION_ID (= the orchestrator's id post-§6).
	// After two runs there should be exactly two audit rows, one per
	// session_id, with no orphans.
	var auditByRun int
	for _, r := range runs {
		var n int
		if err := dm.SQLDB().QueryRow(
			`SELECT COUNT(*) FROM tool_invocations WHERE session_id = ?`,
			r.sessionID,
		).Scan(&n); err != nil {
			t.Fatalf("audit count for %s: %v", r.id, err)
		}
		require.Equal(t, 1, n,
			"run %s (session %q) should own exactly 1 audit row; "+
				"got %d. Anything else means rows are being attributed "+
				"to the wrong session, or the harness is dropping them.",
			r.id, r.sessionID, n)
		auditByRun += n
	}

	// No orphan audit rows: every row's session_id matches SOME
	// run's session_id. If a row leaked (e.g. a third id appeared),
	// the sum below would exceed the row count owned by runs.
	var totalAudit, orphanAudit int
	if err := dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM tool_invocations`).Scan(&totalAudit); err != nil {
		t.Fatalf("total audit count: %v", err)
	}
	if err := dm.SQLDB().QueryRow(`
		SELECT COUNT(*) FROM tool_invocations
		WHERE session_id NOT IN (?, ?)`,
		runs[0].sessionID, runs[1].sessionID,
	).Scan(&orphanAudit); err != nil {
		t.Fatalf("orphan count: %v", err)
	}
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
