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

// TestACrashAfterTheIntentResumesTheRecordedSession: the session id is
// durable and the launch may or may not have happened.
//
// The runtime has no record of the session, so nothing was created, and
// reusing the recorded id cannot duplicate anything. The next run must
// LAUNCH WITH THE RECORDED ID — not mint a new one.
func TestACrashAfterTheIntentResumesTheRecordedSession(t *testing.T) {
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
	fake.launchState = okState("mpm-worker", "succeeded")

	if _, err := engine.RunOnce(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if fake.Launches() != 1 {
		t.Fatalf("the runtime was launched %d times, want 1", fake.Launches())
	}
	// THE RECORDED ID IS THE ONE USED.
	attempts := mustAttempts(t, store, child.ID)
	if len(attempts) != 1 {
		t.Fatalf("the assignment has %d attempts, want 1", len(attempts))
	}
	if attempts[0].NativeSessionID != "ses_recorded_intent" {
		t.Errorf("the launch used the session %q, want the recorded ses_recorded_intent",
			attempts[0].NativeSessionID)
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
	// The session FINISHED, and the adapter never saw it.
	fake.states["ses_ended"] = &oc.SessionState{
		ID: "ses_ended", Outcome: "succeeded", Agent: "mpm-worker",
		Model:    oc.SessionModel{ID: "test", ProviderID: "mpm-sidecar"},
		Location: oc.SessionLocation{Directory: oc.PrefixWorker},
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

	// The lease expires and the assignment is RECLAIMED: a new epoch.
	clock.Advance(1900 * time.Second)
	if _, err := store.Claim(assignment.ClaimParams{
		ID: child.ID, ExpectedEpoch: claimed.ClaimEpoch, Owner: "mpm-dispatcher",
		LeaseSeconds: 900, Actor: assignment.Actor{Address: "mpm-dispatcher", Kind: "agent"},
	}); err != nil {
		t.Fatalf("reclaim: %v", err)
	}

	cfg := testConfig(t)
	engine, fake := clockEngine(t, store, clock, cfg)
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
			sawBlock = true
		}
	}
	if !sawBlock {
		t.Errorf("the report does not say the older session blocks: %+v", report.Outcomes)
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
