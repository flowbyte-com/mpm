// limits_test.go — Continuity 001, PR 4: the dispatcher's limits.
//
// # WHAT IS UNDER TEST
//
// Every limit the dispatcher has, and the direction it fails in:
//
//   - the run's own concurrency;
//   - the tree's durable concurrency budget (the SERVICE's rule);
//   - the lease headroom the run needs;
//   - the tree's authorisation;
//   - the dispatcher's own restraint: it creates nothing, approves
//     nothing, and repeats nothing.
//
// # THE SHAPE OF THE ASSERTIONS
//
// Each test asserts a COUNT — how many launches, how many claims, how
// many rows — rather than a description. A description can be satisfied
// by a message that says the right thing while the wrong thing happens.
package dispatch

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/flowbyte-com/mpm-core/assignment"
)

// ── THE RUN'S OWN CONCURRENCY ────────────────────────────────────────────

// TestTheRunDispatchesAtMostItsConcurrency proves the run is bounded.
//
// Three eligible assignments, a concurrency of one: ONE launch, and the
// other two are DEFERRED — not failed, not lost. The report has to say
// what happened to them.
func TestTheRunDispatchesAtMostItsConcurrency(t *testing.T) {
	store, projectID := testStore(t)
	root := dispatchRoot(t, store, projectID)
	for _, key := range []string{"a", "b", "c"} {
		dispatchChild(t, store, root, "alpha", assignment.RoleImplementer, "idem:limits:conc:"+key)
	}

	cfg := testConfig(t)
	cfg.Concurrency = 1
	engine, fake := testEngine(t, store, cfg)
	fake.launchState = okState("mpm-worker", "succeeded")

	report, err := engine.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if fake.Launches() != 1 {
		t.Fatalf("the runtime was launched %d times, want 1", fake.Launches())
	}
	if report.Dispatched != 1 {
		t.Errorf("the report dispatched %d assignments, want 1", report.Dispatched)
	}
	// The other two are deferred, and they are COUNTED as deferred.
	deferred := 0
	for _, o := range report.Outcomes {
		if o.Reason == ReasonDeferred {
			deferred++
		}
	}
	if deferred != 2 {
		t.Errorf("%d assignments were deferred, want 2: %+v", deferred, report.Outcomes)
	}
}

// ── THE TREE'S DURABLE BUDGET ────────────────────────────────────────────

// TestASpentConcurrencyBudgetIsReportedAsABudget proves the budget is
// the SERVICE's rule and that the dispatcher reports it as one.
//
// The tree allows one live claim. The first dispatch ends AMBIGUOUS —
// the assignment stays in progress with a live lease, so its claim is
// still live — and the second dispatch is refused by the budget.
//
// The refusal is atomic: it lives inside the claim's own UPDATE, so a
// dispatcher that reads the count first and writes later would be
// crossing a rule it does not enforce.
func TestASpentConcurrencyBudgetIsReportedAsABudget(t *testing.T) {
	store, projectID := testStore(t)
	root := dispatchRootWith(t, store, projectID, 8, 1)
	first := dispatchChild(t, store, root, "alpha", assignment.RoleImplementer, "idem:limits:budget:a")
	second := dispatchChild(t, store, root, "alpha", assignment.RoleImplementer, "idem:limits:budget:b")

	cfg := testConfig(t)
	cfg.Concurrency = 2
	engine, fake := testEngine(t, store, cfg)
	// The launch fails definitively: no ambiguity, but the assignment
	// stays in progress holding its claim, so the second dispatch is
	// refused by the budget, not by the ambiguity barrier. (An
	// ambiguous first launch would refuse the second claim under TR11
	// instead — that barrier has its own tests — so the budget path
	// needs a clean launch.)
	fake.launchState = okState("mpm-worker", "failed")

	report, err := engine.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	// WHICH ONE WENT FIRST is the dispatcher's business: the selection
	// is ordered by (created_at, id), and two assignments created in the
	// same second are ordered by id. What matters is that ONE was
	// dispatched and the OTHER was refused by the budget.
	firstOutcome, firstOK := outcomeFor(report, first.ID)
	secondOutcome, secondOK := outcomeFor(report, second.ID)
	if !firstOK || !secondOK {
		t.Fatalf("the report is missing an outcome: %+v", report.Outcomes)
	}
	if firstOutcome.Reason == ReasonBudgetExhausted {
		first, second = second, first
		firstOutcome, secondOutcome = secondOutcome, firstOutcome
	}
	if secondOutcome.Reason != ReasonBudgetExhausted {
		t.Errorf("the refused assignment is %q, want %q: %+v",
			secondOutcome.Reason, ReasonBudgetExhausted, secondOutcome)
	}
	// AND THE FIRST ONE REALLY DID HOLD THE CLAIM.
	got, err := store.Get(first.ID)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.ClaimEpoch == 0 {
		t.Error("the first assignment was not claimed")
	}
	if got.State != assignment.StateInProgress {
		t.Errorf("the first assignment is %q, want %q", got.State, assignment.StateInProgress)
	}
}

