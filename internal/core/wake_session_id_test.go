// wake_session_id_test.go — alpha-4.1.2 D-008 regression test.
//
// Audit finding: the auditor flagged that operators invoking wake-aware
// commands without a session ID saw a bare "session_id is required" error
// with no guidance on how to recover.
//
// Fix (alpha-4 W-006): ErrSessionIDRequired returns a canonical message
// naming both recovery paths (mpm_context read_wake_context's
// session_current_id, and the MPM_SESSION_ID env var). 11 call sites
// consolidated.
//
// This test pins the post-fix contract.

package internal

import (
	"strings"
	"testing"
)

func TestErrSessionIDRequired_NamesBothRecoveryPaths(t *testing.T) {
	err := ErrSessionIDRequired()
	if err == nil {
		t.Fatal("ErrSessionIDRequired returned nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "session_id is required") {
		t.Errorf("error must name the missing field, got: %s", msg)
	}
	// Both recovery paths must be named.
	if !strings.Contains(msg, "session_current_id") {
		t.Errorf("error must name session_current_id as a recovery path, got: %s", msg)
	}
	if !strings.Contains(msg, "MPM_SESSION_ID") {
		t.Errorf("error must name MPM_SESSION_ID env var as a recovery path, got: %s", msg)
	}
}

func TestErrSessionIDRequired_StableShape(t *testing.T) {
	// Calling the helper twice yields identical messages (no incidental
	// state — useful for log-based regression checks downstream).
	a := ErrSessionIDRequired().Error()
	b := ErrSessionIDRequired().Error()
	if a != b {
		t.Errorf("ErrSessionIDRequired message is not stable:\n  first:  %s\n  second: %s", a, b)
	}
}