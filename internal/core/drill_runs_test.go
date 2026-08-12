// drill_runs_test.go — pin the drill-run ledger contract.
//
// Behavioral role: every drill run (synthetic or real-framework) writes
// a row here. The compatibility-matrix query in cmd/mpm's
// handleDrillsReport reads from this table; mpm-drill-runner scheduler
// handler writes here. Renames cascade through scheduler/drill_handler.go
// and the score report.

package internal

import (
	"encoding/json"
	"testing"
)

func TestDrillRunsTable_InsertAndQuery(t *testing.T) {
	dm := NewTestDM(t)

	verdict := json.RawMessage(`{"passed":true,"reasons":[]}`)
	_, err := dm.SQLDB().Exec(`
		INSERT INTO drill_runs (id, drill_id, framework, session_id, status, verdict, started_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"dr-1", "lesson-persistence-001", "synthetic", "sess-1", "passed", string(verdict), 1700000000)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	var status string
	var gotVerdict string
	if err := dm.SQLDB().QueryRow(
		`SELECT status, verdict FROM drill_runs WHERE id = 'dr-1'`,
	).Scan(&status, &gotVerdict); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if status != "passed" {
		t.Fatalf("status = %q, want passed", status)
	}
	if gotVerdict == "" {
		t.Fatal("verdict must round-trip through the JSON column")
	}

	// Index sanity — at least one of the indexes declared with the table
	// must exist so the report query stays cheap.
	var n int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name IN ('idx_drill_runs_session','idx_drill_runs_drill')`,
	).Scan(&n); err != nil {
		t.Fatalf("index probe: %v", err)
	}
	if n < 2 {
		t.Fatalf("expected both drill_runs indexes, got %d", n)
	}
}
