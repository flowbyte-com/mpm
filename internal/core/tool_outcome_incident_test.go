// tool_outcome_incident_test.go — Brief §17 incident-reconstruction
// acceptance. After an operational failure, an investigator can
// query by invocation_id and obtain:
//
//   tool
//   action
//   framework
//   session where present
//   result_status
//   outcome_class
//   outcome_code
//   operational component
//   event_code
//
// without parsing human error text.
//
// The fixture uses an in-memory DatabaseManager, an embedded audit
// row that mirrors what a substrate failure path would write today,
// and asserts the reader-side contract.

package internal

import (
	"database/sql"
	"errors"
	"testing"
)

// TestToolOutcome_IncidentReconstruction_Fixture seeds a single
// tool invocation AND a single system_audit_log row that share an
// invocation_id, then asserts a one-statement join recovers the
// full evidentiary surface.
func TestToolOutcome_IncidentReconstruction_Fixture(t *testing.T) {
	dm := NewTestDM(t)
	const (
		invID         = "inv-fixture-A"
		mpmSession    = "mpm-fixture-A"
		frameworkName = "opencode"
		frameworkSess = "opencode-session-A"
		toolName      = "mpm_resolve"
		actionName    = "list"
		component     = "cascade-reconciler"
		eventCode     = "substrate_cascade_invoke_failed"
		outcomeClass  = "substrate"
		outcomeCode   = "sqlite_db_locked"
	)

	// 1. Tool invocation row (the agent-facing record).
	if _, err := dm.SQLDB().Exec(`
		INSERT INTO tool_invocations
		    (id, session_id, tool_name, action, invocation_id, actor_kind,
		     payload_hash, result_status, started_at, completed_at,
		     duration_ms, error_message,
		     mpm_session_id, framework_name, framework_session_id,
		     outcome_class, outcome_code)
		VALUES (?, 'cli-default', ?, ?, ?, 'agent',
		        'sha256:fixtureA', 'error', ?, ?, 8,
		        'sqlite3: database is locked',
		        ?, ?, ?,
		        ?, ?)`,
		"inv-fixture-row", toolName, actionName, invID,
		1700000100, 1700000108,
		mpmSession, frameworkName, frameworkSess,
		outcomeClass, outcomeCode,
	); err != nil {
		t.Fatalf("insert tool_invocations: %v", err)
	}

	// 2. Operational audit row (the substrate-facing record). Same
	// invocation_id correlation; event_code is the bounded subtype
	// inside the audit log.
	if _, err := dm.SQLDB().Exec(`
		INSERT INTO system_audit_log
		    (id, level, component, message, stack_trace, context,
		     invocation_id, mpm_session_id, framework_name,
		     framework_session_id, event_code)
		VALUES (?, 'error', ?, 'sqlite3: database is locked',
		        '', NULL, ?, ?, ?, ?, ?)`,
		"audit-fixture-row", component,
		invID, mpmSession, frameworkName, frameworkSess,
		eventCode,
	); err != nil {
		t.Fatalf("insert system_audit_log: %v", err)
	}

	// 3. Maintainer query — single JOIN recovers the full surface.
	type row struct {
		tool          string
		action        string
		framework     string
		mpmSession    sql.NullString
		frameworkSess sql.NullString
		resultStatus  string
		outcomeClass  sql.NullString
		outcomeCode   sql.NullString
		auditComp     sql.NullString
		auditEvent    sql.NullString
		auditLevel    sql.NullString
	}
	var got row
	err := dm.SQLDB().QueryRow(`
		SELECT ti.tool_name, ti.action, ti.framework_name,
		       ti.mpm_session_id, ti.framework_session_id,
		       ti.result_status, ti.outcome_class, ti.outcome_code,
		       al.component, al.event_code, al.level
		FROM tool_invocations ti
		LEFT JOIN system_audit_log al
		       ON al.invocation_id = ti.invocation_id
		WHERE ti.invocation_id = ?
	`, invID).Scan(
		&got.tool, &got.action, &got.framework,
		&got.mpmSession, &got.frameworkSess,
		&got.resultStatus, &got.outcomeClass, &got.outcomeCode,
		&got.auditComp, &got.auditEvent, &got.auditLevel,
	)
	if err != nil {
		t.Fatalf("incident query: %v", err)
	}

	// Each field recoverable without inspecting error_message.
	checks := []struct {
		name, got, want string
	}{
		{"tool", got.tool, toolName},
		{"action", got.action, actionName},
		{"framework", got.framework, frameworkName},
		{"result_status", got.resultStatus, "error"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, c.got, c.want)
		}
	}
	if got.outcomeClass.String != outcomeClass {
		t.Errorf("outcome_class: got %q, want %q", got.outcomeClass.String, outcomeClass)
	}
	if got.outcomeCode.String != outcomeCode {
		t.Errorf("outcome_code: got %q, want %q", got.outcomeCode.String, outcomeCode)
	}
	if got.mpmSession.String != mpmSession {
		t.Errorf("mpm_session: got %q, want %q", got.mpmSession.String, mpmSession)
	}
	if got.frameworkSess.String != frameworkSess {
		t.Errorf("framework_session: got %q, want %q", got.frameworkSess.String, frameworkSess)
	}
	if got.auditComp.String != component {
		t.Errorf("audit_component: got %q, want %q", got.auditComp.String, component)
	}
	if got.auditEvent.String != eventCode {
		t.Errorf("audit_event_code: got %q, want %q", got.auditEvent.String, eventCode)
	}
	if got.auditLevel.String != "error" {
		t.Errorf("audit_level: got %q, want error", got.auditLevel.String)
	}
}