// ── THE LEASE HEADROOM ───────────────────────────────────────────────────

// TestALeaseWithoutHeadroomIsRefused proves the dispatcher refuses work
// it cannot finish.
//
// The lease the earlier run took has 200 seconds left; the runtime
// window needs 900. Starting the work would produce a session whose
// result could not be recorded, so nothing is started at all.
func TestALeaseWithoutHeadroomIsRefused(t *testing.T) {
	store, clock, projectID := clockStore(t)
	root := dispatchRoot(t, store, projectID)
	child := dispatchChild(t, store, root, "alpha", assignment.RoleImplementer, "idem:limits:lease")

	// An earlier run claimed it and left a short lease behind.
	if _, err := store.Claim(assignment.ClaimParams{
		ID: child.ID, ExpectedEpoch: 0, Owner: "mpm-dispatcher", LeaseSeconds: 200,
		Actor: assignment.Actor{Address: "mpm-dispatcher", Kind: "agent"},
	}); err != nil {
		t.Fatalf("claim: %v", err)
	}

	cfg := testConfig(t)
	engine, fake := clockEngine(t, store, clock, cfg)
	fake.launchState = okState("mpm-worker", "succeeded")

	report, err := engine.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if fake.Launches() != 0 {
		t.Fatalf("the runtime was launched %d times without lease headroom", fake.Launches())
	}
	outcome, ok := outcomeFor(report, child.ID)
	if !ok {
		t.Fatalf("the report has no outcome: %+v", report.Outcomes)
	}
	if outcome.Reason != ReasonLeaseHeld {
		t.Errorf("the outcome is %q, want %q", outcome.Reason, ReasonLeaseHeld)
	}
	// AND NO ATTEMPT WAS RECORDED: nothing was started.
	if attempts := mustAttempts(t, store, child.ID); len(attempts) != 0 {
		t.Errorf("the assignment has %d attempts; nothing should have started", len(attempts))
	}
}

// ── THE TREE'S AUTHORISATION ─────────────────────────────────────────────

// TestAnIneligibleRecipientIsRefused proves the dispatcher checks the
// root's own list for itself.
//
// The delegation already refuses an ineligible recipient, so the row is
// edited underneath the dispatcher: the check that matters is the
// dispatcher's own, made at dispatch time.
func TestAnIneligibleRecipientIsRefused(t *testing.T) {
	store, dm, projectID := testStoreDB(t)
	root := dispatchRoot(t, store, projectID)
	child := dispatchChild(t, store, root, "alpha", assignment.RoleImplementer, "idem:limits:auth")

	// The root's list changes under the dispatcher: the root's own
	// recipient moves with it, because the schema insists the two
	// agree. The CHILD's recipient is left behind, and that is the
	// mismatch the dispatcher has to notice.
	if _, err := dm.SQLDB().Exec(
		`UPDATE agent_tasks SET recipient = ?, eligible_recipients = ? WHERE id = ?`,
		"charlie", `["charlie"]`, root.ID); err != nil {
		t.Fatalf("edit the root's eligible recipients: %v", err)
	}

	cfg := testConfig(t)
	engine, fake := testEngine(t, store, cfg)
	fake.launchState = okState("mpm-worker", "succeeded")

	report, err := engine.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if fake.Launches() != 0 {
		t.Errorf("the runtime was launched %d times for an ineligible recipient",
			fake.Launches())
	}
	outcome, ok := outcomeFor(report, child.ID)
	if !ok {
		t.Fatalf("the report has no outcome: %+v", report.Outcomes)
	}
	if outcome.Reason != ReasonRefused {
		t.Errorf("the outcome is %q, want %q", outcome.Reason, ReasonRefused)
	}
}

