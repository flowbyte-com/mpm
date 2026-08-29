// f_b1_work_state_machine_test.go — F-B1 work state machine enforcement.
//
// The hostile test surfaced that the substrate silently accepted
// invalid work-status transitions:
//   - cancelling an already-cancelled work
//   - completing an already-completed work
//   - cancelling a done work
//
// These silently masked operator error and broke the verification
// lifecycle (F8.1). The fix adds isValidWorkTransition enforcement
// inside updateWorkStatus; same-state and out-of-order transitions
// now return an explicit error.
package internal

import (
	"strings"
	"testing"
)

// TestF_B1_TransitionTable pins the allowed/denied matrix directly.
func TestF_B1_TransitionTable(t *testing.T) {
	cases := []struct {
		from WorkStatus
		to   WorkStatus
		want bool
	}{
		// Allowed forward transitions
		{WorkStatusOpen, WorkStatusDone, true},
		{WorkStatusOpen, WorkStatusCancelled, true},

		// Reopens
		{WorkStatusDone, WorkStatusOpen, true},
		{WorkStatusCancelled, WorkStatusOpen, true},

		// Same-state (denied)
		{WorkStatusOpen, WorkStatusOpen, false},
		{WorkStatusDone, WorkStatusDone, false},
		{WorkStatusCancelled, WorkStatusCancelled, false},

		// Out-of-order (denied)
		{WorkStatusDone, WorkStatusCancelled, false},
		{WorkStatusCancelled, WorkStatusDone, false},
	}
	for _, tc := range cases {
		got := isValidWorkTransition(tc.from, tc.to)
		if got != tc.want {
			t.Errorf("isValidWorkTransition(%s, %s) = %v, want %v", tc.from, tc.to, got, tc.want)
		}
	}
}

// TestF_B1_CompleteWorkTwiceRejected is the headline regression:
// cancelling an already-cancelled work must return an error.
func TestF_B1_CompleteWorkTwiceRejected(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()

	w := createF_B1Work(t, dm, "test work")

	// First complete: succeeds
	if _, err := dm.CompleteWork(w.ID); err != nil {
		t.Fatalf("first complete: %v", err)
	}

	// Second complete: rejected (done → done is invalid)
	_, err := dm.CompleteWork(w.ID)
	if err == nil {
		t.Fatalf("second complete should be rejected")
	}
	if !strings.Contains(err.Error(), "invalid transition") {
		t.Fatalf("error should mention invalid transition, got: %v", err)
	}
}

// TestF_B1_CancelWorkTwiceRejected mirrors the complete test for cancel.
func TestF_B1_CancelWorkTwiceRejected(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()

	w := createF_B1Work(t, dm, "test work cancel")

	if _, err := dm.CancelWork(w.ID); err != nil {
		t.Fatalf("first cancel: %v", err)
	}

	_, err := dm.CancelWork(w.ID)
	if err == nil {
		t.Fatalf("second cancel should be rejected")
	}
	if !strings.Contains(err.Error(), "invalid transition") {
		t.Fatalf("error should mention invalid transition, got: %v", err)
	}
}

// TestF_B1_CancelAfterDoneRejected pins the order-of-operations
// invariant: done → cancelled is forbidden (must reopen first).
func TestF_B1_CancelAfterDoneRejected(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()

	w := createF_B1Work(t, dm, "test work order")

	if _, err := dm.CompleteWork(w.ID); err != nil {
		t.Fatalf("complete: %v", err)
	}

	_, err := dm.CancelWork(w.ID)
	if err == nil {
		t.Fatalf("done → cancelled should be rejected; must reopen first")
	}
}

