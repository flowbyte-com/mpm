// recovery_test.go — Continuity 001, PR 4: crash recovery and the fences
// that make a duplicate impossible.
//
// # THE FOUR CRASH POINTS
//
// A dispatcher run can die at four places, and each one leaves a
// different record:
//
//	after the CLAIM          the assignment is leased and nothing ran
//	after the INTENT          the session id is durable; the launch may or
//	                          may not have happened
//	during the EXECUTION      the session may still be working
//	after the RUNTIME ENDED   the outcome exists and was never recorded
//
// The first three are recoverable or safely refusable. The fourth is
// reconciled through the existing fenced path, which is the only thing
// that may record a runtime's report.
//
// # WHAT MUST NEVER HAPPEN
//
//   - A second writer into a workspace whose earlier session may still be
//     working.
//   - An outcome that was never observed becoming a success.
//   - A completion published by a claim epoch that has moved.
package dispatch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/assignment"
	oc "github.com/flowbyte-com/mpm-core/opencode"
)

// testClock is an injected clock, so a lease can expire without the test
// waiting for it.
type testClock struct {
	mu  sync.Mutex
	now int64
}

func newTestClock() *testClock {
	return &testClock{now: 1_700_000_000}
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Unix(c.now, 0).UTC()
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now += int64(d.Seconds())
}

// clockStore builds a store over a disposable database with an injected
// clock.
func clockStore(t *testing.T) (*assignment.Store, *testClock, string) {
	t.Helper()
	dm := internal.NewTestLocalOnlyDM(t)
	clock := newTestClock()
	store := assignment.NewStoreWithClock(dm, clock.Now)
	id := "cpj-" + strings.Repeat("a", 16)
	if _, err := dm.SQLDB().Exec(
		`INSERT INTO projects (id, created_by) VALUES (?, 'dispatch-test')`, id); err != nil {
		t.Fatalf("seed the project: %v", err)
	}
	return store, clock, id
}

// clockEngine builds an engine whose clock is the injected one.
func clockEngine(t *testing.T, store *assignment.Store, clock *testClock, cfg Config) (*Engine, *fakeRuntime) {
	t.Helper()
	fake := newFakeRuntime()
	engine, err := NewEngine(store, cfg, func(context.Context, *assignment.Assignment, string) (
		assignment.Runtime, assignment.ReviewRuntime, error) {
		if false {
			return nil, nil, errors.New("unreachable")
		}
		return fake, fake, nil
	})
	if err != nil {
		t.Fatalf("build the engine: %v", err)
	}
	engine.now = clock.Now
	return engine, fake
}

// seedAttempt writes a launch intent the way the adapter does, so the
// recovery has a durable record to work from.
func seedAttempt(t *testing.T, store *assignment.Store, a *assignment.Assignment,
	epoch int64, sessionID, launchState string) *assignment.SessionAttempt {
	t.Helper()
	attempt, err := store.BeginAttempt(assignment.BeginAttemptParams{
		TaskID: a.ID, Epoch: epoch, Runtime: assignment.RuntimeOpenCode,
		IntendedAgent: "mpm-worker", NativeSessionID: sessionID,
		CorrelationRef: "corr", WorkspaceID: a.ID, WorkspacePath: "/tmp/ws",
	})
	if err != nil {
		t.Fatalf("seed the attempt: %v", err)
	}
	if launchState != assignment.SessionIntent {
		if _, err := store.MarkAttemptLaunched(assignment.SessionStateParams{
			AttemptID: attempt.ID, Epoch: epoch,
		}); err != nil {
			t.Fatalf("mark launched: %v", err)
		}
		if launchState == assignment.SessionRunning {
			if _, err := store.MarkAttemptRunning(assignment.SessionStateParams{
				AttemptID: attempt.ID, Epoch: epoch,
			}); err != nil {
				t.Fatalf("mark running: %v", err)
			}
		}
	}
	return attempt
}

// ── CRASH AFTER THE CLAIM ────────────────────────────────────────────────

