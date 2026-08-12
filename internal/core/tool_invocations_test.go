// tool_invocations_test.go — pin the audit table contract:
// schema migrates into BaseTables, columns match the audit-hook contract,
// and INSERT/SELECT round-trip works for the drill scorer.
//
// Behavioral role: this table backs mpm drills score (drill_score.go,
// task 7) and the audit hooks in cmd/mpm/call.go and cmd/mpm-mcp/.
// Renames here cascade through seven files.

package internal

import (
	"testing"
)

func TestToolInvocationsTable_CreatesAndInserts(t *testing.T) {
	dm := NewTestDM(t)

	_, err := dm.SQLDB().Exec(`
		INSERT INTO tool_invocations
		    (id, session_id, tool_name, action, invocation_id,
		     actor_kind, framework_name, payload_hash, result_status,
		     started_at, completed_at, duration_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"inv-1", "sess-1", "mpm_context", "read_wake_context", "uuid-1",
		"agent", "mpm-drill-runner", "sha256:abc", "success",
		1700000000, 1700000001, 1000)
	if err != nil {
		t.Fatalf("insert failed: %v", err)
	}
	var count int
	dm.SQLDB().QueryRow("SELECT COUNT(*) FROM tool_invocations").Scan(&count)
	if count != 1 {
		t.Fatalf("expected 1 row, got %d", count)
	}

	// Index sanity — session_id+started_at composite index exists
	var n int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_tool_invocations_session'`,
	).Scan(&n); err != nil {
		t.Fatalf("index probe: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected idx_tool_invocations_session, got %d rows", n)
	}
}