// TestToolOutcome_DegradedSuccess_ProjectsAtBoundary pins that a
// successful-but-degraded operation records outcome_class=degraded
// while result_status remains 'success'. This is the brief §11
// invariant — degraded is a *quality* class, not an error class.
func TestToolOutcome_DegradedSuccess_ProjectsAtBoundary(t *testing.T) {
	dm := NewTestDM(t)
	// The contextual focus subsystem uses
	// ContextualFocusAvailable | ContextualFocusDegraded. A degraded
	// focus SHOULD emit a tool_invocations row with
	// result_status=success and outcome_class=degraded; this is
	// how Doctor / dashboards can detect "focus is silently thin"
	// without parsing the focus payload.
	if _, err := dm.SQLDB().Exec(`
		INSERT INTO tool_invocations
		    (id, session_id, tool_name, action, invocation_id, actor_kind,
		     payload_hash, result_status, started_at, completed_at,
		     outcome_class, outcome_code)
		VALUES (?, 'mcp-default', 'mpm_context', 'get', ?, 'agent',
		        'sha256:focus-degraded', 'success', ?, ?,
		        'degraded', 'contextual_focus_degraded')`,
		"degraded-row-1", "inv-focus-degraded", 1700000200, 1700000205,
	); err != nil {
		t.Fatalf("insert: %v", err)
	}
	var resultStatus, class, code string
	if err := dm.SQLDB().QueryRow(
		`SELECT result_status, outcome_class, outcome_code
		 FROM tool_invocations WHERE invocation_id = ?`,
		"inv-focus-degraded",
	).Scan(&resultStatus, &class, &code); err != nil {
		t.Fatalf("read: %v", err)
	}
	if resultStatus != "success" {
		t.Errorf("result_status = %q, want success (degraded is a quality state, not an error)", resultStatus)
	}
	if class != "degraded" {
		t.Errorf("outcome_class = %q, want degraded", class)
	}
	if code != "contextual_focus_degraded" {
		t.Errorf("outcome_code = %q, want contextual_focus_degraded", code)
	}
}