// TestACrashAfterTheClaimIsRecoverable: the assignment is leased and
// nothing ran.
//
// The next run finds the claim, sees that no attempt exists, and dispatches
// it normally. Nothing is ambiguous, because nothing was launched.
func TestACrashAfterTheClaimIsRecoverable(t *testing.T) {
	store, clock, projectID := clockStore(t)
	root := dispatchRoot(t, store, projectID)
	child := dispatchChild(t, store, root, "alpha", assignment.RoleImplementer, "idem:rec:claim")

	// The crash: a claim and nothing else.
	claimed, err := store.Claim(assignment.ClaimParams{
		ID: child.ID, ExpectedEpoch: 0, Owner: "mpm-dispatcher", LeaseSeconds: 1800,
		Actor: assignment.Actor{Address: "mpm-dispatcher", Kind: "agent"},
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(mustAttempts(t, store, child.ID)) != 0 {
		t.Fatal("the fixture created an attempt")
	}
	// The lease expires, so the next run reclaims.
	clock.Advance(1900 * time.Second)

	cfg := testConfig(t)
	engine, fake := clockEngine(t, store, clock, cfg)
	fake.launchState = okState("mpm-worker", "succeeded")

	report, err := engine.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if fake.Launches() != 1 {
		t.Fatalf("the runtime was launched %d times, want 1", fake.Launches())
	}
	outcome, ok := outcomeFor(report, child.ID)
	if !ok {
		t.Fatalf("the report has no outcome: %+v", report.Outcomes)
	}
	if outcome.Reason != ReasonCompleted {
		t.Errorf("the outcome is %q, want %q", outcome.Reason, ReasonCompleted)
	}
	got, err := store.Get(child.ID)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.State != assignment.StateCompleted {
		t.Errorf("the assignment is %q, want %q", got.State, assignment.StateCompleted)
	}
	_ = claimed
}

// ── CRASH AFTER THE INTENT ───────────────────────────────────────────────

// TestACrashAfterTheIntentIsInspectedAndThenRunsAgain: the session id is
// durable and the launch may or may not have happened.
//
// THE FIRST RUN MUST NOT LAUNCH. The previous run's lease is still
// live, and a live lease is never reclaimed: the assignment waits for
// it. That is the safe direction, because an expired lease is not
// evidence that the old process stopped.
//
// THE SECOND RUN, AFTER THE LEASE EXPIRES, INSPECTS THE RECORDED SESSION
// before it starts anything. The runtime has never heard of it, so
// nothing was created, and re-running the objective cannot duplicate
// work — but the recorded id is NOT blindly reused: the new attempt
// gets its own id, because the old attempt's writes are fenced out and
// its id no longer means anything to this claim.
func TestACrashAfterTheIntentIsInspectedAndThenRunsAgain(t *testing.T) {
	store, clock, projectID := clockStore(t)
	root := dispatchRoot(t, store, projectID)
	child := dispatchChild(t, store, root, "alpha", assignment.RoleImplementer, "idem:rec:intent")

	claimed, err := store.Claim(assignment.ClaimParams{
		ID: child.ID, ExpectedEpoch: 0, Owner: "mpm-dispatcher", LeaseSeconds: 1800,
		Actor: assignment.Actor{Address: "mpm-dispatcher", Kind: "agent"},
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	seedAttempt(t, store, child, claimed.ClaimEpoch, "ses_recorded_intent", assignment.SessionIntent)

	cfg := testConfig(t)
	engine, fake := clockEngine(t, store, clock, cfg)
	// THE LAUNCH CREATED THE WORKTREE. Seed it before the simulated
	// crash: recovery verifies an existing worktree and never creates
	// one.
	stateAtWorkspace(t, engine, child)

	fake.launchState = okState("mpm-worker", "succeeded")

	// THE FIRST RUN: THE LEASE IS STILL LIVE.
	report, err := engine.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("the first run: %v", err)
	}
	if fake.Launches() != 0 {
		t.Fatalf("the runtime was launched %d times while a live lease was held",
			fake.Launches())
	}
	outcome, ok := outcomeFor(report, child.ID)
	if !ok {
		t.Fatalf("the report has no outcome: %+v", report.Outcomes)
	}
	if outcome.Reason != ReasonLeaseHeld {
		t.Errorf("the outcome is %q, want %q", outcome.Reason, ReasonLeaseHeld)
	}

	// THE LEASE EXPIRES.
	clock.Advance(1900 * time.Second)

	// THE SECOND RUN.
	if _, err := engine.RunOnce(context.Background()); err != nil {
		t.Fatalf("the second run: %v", err)
	}
	if fake.Launches() != 1 {
		t.Fatalf("the runtime was launched %d times after the lease expired, want 1",
			fake.Launches())
	}
	attempts := mustAttempts(t, store, child.ID)
	if len(attempts) != 2 {
		t.Fatalf("the assignment has %d attempts, want 2", len(attempts))
	}
	if attempts[1].NativeSessionID == "ses_recorded_intent" {
		t.Error("the second attempt reused the fenced attempt's session id")
	}
	if attempts[1].ClaimEpoch == attempts[0].ClaimEpoch {
		t.Errorf("the second attempt is at epoch %d, want a NEW epoch",
			attempts[1].ClaimEpoch)
	}
}

// TestTheRecoveryPassRefusesAMissingWorktree proves the recovery pass
// verifies the recorded worktree and never creates one.
//
// An outstanding attempt implies a launch. A missing worktree is a fact
// for a human, not a directory for recovery to conjure: creating one
// while a lease or an older session may still be live would be a second
// writer's workspace.
//
// The fake factory here refuses an empty workspace the way the real one
// does. Without the fix, every inspection of an implementer's session
// fails with "runtime unavailable", and the whole recovery pass turns
// into a refusal: no blocker is ever detected, and no note ever reaches
// the ledger.
func TestTheRecoveryPassRefusesAMissingWorktree(t *testing.T) {
	store, clock, projectID := clockStore(t)
	root := dispatchRoot(t, store, projectID)
	child := dispatchChild(t, store, root, "alpha", assignment.RoleImplementer, "idem:rec:workspace")

	claimed, err := store.Claim(assignment.ClaimParams{
		ID: child.ID, ExpectedEpoch: 0, Owner: "mpm-dispatcher", LeaseSeconds: 1800,
		Actor: assignment.Actor{Address: "mpm-dispatcher", Kind: "agent"},
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	// THE ATTEMPT SAYS A LAUNCH HAPPENED; THE WORKTREE IS GONE.
	seedAttempt(t, store, child, claimed.ClaimEpoch, "ses_workspace", assignment.SessionRunning)

	var seen []string
	strict := newFakeRuntime()
	strict.states["ses_workspace"] = &oc.SessionState{ID: "ses_workspace", Outcome: ""}
	engine, err := NewEngine(store, testConfig(t), func(_ context.Context, a *assignment.Assignment, workspace string) (
		assignment.Runtime, assignment.ReviewRuntime, error) {
		seen = append(seen, workspace)
		if a.Role != assignment.RoleReviewer && workspace == "" {
			return nil, nil, errors.New("the worker runtime needs an authorised workspace")
		}
		return strict, strict, nil
	})
	if err != nil {
		t.Fatalf("build the engine: %v", err)
	}
	engine.now = clock.Now

	report, err := engine.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	// RECOVERY DID NOT CREATE A WORKSPACE. An outstanding attempt implies
	// a launch, and a missing worktree is a fact for a human; creating
	// one while a lease or an older session may still be live would be a
	// second writer's workspace.
	if len(seen) != 0 {
		t.Errorf("the recovery pass provisioned a workspace for a missing worktree: %v", seen)
	}
	// AND NOTHING WAS CREATED. The factory not being called is not the
	// same fact as the path staying absent: a call that created the
	// worktree and then returned an error would pass the check above.
	if _, statErr := os.Lstat(filepath.Join(testConfigRootFromEngine(engine), child.ID)); !os.IsNotExist(statErr) {
		t.Errorf("the recovery pass created the missing worktree (stat: %v)", statErr)
	}
	outcome, ok := outcomeFor(report, child.ID)
	if !ok {
		t.Fatalf("the report has no outcome: %+v", report.Outcomes)
	}
	if outcome.Reason != ReasonRecoveryRequired {
		t.Errorf("the outcome is %q, want %q", outcome.Reason, ReasonRecoveryRequired)
	}
	if !strings.Contains(outcome.Detail, "worktree") {
		t.Errorf("the detail does not name the missing worktree: %s", outcome.Detail)
	}
}

// ── CRASH DURING THE EXECUTION ───────────────────────────────────────────

// TestACrashDuringTheExecutionIsRefusedAndVisible: the session may still
// be working.
//
// This is the case that must NOT produce a second writer. The dispatcher
// must refuse, and it must leave an operator-visible record.
func TestACrashDuringTheExecutionIsRefusedAndVisible(t *testing.T) {
	store, clock, projectID := clockStore(t)
	root := dispatchRoot(t, store, projectID)
	child := dispatchChild(t, store, root, "alpha", assignment.RoleImplementer, "idem:rec:running")

	claimed, err := store.Claim(assignment.ClaimParams{
		ID: child.ID, ExpectedEpoch: 0, Owner: "mpm-dispatcher", LeaseSeconds: 1800,
		Actor: assignment.Actor{Address: "mpm-dispatcher", Kind: "agent"},
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	seedAttempt(t, store, child, claimed.ClaimEpoch, "ses_running", assignment.SessionRunning)

	cfg := testConfig(t)
	engine, fake := clockEngine(t, store, clock, cfg)
	// THE LAUNCH CREATED THE WORKTREE. Seed it so the test exercises
	// the session/drain path it names, not the missing-worktree refusal.
	stateAtWorkspace(t, engine, child)

	// The session exists and has NOT reported an outcome: it is working.
	fake.states["ses_running"] = &oc.SessionState{
		ID: "ses_running", Outcome: "", Agent: "mpm-worker",
	}
	fake.launchState = okState("mpm-worker", "succeeded")

	report, err := engine.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	// NO SECOND WRITER.
	if fake.Launches() != 0 {
		t.Errorf("the runtime was launched %d times while a session was still running",
			fake.Launches())
	}
	outcome, ok := outcomeFor(report, child.ID)
	if !ok {
		t.Fatalf("the report has no outcome: %+v", report.Outcomes)
	}
	if outcome.Reason != ReasonSessionStillRunning && outcome.Reason != ReasonRecoveryRequired {
		t.Errorf("the outcome is %q, want the session to block", outcome.Reason)
	}
	// THE REASON IS THE SESSION, NOT A MISSING WORKTREE. Both outcomes
	// share `recovery_required`; the detail is what separates them.
	wantDetail := "still running"
	if outcome.Reason == ReasonSessionStillRunning {
		wantDetail = "has not reported an outcome"
	}
	if !strings.Contains(outcome.Detail, wantDetail) {
		t.Errorf("the detail is %q, want the running session named", outcome.Detail)
	}
	// THE OPERATOR-VISIBLE RECORD.
	events, err := store.History(child.ID)
	if err != nil {
		t.Fatalf("read the history: %v", err)
	}
	found := false
	for _, e := range events {
		if e.EventType == assignment.EventNote && strings.Contains(e.Note, "recovery required") {
			found = true
		}
	}
	if !found {
		t.Errorf("the assignment's ledger carries no recovery note: %+v", events)
	}
}

// ── CRASH AFTER THE RUNTIME ENDED ────────────────────────────────────────

// TestACrashAfterTheRuntimeEndedIsReconciled: the outcome exists and was
// never recorded.
//
// The dispatcher must record it — through the EXISTING fenced path, which
// is PR 3's adapter, and not by writing the outcome itself.
func TestACrashAfterTheRuntimeEndedIsReconciled(t *testing.T) {
	store, clock, projectID := clockStore(t)
	root := dispatchRoot(t, store, projectID)
	child := dispatchChild(t, store, root, "alpha", assignment.RoleImplementer, "idem:rec:ended")

	claimed, err := store.Claim(assignment.ClaimParams{
		ID: child.ID, ExpectedEpoch: 0, Owner: "mpm-dispatcher", LeaseSeconds: 1800,
		Actor: assignment.Actor{Address: "mpm-dispatcher", Kind: "agent"},
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	seedAttempt(t, store, child, claimed.ClaimEpoch, "ses_ended", assignment.SessionIntent)

	cfg := testConfig(t)
	engine, fake := clockEngine(t, store, clock, cfg)
	// THE LAUNCH CREATED THE WORKTREE. Seed it before the simulated
	// crash: recovery verifies an existing worktree and never creates
	// one.
	ws := stateAtWorkspace(t, engine, child)

	// The session FINISHED, and the adapter never saw it.
	fake.states["ses_ended"] = &oc.SessionState{
		ID: "ses_ended", Outcome: "succeeded", Agent: "mpm-worker",
		Model:    oc.SessionModel{ID: "test", ProviderID: "mpm-sidecar"},
		Location: oc.SessionLocation{Directory: ws},
	}

	report, err := engine.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	// IT IS NOT RE-LAUNCHED. The work was done.
	if fake.Launches() != 0 {
		t.Errorf("the runtime was launched %d times although the session had already "+
			"reported a terminal outcome", fake.Launches())
	}
	got, err := store.Get(child.ID)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.State != assignment.StateCompleted {
		t.Errorf("the assignment is %q, want %q; the observed outcome was not reconciled",
			got.State, assignment.StateCompleted)
	}
	outcome, ok := outcomeFor(report, child.ID)
	if !ok {
		t.Fatalf("the report has no outcome: %+v", report.Outcomes)
	}
	if outcome.RuntimeOutcome != "succeeded" {
		t.Errorf("the outcome reports the runtime outcome %q, want succeeded",
			outcome.RuntimeOutcome)
	}
}

// TestARecoveredFailureIsCounted proves a recovered FAILURE reaches a
// counter.
//
// The report's counts have to cover every outcome the report carries. A
// recovered attempt whose runtime said "failed" produces an outcome with
// the reason `failed` — and before the fix that outcome was reflected in
// NO counter, so the counts did not add up to the outcomes and an
// operator could not see the failure at all.
func TestARecoveredFailureIsCounted(t *testing.T) {
	store, clock, projectID := clockStore(t)
	root := dispatchRoot(t, store, projectID)
	child := dispatchChild(t, store, root, "alpha", assignment.RoleImplementer, "idem:rec:failed")

	claimed, err := store.Claim(assignment.ClaimParams{
		ID: child.ID, ExpectedEpoch: 0, Owner: "mpm-dispatcher", LeaseSeconds: 1800,
		Actor: assignment.Actor{Address: "mpm-dispatcher", Kind: "agent"},
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	seedAttempt(t, store, child, claimed.ClaimEpoch, "ses_failed", assignment.SessionIntent)

	cfg := testConfig(t)
	engine, fake := clockEngine(t, store, clock, cfg)
	// THE LAUNCH CREATED THE WORKTREE. Seed it before the simulated
	// crash: recovery verifies an existing worktree and never creates
	// one.
	ws := stateAtWorkspace(t, engine, child)

	// The session FAILED, and the adapter never saw it.
	fake.states["ses_failed"] = &oc.SessionState{
		ID: "ses_failed", Outcome: "failed", Agent: "mpm-worker",
		Model:    oc.SessionModel{ID: "test", ProviderID: "mpm-sidecar"},
		Location: oc.SessionLocation{Directory: ws},
	}

	report, err := engine.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	outcome, ok := outcomeFor(report, child.ID)
	if !ok {
		t.Fatalf("the report has no outcome: %+v", report.Outcomes)
	}
	if outcome.Reason != ReasonFailed {
		t.Errorf("the outcome is %q, want %q", outcome.Reason, ReasonFailed)
	}
	// THE FAILURE IS COUNTED.
	if report.Failed != 1 {
		t.Errorf("the report failed %d assignments, want 1", report.Failed)
	}
	if report.Completed != 0 || report.Ambiguous != 0 {
		t.Errorf("the report counts a completion or an ambiguity: %+v", report)
	}
}

// TestAnAssignmentWithTwoOutstandingAttemptsIsReportedOnce proves the
// report says one thing about an assignment.
//
// PR 3's reconcile creates a NEW attempt when the epoch advances, and
// the older one stays outstanding. So an assignment can carry two
// outstanding attempts — and the recovery pass inspected both. Before
// the fix it emitted TWO outcomes for the same assignment: one for the
// old session, which may still be working, and one for the new one.
//
// EVERY attempt is still inspected — an older session may still be
// working, and that is a blocker — but the assignment is reported ONCE.
func TestAnAssignmentWithTwoOutstandingAttemptsIsReportedOnce(t *testing.T) {
	store, clock, projectID := clockStore(t)
	root := dispatchRoot(t, store, projectID)
	child := dispatchChild(t, store, root, "alpha", assignment.RoleImplementer, "idem:rec:twice")

	// The first attempt, at epoch 1.
	first, err := store.Claim(assignment.ClaimParams{
		ID: child.ID, ExpectedEpoch: 0, Owner: "mpm-dispatcher", LeaseSeconds: 1800,
		Actor: assignment.Actor{Address: "mpm-dispatcher", Kind: "agent"},
	})
	if err != nil {
		t.Fatalf("the first claim: %v", err)
	}
	seedAttempt(t, store, child, first.ClaimEpoch, "ses_older", assignment.SessionRunning)

	// The lease expires and a second attempt is made, at epoch 2.
	clock.Advance(1900 * time.Second)
	second, err := store.Claim(assignment.ClaimParams{
		ID: child.ID, ExpectedEpoch: first.ClaimEpoch, Owner: "mpm-dispatcher",
		LeaseSeconds: 1800,
		Actor:        assignment.Actor{Address: "mpm-dispatcher", Kind: "agent"},
	})
	if err != nil {
		t.Fatalf("the second claim: %v", err)
	}
	seedAttempt(t, store, child, second.ClaimEpoch, "ses_newer", assignment.SessionIntent)
	if len(mustAttempts(t, store, child.ID)) != 2 {
		t.Fatalf("the assignment has %d attempts, want 2",
			len(mustAttempts(t, store, child.ID)))
	}

	cfg := testConfig(t)
	engine, fake := clockEngine(t, store, clock, cfg)
	// THE LAUNCH CREATED THE WORKTREE. Seed it before the simulated
	// crash: recovery verifies an existing worktree and never creates
	// one.
	ws := stateAtWorkspace(t, engine, child)

	// The OLD session is still working; the NEW one reported an outcome.
	fake.states["ses_older"] = &oc.SessionState{ID: "ses_older", Outcome: "", Agent: "mpm-worker"}
	fake.states["ses_newer"] = &oc.SessionState{
		ID: "ses_newer", Outcome: "succeeded", Agent: "mpm-worker",
		Model:    oc.SessionModel{ID: "test", ProviderID: "mpm-sidecar"},
		Location: oc.SessionLocation{Directory: ws},
	}

	report, err := engine.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	// ONE OUTCOME FOR THE ASSIGNMENT.
	seen := 0
	for _, o := range report.Outcomes {
		if o.AssignmentID == child.ID {
			seen++
		}
	}
	if seen != 1 {
		t.Errorf("the report says %d things about the assignment, want 1: %+v",
			seen, report.Outcomes)
	}
	// AND THE OLD SESSION STILL BLOCKS. The blocker is the safe answer:
	// the older session may still be working.
	outcome, ok := outcomeFor(report, child.ID)
	if !ok {
		t.Fatalf("the report has no outcome: %+v", report.Outcomes)
	}
	if outcome.Reason != ReasonSessionStillRunning &&
		outcome.Reason != ReasonRecoveryRequired {
		t.Errorf("the outcome is %q, want the older session to block", outcome.Reason)
	}

	// AND THE NEWER ATTEMPT'S OUTCOME WAS STILL RECORDED.
	//
	// THE BLOCKER MUST NOT COST THE OTHER ATTEMPT ITS RECORD. The
	// reconciliation IS the write that records a runtime's outcome, so
	// stopping at the first blocker would lose it — and a write that
	// never happens is work lost, not a report tidied up.
	got, err := store.Get(child.ID)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.State != assignment.StateCompleted {
		t.Errorf("the assignment is %q, want %q; the newer attempt's outcome was not "+
			"reconciled because the older attempt blocked", got.State, assignment.StateCompleted)
	}
	attempts := mustAttempts(t, store, child.ID)
	for _, a := range attempts {
		if a.ClaimEpoch != got.ClaimEpoch {
			// The OLDER attempt stays outstanding: its epoch is fenced
			// out, and its session may still be working. It is a
			// blocker, not something to force to a terminal state.
			continue
		}
		if a.LaunchState == assignment.SessionRunning ||
			a.LaunchState == assignment.SessionIntent {
			t.Errorf("the attempt %s at the current epoch is %q, want a terminal state",
				a.ID, a.LaunchState)
		}
	}
}

// TestACancelledDrainSaysTheRunWasCancelled proves the cancellation is
// not mistaken for a runtime that went away.
//
// The two are different facts, and the cancellation is checked FIRST: an
// inspection that fails because the context was cancelled is not a
// runtime that vanished, and telling the operator the runtime went away
// when the operator stopped the run would be a lie.
func TestACancelledDrainSaysTheRunWasCancelled(t *testing.T) {
	store, clock, projectID := clockStore(t)
	root := dispatchRoot(t, store, projectID)
	child := dispatchChild(t, store, root, "alpha", assignment.RoleImplementer, "idem:rec:cancelled")

	claimed, err := store.Claim(assignment.ClaimParams{
		ID: child.ID, ExpectedEpoch: 0, Owner: "mpm-dispatcher", LeaseSeconds: 1800,
		Actor: assignment.Actor{Address: "mpm-dispatcher", Kind: "agent"},
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	seedAttempt(t, store, child, claimed.ClaimEpoch, "ses_cancelled", assignment.SessionRunning)

	cfg := testConfig(t)
	// A drain WINDOW, so the drain reaches its wait. Without one it
	// short-circuits with its own message, and the cancellation would
	// never be reached.
	cfg.DrainTimeoutSeconds = 5
	cfg.DrainPollIntervalSeconds = 1
	engine, fake := clockEngine(t, store, clock, cfg)
	// The session has NOT reported an outcome, so the drain runs.
	fake.states["ses_cancelled"] = &oc.SessionState{ID: "ses_cancelled", Outcome: ""}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	report, err := engine.RunOnce(ctx)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	outcome, ok := outcomeFor(report, child.ID)
	if !ok {
		t.Fatalf("the report has no outcome: %+v", report.Outcomes)
	}
	if !strings.Contains(outcome.Detail, "cancel") {
		t.Errorf("the detail is %q, want the cancellation to be named", outcome.Detail)
	}
	if strings.Contains(outcome.Detail, "went away") ||
		strings.Contains(outcome.Detail, "could not be asked") {
		t.Errorf("the detail blames the runtime: %q", outcome.Detail)
	}
}

// TestTheRecoveryPassNeverLaunches proves the recovery pass cannot
// START work.
//
// # THE SEQUENCE THIS REPRODUCES
//
// An assignment with TWO outstanding attempts: an older one at epoch 1
// whose session may still be working, and a newer one at epoch 2 whose
// session reported "failed". The older session reaches a terminal
// outcome INSIDE the drain window, so the drain reconciles — and PR 3's
// adapter acts on the NEWEST attempt, which records "failed". The next
// iteration then observes the newer attempt's terminal outcome and
// reconciles it AGAIN — and the adapter, seeing a terminal attempt,
// would mint a NEW attempt and LAUNCH it.
//
// A recovery pass that launches is a dispatcher that dispatches from
// the recovery path: a second writer into a workspace whose earlier
// session may still be working. That is the one thing this package
// exists to prevent.
//
// So the recovery pass asks the adapter to RECORD ONLY, and the adapter
// REFUSES to resume or to mint an attempt when it cannot record.
func TestTheRecoveryPassNeverLaunches(t *testing.T) {
	store, clock, projectID := clockStore(t)
	root := dispatchRoot(t, store, projectID)
	child := dispatchChild(t, store, root, "alpha", assignment.RoleImplementer, "idem:rec:launch")

	// The first attempt, at epoch 1.
	first, err := store.Claim(assignment.ClaimParams{
		ID: child.ID, ExpectedEpoch: 0, Owner: "mpm-dispatcher", LeaseSeconds: 1800,
		Actor: assignment.Actor{Address: "mpm-dispatcher", Kind: "agent"},
	})
	if err != nil {
		t.Fatalf("the first claim: %v", err)
	}
	seedAttempt(t, store, child, first.ClaimEpoch, "ses_older", assignment.SessionRunning)

	// The lease expires and a second attempt is made, at epoch 2.
	clock.Advance(1900 * time.Second)
	second, err := store.Claim(assignment.ClaimParams{
		ID: child.ID, ExpectedEpoch: first.ClaimEpoch, Owner: "mpm-dispatcher",
		LeaseSeconds: 1800,
		Actor:        assignment.Actor{Address: "mpm-dispatcher", Kind: "agent"},
	})
	if err != nil {
		t.Fatalf("the second claim: %v", err)
	}
	seedAttempt(t, store, child, second.ClaimEpoch, "ses_newer", assignment.SessionIntent)

	cfg := testConfig(t)
	// A DRAIN WINDOW, so the drain waits and can see the older session
	// report an outcome.
	cfg.DrainTimeoutSeconds = 5
	cfg.DrainPollIntervalSeconds = 1
	engine, fake := clockEngine(t, store, clock, cfg)
	// THE LAUNCH CREATED THE WORKTREE. Seed it so the test exercises
	// the session/drain path it names, not the missing-worktree refusal.
	ws := stateAtWorkspace(t, engine, child)

	// The older session reports an outcome only AFTER the first
	// inspection — which is what the drain waits for.
	seen := 0
	fake.inspectHook = func(sessionID string) *oc.SessionState {
		if sessionID == "ses_older" {
			seen++
			if seen == 1 {
				return &oc.SessionState{ID: "ses_older", Outcome: "", Agent: "mpm-worker"}
			}
			return &oc.SessionState{
				ID: "ses_older", Outcome: "succeeded", Agent: "mpm-worker",
				Model:    oc.SessionModel{ID: "test", ProviderID: "mpm-sidecar"},
				Location: oc.SessionLocation{Directory: ws},
			}
		}
		if sessionID == "ses_newer" {
			return &oc.SessionState{
				ID: "ses_newer", Outcome: "failed", Agent: "mpm-worker",
				Model:    oc.SessionModel{ID: "test", ProviderID: "mpm-sidecar"},
				Location: oc.SessionLocation{Directory: ws},
			}
		}
		return nil
	}
	fake.launchState = okState("mpm-worker", "succeeded")

	report, err := engine.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	// NOTHING WAS LAUNCHED.
	if fake.Launches() != 0 {
		t.Errorf("the runtime was launched %d times; the recovery pass must never launch",
			fake.Launches())
	}
	// AND NO NEW ATTEMPT WAS MINTED.
	attempts := mustAttempts(t, store, child.ID)
	if len(attempts) != 2 {
		t.Errorf("the assignment has %d attempts, want 2; the recovery pass minted one",
			len(attempts))
	}
	// AND THE REPORT SAYS WHAT HAPPENED.
	outcome, ok := outcomeFor(report, child.ID)
	if !ok {
		t.Fatalf("the report has no outcome: %+v", report.Outcomes)
	}
	if outcome.Reason != ReasonFailed && outcome.Reason != ReasonSessionStillRunning {
		t.Errorf("the outcome is %q, want the observed outcome", outcome.Reason)
	}
	// THE DRAIN RAN. The outer pass inspects once; a second inspection
	// can only come from the drain.
	if seen < 2 {
		t.Errorf("the older session was inspected %d time(s); the drain did not run", seen)
	}
}

// TestACancelledRunWritesNoNoteInAnyBranch proves the promise holds in
// the branch the DEFAULT configuration takes.
//
// The drain window ships DISABLED, so the "no drain window configured"
// branch is the one a cancelled run is most likely to reach — and it is
// the one that had no guard.
//
// A note that says "recovery required" for a deliberate stop would train
// the operator to ignore the notes that matter.
func TestACancelledRunWritesNoNoteInAnyBranch(t *testing.T) {
	store, clock, projectID := clockStore(t)
	root := dispatchRoot(t, store, projectID)
	child := dispatchChild(t, store, root, "alpha", assignment.RoleImplementer, "idem:rec:nonote")

	claimed, err := store.Claim(assignment.ClaimParams{
		ID: child.ID, ExpectedEpoch: 0, Owner: "mpm-dispatcher", LeaseSeconds: 1800,
		Actor: assignment.Actor{Address: "mpm-dispatcher", Kind: "agent"},
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	seedAttempt(t, store, child, claimed.ClaimEpoch, "ses_nonote", assignment.SessionRunning)

	cfg := testConfig(t)
	// The DEFAULT drain: no window configured. This is the branch the
	// default configuration takes.
	if cfg.DrainTimeoutSeconds != 0 {
		t.Fatalf("the test configuration has a drain window (%d); this test is about the "+
			"branch that has none", cfg.DrainTimeoutSeconds)
	}
	engine, fake := clockEngine(t, store, clock, cfg)
	// THE LAUNCH CREATED THE WORKTREE. Seed it before the simulated
	// crash: recovery verifies an existing worktree and never creates
	// one.
	stateAtWorkspace(t, engine, child)

	ctx, cancel := context.WithCancel(context.Background())
	// The operator stops the run at the moment the session is inspected.
	fake.inspectHook = func(sessionID string) *oc.SessionState {
		cancel()
		return &oc.SessionState{ID: "ses_nonote", Outcome: "", Agent: "mpm-worker"}
	}

	if _, err := engine.RunOnce(ctx); err != nil {
		t.Fatalf("run: %v", err)
	}

	// NO NOTE WAS WRITTEN.
	history, err := store.History(child.ID)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	for _, ev := range history {
		if ev.EventType == assignment.EventNote {
			t.Errorf("the cancelled run wrote a note: %q", ev.Note)
		}
	}
}

// ── AN OLDER-EPOCH SESSION BLOCKS THE WORKSPACE ─────────────────────────

// TestARunningOlderEpochSessionBlocksTheWorkspace is requirement 7.
//
// The lease expired and the assignment was reclaimed, so the dispatcher now
// holds a NEWER epoch — but the OLD session may still be working. A second
// writer into the same worktree is exactly the duplicate the attempt
// record exists to prevent, so it must be refused.
func TestARunningOlderEpochSessionBlocksTheWorkspace(t *testing.T) {
	store, clock, projectID := clockStore(t)
	root := dispatchRoot(t, store, projectID)
	child := dispatchChild(t, store, root, "alpha", assignment.RoleImplementer, "idem:rec:oldepoch")

	// The first holder claims and starts a session.
	claimed, err := store.Claim(assignment.ClaimParams{
		ID: child.ID, ExpectedEpoch: 0, Owner: "mpm-dispatcher", LeaseSeconds: 1800,
		Actor: assignment.Actor{Address: "mpm-dispatcher", Kind: "agent"},
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	seedAttempt(t, store, child, claimed.ClaimEpoch, "ses_old", assignment.SessionRunning)

	// The lease expires. The dispatcher does the reclaiming itself: the
	// assignment is not pre-claimed here, because a live lease is never
	// taken over, and the fixture must not hand the dispatcher a claim it
	// could not have taken. So the ONLY thing that can stop the dispatch
	// below is the recovery pass finding the old session still running.
	clock.Advance(1900 * time.Second)

	cfg := testConfig(t)
	engine, fake := clockEngine(t, store, clock, cfg)
	// THE LAUNCH CREATED THE WORKTREE. Seed it so the test exercises
	// the session/drain path it names, not the missing-worktree refusal.
	stateAtWorkspace(t, engine, child)

	// The OLD session is still working.
	fake.states["ses_old"] = &oc.SessionState{ID: "ses_old", Outcome: "", Agent: "mpm-worker"}
	fake.launchState = okState("mpm-worker", "succeeded")

	report, err := engine.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if fake.Launches() != 0 {
		t.Errorf("the runtime was launched %d times while an older-epoch session was "+
			"still working; that is a second writer into the same workspace",
			fake.Launches())
	}
	sawBlock := false
	for _, o := range report.Outcomes {
		if o.AssignmentID == child.ID &&
			(o.Reason == ReasonSessionStillRunning || o.Reason == ReasonRecoveryRequired) {
			// THE REASON IS THE SESSION, NOT A MISSING WORKTREE. Both
			// outcomes share `recovery_required`; the detail is what
			// separates them, and the two reasons have different text.
			wantDetail := "still running"
			if o.Reason == ReasonSessionStillRunning {
				wantDetail = "has not reported an outcome"
			}
			if !strings.Contains(o.Detail, wantDetail) {
				t.Errorf("the outcome %q does not name the running session: %q", o.Reason, o.Detail)
			}
			sawBlock = true
		}
	}
	if !sawBlock {
		t.Errorf("the report does not say the older session blocks: %+v", report.Outcomes)
	}
}

// TestAnOlderEpochAttemptWhoseSessionIsGoneDoesNotBlock proves the
// recovery pass asks the RUNTIME rather than doing epoch arithmetic.
//
// An expired lease is NOT evidence that the old process stopped, so the
// dispatcher asks the runtime what became of the session. When the
// runtime has no record of it, the attempt is HISTORY: it blocks
// nothing, and the assignment is dispatched.
//
// Without this, every reclaimed assignment would be blocked forever by
// an attempt whose session the runtime had long since forgotten.
func TestAnOlderEpochAttemptWhoseSessionIsGoneDoesNotBlock(t *testing.T) {
	store, clock, projectID := clockStore(t)
	root := dispatchRoot(t, store, projectID)
	child := dispatchChild(t, store, root, "alpha", assignment.RoleImplementer, "idem:rec:gone")

	claimed, err := store.Claim(assignment.ClaimParams{
		ID: child.ID, ExpectedEpoch: 0, Owner: "mpm-dispatcher", LeaseSeconds: 1800,
		Actor: assignment.Actor{Address: "mpm-dispatcher", Kind: "agent"},
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	seedAttempt(t, store, child, claimed.ClaimEpoch, "ses_gone", assignment.SessionRunning)

	// The lease expires. The dispatcher itself does the reclaiming — the
	// assignment is not pre-claimed here, because a live lease is never
	// taken over and the fixture must not hand the dispatcher a claim it
	// could not have taken.
	clock.Advance(1900 * time.Second)

	cfg := testConfig(t)
	engine, fake := clockEngine(t, store, clock, cfg)
	// THE LAUNCH CREATED THE WORKTREE. Seed it before the simulated
	// crash: recovery verifies an existing worktree and never creates
	// one.
	stateAtWorkspace(t, engine, child)

	// The runtime has NO record of the old session: it is gone.
	fake.launchState = okState("mpm-worker", "succeeded")

	report, err := engine.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if fake.Launches() != 1 {
		t.Fatalf("the runtime was launched %d times, want 1; an attempt whose "+
			"session is gone must not block the work forever", fake.Launches())
	}
	// AND THE REPORT SAYS WHY IT WAS NOT A BLOCKER.
	outcome, ok := outcomeFor(report, child.ID)
	if !ok {
		t.Fatalf("the report has no outcome: %+v", report.Outcomes)
	}
	if outcome.Reason != ReasonDeferred && outcome.Reason != ReasonCompleted {
		t.Errorf("the outcome is %q, want the old attempt to be history", outcome.Reason)
	}
}

// TestAnUnreachableRuntimeBlocksTheOlderEpochAttempt proves the
// UNKNOWN case blocks.
//
// When the runtime cannot be asked, nobody can say whether the old
// session is still working. A second writer cannot be ruled out, so the
// dispatcher refuses rather than guessing.
func TestAnUnreachableRuntimeBlocksTheOlderEpochAttempt(t *testing.T) {
	store, clock, projectID := clockStore(t)
	root := dispatchRoot(t, store, projectID)
	child := dispatchChild(t, store, root, "alpha", assignment.RoleImplementer, "idem:rec:unreachable")

	claimed, err := store.Claim(assignment.ClaimParams{
		ID: child.ID, ExpectedEpoch: 0, Owner: "mpm-dispatcher", LeaseSeconds: 1800,
		Actor: assignment.Actor{Address: "mpm-dispatcher", Kind: "agent"},
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	seedAttempt(t, store, child, claimed.ClaimEpoch, "ses_unreachable", assignment.SessionRunning)

	// The lease expires, so the dispatcher CAN claim — and then the
	// recovery pass asks about the old session and cannot get an answer.
	clock.Advance(1900 * time.Second)

	cfg := testConfig(t)
	engine, fake := clockEngine(t, store, clock, cfg)
	// THE LAUNCH CREATED THE WORKTREE. Seed it so the test exercises
	// the session/drain path it names, not the missing-worktree refusal.
	stateAtWorkspace(t, engine, child)

	// The runtime cannot be asked.
	fake.inspectErr = errors.New("the runtime is unreachable")
	fake.launchState = okState("mpm-worker", "succeeded")

	report, err := engine.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if fake.Launches() != 0 {
		t.Errorf("the runtime was launched %d times although the older session "+
			"could not be inspected", fake.Launches())
	}
	sawBlock := false
	for _, o := range report.Outcomes {
		if o.AssignmentID == child.ID &&
			(o.Reason == ReasonRuntimeUnavailable) {
			sawBlock = true
		}
	}
	if !sawBlock {
		t.Errorf("the report does not say the unknown session blocks: %+v", report.Outcomes)
	}
}

// ── A STALE EPOCH CANNOT PUBLISH A COMPLETION ───────────────────────────

// TestAStaleEpochCannotPublishACompletion is requirement 6.
//
// The dispatcher holds an epoch. If the claim moves under it, its
// completion must be refused — the completion carries the epoch it was
// written under, and a stale one matches no row.
func TestAStaleEpochCannotPublishACompletion(t *testing.T) {
	store, clock, projectID := clockStore(t)
	root := dispatchRoot(t, store, projectID)
	child := dispatchChild(t, store, root, "alpha", assignment.RoleImplementer, "idem:rec:stale")

	claimed, err := store.Claim(assignment.ClaimParams{
		ID: child.ID, ExpectedEpoch: 0, Owner: "mpm-dispatcher", LeaseSeconds: 1800,
		Actor: assignment.Actor{Address: "mpm-dispatcher", Kind: "agent"},
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	// The claim moves.
	clock.Advance(1900 * time.Second)
	if _, err := store.Claim(assignment.ClaimParams{
		ID: child.ID, ExpectedEpoch: claimed.ClaimEpoch, Owner: "mpm-dispatcher",
		LeaseSeconds: 900, Actor: assignment.Actor{Address: "mpm-dispatcher", Kind: "agent"},
	}); err != nil {
		t.Fatalf("reclaim: %v", err)
	}

	// The OLD holder publishes a completion.
	_, err = store.Complete(assignment.FencedParams{
		ID: child.ID, Epoch: claimed.ClaimEpoch,
		Actor: assignment.Actor{Address: "alpha", Kind: "agent"},
	})
	if err == nil {
		t.Fatal("a stale epoch published a completion")
	}
	if !errors.Is(err, assignment.ErrStaleClaim) {
		t.Errorf("the completion was refused with %v, want a stale claim", err)
	}
	got, err := store.Get(child.ID)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.State == assignment.StateCompleted {
		t.Error("the assignment completed under a stale epoch")
	}
}

// ── THE DISPATCHER TOUCHES NOTHING ELSE ─────────────────────────────────

// TestTheDispatcherModifiesNothingOutsideTheAssignmentState is
// requirement 12.
//
// The dispatcher reads and writes the assignment state and nothing else.
// It creates no file outside its configured workspace root, and it does
// not touch the reviewer's repository.
func TestTheDispatcherModifiesNothingOutsideTheAssignmentState(t *testing.T) {
	store, clock, projectID := clockStore(t)
	root := dispatchRoot(t, store, projectID)
	dispatchChild(t, store, root, "alpha", assignment.RoleImplementer, "idem:rec:scope")

	cfg := testConfig(t)
	engine, fake := clockEngine(t, store, clock, cfg)
	fake.launchState = okState("mpm-worker", "succeeded")

	reviewRepo := cfg.ReviewRepoPath
	before, err := os.ReadDir(reviewRepo)
	if err != nil {
		t.Fatalf("read the review repository: %v", err)
	}

	if _, err := engine.RunOnce(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	after, err := os.ReadDir(reviewRepo)
	if err != nil {
		t.Fatalf("read the review repository: %v", err)
	}
	if len(before) != len(after) {
		t.Errorf("the review repository changed: %d entries -> %d", len(before), len(after))
	}
	// The workspace root holds exactly the one worktree it was told to
	// create, and nothing else.
	entries, err := os.ReadDir(cfg.WorkspaceRoot)
	if err != nil {
		t.Fatalf("read the workspace root: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("the workspace root holds %d entries, want 1", len(entries))
	}
}

// testConfigRootFromEngine is the configured workspace root of an engine.
func testConfigRootFromEngine(e *Engine) string {
	return e.cfg.WorkspaceRoot
}

// stateAtWorkspace records the worktree the launch would have created and
// returns its host path, which is what a native runtime reports as the
// session's directory.
func stateAtWorkspace(t *testing.T, engine *Engine, a *assignment.Assignment) string {
	t.Helper()
	ws, err := engine.workspaceFor(a)
	if err != nil {
		t.Fatalf("record the worktree the launch created: %v", err)
	}
	return ws
}
