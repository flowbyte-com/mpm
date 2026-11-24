package dispatch

// R1 regression: an ambiguous attempt blocks the dispatcher BEFORE a claim,
// even after lease expiry and with a genuine host-owned worktree.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/flowbyte-com/mpm-core/assignment"
)

func TestDispatcherRefusesAmbiguousAttemptBeforeClaim(t *testing.T) {
	store, projectID := testStore(t)
	root := dispatchRoot(t, store, projectID)
	child := dispatchChild(t, store, root, "alpha", assignment.RoleImplementer, "idem:r1:ambiguous:child")

	cfg := testConfig(t)
	cfg.MaxDispatchAttempts = 2
	engine, fake := testEngine(t, store, cfg)
	fake.launchState = okState("mpm-worker", "succeeded")
	workspace, err := engine.workspaceFor(child)
	if err != nil {
		t.Fatalf("provision the real worktree: %v", err)
	}

	actor := assignment.Actor{Address: "alpha", Kind: "agent"}
	claimed, err := store.Claim(assignment.ClaimParams{
		ID: child.ID, ExpectedEpoch: 0, Owner: "alpha", LeaseSeconds: 1, Actor: actor,
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := store.Start(assignment.FencedParams{
		ID: child.ID, Epoch: claimed.ClaimEpoch, Actor: actor,
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	att, err := store.BeginAttempt(assignment.BeginAttemptParams{
		TaskID: child.ID, Epoch: claimed.ClaimEpoch, Runtime: assignment.RuntimeOpenCode,
		IntendedAgent: "mpm-worker", NativeSessionID: "ses_r1_ambiguous",
		CorrelationRef: "corr-r1", WorkspaceID: child.ID, WorkspacePath: workspace,
	})
	if err != nil {
		t.Fatalf("begin attempt: %v", err)
	}
	if _, err := store.MarkAttemptLaunched(assignment.SessionStateParams{
		AttemptID: att.ID, Epoch: claimed.ClaimEpoch,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkAttemptAmbiguous(assignment.SessionStateParams{
		AttemptID: att.ID, Epoch: claimed.ClaimEpoch,
		Error: "the runtime produced no session state",
	}); err != nil {
		t.Fatal(err)
	}

	// The lease is deliberately expired. Expiration is not retry
	// authorisation.
	time.Sleep(2 * time.Second)
	report, err := engine.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if fake.Launches() != 0 {
		t.Fatalf("the dispatcher launched the worker %d time(s)", fake.Launches())
	}
	attempts, _ := store.AttemptsFor(child.ID)
	if len(attempts) != 1 {
		t.Fatalf("attempts = %d, want 1 (no second attempt)", len(attempts))
	}
	row, _ := store.Get(child.ID)
	if row.ClaimEpoch != claimed.ClaimEpoch {
		t.Fatalf("dispatcher advanced the epoch to %d; refusal must not claim", row.ClaimEpoch)
	}
	found := false
	for _, o := range report.Outcomes {
		if o.AssignmentID == child.ID && o.Reason == ReasonRecoveryRequired {
			found = true
		}
	}
	if !found {
		t.Fatalf("no recovery_required outcome for the ambiguous assignment: %+v", report.Outcomes)
	}
}

func TestAmbiguousAdapterRefusalIsRecoveryRequired(t *testing.T) {
	if got := classifyAdapterRefusal(fmt.Errorf("%w: test", assignment.ErrAmbiguousAttempt)); got != ReasonRecoveryRequired {
		t.Fatalf("reason = %q, want %q", got, ReasonRecoveryRequired)
	}
}
