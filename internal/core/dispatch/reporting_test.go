// reporting_test.go — Continuity 001, PR 4: what the REPORT says.
//
// # WHAT IS UNDER TEST
//
// The report is the operator's whole view of a run, so a count that means
// the wrong thing is worse than no count at all. These tests pin three
// facts that are easy to get wrong and hard to notice:
//
//   - a FAILED attempt was DISPATCHED: the work reached the adapter and
//     failed there. Nothing refused it, so it is not a refusal.
//
//   - a lease the dispatcher holds ITSELF, too short to record the
//     result, is `lease_insufficient` and not `lease_held` — the second
//     would send an operator looking for another holder who does not
//     exist.
//
//   - a RECOVERED completion is `completed` and not `dispatched`: this
//     run did not launch it, it only recorded what the runtime said.
package dispatch

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	core "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/assignment"
)

// TestAFailedAttemptIsDispatchedAndNotRefused proves the failure is
// counted as a dispatch.
//
// The work WAS handed to the adapter, and it failed there. A refusal is
// the opposite: a rule said no and nothing was launched. Counting a
// failure as a refusal would tell an operator that the dispatcher never
// tried, which is the opposite of what happened.
func TestAFailedAttemptIsDispatchedAndNotRefused(t *testing.T) {
	store, projectID := testStore(t)
	root := dispatchRoot(t, store, projectID)
	child := dispatchChild(t, store, root, "alpha", assignment.RoleImplementer, "idem:report:failed")

	cfg := testConfig(t)
	engine, fake := testEngine(t, store, cfg)
	// The runtime reports a FAILED outcome, so the attempt is terminal
	// and failed.
	fake.launchState = okState("mpm-worker", "failed")

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
	if report.Dispatched != 1 {
		t.Errorf("the report dispatched %d assignments, want 1", report.Dispatched)
	}
	if report.Completed != 0 {
		t.Errorf("the report completed %d assignments, want 0", report.Completed)
	}
	if report.Failed != 1 {
		t.Errorf("the report failed %d assignments, want 1", report.Failed)
	}
	if report.Refused != 0 {
		t.Errorf("the report refused %d assignments, want 0 — a failure is not a refusal",
			report.Refused)
	}
	if report.Deferred != 0 || report.Ambiguous != 0 {
		t.Errorf("the report counts a deferral or an ambiguity: %+v", report)
	}
}

// countsOf is a report's six counters, in one comparable value.
func countsOf(r *Report) string {
	return fmt.Sprintf("dispatched=%d completed=%d failed=%d refused=%d deferred=%d ambiguous=%d",
		r.Dispatched, r.Completed, r.Failed, r.Refused, r.Deferred, r.Ambiguous)
}

// TestEveryReasonCountsWhereItSaysItCounts proves the mapping.
//
// The dispatch path and the recovery path report the SAME reasons, and
// a reason that counted one way in one path and another way in the
// other would make the report depend on where the work happened — the
// same live lease would be a refusal in one pass and a deferral in the
// next. So the mapping is ONE function, and this test pins it.
//
// `launched` is what separates a dispatch from a recovery: a reconciled
// outcome was handed over by an EARLIER pass.
func TestEveryReasonCountsWhereItSaysItCounts(t *testing.T) {
	for _, tc := range []struct {
		reason string
		want   Report
	}{
		{ReasonCompleted, Report{Dispatched: 1, Completed: 1}},
		{ReasonFailed, Report{Dispatched: 1, Failed: 1}},
		{ReasonAmbiguous, Report{Dispatched: 1, Ambiguous: 1}},
		{ReasonDispatched, Report{Dispatched: 1}},
		{ReasonDeferred, Report{Deferred: 1}},
		{ReasonLeaseHeld, Report{Deferred: 1}},
		{ReasonStaleEpoch, Report{Deferred: 1}},
		{ReasonSessionStillRunning, Report{Deferred: 1}},
		{ReasonRecoveryRequired, Report{Deferred: 1}},
		{ReasonBudgetExhausted, Report{Refused: 1}},
		{ReasonRuntimeUnavailable, Report{Refused: 1}},
		{ReasonReviewerUnavailable, Report{Refused: 1}},
		{ReasonOperatorGatePending, Report{Refused: 1}},
		{ReasonLeaseInsufficient, Report{Refused: 1}},
		{ReasonRefused, Report{Refused: 1}},
	} {
		report := &Report{}
		countOutcome(report, Outcome{Reason: tc.reason}, true)
		if countsOf(report) != countsOf(&tc.want) {
			t.Errorf("the reason %q counted %s, want %s",
				tc.reason, countsOf(report), countsOf(&tc.want))
		}
		// AND THE RECOVERY PATH COUNTS THE SAME WAY, except that it did
		// not launch anything.
		recovered := &Report{}
		countOutcome(recovered, Outcome{Reason: tc.reason}, false)
		want := tc.want
		want.Dispatched = 0
		if tc.reason == ReasonDispatched {
			// A RECOVERED DISPATCH is not a dispatch BY THIS PASS, and
			// the assignment did not reach a terminal state, so it is a
			// deferral. It is counted SOMEWHERE: an outcome that no
			// counter reflects is a fact the operator cannot see.
			want = Report{Deferred: 1}
		}
		if countsOf(recovered) != countsOf(&want) {
			t.Errorf("the reason %q counted %s in the recovery path, want %s",
				tc.reason, countsOf(recovered), countsOf(&want))
		}
	}
}

