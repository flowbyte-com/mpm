// budget_test.go — Continuity 001, PR 4: the aggregate root budgets.
//
// PR 1 stored three budgets on the root and deferred their enforcement.
// These tests prove the enforcement is there, and — the part that matters
// — that it is ATOMIC with the write it protects.
//
// Every test here asserts a REFUSAL or a COUNT. A budget that is merely
// checked is not a budget: a check that reads the count and then writes can
// be crossed by another writer between the two operations, and a tree whose
// limit is "usually" respected is a tree whose limit is not a limit.

package assignment

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// budgetRoot creates a root with the budgets a test needs.
func budgetRoot(t *testing.T, store *Store, projectID string,
	maxTasks, maxConcurrency, maxResourceUnits int64) *Assignment {
	t.Helper()
	return mustCreateRoot(t, store, projectID, func(p *CreateRootParams) {
		p.MaxTasks = maxTasks
		p.MaxConcurrency = maxConcurrency
		p.MaxResourceUnits = maxResourceUnits
		p.EligibleRecipients = []string{"alpha", "beta", "charlie"}
		p.MaxDepth = 3
	})
}

// isBudgetRefusal says the error is the budget's refusal and not some
// other constraint.
//
// The distinction matters: a budget refusal means "not now", and any other
// constraint means "not like this".
func isBudgetRefusal(err error) bool {
	return errors.Is(err, ErrBudgetExhausted)
}

// TestDelegateRefusesWhenTheTaskBudgetIsExhausted is max_tasks.
//
// The root's tree may hold max_tasks assignments IN TOTAL — the root is
// one of them, because a root's root_id is its own id.
func TestDelegateRefusesWhenTheTaskBudgetIsExhausted(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	// One for the root, two for children.
	root := budgetRoot(t, store, projectID, 3, 4, 100)

	mustDelegate(t, store, root, func(p *DelegateParams) {
		p.IdempotencyKey = "idem:budget:tasks:1"
	})
	mustDelegate(t, store, root, func(p *DelegateParams) {
		p.IdempotencyKey = "idem:budget:tasks:2"
	})

	_, _, err := store.Delegate(DelegateParams{
		ParentID:       root.ID,
		Title:          "one too many",
		Objective:      "the objective",
		Recipient:      "beta",
		Role:           RoleImplementer,
		IdempotencyKey: "idem:budget:tasks:3",
	})
	if err == nil {
		t.Fatal("the tree accepted one assignment more than max_tasks allows")
	}
	if !isBudgetRefusal(err) {
		t.Fatalf("refused, but not by the budget: %v", err)
	}
	if !strings.Contains(err.Error(), "max_tasks") {
		t.Errorf("the refusal does not name the budget: %v", err)
	}

	// The tree is unchanged: the refusal created nothing.
	status, err := store.BudgetStatus(root.ID)
	if err != nil {
		t.Fatalf("budget status: %v", err)
	}
	if status.Tasks != 3 {
		t.Errorf("the tree holds %d assignments, want 3", status.Tasks)
	}
}

// TestDelegateRefusesWhenTheResourceBudgetIsExhausted is
// max_resource_units.
//
// resource_units is a DECLARED reservation, charged once when the
// assignment is created. It is not a measurement of provider spend, and
// the test says so by naming the units rather than pretending to count
// tokens.
func TestDelegateRefusesWhenTheResourceBudgetIsExhausted(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	root := budgetRoot(t, store, projectID, 10, 4, 10)

	mustDelegate(t, store, root, func(p *DelegateParams) {
		p.IdempotencyKey = "idem:budget:units:1"
		p.ResourceUnits = 6
	})

	_, _, err := store.Delegate(DelegateParams{
		ParentID:       root.ID,
		Title:          "too expensive",
		Objective:      "the objective",
		Recipient:      "beta",
		Role:           RoleImplementer,
		IdempotencyKey: "idem:budget:units:2",
		ResourceUnits:  5,
	})
	if err == nil {
		t.Fatal("the tree accepted a delegation over its resource budget")
	}
	if !isBudgetRefusal(err) {
		t.Fatalf("refused, but not by the budget: %v", err)
	}
	if !strings.Contains(err.Error(), "max_resource_units") {
		t.Errorf("the refusal does not name the budget: %v", err)
	}

	status, err := store.BudgetStatus(root.ID)
	if err != nil {
		t.Fatalf("budget status: %v", err)
	}
	if status.Units != 6 {
		t.Errorf("the tree declared %d units, want 6", status.Units)
	}
}