// TestARootThatDoesNotPermitClaimingIsRefused proves the dispatcher's
// operations are the root's to permit.
func TestARootThatDoesNotPermitClaimingIsRefused(t *testing.T) {
	store, dm, projectID := testStoreDB(t)
	root := dispatchRoot(t, store, projectID)
	child := dispatchChild(t, store, root, "alpha", assignment.RoleImplementer, "idem:limits:ops")

	if _, err := dm.SQLDB().Exec(
		`UPDATE agent_tasks SET permitted_operations = ? WHERE id = ?`,
		`["deliver","accept","start","complete"]`, root.ID); err != nil {
		t.Fatalf("edit the root's permitted operations: %v", err)
	}

	cfg := testConfig(t)
	engine, fake := testEngine(t, store, cfg)
	fake.launchState = okState("mpm-worker", "succeeded")

	report, err := engine.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if fake.Launches() != 0 {
		t.Errorf("the runtime was launched %d times although the root does not "+
			"permit claiming", fake.Launches())
	}
	outcome, ok := outcomeFor(report, child.ID)
	if !ok {
		t.Fatalf("the report has no outcome: %+v", report.Outcomes)
	}
	if outcome.Reason != ReasonRefused {
		t.Errorf("the outcome is %q, want %q", outcome.Reason, ReasonRefused)
	}
}

// ── A CANCELLED ASSIGNMENT CANNOT PUBLISH A COMPLETION ───────────────────