// TestARecordOnlyRefusalIsADeferral proves the refusal is bucketed by
// what it means.
//
// The adapter refused to mint an attempt because the caller asked for no
// launch. The attempt stays OUTSTANDING and the work is not lost: a
// later pass may dispatch the assignment normally. So it is a DEFERRAL
// — "nothing was launched, and the work will be considered again" — and
// not a refusal, which says the work will not be tried again.
func TestARecordOnlyRefusalIsADeferral(t *testing.T) {
	reason := classifyAdapterRefusal(
		fmt.Errorf("%w: the outcome could not be recorded", assignment.ErrRecordOnlyRefused))
	if reason != ReasonDeferred {
		t.Errorf("the reason is %q, want %q", reason, ReasonDeferred)
	}
	// And a plain constraint refusal is still a refusal.
	reason = classifyAdapterRefusal(
		fmt.Errorf("%w: something else", assignment.ErrConstraint))
	if reason != ReasonRefused {
		t.Errorf("the reason is %q, want %q", reason, ReasonRefused)
	}
}

// TestALeaseTooShortToRecordTheResultIsNotLeaseHeld proves the two lease
// reasons are not the same fact.
//
// The clock here advances on every read, so the lease the dispatcher
// takes is already too short by the time the dispatcher checks it. That
// is the defensive case: the configuration already refuses a lease that
// cannot cover the runtime window, so this can only fire if the store
// clamped the lease or the clock moved under the run.
//
// The reason must be `lease_insufficient` and NOT `lease_held`:
// `lease_held` says somebody ELSE has the work, and an operator reading
// it would go looking for a holder that does not exist.
func TestALeaseTooShortToRecordTheResultIsNotLeaseHeld(t *testing.T) {
	dm := core.NewTestLocalOnlyDM(t)
	base := time.Unix(1700000000, 0).UTC()
	var mu sync.Mutex
	calls := 0
	advancing := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		calls++
		return base.Add(time.Duration(calls) * 60 * time.Second)
	}
	store := assignment.NewStoreWithClock(dm, advancing)
	projectID := "cpj-" + strings.Repeat("a", 16)
	if _, err := dm.SQLDB().Exec(
		`INSERT INTO projects (id, created_by) VALUES (?, 'dispatch-test')`, projectID); err != nil {
		t.Fatalf("seed the project: %v", err)
	}

	root := dispatchRoot(t, store, projectID)
	child := dispatchChild(t, store, root, "alpha", assignment.RoleImplementer, "idem:report:lease")

	cfg := testConfig(t)
	engine, fake := testEngine(t, store, cfg)
	engine.now = advancing
	fake.launchState = okState("mpm-worker", "succeeded")

	report, err := engine.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	outcome, ok := outcomeFor(report, child.ID)
	if !ok {
		t.Fatalf("the report has no outcome: %+v", report.Outcomes)
	}
	if outcome.Reason != ReasonLeaseInsufficient {
		t.Errorf("the outcome is %q, want %q", outcome.Reason, ReasonLeaseInsufficient)
	}
	if outcome.Reason == ReasonLeaseHeld {
		t.Error("the lease reason says somebody ELSE holds the lease")
	}
	// And NOTHING WAS LAUNCHED: the whole point of the check is that a
	// result which cannot be recorded must not be produced.
	if fake.Launches() != 0 {
		t.Errorf("the runtime was launched %d times, want 0", fake.Launches())
	}
	if report.Refused != 1 {
		t.Errorf("the report refused %d assignments, want 1", report.Refused)
	}
	if report.Dispatched != 0 {
		t.Errorf("the report dispatched %d assignments, want 0", report.Dispatched)
	}
}
