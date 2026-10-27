package internal

// m5_trust_helpers_test.go — shared fixtures for the M5 trust-defect
// regression suite.
//
// This file has NO build tag on purpose. The characterization suite
// (m5_trust_characterization_test.go, always compiled) and the contract suite
// (m5_trust_contract_test.go, gated behind `m5contract`) share these helpers,
// so there is exactly one definition of "an isolated, disposable MPM
// workspace" in the tree.
//
// ISOLATION CONTRACT (docs/dev/dogfooding-runbook.md §5).
//
// Every helper here pins HOME, MPM_WORKSPACE and MPM_DB_PATH to disposable
// locations and opens a disposable database at the canonical
// <workspace>/src/db/mpm.db path. No test in this suite can reach the
// operator's live database at ~/.mpm/src/db/mpm.db.
//
// The DB-safety AST guard (internal/core/test_db_safety_test.go) flags
// `NewDatabaseManager("")` — the empty argument is what falls through to
// $HOME/.mpm. These helpers always pass a non-empty disposable root, so the
// guard correctly does not flag them, and no whitelist entry is needed.

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// m5IsolatedWorkspace pins HOME, MPM_WORKSPACE and MPM_DB_PATH to disposable
// locations and returns the disposable workspace root.
//
// HOME is pinned because config.GetMPMDir() falls back to $HOME/.mpm when
// MPM_WORKSPACE is unset, and because workspace-scoped files (active.json,
// toxicphrases.txt) are created under the resolved root.
//
// MPM_DB_PATH is pinned because mpm-critic and mpm-scheduler honour it while
// mpm and mpm-mcp ignore it — an unpinned value could silently redirect a
// later subprocess at the live database.
func m5IsolatedWorkspace(t *testing.T) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	ws := t.TempDir()
	t.Setenv("MPM_WORKSPACE", ws)
	t.Setenv("MPM_DB_PATH", filepath.Join(ws, "src", "db", "mpm.db"))
	return ws
}

// m5NewDM opens a disposable DatabaseManager rooted at the isolated
// workspace. The database therefore lives at <workspace>/src/db/mpm.db —
// exactly the path MPM_DB_PATH names — so the suite exercises the production
// path resolution rather than a test-only shortcut.
func m5NewDM(t *testing.T) *DatabaseManager {
	t.Helper()
	ws := m5IsolatedWorkspace(t)
	dm, err := NewDatabaseManager(ws)
	if err != nil {
		t.Fatalf("m5NewDM: NewDatabaseManager(%q): %v", ws, err)
	}
	t.Cleanup(func() { _ = dm.Close() })
	return dm
}

// m5ScalarFloat runs a single-value query and returns the float.
func m5ScalarFloat(t *testing.T, db *sql.DB, query string, args ...interface{}) float64 {
	t.Helper()
	var v sql.NullFloat64
	if err := db.QueryRow(query, args...).Scan(&v); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	if !v.Valid {
		t.Fatalf("query %q returned NULL", query)
	}
	return v.Float64
}

// m5ScalarInt runs a single-value count query and returns the int.
func m5ScalarInt(t *testing.T, db *sql.DB, query string, args ...interface{}) int {
	t.Helper()
	var v sql.NullInt64
	if err := db.QueryRow(query, args...).Scan(&v); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	if !v.Valid {
		t.Fatalf("query %q returned NULL", query)
	}
	return int(v.Int64)
}

// m5Confidence reads the stored confidence of an artifact row.
func m5Confidence(t *testing.T, dm *DatabaseManager, artifactID string) float64 {
	t.Helper()
	return m5ScalarFloat(t, dm.SQLDB(),
		`SELECT confidence FROM memories WHERE id = ?`, artifactID)
}

// m5ProposePendingTheory creates a pending theory (a critic proposal) and
// returns its id. A pending theory is the artifact the critic's proposals
// become, and it is the artifact M5's evidence rules apply to.
func m5ProposePendingTheory(t *testing.T, dm *DatabaseManager, hypothesis string, tags []string) string {
	t.Helper()
	res, err := dm.ProposeTheory(hypothesis, "Demonstrate the claim.", nil, nil, tags)
	if err != nil {
		t.Fatalf("ProposeTheory(%q): %v", hypothesis, err)
	}
	id, _ := res["id"].(string)
	if id == "" {
		t.Fatalf("ProposeTheory(%q) returned no id: %v", hypothesis, res)
	}
	return id
}

// m5SeedToolInvocation inserts a minimal tool_invocations row and returns its
// id.
//
// The table is CHECK-constrained to tool invocations and is NOT NULL on
// session_id, tool_name, action, invocation_id, actor_kind, payload_hash,
// result_status and started_at. The row exists so a proposal can cite a
// tool invocation — the citation kind mpm-event/0.1 §3.7 explicitly
// contemplates ("each of them must resolve to a stored event (or to a
// tool_invocations row)").
func m5SeedToolInvocation(t *testing.T, dm *DatabaseManager, id string) string {
	t.Helper()
	_, err := dm.SQLDB().Exec(
		`INSERT INTO tool_invocations
		   (id, session_id, tool_name, action, invocation_id, actor_kind,
		    payload_hash, result_status, started_at, completed_at, duration_ms,
		    framework_name, mpm_session_id, framework_session_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, "m5-session", "mpm_memory", "save", "inv-"+id, "agent",
		"sha256:"+id, "success", 1793000000, 1793000000, 1,
		"opencode", "m5-mpm-session", "m5-framework-session")
	if err != nil {
		t.Fatalf("seed tool_invocations row %q: %v", id, err)
	}
	return id
}