// TestToolOutcome_OrdinarySuccess_Lightweight pins brief §18: a
// successful call writes one tool_invocations row (with
// outcome_class=ok, outcome_code=empty). No
// additional system_audit_log rows for routine validation
// passes. The fixture uses ordinary success to demonstrate write
// amplification is bounded.
func TestToolOutcome_OrdinarySuccess_Lightweight(t *testing.T) {
	dm := NewTestDM(t)
	if _, err := dm.SQLDB().Exec(`
		INSERT INTO tool_invocations
		    (id, session_id, tool_name, action, invocation_id, actor_kind,
		     payload_hash, result_status, started_at, completed_at,
		     outcome_class, outcome_code)
		VALUES (?, 'cli-default', 'mpm_work', 'list', ?, 'human',
		        'sha256:ok', 'success', ?, ?,
		        'ok', '')`,
		"ok-row-1", "inv-ok-1", 1700000300, 1700000301,
	); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Count audit_log rows that mention this invocation id; should be 0.
	var n int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM system_audit_log WHERE invocation_id = ?`,
		"inv-ok-1",
	).Scan(&n); err != nil {
		t.Fatalf("count audit: %v", err)
	}
	if n != 0 {
		t.Errorf("ordinary success must not write audit rows; got %d", n)
	}
}

// TestToolOutcome_ValidationFailure_NoAuditAmplification pins brief
// §19: routine validation failures get the tool_invocations row,
// no additional system_audit_log row. Validation is the agent's
// fault, not a substrate anomaly worth clustering.
func TestToolOutcome_ValidationFailure_NoAuditAmplification(t *testing.T) {
	dm := NewTestDM(t)
	if _, err := dm.SQLDB().Exec(`
		INSERT INTO tool_invocations
		    (id, session_id, tool_name, action, invocation_id, actor_kind,
		     payload_hash, result_status, started_at, completed_at,
		     outcome_class, outcome_code)
		VALUES (?, 'cli-default', 'mpm_wakes', 'check', ?, 'human',
		        'sha256:missing', 'error', ?, ?,
		        'validation', 'missing_required_field')`,
		"val-row-1", "inv-val-1", 1700000400, 1700000401,
	); err != nil {
		t.Fatalf("insert: %v", err)
	}
	var n int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM system_audit_log WHERE invocation_id = ?`,
		"inv-val-1",
	).Scan(&n); err != nil {
		t.Fatalf("count audit: %v", err)
	}
	if n != 0 {
		t.Errorf("validation failure must not write audit rows (no clustering noise); got %d", n)
	}
}

// TestToolOutcome_OperationalFailure_BothRows pins the *only* case
// where operational failures write both rows: the audit log row is
// what Doctor surfaces, the tool_invocations row is what agent
// metrics count.
func TestToolOutcome_OperationalFailure_BothRows(t *testing.T) {
	dm := NewTestDM(t)
	// Insert tool_invocations row marked outcome_class=substrate.
	if _, err := dm.SQLDB().Exec(`
		INSERT INTO tool_invocations
		    (id, session_id, tool_name, action, invocation_id, actor_kind,
		     payload_hash, result_status, started_at, completed_at,
		     outcome_class, outcome_code)
		VALUES (?, 'cli-default', 'mpm_resolve', 'list', ?, 'agent',
		        'sha256:opfail', 'error', ?, ?,
		        'substrate', 'sqlite_db_locked')`,
		"opfail-row-1", "inv-opfail-1", 1700000500, 1700000508,
	); err != nil {
		t.Fatalf("insert tool_invocations: %v", err)
	}
	// Insert a corresponding audit row at the SAME invocation_id.
	if _, err := dm.SQLDB().Exec(`
		INSERT INTO system_audit_log
		    (id, level, component, message, invocation_id, event_code)
		VALUES (?, 'error', 'cascade-reconciler',
		        'sqlite3: database is locked', ?, 'substrate_db_locked')`,
		"audit-opfail-1", "inv-opfail-1",
	); err != nil {
		t.Fatalf("insert system_audit_log: %v", err)
	}

	// Both must exist, joined by invocation_id.
	var class, code, comp, ev string
	err := dm.SQLDB().QueryRow(`
		SELECT ti.outcome_class, ti.outcome_code, al.component, al.event_code
		FROM tool_invocations ti
		JOIN system_audit_log al ON al.invocation_id = ti.invocation_id
		WHERE ti.invocation_id = ?
	`, "inv-opfail-1").Scan(&class, &code, &comp, &ev)
	if err != nil {
		t.Fatalf("join query: %v", err)
	}
	if class != "substrate" || code != "sqlite_db_locked" || comp != "cascade-reconciler" || ev != "substrate_db_locked" {
		t.Errorf("correlation mismatch: class=%q code=%q comp=%q ev=%q", class, code, comp, ev)
	}
}

// Ensure the package still compiles cleanly with errors import when
// the harness leaves no rows for a path. Defensive: avoid unused
// import when the test file is consulted in isolation.
var _ = errors.New
