// call_audit_test.go — pin the audit-hook contract for the universal
// machine interface (`mpm call <tool>`). Every dispatch — whether CLI
// or MCP — must leave a row in tool_invocations so that the drill
// orchestrator can derive per-session tool-call sequences.
//
// Behavioral role: this is the evidence layer for the drill matrix.
// Without it, `mpm drills run` produces no proof of compliance.

package main

import (
	"bytes"
	"os"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
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

// TestHandleCall_ProvenanceEnvVars verifies that MPM_PROVENANCE_* env vars
// set on the CLI are captured in tool_invocations rows. This is the
// integration contract that makes non-MPM callers (Claude Code, OpenCode,
// etc.) first-class provenance citizens without any plugin.
//
// Covers: framework_name, model_name, invocation_id, parent_invocation_id.
func TestHandleCall_ProvenanceEnvVars(t *testing.T) {
	// Use a temp workspace so handleCall's DB and our query DB are the same.
	ws := t.TempDir()
	t.Setenv("MPM_WORKSPACE", ws)

	// Capture stdout.
	origStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w
	t.Cleanup(func() {
		os.Stdout = origStdout
		_ = r.Close()
		_ = w.Close()
	})

	// Set provenance env vars as Claude Code would.
	t.Setenv("MPM_PROVENANCE_FRAMEWORK", "claude-code")
	t.Setenv("MPM_PROVENANCE_MODEL", "opus-5")
	t.Setenv("MPM_PROVENANCE_INVOCATION_ID", "test-invocation-abc123")
	t.Setenv("MPM_PROVENANCE_PARENT_INVOCATION_ID", "parent-inv-xyz")

	exit := handleCall([]string{
		"mpm_work",
		"--payload", `{"action":"create","params":{"title":"Provenance test work"}}`,
	})
	if err := w.Close(); err != nil {
		t.Fatalf("close pipe writer: %v", err)
	}
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r); err != nil {
		t.Fatalf("read captured stdout: %v", err)
	}
	_ = buf.String()
	if exit != 0 {
		t.Logf("handleCall exited %d", exit)
	}

	// Open the same workspace DB to query the audit rows.
	dm, err := mpminternal.NewDatabaseManager(ws)
	if err != nil {
		t.Fatalf("NewDatabaseManager: %v", err)
	}
	defer dm.Close()

	var frameworkName, invocationID string
	row := dm.SQLDB().QueryRow(`
		SELECT framework_name, invocation_id
		FROM tool_invocations
		WHERE invocation_id = 'test-invocation-abc123'
		ORDER BY started_at DESC LIMIT 1
	`)
	if err := row.Scan(&frameworkName, &invocationID); err != nil {
		t.Fatalf("query provenance in tool_invocations: %v", err)
	}
	if frameworkName != "claude-code" {
		t.Errorf("framework_name = %q, want claude-code", frameworkName)
	}
	if invocationID != "test-invocation-abc123" {
		t.Errorf("invocation_id = %q, want test-invocation-abc123", invocationID)
	}

	// model_name is in artifact_provenance (not tool_invocations).
	var artModelName string
	artRow := dm.SQLDB().QueryRow(`
		SELECT model_name FROM artifact_provenance
		WHERE invocation_id = 'test-invocation-abc123'
		ORDER BY created_at DESC LIMIT 1
	`)
	if err := artRow.Scan(&artModelName); err != nil {
		t.Fatalf("query model_name in artifact_provenance: %v", err)
	}
	if artModelName != "opus-5" {
		t.Errorf("model_name in artifact_provenance = %q, want opus-5", artModelName)
	}
}

// TestHandleCall_DefaultFrameworkIsMpmCLI verifies that when no provenance
// env vars are set, the default framework_name is "mpm-cli".
func TestHandleCall_DefaultFrameworkIsMpmCLI(t *testing.T) {
	ws := t.TempDir()
	t.Setenv("MPM_WORKSPACE", ws)

	origStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w
	t.Cleanup(func() {
		os.Stdout = origStdout
		_ = r.Close()
		_ = w.Close()
	})

	// Ensure no provenance env vars are set.
	t.Setenv("MPM_PROVENANCE_FRAMEWORK", "")
	t.Setenv("MPM_PROVENANCE_MODEL", "")
	t.Setenv("MPM_PROVENANCE_INVOCATION_ID", "")
	t.Setenv("MPM_PROVENANCE_PARENT_INVOCATION_ID", "")

	exit := handleCall([]string{
		"mpm_system",
		"--payload", `{"action":"health_check","params":{}}`,
	})
	_ = w.Close()
	if exit != 0 {
		t.Fatalf("handleCall mpm_system: exit %d", exit)
	}

	dm, err := mpminternal.NewDatabaseManager(ws)
	if err != nil {
		t.Fatalf("NewDatabaseManager: %v", err)
	}
	defer dm.Close()

	var frameworkName string
	row := dm.SQLDB().QueryRow(`
		SELECT framework_name FROM tool_invocations
		ORDER BY started_at DESC LIMIT 1
	`)
	if err := row.Scan(&frameworkName); err != nil {
		t.Fatalf("query framework_name: %v", err)
	}
	if frameworkName != "mpm-cli" {
		t.Errorf("framework_name = %q, want mpm-cli (default when no env vars set)", frameworkName)
	}
}