// TestClaimRefusesWhenTheConcurrencyBudgetIsExhausted is max_concurrency.
//
// A claim is live while it holds a lease AND the work is unfinished, so a
// FINISHED assignment does not fill the tree's concurrency budget.
func TestClaimRefusesWhenTheConcurrencyBudgetIsExhausted(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	root := budgetRoot(t, store, projectID, 10, 1, 100)

	first := mustDelegate(t, store, root, func(p *DelegateParams) {
		p.IdempotencyKey = "idem:budget:conc:1"
	})
	second := mustDelegate(t, store, root, func(p *DelegateParams) {
		p.IdempotencyKey = "idem:budget:conc:2"
	})

	actor := Actor{Address: "beta", Kind: "agent"}
	if _, err := store.Claim(ClaimParams{
		ID: first.ID, ExpectedEpoch: 0, Owner: "beta", LeaseSeconds: 600, Actor: actor,
	}); err != nil {
		t.Fatalf("the first claim: %v", err)
	}

	_, err := store.Claim(ClaimParams{
		ID: second.ID, ExpectedEpoch: 0, Owner: "beta", LeaseSeconds: 600, Actor: actor,
	})
	if err == nil {
		t.Fatal("the tree accepted a claim over its concurrency budget")
	}
	if !isBudgetRefusal(err) {
		t.Fatalf("refused, but not by the budget: %v", err)
	}
	if !strings.Contains(err.Error(), "max_concurrency") {
		t.Errorf("the refusal does not name the budget: %v", err)
	}
}

// TestAFinishedAssignmentDoesNotFillTheConcurrencyBudget is the other half
// of the liveness rule.
//
// A finished assignment keeps the lease it was given — the lease is not
// released on completion — so a budget counted over leases alone would
// fill the tree with FINISHED work and refuse the next real task.
func TestAFinishedAssignmentDoesNotFillTheConcurrencyBudget(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	root := budgetRoot(t, store, projectID, 10, 1, 100)

	first := mustDelegate(t, store, root, func(p *DelegateParams) {
		p.IdempotencyKey = "idem:budget:done:1"
	})
	second := mustDelegate(t, store, root, func(p *DelegateParams) {
		p.IdempotencyKey = "idem:budget:done:2"
	})

	actor := Actor{Address: "beta", Kind: "agent"}
	// Finish the first one. The lease stays.
	mustAdvance(t, store, first, StateCompleted)

	// The second one can be claimed: the finished work does not count.
	if _, err := store.Claim(ClaimParams{
		ID: second.ID, ExpectedEpoch: 0, Owner: "beta", LeaseSeconds: 600, Actor: actor,
	}); err != nil {
		t.Fatalf("the claim after the first finished: %v", err)
	}
}

// TestConcurrentDelegationsCannotExceedMaxTasks is the ATOMICITY test.
//
// Two delegations race. A read-then-write budget check would let both
// read "one slot left" and both insert; the count and the write are one
// statement here, so exactly one crosses the limit.
func TestConcurrentDelegationsCannotExceedMaxTasks(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	// The root plus ONE child may exist.
	root := budgetRoot(t, store, projectID, 2, 4, 100)

	// THE BARRIER. Every racer is held at the seam just before its
	// INSERT, so they all have READ the tree's task count before ANY of
	// them writes. Without it the racers can be scheduled one at a time,
	// each seeing the previous one's committed row — and a check that is
	// only wrong when two writers overlap would pass by luck.
	const racers = 6
	release := make(chan struct{})
	var arrived int32
	store.fault = func(stage string) error {
		if stage != "delegate" {
			return nil
		}
		if atomic.AddInt32(&arrived, 1) == racers {
			close(release)
		}
		<-release
		return nil
	}
	t.Cleanup(func() { store.fault = nil })

	var wg sync.WaitGroup
	start := make(chan struct{})
	var created int32
	errs := make([]error, racers)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, ok, err := store.Delegate(DelegateParams{
				ParentID:       root.ID,
				Title:          "racing child",
				Objective:      "the objective",
				Recipient:      "beta",
				Role:           RoleImplementer,
				IdempotencyKey: fmt.Sprintf("idem:budget:race:%d", i),
			})
			if err == nil && ok {
				atomic.AddInt32(&created, 1)
			}
			errs[i] = err
		}(i)
	}
	close(start)
	wg.Wait()

	if got := atomic.LoadInt32(&created); got != 1 {
		t.Fatalf("%d delegations crossed a max_tasks of 2 (root + 1 child)", got)
	}
	// Every refusal must be a BUDGET refusal, not a crash.
	for i, err := range errs {
		if err != nil && !isBudgetRefusal(err) {
			t.Errorf("racer %d failed for the wrong reason: %v", i, err)
		}
	}
	status, err := store.BudgetStatus(root.ID)
	if err != nil {
		t.Fatalf("budget status: %v", err)
	}
	if status.Tasks != 2 {
		t.Errorf("the tree holds %d assignments, want 2", status.Tasks)
	}
}