// TestF_B1_ReopenRoundTripAccepted verifies the happy path: open
// → done → open is allowed.
func TestF_B1_ReopenRoundTripAccepted(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()

	w := createF_B1Work(t, dm, "test work reopen")

	if _, err := dm.CompleteWork(w.ID); err != nil {
		t.Fatalf("complete: %v", err)
	}

	if _, err := dm.UpdateWork(w.ID, WorkStatusOpen); err != nil {
		t.Fatalf("reopen after done: %v", err)
	}

	// And from cancelled → open
	if _, err := dm.CancelWork(w.ID); err != nil {
		t.Fatalf("cancel after reopen: %v", err)
	}
	if _, err := dm.UpdateWork(w.ID, WorkStatusOpen); err != nil {
		t.Fatalf("reopen after cancelled: %v", err)
	}
}

// TestF_B1_UpdateWorkNotFound is the missing-id contract: an invalid
// id returns "work not found" rather than silently succeeding.
func TestF_B1_UpdateWorkNotFound(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()

	_, err := dm.CompleteWork("nonexistent-id-xyz")
	if err == nil {
		t.Fatalf("complete on nonexistent work should fail")
	}
	if !strings.Contains(err.Error(), "work not found") {
		t.Fatalf("error should mention work not found, got: %v", err)
	}
}

// createF_B1Work seeds an open work and returns it.
func createF_B1Work(t *testing.T, dm *DatabaseManager, title string) *Work {
	t.Helper()
	w, err := dm.AddWork(title, "test body", "")
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	if w == nil {
		t.Fatalf("create work: nil")
	}
	return w
}

// TestF_B1_EventSourcedPathRejected pins the gate-2026-08-29 fix: the
// event-sourced path (UpdateWorkWithContext → AppendWorkEvent) MUST also
// enforce the state machine. Previously the legacy updateWorkStatus path
// had the check and the event-sourced path silently bypassed it, so
// `mpm_work action=update status=cancelled` on a `done` work succeeded.
// This is the regression the gate adversarial exploration surfaced.
func TestF_B1_EventSourcedPathRejected(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()

	w := createF_B1Work(t, dm, "test work event-sourced gate")

	// Move to done via the event-sourced path (CompleteWorkWithContext).
	if _, err := dm.CompleteWorkWithContext(w.ID, "", ActiveContext{}); err != nil {
		t.Fatalf("complete via event path: %v", err)
	}

	// Now attempt done → cancelled via UpdateWorkWithContext. This is the
	// exact path the gate surfaced: mpm_work action=update status=cancelled.
	_, err := dm.UpdateWorkWithContext(w.ID, "", "", string(WorkStatusCancelled), ActiveContext{})
	if err == nil {
		t.Fatalf("event-sourced done → cancelled should be rejected (gate regression)")
	}
	if !strings.Contains(err.Error(), "invalid transition") {
		t.Fatalf("error should mention invalid transition, got: %v", err)
	}

	// Also exercise done → done via the same path (same-state, also denied).
	_, err = dm.UpdateWorkWithContext(w.ID, "", "", string(WorkStatusDone), ActiveContext{})
	if err == nil {
		t.Fatalf("event-sourced done → done should be rejected")
	}
}

// TestF_B1_EventSourcedReopenAllowed pins the happy path on the
// event-sourced side: done → open must be permitted (it's a reopen).
func TestF_B1_EventSourcedReopenAllowed(t *testing.T) {
	dm := NewTestDM(t)
	defer dm.Close()

	w := createF_B1Work(t, dm, "test work event-sourced reopen")

	if _, err := dm.CompleteWorkWithContext(w.ID, "", ActiveContext{}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if _, err := dm.UpdateWorkWithContext(w.ID, "", "", string(WorkStatusOpen), ActiveContext{}); err != nil {
		t.Fatalf("event-sourced done → open (reopen) should be allowed, got: %v", err)
	}

	// And cancelled → open via the event-sourced path.
	if _, err := dm.CancelWorkWithContext(w.ID, "", ActiveContext{}); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if _, err := dm.UpdateWorkWithContext(w.ID, "", "", string(WorkStatusOpen), ActiveContext{}); err != nil {
		t.Fatalf("event-sourced cancelled → open (reopen) should be allowed, got: %v", err)
	}
}