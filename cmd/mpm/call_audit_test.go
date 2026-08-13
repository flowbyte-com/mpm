// call_audit_test.go — pin the audit-hook contract for the universal
// machine interface (`mpm call <tool>`). Every dispatch — whether CLI
// or MCP — must leave a row in tool_invocations so that the drill
// orchestrator can derive per-session tool-call sequences.
//
// Behavioral role: this is the evidence layer for the drill matrix.
// Without it, `mpm drills run` produces no proof of compliance.

package main

import (
	"testing"
)

// TestRunHandler_WritesAuditRow verifies that the test entry point
// `runHandler` (the same path used by `handleCall` production code)
// inserts a tool_invocations row capturing tool_name, result_status,
// and session linkage.
func TestRunHandler_WritesAuditRow(t *testing.T) {
	dm := newTestDMForCmd(t)

	if _, err := runHandler(dm, "mpm_system", map[string]interface{}{
		"action": "health_check",
		"params": map[string]interface{}{},
	}); err != nil {
		t.Fatalf("runHandler mpm_system/health_check: %v", err)
	}

	var count int
	if err := dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM tool_invocations`).Scan(&count); err != nil {
		t.Fatalf("count tool_invocations: %v", err)
	}
	if count < 1 {
		t.Fatalf("expected >=1 audit row from runHandler, got %d", count)
	}

	var toolName, resultStatus, actorKind string
	row := dm.SQLDB().QueryRow(`SELECT tool_name, result_status, actor_kind FROM tool_invocations ORDER BY started_at DESC LIMIT 1`)
	if err := row.Scan(&toolName, &resultStatus, &actorKind); err != nil {
		t.Fatalf("scan audit row: %v", err)
	}
	if toolName != "mpm_system" {
		t.Fatalf("tool_name = %q, want mpm_system", toolName)
	}
	if resultStatus != "success" {
		t.Fatalf("result_status = %q, want success", resultStatus)
	}
	if actorKind == "" {
		t.Fatalf("actor_kind must not be empty (drill scorer groups on it)")
	}
}