// TestConcurrentClaimsCannotExceedMaxConcurrency is the ATOMICITY test for
// the concurrency budget.
//
// Several assignments are claimed at once, and the tree allows TWO live
// claims. A read-then-write check would let every racer read "one slot
// left"; the count lives in the UPDATE's own WHERE clause, so exactly two
// claims land.
func TestConcurrentClaimsCannotExceedMaxConcurrency(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	root := budgetRoot(t, store, projectID, 20, 2, 100)

	const racers = 8
	children := make([]*Assignment, racers)
	for i := range children {
		children[i] = mustDelegate(t, store, root, func(p *DelegateParams) {
			p.IdempotencyKey = fmt.Sprintf("idem:budget:claimrace:%d", i)
		})
	}

	actor := Actor{Address: "beta", Kind: "agent"}
	// THE BARRIER, for the same reason: every racer reads the tree's
	// live-claim count before any of them writes.
	release := make(chan struct{})
	var arrived int32
	store.fault = func(stage string) error {
		if stage != "claim" {
			return nil
		}
		if atomic.AddInt32(&arrived, 1) == racers {
			close(release)
		}
		<-release
		return nil
	}
	t.Cleanup(func() { store.fault = nil })

	var wg sync.WaitGroup
	start := make(chan struct{})
	var claimed int32
	for i := range children {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := store.Claim(ClaimParams{
				ID: children[i].ID, ExpectedEpoch: 0, Owner: "beta",
				LeaseSeconds: 600, Actor: actor,
			})
			if err == nil {
				atomic.AddInt32(&claimed, 1)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if got := atomic.LoadInt32(&claimed); got != 2 {
		t.Fatalf("%d claims crossed a max_concurrency of 2", got)
	}
	status, err := store.BudgetStatus(root.ID)
	if err != nil {
		t.Fatalf("budget status: %v", err)
	}
	if status.LiveClaims != 2 {
		t.Errorf("%d live claims, want 2", status.LiveClaims)
	}
}

// TestARetryDoesNotChargeTheResourceBudgetTwice is the retry rule.
//
// A retry is a new ATTEMPT on the same assignment row, and the declared
// reservation lives on the row. Charging per attempt would let a failing
// task bill the root again and again for one logical task.
func TestARetryDoesNotChargeTheResourceBudgetTwice(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	root := budgetRoot(t, store, projectID, 10, 4, 10)

	child := mustDelegate(t, store, root, func(p *DelegateParams) {
		p.IdempotencyKey = "idem:budget:retry"
		p.ResourceUnits = 9
	})

	before, err := store.BudgetStatus(root.ID)
	if err != nil {
		t.Fatalf("budget status: %v", err)
	}

	// Three attempts on the same row.
	actor := Actor{Address: "beta", Kind: "agent"}
	mustAdvance(t, store, child, StateAccepted)
	claimed, err := store.Claim(ClaimParams{
		ID: child.ID, ExpectedEpoch: 0, Owner: "beta", LeaseSeconds: 600, Actor: actor,
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	for i := 0; i < 3; i++ {
		attempt, err := store.BeginAttempt(BeginAttemptParams{
			TaskID: child.ID, Epoch: claimed.ClaimEpoch, Runtime: RuntimeOpenCode,
			IntendedAgent: "mpm-worker", NativeSessionID: fmt.Sprintf("ses_retry_%d", i),
			CorrelationRef: "corr", WorkspaceID: "ws", WorkspacePath: "/tmp/ws",
		})
		if err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
		if _, err := store.MarkAttemptFailed(SessionStateParams{
			AttemptID: attempt.ID, Epoch: claimed.ClaimEpoch, Error: "deliberate",
		}); err != nil {
			t.Fatalf("mark failed %d: %v", i, err)
		}
	}

	after, err := store.BudgetStatus(root.ID)
	if err != nil {
		t.Fatalf("budget status: %v", err)
	}
	if after.Units != before.Units {
		t.Errorf("the retries changed the declared units: %d -> %d",
			before.Units, after.Units)
	}
	if after.Tasks != before.Tasks {
		t.Errorf("the retries changed the task count: %d -> %d",
			before.Tasks, after.Tasks)
	}
}

// TestTheBudgetsAreEnforcedForDirectCallersNotOnlyTheDispatcher is the
// point of putting the checks in the SERVICE.
//
// A budget enforced only in the dispatcher is a budget the dispatcher can
// be talked out of. These calls are the plain assignment API — no
// dispatcher, no runtime — and they are refused.
func TestTheBudgetsAreEnforcedForDirectCallersNotOnlyTheDispatcher(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	root := budgetRoot(t, store, projectID, 2, 1, 100)

	// The task budget, through the plain API.
	if _, _, err := store.Delegate(DelegateParams{
		ParentID:       root.ID,
		Title:          "direct",
		Objective:      "the objective",
		Recipient:      "beta",
		Role:           RoleImplementer,
		IdempotencyKey: "idem:budget:direct:1",
	}); err != nil {
		t.Fatalf("the first delegation: %v", err)
	}
	if _, _, err := store.Delegate(DelegateParams{
		ParentID:       root.ID,
		Title:          "direct",
		Objective:      "the objective",
		Recipient:      "beta",
		Role:           RoleImplementer,
		IdempotencyKey: "idem:budget:direct:2",
	}); !isBudgetRefusal(err) {
		t.Fatalf("the direct API was not budget-bound: %v", err)
	}

	// The concurrency budget, through the plain API.
	actor := Actor{Address: "beta", Kind: "agent"}
	children := mustList(t, store, root)
	if len(children) < 2 {
		t.Fatalf("the tree holds %d assignments, want 2", len(children))
	}
	if _, err := store.Claim(ClaimParams{
		ID: children[0].ID, ExpectedEpoch: 0, Owner: "beta", LeaseSeconds: 600, Actor: actor,
	}); err != nil {
		t.Fatalf("the first claim: %v", err)
	}
	if _, err := store.Claim(ClaimParams{
		ID: children[1].ID, ExpectedEpoch: 0, Owner: "beta", LeaseSeconds: 600, Actor: actor,
	}); !isBudgetRefusal(err) {
		t.Fatalf("the direct API was not concurrency-bound: %v", err)
	}
}

// mustList reads a tree's assignments, oldest first.
func mustList(t *testing.T, store *Store, root *Assignment) []*Assignment {
	t.Helper()
	out, err := store.ListByRoot(root.ID)
	if err != nil {
		t.Fatalf("list the tree: %v", err)
	}
	return out
}

// TestBudgetStatusNamesTheRoot is a small guard on the report.
func TestBudgetStatusNamesTheRoot(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	root := budgetRoot(t, store, projectID, 5, 2, 50)
	status, err := store.BudgetStatus(root.ID)
	if err != nil {
		t.Fatalf("budget status: %v", err)
	}
	if status.RootID != root.ID {
		t.Errorf("the status names %q, want %q", status.RootID, root.ID)
	}
	if status.MaxTasks != 5 || status.MaxConcurrent != 2 || status.MaxUnits != 50 {
		t.Errorf("the status does not carry the root's budgets: %+v", status)
	}
	if status.Tasks != 1 {
		t.Errorf("the tree holds %d assignments, want 1 (the root)", status.Tasks)
	}
}

// The context import is used by the dispatcher tests elsewhere; this file
// references it so the import list stays honest.
var _ = context.Background
