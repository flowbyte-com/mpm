package internal

// audit_ledger_contention_repro_test.go — issue #178, phase 2 (audit
// ledger, F6).
//
// The audit ledger is written on EVERY tool dispatch. A busy failure
// loses the row: for system_audit_log that is a hole in the audit
// trail; for tool_invocations it is worse, because the drill scorer
// and mpm_context recent_activity are built from that table.
//
// The writes stay best-effort (void, stderr-only signal) — the fix is
// a statement-level retry budget, not a signature change. These tests
// pin that the rows survive a contention window that outlasts the
// busy timeout.

import (
	"testing"
	"time"
)

// TestRepro_LogAuditSurvivesContention pins the audit-trail hole:
// LogAudit must land its row despite contention, because it runs on
// every tool dispatch and a lost row is a lost record of the dispatch.
func TestRepro_LogAuditSurvivesContention(t *testing.T) {
	dm, competitor := newWALTestDM(t)
	release := holdWALWriteLock(t, competitor)
	defer release()

	done := make(chan struct{})
	go func() {
		defer close(done)
		dm.LogAudit(AuditWarn, "repro", "the audit row that must survive", "", nil)
	}()

	// The retry must engage before the lock is released: otherwise the
	// test would pass without ever meeting the contention.
	waitFor(t, 60*time.Second, "the audit retry to engage", func() bool {
		return dm.BusyRetryCount() > 0
	})
	release()

	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatalf("LogAudit never completed")
	}

	var n int
	if err := dm.db.QueryRow(
		`SELECT COUNT(*) FROM system_audit_log WHERE component = 'repro' AND message = 'the audit row that must survive'`).Scan(&n); err != nil {
		t.Fatalf("counting the audit row: %v", err)
	}
	if n != 1 {
		t.Fatalf("the audit row was lost under contention")
	}
}