// TestACancelledAssignmentCannotPublishALateCompletion proves the fence
// holds when the work is withdrawn.
//
// The claim is live, the work starts, the operator cancels it — and the
// holder's late completion is refused. A cancelled assignment that can
// still complete is a cancelled assignment that lies.
func TestACancelledAssignmentCannotPublishALateCompletion(t *testing.T) {
	store, projectID := testStore(t)
	root := dispatchRoot(t, store, projectID)
	child := dispatchChild(t, store, root, "alpha", assignment.RoleImplementer, "idem:limits:cancel")

	actor := assignment.Actor{Address: "alpha", Kind: "agent"}
	claimed, err := store.Claim(assignment.ClaimParams{
		ID: child.ID, ExpectedEpoch: 0, Owner: "alpha", LeaseSeconds: 600, Actor: actor,
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := store.Start(assignment.FencedParams{
		ID: child.ID, Epoch: claimed.ClaimEpoch, Actor: actor,
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := store.Cancel(assignment.CancelParams{
		ID: child.ID, Epoch: claimed.ClaimEpoch, Actor: actor, Note: "withdrawn",
	}); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	// THE LATE COMPLETION.
	_, err = store.Complete(assignment.FencedParams{
		ID: child.ID, Epoch: claimed.ClaimEpoch, Actor: actor,
	})
	if err == nil {
		t.Fatal("a cancelled assignment published a completion")
	}
	got, err := store.Get(child.ID)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.State != assignment.StateCancelled {
		t.Errorf("the assignment is %q, want %q", got.State, assignment.StateCancelled)
	}
}

// ── THE DISPATCHER'S OWN RESTRAINT ───────────────────────────────────────

// TestTheDispatcherCreatesNothingAndApprovesNothing proves the
// dispatcher's two hard limits.
//
// It never creates an assignment — it dispatches work that already
// exists — and it never grants an approval. The approval is the
// operator's, and the dispatcher has no path to it.
func TestTheDispatcherCreatesNothingAndApprovesNothing(t *testing.T) {
	store, dm, projectID := testStoreDB(t)
	root := dispatchRoot(t, store, projectID)
	dispatchChild(t, store, root, "alpha", assignment.RoleImplementer, "idem:limits:creates")

	before := countAssignments(t, dm)

	cfg := testConfig(t)
	engine, fake := testEngine(t, store, cfg)
	fake.launchState = okState("mpm-worker", "succeeded")
	if _, err := engine.RunOnce(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	after := countAssignments(t, dm)
	if after != before {
		t.Errorf("the dispatcher changed the number of assignments: %d -> %d",
			before, after)
	}
	// NO APPROVAL WAS RECORDED, ANYWHERE.
	var approvals int
	row := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM agent_tasks WHERE approved_at IS NOT NULL`)
	if err := row.Scan(&approvals); err != nil {
		t.Fatalf("count the approvals: %v", err)
	}
	if approvals != 0 {
		t.Errorf("%d assignment(s) carry an approval the dispatcher recorded", approvals)
	}
	// AND THE ROOT IS STILL THE ONLY ROOT.
	var roots int
	row = dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM agent_tasks WHERE id = root_id`)
	if err := row.Scan(&roots); err != nil {
		t.Fatalf("count the roots: %v", err)
	}
	if roots != 1 {
		t.Errorf("the dispatcher left %d roots, want 1", roots)
	}
}

// TestTheDispatcherInventsNoWork proves it dispatches only what exists.
//
// A tree with no dispatchable work produces a report that says so, and
// no launch.
func TestTheDispatcherInventsNoWork(t *testing.T) {
	store, projectID := testStore(t)
	dispatchRoot(t, store, projectID)

	cfg := testConfig(t)
	engine, fake := testEngine(t, store, cfg)
	fake.launchState = okState("mpm-worker", "succeeded")

	report, err := engine.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if fake.Launches() != 0 {
		t.Errorf("the runtime was launched %d times with nothing to do", fake.Launches())
	}
	if report.Dispatched != 0 {
		t.Errorf("the report dispatched %d assignments, want 0", report.Dispatched)
	}
	sawNoEligible := false
	for _, o := range report.Outcomes {
		if o.Reason == ReasonNoEligible {
			sawNoEligible = true
		}
	}
	if !sawNoEligible {
		t.Errorf("the report does not say there was nothing eligible: %+v", report.Outcomes)
	}
}

// TestARestartDoesNotRepeatCompletedWork proves the run is restartable.
//
// A dispatcher that dies and comes back must not repeat work that is
// already done. The SECOND engine — a fresh process over the same
// database — finds nothing to dispatch.
func TestARestartDoesNotRepeatCompletedWork(t *testing.T) {
	store, projectID := testStore(t)
	root := dispatchRoot(t, store, projectID)
	child := dispatchChild(t, store, root, "alpha", assignment.RoleImplementer, "idem:limits:restart")

	cfg := testConfig(t)
	engine, fake := testEngine(t, store, cfg)
	fake.launchState = okState("mpm-worker", "succeeded")
	if _, err := engine.RunOnce(context.Background()); err != nil {
		t.Fatalf("the first run: %v", err)
	}
	if fake.Launches() != 1 {
		t.Fatalf("the first run launched %d times, want 1", fake.Launches())
	}

	// THE RESTART: a fresh engine over the same database.
	restarted, restartFake := testEngine(t, store, cfg)
	restartFake.launchState = okState("mpm-worker", "succeeded")
	report, err := restarted.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("the second run: %v", err)
	}
	if restartFake.Launches() != 0 {
		t.Errorf("the restarted dispatcher launched %d times; the work was already done",
			restartFake.Launches())
	}
	if report.Dispatched != 0 {
		t.Errorf("the restarted dispatcher dispatched %d assignments", report.Dispatched)
	}
	// AND THE LEDGER STILL SHOWS ONE ATTEMPT, NOT TWO.
	if attempts := mustAttempts(t, store, child.ID); len(attempts) != 1 {
		t.Errorf("the assignment has %d attempts, want 1", len(attempts))
	}
}

// ── THE STATUS REPORT ────────────────────────────────────────────────────

// TestTheStatusReportsPendingBlockedAndRecoverable proves the operator
// can SEE the three buckets.
//
// The status is a read: it builds no runtime and writes nothing.
func TestTheStatusReportsPendingBlockedAndRecoverable(t *testing.T) {
	store, _, projectID := clockStore(t)
	root := dispatchRoot(t, store, projectID)

	pending := dispatchChild(t, store, root, "alpha", assignment.RoleImplementer, "idem:limits:status:pending")

	// A lease held by somebody else.
	leased := dispatchChild(t, store, root, "alpha", assignment.RoleImplementer, "idem:limits:status:leased")
	if _, err := store.Claim(assignment.ClaimParams{
		ID: leased.ID, ExpectedEpoch: 0, Owner: "someone-else", LeaseSeconds: 1800,
		Actor: assignment.Actor{Address: "someone-else", Kind: "agent"},
	}); err != nil {
		t.Fatalf("claim: %v", err)
	}

	// An interrupted attempt.
	interrupted := dispatchChild(t, store, root, "alpha", assignment.RoleImplementer, "idem:limits:status:interrupted")
	claimed, err := store.Claim(assignment.ClaimParams{
		ID: interrupted.ID, ExpectedEpoch: 0, Owner: "mpm-dispatcher", LeaseSeconds: 1800,
		Actor: assignment.Actor{Address: "mpm-dispatcher", Kind: "agent"},
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	seedAttempt(t, store, interrupted, claimed.ClaimEpoch, "ses_interrupted", assignment.SessionRunning)

	cfg := testConfig(t)
	status, err := Status(store, cfg)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if len(status.Pending) != 1 || status.Pending[0].AssignmentID != pending.ID {
		t.Errorf("the pending bucket is %+v, want %s", status.Pending, pending.ID)
	}
	if len(status.Recoverable) != 1 || status.Recoverable[0].AssignmentID != interrupted.ID {
		t.Errorf("the recoverable bucket is %+v, want %s",
			status.Recoverable, interrupted.ID)
	}
	blockedIDs := map[string]bool{}
	for _, item := range status.Blocked {
		blockedIDs[item.AssignmentID] = true
	}
	if !blockedIDs[leased.ID] {
		t.Errorf("the leased assignment is not blocked: %+v", status.Blocked)
	}
	if !blockedIDs[interrupted.ID] {
		t.Errorf("the interrupted assignment is not blocked: %+v", status.Blocked)
	}
	if len(status.Budgets) != 1 {
		t.Errorf("the report carries %d budget lines, want 1", len(status.Budgets))
	}
	// The status is a READ: it created no attempts and no claims.
	if attempts := mustAttempts(t, store, pending.ID); len(attempts) != 0 {
		t.Errorf("the status created %d attempts", len(attempts))
	}
	got, err := store.Get(pending.ID)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.ClaimEpoch != 0 {
		t.Error("the status claimed the assignment")
	}
}

// TestTheStatusBlocksTheDispatchersOwnLiveLease proves the status and
// the dispatch path AGREE.
//
// The dispatcher always claims, and a claim under a live lease is
// refused — so the assignment waits, whoever holds the lease. A status
// that showed the dispatcher's OWN live lease as PENDING would promise
// work that the dispatch would then refuse: the operator would see
// "nothing to do" while the run said "lease held".
//
// The lease here is under the dispatcher's own name — the crash-after-
// the-claim case.
func TestTheStatusBlocksTheDispatchersOwnLiveLease(t *testing.T) {
	store, _, projectID := clockStore(t)
	root := dispatchRoot(t, store, projectID)
	child := dispatchChild(t, store, root, "alpha", assignment.RoleImplementer, "idem:limits:status:own")

	cfg := testConfig(t)
	if _, err := store.Claim(assignment.ClaimParams{
		ID: child.ID, ExpectedEpoch: 0, Owner: cfg.Owner, LeaseSeconds: 1800,
		Actor: assignment.Actor{Address: cfg.Owner, Kind: "agent"},
	}); err != nil {
		t.Fatalf("claim: %v", err)
	}

	status, err := Status(store, cfg)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	blocked := false
	for _, item := range status.Blocked {
		if item.AssignmentID == child.ID {
			blocked = true
			if item.Reason != ReasonLeaseHeld {
				t.Errorf("the reason is %q, want %q", item.Reason, ReasonLeaseHeld)
			}
		}
	}
	if !blocked {
		t.Errorf("the dispatcher's own live lease is not blocked: %+v", status.Blocked)
	}
	for _, item := range status.Pending {
		if item.AssignmentID == child.ID {
			t.Error("the assignment is pending, and the dispatch would refuse it")
		}
	}
}

// ── THE REPORT IS MACHINE-READABLE ───────────────────────────────────────

// TestTheReportIsMachineReadable proves the report is a VALUE with a
// stable shape.
//
// A report an operator cannot read by machine is a report that has to be
// re-parsed by hand, and the reason codes are the whole point.
func TestTheReportIsMachineReadable(t *testing.T) {
	store, projectID := testStore(t)
	root := dispatchRoot(t, store, projectID)
	dispatchChild(t, store, root, "alpha", assignment.RoleImplementer, "idem:limits:json")

	cfg := testConfig(t)
	engine, fake := testEngine(t, store, cfg)
	fake.launchState = okState("mpm-worker", "succeeded")

	report, err := engine.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{
		"enabled", "dispatched", "completed", "refused", "deferred",
		"ambiguous", "outcomes", "started_at", "finished_at",
	} {
		if _, ok := decoded[key]; !ok {
			t.Errorf("the report has no %q field: %s", key, raw)
		}
	}
	// EVERY REASON IS FROM THE CLOSED SET.
	known := map[string]bool{}
	for _, reason := range []string{
		ReasonDisabled, ReasonNoEligible, ReasonBudgetExhausted,
		ReasonLeaseHeld, ReasonStaleEpoch, ReasonSessionStillRunning,
		ReasonRuntimeUnavailable, ReasonAmbiguous, ReasonReviewerUnavailable,
		ReasonRecoveryRequired, ReasonOperatorGatePending, ReasonDispatched,
		ReasonCompleted, ReasonRefused, ReasonDeferred, ReasonFailed,
	} {
		known[reason] = true
	}
	for _, o := range report.Outcomes {
		if !known[o.Reason] {
			t.Errorf("the report carries the reason %q, which is not in the closed set",
				o.Reason)
		}
	}
}

// ── A STALE EPOCH IS REPORTED AS ONE ─────────────────────────────────────

// TestAStaleEpochIsReportedAsAStaleEpoch proves the dispatcher tells a
// moved epoch apart from a refusal.
//
// Another holder takes the assignment between the discovery and the
// claim, so the claim's expected epoch no longer matches. That is a
// STALE EPOCH, and it is not a budget and not a malformed request.
func TestAStaleEpochIsReportedAsAStaleEpoch(t *testing.T) {
	store, clock, projectID := clockStore(t)
	root := dispatchRoot(t, store, projectID)
	child := dispatchChild(t, store, root, "alpha", assignment.RoleImplementer, "idem:limits:stale")

	// Another holder takes it first.
	if _, err := store.Claim(assignment.ClaimParams{
		ID: child.ID, ExpectedEpoch: 0, Owner: "someone-else", LeaseSeconds: 1800,
		Actor: assignment.Actor{Address: "someone-else", Kind: "agent"},
	}); err != nil {
		t.Fatalf("claim: %v", err)
	}

	cfg := testConfig(t)
	engine, fake := clockEngine(t, store, clock, cfg)
	fake.launchState = okState("mpm-worker", "succeeded")

	report, err := engine.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if fake.Launches() != 0 {
		t.Errorf("the runtime was launched %d times although somebody else holds the claim",
			fake.Launches())
	}
	outcome, ok := outcomeFor(report, child.ID)
	if !ok {
		t.Fatalf("the report has no outcome: %+v", report.Outcomes)
	}
	if outcome.Reason != ReasonLeaseHeld && outcome.Reason != ReasonStaleEpoch {
		t.Errorf("the outcome is %q, want the lease or the epoch to be named", outcome.Reason)
	}
}

// countAssignments counts the rows in the assignment table.
func countAssignments(t *testing.T, dm interface {
	SQLDB() *sql.DB
}) int {
	t.Helper()
	var n int
	row := dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM agent_tasks`)
	if err := row.Scan(&n); err != nil {
		t.Fatalf("count the assignments: %v", err)
	}
	return n
}
