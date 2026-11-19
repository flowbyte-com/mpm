package assignment

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	internal "github.com/flowbyte-com/mpm-core"
)

// ── the fixtures ──────────────────────────────────────────────────────────

// testClock is a clock the tests move by hand.
//
// The lease semantics are observed through it rather than waited for: no
// test sleeps, and the reclaim path is exercised deterministically.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func newTestClock() *testClock {
	return &testClock{now: time.Unix(1_700_000_000, 0).UTC()}
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// newTestStore builds a Store over a disposable file-backed database, a
// frozen clock and a seeded project.
func newTestStore(t *testing.T) (*Store, *internal.DatabaseManager, *testClock, string) {
	t.Helper()
	dm := internal.NewTestLocalOnlyDM(t)
	clock := newTestClock()
	store := NewStoreWithClock(dm, clock.Now)
	return store, dm, clock, seedProject(t, dm)
}

// seedProject creates the project the assignments belong to.
func seedProject(t *testing.T, dm *internal.DatabaseManager) string {
	t.Helper()
	id := "cpj-" + strings.Repeat("a", 16)
	if _, err := dm.SQLDB().Exec(
		`INSERT INTO projects (id, created_by) VALUES (?, 'api-test')`, id); err != nil {
		t.Fatalf("seed the project: %v", err)
	}
	return id
}

// mustDo runs an operation and fails the test if it errors.
//
// The operation is a closure because Go cannot spread a two-value call
// into a call that also takes the *testing.T.
func mustDo(t *testing.T, op func() (*Assignment, error)) *Assignment {
	t.Helper()
	a, err := op()
	if err != nil {
		t.Fatalf("the operation failed: %v", err)
	}
	return a
}

// mustCreateRoot creates a valid operator-authorised root.
func mustCreateRoot(t *testing.T, store *Store, projectID string, mut ...func(*CreateRootParams)) *Assignment {
	t.Helper()
	p := CreateRootParams{
		ProjectID:           projectID,
		Title:               "the title",
		Objective:           "the objective",
		Recipient:           "alpha",
		IdempotencyKey:      "idem:root:" + t.Name(),
		PermittedOperations: []string{"read", "write"},
		EligibleRecipients:  []string{"alpha", "beta"},
		MaxDepth:            2,
		MaxTasks:            4,
		MaxConcurrency:      2,
		MaxResourceUnits:    100,
	}
	for _, m := range mut {
		m(&p)
	}
	a, created, err := store.CreateRoot(NewLocalOperatorAuthority("operator"), p)
	if err != nil {
		t.Fatalf("create the root: %v", err)
	}
	if !created {
		t.Fatalf("the root was not created")
	}
	return a
}

// mustDelegate creates a valid child of the parent.
func mustDelegate(t *testing.T, store *Store, parent *Assignment, mut ...func(*DelegateParams)) *Assignment {
	t.Helper()
	p := DelegateParams{
		ParentID:       parent.ID,
		Title:          "the child title",
		Objective:      "the child objective",
		Recipient:      "beta",
		Role:           RoleImplementer,
		IdempotencyKey: "idem:child:" + t.Name(),
	}
	for _, m := range mut {
		m(&p)
	}
	a, created, err := store.Delegate(p)
	if err != nil {
		t.Fatalf("delegate: %v", err)
	}
	if !created {
		t.Fatalf("the child was not created")
	}
	return a
}

// mustAdvance moves an assignment to the given state through the real
// transitions, so the tests never bypass the service.
func mustAdvance(t *testing.T, store *Store, a *Assignment, to string) *Assignment {
	t.Helper()
	actor := Actor{Address: a.Recipient, Kind: "agent"}
	switch to {
	case StateDelivered:
		return mustDo(t, func() (*Assignment, error) {
			return store.Deliver(FencedParams{ID: a.ID, Epoch: a.ClaimEpoch, Actor: actor})
		})
	case StateAcknowledged:
		mustDo(t, func() (*Assignment, error) {
			return store.Deliver(FencedParams{ID: a.ID, Epoch: a.ClaimEpoch, Actor: actor})
		})
		return mustDo(t, func() (*Assignment, error) {
			return store.Acknowledge(FencedParams{ID: a.ID, Epoch: a.ClaimEpoch, Actor: actor})
		})
	case StateAccepted:
		mustAdvance(t, store, a, StateAcknowledged)
		return mustDo(t, func() (*Assignment, error) {
			return store.Accept(FencedParams{ID: a.ID, Epoch: a.ClaimEpoch, Actor: actor})
		})
	case StateInProgress:
		mustAdvance(t, store, a, StateAccepted)
		claimed := mustDo(t, func() (*Assignment, error) {
			return store.Claim(ClaimParams{
				ID: a.ID, ExpectedEpoch: a.ClaimEpoch, Owner: a.Recipient, LeaseSeconds: 600,
				Actor: actor,
			})
		})
		return mustDo(t, func() (*Assignment, error) {
			return store.Start(FencedParams{ID: a.ID, Epoch: claimed.ClaimEpoch, Actor: actor})
		})
	case StateCompleted:
		mustAdvance(t, store, a, StateInProgress)
		cur := mustGet(t, store, a.ID)
		return mustDo(t, func() (*Assignment, error) {
			return store.Complete(FencedParams{ID: a.ID, Epoch: cur.ClaimEpoch, Actor: actor})
		})
	}
	t.Fatalf("no fixture path to %q", to)
	return nil
}

// mustGet reads an assignment and fails the test if it is gone.
func mustGet(t *testing.T, store *Store, id string) *Assignment {
	t.Helper()
	a, err := store.Get(id)
	if err != nil {
		t.Fatalf("read %s: %v", id, err)
	}
	return a
}

// eventTypes lists the ledger's event types for an assignment.
func eventTypes(t *testing.T, store *Store, id string) []string {
	t.Helper()
	events, err := store.History(id)
	if err != nil {
		t.Fatalf("read the history of %s: %v", id, err)
	}
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, e.EventType)
	}
	return out
}

// countRows counts the rows in a table.
func countRows(t *testing.T, dm *internal.DatabaseManager, table string) int {
	t.Helper()
	var n int
	if err := dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// agentActor is the attribution of the addressed agent.
func agentActor(address string) Actor { return Actor{Address: address, Kind: "agent"} }

// operatorActor is the attribution of the operator.
func operatorActor() Actor { return Actor{Address: "operator", Kind: "operator"} }

// ── 1. the operator's root, and the agent's refusal to make one ──────────

// TestAPIRootCreationRequiresTheOperatorAuthority proves that the
// operator's path is a boundary, not a flag.
//
// The authority is an interface with an unexported method, so nothing
// outside the package can fabricate one. A call that arrives without it
// is refused before anything is written.
func TestAPIRootCreationRequiresTheOperatorAuthority(t *testing.T) {
	store, dm, _, projectID := newTestStore(t)

	p := CreateRootParams{
		ProjectID:           projectID,
		Title:               "the title",
		Objective:           "the objective",
		Recipient:           "alpha",
		IdempotencyKey:      "idem:unauthorised",
		PermittedOperations: []string{"read"},
		EligibleRecipients:  []string{"alpha"},
		MaxDepth:            1,
		MaxTasks:            1,
		MaxConcurrency:      1,
		MaxResourceUnits:    1,
	}

	if _, _, err := store.CreateRoot(nil, p); !errors.Is(err, ErrNotAuthorised) {
		t.Fatalf("a root creation without the operator's authority returned %v, want %v",
			err, ErrNotAuthorised)
	}

	// Nothing was written: no row and no ledger entry.
	if n := countRows(t, dm, "agent_tasks"); n != 0 {
		t.Errorf("the refused root creation left %d rows, want 0", n)
	}
	if n := countRows(t, dm, "agent_task_events"); n != 0 {
		t.Errorf("the refused root creation left %d events, want 0", n)
	}
}

// TestAPIRootCreationSucceeds proves the operator's path works, and that
// the root carries the tree's budgets.
func TestAPIRootCreationSucceeds(t *testing.T) {
	store, _, _, projectID := newTestStore(t)

	root := mustCreateRoot(t, store, projectID)

	if root.State != StateEnqueued {
		t.Errorf("the root is %q, want %q", root.State, StateEnqueued)
	}
	if root.ClaimEpoch != 0 || root.Owner != "" || root.LeaseExpiresAt != nil {
		t.Errorf("the root was born claimed: epoch=%d owner=%q lease=%v",
			root.ClaimEpoch, root.Owner, root.LeaseExpiresAt)
	}
	if root.Depth != 0 || root.ParentID != "" || root.RootID != root.ID {
		t.Errorf("the root's tree position is (depth=%d parent=%q root=%q), want (0, \"\", itself)",
			root.Depth, root.ParentID, root.RootID)
	}
	if root.MaxTasks != 4 || root.MaxConcurrency != 2 ||
		root.MaxResourceUnits != 100 || root.MaxDepth != 2 {
		t.Errorf("the root carries the budgets %d/%d/%d/%d, want 4/2/100/2",
			root.MaxDepth, root.MaxTasks, root.MaxConcurrency, root.MaxResourceUnits)
	}

	// The creation is in the ledger.
	if got := eventTypes(t, store, root.ID); len(got) != 1 || got[0] != EventCreated {
		t.Errorf("the root's ledger reads %v, want [%s]", got, EventCreated)
	}
}

// TestAPIRootCreationValidatesItsInputs proves the root's own rules are
// enforced before anything is written.
func TestAPIRootCreationValidatesItsInputs(t *testing.T) {
	store, dm, _, projectID := newTestStore(t)

	base := CreateRootParams{
		ProjectID:           projectID,
		Title:               "the title",
		Objective:           "the objective",
		Recipient:           "alpha",
		IdempotencyKey:      "idem:base",
		PermittedOperations: []string{"read"},
		EligibleRecipients:  []string{"alpha"},
		MaxDepth:            1,
		MaxTasks:            1,
		MaxConcurrency:      1,
		MaxResourceUnits:    1,
	}

	cases := []struct {
		name string
		mut  func(*CreateRootParams)
	}{
		{"no project", func(p *CreateRootParams) { p.ProjectID = "" }},
		{"no title", func(p *CreateRootParams) { p.Title = "" }},
		{"no objective", func(p *CreateRootParams) { p.Objective = "" }},
		{"no recipient", func(p *CreateRootParams) { p.Recipient = "" }},
		{"no idempotency key", func(p *CreateRootParams) { p.IdempotencyKey = "" }},
		{"no permitted operation", func(p *CreateRootParams) { p.PermittedOperations = nil }},
		{"no eligible recipient", func(p *CreateRootParams) { p.EligibleRecipients = nil }},
		{"the recipient is not eligible", func(p *CreateRootParams) {
			p.EligibleRecipients = []string{"gamma"}
		}},
		{"no task budget", func(p *CreateRootParams) { p.MaxTasks = 0 }},
		{"no concurrency budget", func(p *CreateRootParams) { p.MaxConcurrency = 0 }},
		{"a negative depth budget", func(p *CreateRootParams) { p.MaxDepth = -1 }},
		{"an unknown role", func(p *CreateRootParams) { p.Role = "operator" }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := base
			p.IdempotencyKey = "idem:case:" + tc.name
			tc.mut(&p)
			if _, _, err := store.CreateRoot(NewLocalOperatorAuthority("operator"), p); err == nil {
				t.Fatalf("the invalid root was ACCEPTED")
			}
		})
	}

	if n := countRows(t, dm, "agent_tasks"); n != 0 {
		t.Errorf("the refused root creations left %d rows, want 0", n)
	}
	if n := countRows(t, dm, "agent_task_events"); n != 0 {
		t.Errorf("the refused root creations left %d events, want 0", n)
	}
}

// ── 2. the bounded child ─────────────────────────────────────────────────

// TestAPIBoundedChildCreation proves a child is created inside the root's
// constraints and carries NONE of the root's budgets.
//
// The inheritance is structural: the schema's root/child CHECK makes a
// child that carries a budget an illegal row, so there is no column in
// which a child could widen what it was given.
func TestAPIBoundedChildCreation(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	root := mustCreateRoot(t, store, projectID)

	child := mustDelegate(t, store, root)

	if child.Depth != 1 {
		t.Errorf("the child is at depth %d, want 1", child.Depth)
	}
	if child.ParentID != root.ID || child.RootID != root.ID {
		t.Errorf("the child's tree position is (parent=%q root=%q), want (%q, %q)",
			child.ParentID, child.RootID, root.ID, root.ID)
	}
	if child.MaxTasks != 0 || child.MaxConcurrency != 0 ||
		child.MaxResourceUnits != 0 || child.MaxDepth != 0 {
		t.Errorf("the child carries the budgets %d/%d/%d/%d, want none",
			child.MaxDepth, child.MaxTasks, child.MaxConcurrency, child.MaxResourceUnits)
	}
	if len(child.PermittedOperations) != 0 {
		t.Errorf("the child carries the operations %v, want none", child.PermittedOperations)
	}
	if len(child.EligibleRecipients) != 0 {
		t.Errorf("the child carries the recipients %v, want none", child.EligibleRecipients)
	}
	if child.State != StateEnqueued {
		t.Errorf("the child is %q, want %q", child.State, StateEnqueued)
	}
	if got := eventTypes(t, store, child.ID); len(got) != 1 || got[0] != EventCreated {
		t.Errorf("the child's ledger reads %v, want [%s]", got, EventCreated)
	}
}

// TestAPIChildOutsideTheRootAllowListIsRefused proves N2.
//
// The child's recipient must be among the ROOT's eligible recipients.
// The schema does not enforce it — a CHECK cannot contain a subquery —
// so this is the API's rule and the API must keep it.
func TestAPIChildOutsideTheRootAllowListIsRefused(t *testing.T) {
	store, dm, _, projectID := newTestStore(t)
	root := mustCreateRoot(t, store, projectID, func(p *CreateRootParams) {
		p.EligibleRecipients = []string{"alpha"}
	})

	before := countRows(t, dm, "agent_tasks")
	eventsBefore := countRows(t, dm, "agent_task_events")

	if _, _, err := store.Delegate(DelegateParams{
		ParentID:       root.ID,
		Title:          "the child",
		Objective:      "the objective",
		Recipient:      "gamma",
		IdempotencyKey: "idem:outside-the-list",
	}); !errors.Is(err, ErrConstraint) {
		t.Fatalf("a child addressed outside the root's allow-list returned %v, want %v",
			err, ErrConstraint)
	}

	// NO PARTIAL WRITE.
	if n := countRows(t, dm, "agent_tasks"); n != before {
		t.Errorf("the refused delegation left %d rows, want %d", n, before)
	}
	if n := countRows(t, dm, "agent_task_events"); n != eventsBefore {
		t.Errorf("the refused delegation left %d events, want %d", n, eventsBefore)
	}
}

// TestAPIChildBeyondTheRootDepthIsRefused proves N3.
//
// The child's depth is within the ROOT's max_depth — not the parent's,
// because the parent is not the budget holder.
func TestAPIChildBeyondTheRootDepthIsRefused(t *testing.T) {
	store, dm, _, projectID := newTestStore(t)
	root := mustCreateRoot(t, store, projectID, func(p *CreateRootParams) {
		p.MaxDepth = 1
		p.EligibleRecipients = []string{"alpha", "beta", "gamma"}
	})

	child := mustDelegate(t, store, root, func(p *DelegateParams) { p.Recipient = "beta" })

	before := countRows(t, dm, "agent_tasks")

	// The grandchild would be at depth 2, and the root allows 1.
	if _, _, err := store.Delegate(DelegateParams{
		ParentID:       child.ID,
		Title:          "the grandchild",
		Objective:      "the objective",
		Recipient:      "gamma",
		IdempotencyKey: "idem:too-deep",
	}); !errors.Is(err, ErrConstraint) {
		t.Fatalf("a child beyond the root's depth returned %v, want %v", err, ErrConstraint)
	}
	if n := countRows(t, dm, "agent_tasks"); n != before {
		t.Errorf("the refused delegation left %d rows, want %d", n, before)
	}
}

// TestAPIDelegationFromATerminalParentIsRefused proves a withdrawn or
// gated assignment is not delegated from.
func TestAPIDelegationFromATerminalParentIsRefused(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	root := mustCreateRoot(t, store, projectID)

	cancelled := mustDelegate(t, store, root, func(p *DelegateParams) {
		p.IdempotencyKey = "idem:cancelled-parent"
	})
	mustDo(t, func() (*Assignment, error) {
		return store.Cancel(CancelParams{
			ID: cancelled.ID, Epoch: cancelled.ClaimEpoch, Actor: operatorActor(),
		})
	})

	if _, _, err := store.Delegate(DelegateParams{
		ParentID:       cancelled.ID,
		Title:          "the child",
		Objective:      "the objective",
		Recipient:      "beta",
		IdempotencyKey: "idem:from-a-cancelled-parent",
	}); !errors.Is(err, ErrConstraint) {
		t.Fatalf("a delegation from a cancelled parent returned %v, want %v", err, ErrConstraint)
	}
}

// ── 3. idempotency ───────────────────────────────────────────────────────

// TestAPIDuplicateCreationIsIdempotent proves a repeated request resolves
// to the EXISTING logical assignment rather than creating a second one.
func TestAPIDuplicateCreationIsIdempotent(t *testing.T) {
	store, dm, _, projectID := newTestStore(t)

	p := CreateRootParams{
		ProjectID:           projectID,
		Title:               "the title",
		Objective:           "the objective",
		Recipient:           "alpha",
		IdempotencyKey:      "idem:the-key",
		PermittedOperations: []string{"read"},
		EligibleRecipients:  []string{"alpha"},
		MaxDepth:            1,
		MaxTasks:            1,
		MaxConcurrency:      1,
		MaxResourceUnits:    1,
	}

	first, created, err := store.CreateRoot(NewLocalOperatorAuthority("operator"), p)
	if err != nil || !created {
		t.Fatalf("the first creation failed: %v (created=%v)", err, created)
	}

	second, created, err := store.CreateRoot(NewLocalOperatorAuthority("operator"), p)
	if err != nil {
		t.Fatalf("the duplicate creation failed: %v", err)
	}
	if created {
		t.Errorf("the duplicate creation reported that it created a second assignment")
	}
	if second.ID != first.ID {
		t.Errorf("the duplicate creation returned %s, want the existing %s", second.ID, first.ID)
	}
	if n := countRows(t, dm, "agent_tasks"); n != 1 {
		t.Errorf("the duplicate creation left %d assignments, want 1", n)
	}
	if n := countRows(t, dm, "agent_task_events"); n != 1 {
		t.Errorf("the duplicate creation left %d events, want 1", n)
	}
}

// TestAPIDuplicateCreationWithADifferentAssignmentIsAConflict proves the
// service does not hand back a row that does not match the request.
func TestAPIDuplicateCreationWithADifferentAssignmentIsAConflict(t *testing.T) {
	store, dm, _, projectID := newTestStore(t)

	p := CreateRootParams{
		ProjectID:           projectID,
		Title:               "the title",
		Objective:           "the objective",
		Recipient:           "alpha",
		IdempotencyKey:      "idem:the-key",
		PermittedOperations: []string{"read"},
		EligibleRecipients:  []string{"alpha"},
		MaxDepth:            1,
		MaxTasks:            1,
		MaxConcurrency:      1,
		MaxResourceUnits:    1,
	}
	if _, _, err := store.CreateRoot(NewLocalOperatorAuthority("operator"), p); err != nil {
		t.Fatalf("the first creation failed: %v", err)
	}

	p.Recipient = "beta"
	p.EligibleRecipients = []string{"alpha", "beta"}
	if _, _, err := store.CreateRoot(NewLocalOperatorAuthority("operator"), p); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("the conflicting creation returned %v, want %v", err, ErrIdempotencyConflict)
	}
	if n := countRows(t, dm, "agent_tasks"); n != 1 {
		t.Errorf("the conflicting creation left %d assignments, want 1", n)
	}
}

// TestAPITheDuplicateIsResolvedWhenTheLookupCannotSeeItYet proves the
// READ-BACK.
//
// The early lookup resolves a repeated key by finding the existing
// assignment. But there is a schedule in which it CANNOT: the other
// party's transaction has not committed yet, so the lookup finds
// nothing, and the INSERT is the one that hits the unique index. That
// is the schedule the read-back exists for, and it is otherwise
// reachable only by winning a race.
//
// So the test reproduces the SCHEDULE: the fault seam makes the
// lookup find nothing, exactly as it would before the other party's
// commit. The service must then resolve the duplicate through the
// read-back — and return the EXISTING assignment, not an error.
func TestAPITheDuplicateIsResolvedWhenTheLookupCannotSeeItYet(t *testing.T) {
	store, dm, _, projectID := newTestStore(t)

	p := CreateRootParams{
		ProjectID:           projectID,
		Title:               "the title",
		Objective:           "the objective",
		Recipient:           "alpha",
		IdempotencyKey:      "idem:not-yet-visible",
		PermittedOperations: []string{"read"},
		EligibleRecipients:  []string{"alpha"},
		MaxDepth:            1,
		MaxTasks:            1,
		MaxConcurrency:      1,
		MaxResourceUnits:    1,
	}

	first, created, err := store.CreateRoot(NewLocalOperatorAuthority("operator"), p)
	if err != nil || !created {
		t.Fatalf("the first creation failed: %v (created=%v)", err, created)
	}

	// The schedule: the duplicate is not yet visible to the lookup.
	store.fault = func(stage string) error {
		if stage == "idempotency-lookup" {
			return errSkipLookup
		}
		return nil
	}
	second, created, err := store.CreateRoot(NewLocalOperatorAuthority("operator"), p)
	store.fault = nil
	if err != nil {
		t.Fatalf("the duplicate was not resolved by the read-back: %v", err)
	}
	if created {
		t.Errorf("the duplicate reported that it created a second assignment")
	}
	if second.ID != first.ID {
		t.Errorf("the duplicate returned %s, want the existing %s", second.ID, first.ID)
	}
	if n := countRows(t, dm, "agent_tasks"); n != 1 {
		t.Errorf("the duplicate left %d assignments, want 1", n)
	}
	if n := countRows(t, dm, "agent_task_events"); n != 1 {
		t.Errorf("the duplicate left %d events, want 1", n)
	}
}

// TestAPIConcurrentDuplicateCreationResolvesToOneAssignment proves two
// racing requests produce ONE logical assignment.
//
// The unique index makes exactly one of them win, and the loser reads the
// winner's row back rather than creating a second.
func TestAPIConcurrentDuplicateCreationResolvesToOneAssignment(t *testing.T) {
	store, dm, _, projectID := newTestStore(t)

	mk := func() CreateRootParams {
		return CreateRootParams{
			ProjectID:           projectID,
			Title:               "the title",
			Objective:           "the objective",
			Recipient:           "alpha",
			IdempotencyKey:      "idem:the-race",
			PermittedOperations: []string{"read"},
			EligibleRecipients:  []string{"alpha"},
			MaxDepth:            1,
			MaxTasks:            1,
			MaxConcurrency:      1,
			MaxResourceUnits:    1,
		}
	}

	type outcome struct {
		id      string
		created bool
		err     error
	}
	results := make([]outcome, 2)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			a, created, err := store.CreateRoot(NewLocalOperatorAuthority("operator"), mk())
			id := ""
			if a != nil {
				id = a.ID
			}
			results[i] = outcome{id: id, created: created, err: err}
		}(i)
	}
	close(start)
	wg.Wait()

	ids := map[string]bool{}
	createdCount := 0
	for _, r := range results {
		if r.err != nil {
			t.Fatalf("a racing creation failed: %v", r.err)
		}
		ids[r.id] = true
		if r.created {
			createdCount++
		}
	}
	if len(ids) != 1 {
		t.Errorf("the racing creations produced %d different assignments, want 1", len(ids))
	}
	if createdCount != 1 {
		t.Errorf("%d of the racing creations reported that they created the assignment, want 1",
			createdCount)
	}
	if n := countRows(t, dm, "agent_tasks"); n != 1 {
		t.Errorf("the racing creations left %d assignments, want 1", n)
	}
}

// ── 4 and 5. claims, leases and the fence ────────────────────────────────

// TestAPITwoSimultaneousClaimsProduceOneOwner proves the claim is a
// compare-and-swap.
//
// Two workers present the same epoch; exactly one of them becomes the
// valid owner, and the epoch advances exactly once.
func TestAPITwoSimultaneousClaimsProduceOneOwner(t *testing.T) {
	store, dm, _, projectID := newTestStore(t)
	root := mustCreateRoot(t, store, projectID)

	type outcome struct {
		epoch int64
		owner string
		err   error
	}
	results := make([]outcome, 2)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			owner := []string{"worker-a", "worker-b"}[i]
			a, err := store.Claim(ClaimParams{
				ID: root.ID, ExpectedEpoch: 0, Owner: owner, LeaseSeconds: 600,
				Actor: agentActor(owner),
			})
			if err != nil {
				results[i] = outcome{err: err}
				return
			}
			results[i] = outcome{epoch: a.ClaimEpoch, owner: a.Owner}
		}(i)
	}
	close(start)
	wg.Wait()

	winners := 0
	for _, r := range results {
		if r.err == nil {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("%d of the two simultaneous claims succeeded, want 1 (%+v)", winners, results)
	}

	final := mustGet(t, store, root.ID)
	if final.ClaimEpoch != 1 {
		t.Errorf("the assignment is at epoch %d after the race, want 1", final.ClaimEpoch)
	}
	if final.Owner == "" {
		t.Errorf("the assignment has no owner after the race")
	}
	if n := countRows(t, dm, "agent_task_events"); n != 2 {
		t.Errorf("the race left %d events, want 2 (the creation and one claim)", n)
	}
}

// TestAPIALiveLeaseIsNotStealable proves a claim is refused while
// another holder's lease is alive.
func TestAPIALiveLeaseIsNotStealable(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	root := mustCreateRoot(t, store, projectID)

	claimed := mustDo(t, func() (*Assignment, error) {
		return store.Claim(ClaimParams{
			ID: root.ID, ExpectedEpoch: 0, Owner: "worker-a", LeaseSeconds: 600,
			Actor: agentActor("worker-a"),
		})
	})

	if _, err := store.Claim(ClaimParams{
		ID: root.ID, ExpectedEpoch: claimed.ClaimEpoch, Owner: "worker-b", LeaseSeconds: 600,
		Actor: agentActor("worker-b"),
	}); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("a claim over a live lease returned %v, want %v", err, ErrLeaseHeld)
	}

	// And the holder cannot claim its own live lease either — it must
	// renew it.
	if _, err := store.Claim(ClaimParams{
		ID: root.ID, ExpectedEpoch: claimed.ClaimEpoch, Owner: "worker-a", LeaseSeconds: 600,
		Actor: agentActor("worker-a"),
	}); !errors.Is(err, ErrAlreadyClaimed) {
		t.Fatalf("a holder's own claim over a live lease returned %v, want %v", err, ErrAlreadyClaimed)
	}
}

// TestAPIAnExpiredLeaseIsReclaimableAndThePreviousHolderIsFencedOut
// proves the reclaim semantics and the fence together.
//
// This is acceptance test 5: an expired lease can be reclaimed, and the
// previous holder's epoch is fenced out — its write matches no row.
func TestAPIAnExpiredLeaseIsReclaimableAndThePreviousHolderIsFencedOut(t *testing.T) {
	store, _, clock, projectID := newTestStore(t)
	root := mustCreateRoot(t, store, projectID)

	// The assignment is IN PROGRESS, so that the fenced write below is
	// a LEGAL transition. A test that presents a stale epoch to an
	// assignment sitting in 'enqueued' would be refused by the state
	// machine before the fence was ever consulted — it would prove
	// the state machine and call it the fence.
	first := mustAdvance(t, store, root, StateInProgress)
	if first.ClaimEpoch != 1 {
		t.Fatalf("the first claim is at epoch %d, want 1", first.ClaimEpoch)
	}

	// The lease expires.
	clock.Advance(601 * time.Second)

	second := mustDo(t, func() (*Assignment, error) {
		return store.Claim(ClaimParams{
			ID: root.ID, ExpectedEpoch: first.ClaimEpoch, Owner: "worker-b", LeaseSeconds: 600,
			Actor: agentActor("worker-b"),
		})
	})
	if second.ClaimEpoch != 2 {
		t.Fatalf("the reclaim is at epoch %d, want 2", second.ClaimEpoch)
	}
	if second.Owner != "worker-b" {
		t.Errorf("the reclaim's owner is %q, want worker-b", second.Owner)
	}

	// THE FENCE. The previous holder's write presents the epoch it
	// holds, and it matches NO ROW.
	_, err := store.Complete(FencedParams{
		ID: root.ID, Epoch: first.ClaimEpoch, Actor: agentActor("worker-a"),
	})
	if !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("the fenced-out holder's write returned %v, want %v", err, ErrStaleClaim)
	}

	// The row is untouched by the fenced write.
	final := mustGet(t, store, root.ID)
	if final.State != StateInProgress {
		t.Errorf("the assignment is %q after the fenced write, want %q", final.State, StateInProgress)
	}
	if final.ClaimEpoch != 2 {
		t.Errorf("the assignment is at epoch %d after the fenced write, want 2", final.ClaimEpoch)
	}
}

// TestAPIRenewalAdvancesTheEpoch proves a renewal is a claim by the same
// holder: the epoch advances and the lease is new.
//
// PR 1's rule is that the lease changes only when the epoch advances, so
// a renewal that kept the epoch could not move the lease at all.
func TestAPIRenewalAdvancesTheEpoch(t *testing.T) {
	store, _, clock, projectID := newTestStore(t)
	root := mustCreateRoot(t, store, projectID)

	// The assignment is ACCEPTED, so that the fenced write below is a
	// legal transition and the fence is what refuses it.
	accepted := mustAdvance(t, store, root, StateAccepted)

	claimed := mustDo(t, func() (*Assignment, error) {
		return store.Claim(ClaimParams{
			ID: accepted.ID, ExpectedEpoch: accepted.ClaimEpoch, Owner: "worker-a",
			LeaseSeconds: 600, Actor: agentActor("worker-a"),
		})
	})
	firstLease := *claimed.LeaseExpiresAt

	clock.Advance(60 * time.Second)
	renewed := mustDo(t, func() (*Assignment, error) {
		return store.Renew(RenewParams{
			ID: root.ID, ExpectedEpoch: claimed.ClaimEpoch, Owner: "worker-a", LeaseSeconds: 900,
			Actor: agentActor("worker-a"),
		})
	})
	if renewed.ClaimEpoch != claimed.ClaimEpoch+1 {
		t.Errorf("the renewal is at epoch %d, want %d", renewed.ClaimEpoch, claimed.ClaimEpoch+1)
	}
	if renewed.Owner != "worker-a" {
		t.Errorf("the renewal's owner is %q, want worker-a", renewed.Owner)
	}
	if renewed.LeaseExpiresAt == nil || *renewed.LeaseExpiresAt == firstLease {
		t.Errorf("the renewal's lease is %v, want a new one (the old was %d)",
			renewed.LeaseExpiresAt, firstLease)
	}

	// The OLD epoch is now stale: the holder must use its new epoch.
	if _, err := store.Start(FencedParams{
		ID: root.ID, Epoch: claimed.ClaimEpoch, Actor: agentActor("worker-a"),
	}); !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("a write with the pre-renewal epoch returned %v, want %v", err, ErrStaleClaim)
	}
}

// TestAClaimPresentingAStaleEpochIsRefused proves the claim's
// COMPARE-AND-SWAP.
//
// The caller presents the epoch it believes is current. A claim that
// presents a stale one is refused AS STALE — not as "somebody else
// holds the lease", which is a different refusal with a different
// cause, and which would hide a broken epoch check behind a working
// lease check.
//
// The distinction matters: the lease check refuses a claim on a LIVE
// lease, whoever presents it. The epoch check refuses a claim that
// presents an epoch that is no longer current, whoever holds the
// lease. Two different invariants, and this test is the one that
// measures the second.
func TestAPIClaimPresentingAStaleEpochIsRefused(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	root := mustCreateRoot(t, store, projectID)

	claimed := mustDo(t, func() (*Assignment, error) {
		return store.Claim(ClaimParams{
			ID: root.ID, ExpectedEpoch: 0, Owner: "worker-a", LeaseSeconds: 600,
			Actor: agentActor("worker-a"),
		})
	})

	// The claim presents epoch 0; the assignment is at epoch 1.
	_, err := store.Claim(ClaimParams{
		ID: root.ID, ExpectedEpoch: 0, Owner: "worker-b", LeaseSeconds: 600,
		Actor: agentActor("worker-b"),
	})
	if !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("a claim presenting a stale epoch returned %v, want %v", err, ErrStaleClaim)
	}

	// And nothing moved.
	final := mustGet(t, store, root.ID)
	if final.Owner != "worker-a" {
		t.Errorf("the refused claim changed the holder to %q", final.Owner)
	}
	if final.ClaimEpoch != claimed.ClaimEpoch {
		t.Errorf("the refused claim moved the epoch to %d, want %d",
			final.ClaimEpoch, claimed.ClaimEpoch)
	}
}

// TestAPIOnlyTheHolderMayRenew proves a renewal is not a takeover.
func TestAPIOnlyTheHolderMayRenew(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	root := mustCreateRoot(t, store, projectID)

	claimed := mustDo(t, func() (*Assignment, error) {
		return store.Claim(ClaimParams{
			ID: root.ID, ExpectedEpoch: 0, Owner: "worker-a", LeaseSeconds: 600,
			Actor: agentActor("worker-a"),
		})
	})

	if _, err := store.Renew(RenewParams{
		ID: root.ID, ExpectedEpoch: claimed.ClaimEpoch, Owner: "worker-b", LeaseSeconds: 600,
		Actor: agentActor("worker-b"),
	}); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("a renewal by a non-holder returned %v, want %v", err, ErrLeaseHeld)
	}
}

// TestAPITerminalAssignmentsAreNotClaimable proves the claim's liveness.
func TestAPITerminalAssignmentsAreNotClaimable(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	root := mustCreateRoot(t, store, projectID)

	cancelled := mustDo(t, func() (*Assignment, error) {
		return store.Cancel(CancelParams{
			ID: root.ID, Epoch: root.ClaimEpoch, Actor: operatorActor(),
		})
	})
	if _, err := store.Claim(ClaimParams{
		ID: cancelled.ID, ExpectedEpoch: cancelled.ClaimEpoch, Owner: "worker-a", LeaseSeconds: 600,
		Actor: agentActor("worker-a"),
	}); !errors.Is(err, ErrConstraint) {
		t.Fatalf("a claim on a cancelled assignment returned %v, want %v", err, ErrConstraint)
	}
}

// ── 6. one event per transition, atomically ──────────────────────────────

// TestAPIEveryTransitionWritesExactlyOneEvent proves the ledger rule.
//
// Every transition appends exactly one event to the transitioned
// assignment's ledger, and the event names the state it moved to.
func TestAPIEveryTransitionWritesExactlyOneEvent(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	root := mustCreateRoot(t, store, projectID)

	steps := []struct {
		name string
		run  func(a *Assignment) *Assignment
		want string
	}{
		{"delivered", func(a *Assignment) *Assignment {
			return mustDo(t, func() (*Assignment, error) {
				return store.Deliver(FencedParams{ID: a.ID, Epoch: a.ClaimEpoch,
					Actor: agentActor(a.Recipient)})
			})
		}, EventDelivered},
		{"acknowledged", func(a *Assignment) *Assignment {
			return mustDo(t, func() (*Assignment, error) {
				return store.Acknowledge(FencedParams{ID: a.ID, Epoch: a.ClaimEpoch,
					Actor: agentActor(a.Recipient)})
			})
		}, EventAcknowledged},
		{"accepted", func(a *Assignment) *Assignment {
			return mustDo(t, func() (*Assignment, error) {
				return store.Accept(FencedParams{ID: a.ID, Epoch: a.ClaimEpoch,
					Actor: agentActor(a.Recipient)})
			})
		}, EventAccepted},
	}

	current := root
	for _, step := range steps {
		before := len(eventTypes(t, store, current.ID))
		current = step.run(current)
		after := eventTypes(t, store, current.ID)
		if len(after) != before+1 {
			t.Fatalf("the %s transition left the ledger at %d events, want %d",
				step.name, len(after), before+1)
		}
		if after[len(after)-1] != step.want {
			t.Errorf("the %s transition wrote %q, want %q", step.name, after[len(after)-1], step.want)
		}
	}

	// A claim writes its own event, and the state does not move.
	before := len(eventTypes(t, store, current.ID))
	claimed := mustDo(t, func() (*Assignment, error) {
		return store.Claim(ClaimParams{
			ID: current.ID, ExpectedEpoch: current.ClaimEpoch, Owner: "alpha", LeaseSeconds: 600,
			Actor: agentActor("alpha"),
		})
	})
	after := eventTypes(t, store, claimed.ID)
	if len(after) != before+1 {
		t.Fatalf("the claim left the ledger at %d events, want %d", len(after), before+1)
	}
	if after[len(after)-1] != EventClaimed {
		t.Errorf("the claim wrote %q, want %q", after[len(after)-1], EventClaimed)
	}

	// And the working transitions.
	started := mustDo(t, func() (*Assignment, error) {
		return store.Start(FencedParams{ID: claimed.ID, Epoch: claimed.ClaimEpoch,
			Actor: agentActor("alpha")})
	})
	if got := eventTypes(t, store, started.ID); got[len(got)-1] != EventStarted {
		t.Errorf("the start transition wrote %q, want %q", got[len(got)-1], EventStarted)
	}
	completed := mustDo(t, func() (*Assignment, error) {
		return store.Complete(FencedParams{ID: started.ID, Epoch: started.ClaimEpoch,
			Actor: agentActor("alpha")})
	})
	if got := eventTypes(t, store, completed.ID); got[len(got)-1] != EventCompleted {
		t.Errorf("the complete transition wrote %q, want %q", got[len(got)-1], EventCompleted)
	}
}

// TestAPIAFailedTransitionRollsBackItsEvent proves the state change and
// its ledger entry share a transaction.
//
// The fault is injected between the two writes, and the rollback must
// take BOTH back: a ledger entry that records a transition that did not
// happen is a ledger that lies.
func TestAPIAFailedTransitionRollsBackItsEvent(t *testing.T) {
	store, dm, _, projectID := newTestStore(t)
	root := mustCreateRoot(t, store, projectID)

	eventsBefore := countRows(t, dm, "agent_task_events")

	store.fault = func(stage string) error {
		if stage == "transition-event" {
			return errors.New("injected failure between the state change and its event")
		}
		return nil
	}
	_, err := store.Deliver(FencedParams{ID: root.ID, Epoch: root.ClaimEpoch,
		Actor: agentActor("alpha")})
	store.fault = nil
	if err == nil {
		t.Fatalf("the injected failure was swallowed")
	}

	// The state did not move...
	final := mustGet(t, store, root.ID)
	if final.State != StateEnqueued {
		t.Errorf("the assignment is %q after the rolled-back transition, want %q",
			final.State, StateEnqueued)
	}
	// ...and neither did the ledger.
	if n := countRows(t, dm, "agent_task_events"); n != eventsBefore {
		t.Errorf("the rolled-back transition left %d events, want %d", n, eventsBefore)
	}
}

// TestAPIAFailedCreationRollsBackItsEvent proves the same for a creation.
func TestAPIAFailedCreationRollsBackItsEvent(t *testing.T) {
	store, dm, _, projectID := newTestStore(t)

	store.fault = func(stage string) error {
		if stage == "create-root-event" {
			return errors.New("injected failure between the row and its event")
		}
		return nil
	}
	_, _, err := store.CreateRoot(NewLocalOperatorAuthority("operator"), CreateRootParams{
		ProjectID:           projectID,
		Title:               "the title",
		Objective:           "the objective",
		Recipient:           "alpha",
		IdempotencyKey:      "idem:rolled-back",
		PermittedOperations: []string{"read"},
		EligibleRecipients:  []string{"alpha"},
		MaxDepth:            1,
		MaxTasks:            1,
		MaxConcurrency:      1,
		MaxResourceUnits:    1,
	})
	store.fault = nil
	if err == nil {
		t.Fatalf("the injected failure was swallowed")
	}
	if n := countRows(t, dm, "agent_tasks"); n != 0 {
		t.Errorf("the rolled-back creation left %d rows, want 0", n)
	}
	if n := countRows(t, dm, "agent_task_events"); n != 0 {
		t.Errorf("the rolled-back creation left %d events, want 0", n)
	}
}

// ── 7. the states are distinct ───────────────────────────────────────────

// TestAPIDeliveryAcknowledgementAcceptanceAndCompletionAreDistinct
// proves the four steps are four states, not one.
//
// The acceptance criteria call this out, and it is the difference between
// "handed to a runtime", "taken by the recipient", "the recipient will do
// it" and "the work is finished".
func TestAPIDeliveryAcknowledgementAcceptanceAndCompletionAreDistinct(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	root := mustCreateRoot(t, store, projectID)

	delivered := mustDo(t, func() (*Assignment, error) {
		return store.Deliver(FencedParams{ID: root.ID, Epoch: root.ClaimEpoch,
			Actor: agentActor("alpha")})
	})
	if delivered.State != StateDelivered {
		t.Fatalf("the assignment is %q after the delivery, want %q", delivered.State, StateDelivered)
	}

	// Acceptance is refused before the acknowledgement.
	if _, err := store.Accept(FencedParams{ID: root.ID, Epoch: delivered.ClaimEpoch,
		Actor: agentActor("alpha")}); !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("an acceptance before the acknowledgement returned %v, want %v",
			err, ErrIllegalTransition)
	}

	acknowledged := mustDo(t, func() (*Assignment, error) {
		return store.Acknowledge(FencedParams{ID: root.ID, Epoch: delivered.ClaimEpoch,
			Actor: agentActor("alpha")})
	})
	if acknowledged.State != StateAcknowledged {
		t.Fatalf("the assignment is %q after the acknowledgement, want %q",
			acknowledged.State, StateAcknowledged)
	}

	// Completion is refused before the acceptance.
	if _, err := store.Complete(FencedParams{ID: root.ID, Epoch: acknowledged.ClaimEpoch,
		Actor: agentActor("alpha")}); !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("a completion before the acceptance returned %v, want %v",
			err, ErrIllegalTransition)
	}

	accepted := mustDo(t, func() (*Assignment, error) {
		return store.Accept(FencedParams{ID: root.ID, Epoch: acknowledged.ClaimEpoch,
			Actor: agentActor("alpha")})
	})
	if accepted.State != StateAccepted {
		t.Fatalf("the assignment is %q after the acceptance, want %q", accepted.State, StateAccepted)
	}

	// And in_progress needs a claim: there would be no lease to expire.
	if _, err := store.Start(FencedParams{ID: root.ID, Epoch: accepted.ClaimEpoch,
		Actor: agentActor("alpha")}); !errors.Is(err, ErrConstraint) {
		t.Fatalf("a start without a claim returned %v, want %v", err, ErrConstraint)
	}
}

// TestAPIAnUndeliveredAssignmentCannotBeAcknowledged proves the order is
// enforced, not merely conventional.
func TestAPIAnUndeliveredAssignmentCannotBeAcknowledged(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	root := mustCreateRoot(t, store, projectID)

	if _, err := store.Acknowledge(FencedParams{ID: root.ID, Epoch: root.ClaimEpoch,
		Actor: agentActor("alpha")}); !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("an acknowledgement before the delivery returned %v, want %v",
			err, ErrIllegalTransition)
	}
}

// ── 8. the work survives a restart ───────────────────────────────────────

// TestAPIRestartPreservesAssignmentsEventsAndClaims proves the work is
// durable: the process goes away and comes back, and the assignments,
// their ledger and their claims are still there.
//
// The workspace is reopened from the same directory, so the database is
// the same file the previous process wrote.
func TestAPIRestartPreservesAssignmentsEventsAndClaims(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MPM_WORKSPACE", dir)
	t.Setenv("MPM_SHARED_DB", "")

	dm, err := internal.NewDatabaseManager("")
	if err != nil {
		t.Fatalf("open the workspace: %v", err)
	}
	clock := newTestClock()
	store := NewStoreWithClock(dm, clock.Now)
	projectID := seedProject(t, dm)

	root := mustCreateRoot(t, store, projectID)
	delivered := mustDo(t, func() (*Assignment, error) {
		return store.Deliver(FencedParams{ID: root.ID, Epoch: root.ClaimEpoch,
			Actor: agentActor("alpha")})
	})
	acknowledged := mustDo(t, func() (*Assignment, error) {
		return store.Acknowledge(FencedParams{ID: delivered.ID, Epoch: delivered.ClaimEpoch,
			Actor: agentActor("alpha")})
	})
	accepted := mustDo(t, func() (*Assignment, error) {
		return store.Accept(FencedParams{ID: acknowledged.ID, Epoch: acknowledged.ClaimEpoch,
			Actor: agentActor("alpha")})
	})
	claimed := mustDo(t, func() (*Assignment, error) {
		return store.Claim(ClaimParams{
			ID: accepted.ID, ExpectedEpoch: accepted.ClaimEpoch, Owner: "alpha", LeaseSeconds: 600,
			Actor: agentActor("alpha"),
		})
	})

	// The process goes away.
	if err := dm.Close(); err != nil {
		t.Fatalf("close the workspace: %v", err)
	}

	// And comes back.
	dm2, err := internal.NewDatabaseManager("")
	if err != nil {
		t.Fatalf("reopen the workspace: %v", err)
	}
	t.Cleanup(func() { dm2.Close() })
	store2 := NewStoreWithClock(dm2, clock.Now)

	restored := mustGet(t, store2, root.ID)
	if restored.State != StateAccepted {
		t.Errorf("the assignment is %q after the restart, want %q", restored.State, StateAccepted)
	}
	if restored.ClaimEpoch != claimed.ClaimEpoch {
		t.Errorf("the assignment is at epoch %d after the restart, want %d",
			restored.ClaimEpoch, claimed.ClaimEpoch)
	}
	if restored.Owner != "alpha" {
		t.Errorf("the assignment's owner is %q after the restart, want alpha", restored.Owner)
	}
	if restored.LeaseExpiresAt == nil || *restored.LeaseExpiresAt != *claimed.LeaseExpiresAt {
		t.Errorf("the assignment's lease is %v after the restart, want %d",
			restored.LeaseExpiresAt, *claimed.LeaseExpiresAt)
	}

	// The ledger survived too.
	events, err := store2.History(root.ID)
	if err != nil {
		t.Fatalf("read the history after the restart: %v", err)
	}
	if len(events) != 5 {
		t.Errorf("the ledger holds %d entries after the restart, want 5", len(events))
	}
	if events[len(events)-1].EventType != EventClaimed {
		t.Errorf("the last ledger entry is %q after the restart, want %q",
			events[len(events)-1].EventType, EventClaimed)
	}

	// And the work is still RECOVERABLE: the holder can carry on with
	// the epoch it holds.
	started := mustDo(t, func() (*Assignment, error) {
		return store2.Start(FencedParams{ID: root.ID, Epoch: claimed.ClaimEpoch,
			Actor: agentActor("alpha")})
	})
	if started.State != StateInProgress {
		t.Errorf("the assignment is %q after the restart, want %q", started.State, StateInProgress)
	}
}

// ── 9. the refusals ──────────────────────────────────────────────────────

// TestAPIInvalidTransitionsAreRefused proves the state machine is
// enforced through the service, and not only by the schema.
func TestAPIInvalidTransitionsAreRefused(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	root := mustCreateRoot(t, store, projectID)

	// The gate cannot be reached from the start: the implementer cannot
	// skip its review.
	if _, err := store.HandoffToOperator(HandoffParams{
		ID: root.ID, Epoch: root.ClaimEpoch, AwaitingAction: "merge PR #260",
		Actor: agentActor("alpha"),
	}); !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("a hand-off that skipped the review returned %v, want %v",
			err, ErrIllegalTransition)
	}

	// The gate's action is required.
	if _, err := store.HandoffToOperator(HandoffParams{
		ID: root.ID, Epoch: root.ClaimEpoch, AwaitingAction: "",
		Actor: agentActor("alpha"),
	}); !errors.Is(err, ErrConstraint) {
		t.Fatalf("a hand-off with no action returned %v, want %v", err, ErrConstraint)
	}
}

// TestAPIForgedApprovalFieldsAreRefused proves the operator's gate holds.
//
// The approval is an operator operation, it must name the action the gate
// awaited, and every field of it is required.
func TestAPIForgedApprovalFieldsAreRefused(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	root := mustCreateRoot(t, store, projectID)

	// An approval without the operator's authority is refused, and it
	// never reaches the row.
	if _, err := store.RecordApproval(nil, ApprovalParams{
		ID: root.ID, Action: "merge PR #260", Revision: "abc", Source: "operator", Evidence: "ev://x",
	}); !errors.Is(err, ErrNotAuthorised) {
		t.Fatalf("an approval without the operator's authority returned %v, want %v",
			err, ErrNotAuthorised)
	}

	// An approval of an assignment that is not at the gate is refused.
	if _, err := store.RecordApproval(NewLocalOperatorAuthority("operator"), ApprovalParams{
		ID: root.ID, Action: "merge PR #260", Revision: "abc", Source: "operator", Evidence: "ev://x",
	}); !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("an approval of an assignment that is not at the gate returned %v, want %v",
			err, ErrIllegalTransition)
	}

	// Every field is required.
	full := ApprovalParams{
		ID: root.ID, Action: "merge PR #260", Revision: "abc", Source: "operator", Evidence: "ev://x",
	}
	for _, tc := range []struct {
		name string
		mut  func(*ApprovalParams)
	}{
		{"no action", func(p *ApprovalParams) { p.Action = "" }},
		{"no revision", func(p *ApprovalParams) { p.Revision = "" }},
		{"no source", func(p *ApprovalParams) { p.Source = "" }},
		{"no evidence", func(p *ApprovalParams) { p.Evidence = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := full
			tc.mut(&p)
			if _, err := store.RecordApproval(NewLocalOperatorAuthority("operator"), p); !errors.Is(err, ErrConstraint) {
				t.Fatalf("an approval with %s returned %v, want %v", tc.name, err, ErrConstraint)
			}
		})
	}
}

// TestAPITheApprovalMustNameTheGateAction proves the approval says what
// was approved RELATIVE TO what was asked.
func TestAPITheApprovalMustNameTheGateAction(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	_, subject, reviewer := mustReviewSubject(t, store, projectID)

	// The implementer cannot skip its review: it hands the work to the
	// operator's gate only after an INDEPENDENT reviewer passed it.
	cur := mustGet(t, store, subject.ID)
	if _, err := store.HandoffToOperator(HandoffParams{
		ID: cur.ID, Epoch: cur.ClaimEpoch, AwaitingAction: "merge PR #260",
		Actor: agentActor("alpha"),
	}); !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("a hand-off that skipped the review returned %v, want %v",
			err, ErrIllegalTransition)
	}

	// The review passes, and the SUBJECT moves to review_passed.
	reviewing := mustStartReviewer(t, store, reviewer)
	mustDo(t, func() (*Assignment, error) {
		return store.RecordReviewResult(ReviewResultParams{
			ReviewerID: reviewing.ID,
			Verdict:    VerdictPassed,
			Epoch:      reviewing.ClaimEpoch,
			Actor:      agentActor("charlie"),
			Note:       "the work is correct",
		})
	})

	// And NOW the hand-off is legal.
	cur = mustGet(t, store, subject.ID)
	gated := mustDo(t, func() (*Assignment, error) {
		return store.HandoffToOperator(HandoffParams{
			ID: cur.ID, Epoch: cur.ClaimEpoch, AwaitingAction: "merge PR #260",
			Actor: agentActor("alpha"),
		})
	})
	if gated.State != StateAwaitingOperator {
		t.Fatalf("the assignment is %q after the hand-off, want %q",
			gated.State, StateAwaitingOperator)
	}
	if gated.AwaitingAction != "merge PR #260" {
		t.Errorf("the gate awaits %q, want %q", gated.AwaitingAction, "merge PR #260")
	}

	// A DIFFERENT action is refused.
	if _, err := store.RecordApproval(NewLocalOperatorAuthority("operator"), ApprovalParams{
		ID: gated.ID, Action: "merge PR #999", Revision: "abc", Source: "operator", Evidence: "ev://x",
	}); !errors.Is(err, ErrConstraint) {
		t.Fatalf("an approval of a different action returned %v, want %v", err, ErrConstraint)
	}

	// The SAME action is accepted, and the row records the evidence.
	approved := mustDo(t, func() (*Assignment, error) {
		return store.RecordApproval(NewLocalOperatorAuthority("operator"), ApprovalParams{
			ID: gated.ID, Action: "merge PR #260", Revision: "abc123", Source: "operator",
			Evidence: "docs/evidence/continuity-001-pr2/manifest.json",
		})
	})
	if approved.State != StateOperatorApproved {
		t.Errorf("the assignment is %q after the approval, want %q", approved.State, StateOperatorApproved)
	}
	if approved.ApprovalRevision != "abc123" || approved.ApprovalEvidence == "" {
		t.Errorf("the approval recorded (revision=%q evidence=%q), want the operator's values",
			approved.ApprovalRevision, approved.ApprovalEvidence)
	}
	if approved.ApprovedAt == nil {
		t.Errorf("the approval recorded no moment")
	}
}

// ── the review ────────────────────────────────────────────────────────────

// mustCreateReviewer creates a reviewer assignment that reviews the
// subject.
func mustCreateReviewer(t *testing.T, store *Store, parent *Assignment, subject *Assignment, mut ...func(*DelegateParams)) *Assignment {
	t.Helper()
	p := DelegateParams{
		ParentID:       parent.ID,
		Title:          "review the work",
		Objective:      "the review",
		Recipient:      "charlie",
		Role:           RoleReviewer,
		IdempotencyKey: "idem:reviewer:" + t.Name(),
		Payload:        map[string]interface{}{PayloadKeyReviewTarget: subject.ID},
	}
	for _, m := range mut {
		m(&p)
	}
	a, created, err := store.Delegate(p)
	if err != nil {
		t.Fatalf("delegate the reviewer: %v", err)
	}
	if !created {
		t.Fatalf("the reviewer was not created")
	}
	return a
}

// mustStartReviewer takes a reviewer to the point where it is reviewing.
//
// The reviewer walks the path: delivered, acknowledged, accepted, claimed
// and started. It cannot jump to in_progress, and the service says so.
func mustStartReviewer(t *testing.T, store *Store, reviewer *Assignment) *Assignment {
	t.Helper()
	cur := reviewer
	cur = mustDo(t, func() (*Assignment, error) {
		return store.Deliver(FencedParams{ID: cur.ID, Epoch: cur.ClaimEpoch,
			Actor: agentActor("charlie")})
	})
	cur = mustDo(t, func() (*Assignment, error) {
		return store.Acknowledge(FencedParams{ID: cur.ID, Epoch: cur.ClaimEpoch,
			Actor: agentActor("charlie")})
	})
	cur = mustDo(t, func() (*Assignment, error) {
		return store.Accept(FencedParams{ID: cur.ID, Epoch: cur.ClaimEpoch,
			Actor: agentActor("charlie")})
	})
	cur = mustDo(t, func() (*Assignment, error) {
		return store.Claim(ClaimParams{
			ID: cur.ID, ExpectedEpoch: cur.ClaimEpoch, Owner: "charlie", LeaseSeconds: 600,
			Actor: agentActor("charlie"),
		})
	})
	return mustDo(t, func() (*Assignment, error) {
		return store.Start(FencedParams{ID: cur.ID, Epoch: cur.ClaimEpoch,
			Actor: agentActor("charlie")})
	})
}

// mustReviewSubject builds the reviewable shape: a root, an implementer
// child that finished its work, and a reviewer that is a SIBLING of the
// subject.
//
// The reviewer is a sibling on purpose. A reviewer delegated BY the work
// it reviews has the subject on its parent chain, and that is refused —
// so the independent reviewer is delegated by the root, which is the only
// party that can pick a judge without picking its own homework.
func mustReviewSubject(t *testing.T, store *Store, projectID string) (*Assignment, *Assignment, *Assignment) {
	t.Helper()
	root := mustCreateRoot(t, store, projectID, func(p *CreateRootParams) {
		p.EligibleRecipients = []string{"alpha", "charlie"}
		p.MaxDepth = 2
	})
	subject := mustDelegate(t, store, root, func(p *DelegateParams) {
		p.Recipient = "alpha"
		p.IdempotencyKey = "idem:subject:" + t.Name()
	})
	subject = mustAdvance(t, store, subject, StateCompleted)
	reviewer := mustCreateReviewer(t, store, root, subject)
	return root, subject, reviewer
}

// TestAPIReviewVerdictMovesTheSubject proves the review is the subject's
// transition, and that the reviewer's own assignment records the verdict
// too.
func TestAPIReviewVerdictMovesTheSubject(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	_, subject, reviewer := mustReviewSubject(t, store, projectID)
	reviewing := mustStartReviewer(t, store, reviewer)

	reviewed := mustDo(t, func() (*Assignment, error) {
		return store.RecordReviewResult(ReviewResultParams{
			ReviewerID: reviewing.ID,
			Verdict:    VerdictPassed,
			Epoch:      reviewing.ClaimEpoch,
			Actor:      agentActor("charlie"),
			Note:       "the work is correct",
		})
	})
	if reviewed.State != StateReviewPassed {
		t.Errorf("the reviewer is %q after the verdict, want %q", reviewed.State, StateReviewPassed)
	}

	// The SUBJECT moved, and its ledger says so.
	after := mustGet(t, store, subject.ID)
	if after.State != StateReviewPassed {
		t.Errorf("the subject is %q after the verdict, want %q", after.State, StateReviewPassed)
	}
	subjectEvents := eventTypes(t, store, subject.ID)
	if subjectEvents[len(subjectEvents)-1] != EventReviewPassed {
		t.Errorf("the subject's last event is %q, want %q",
			subjectEvents[len(subjectEvents)-1], EventReviewPassed)
	}
}

// TestAPIChangesRequestedLeavesTheSubjectAndRecordsANote proves a failed
// review is not invisible on the work it judges.
func TestAPIChangesRequestedLeavesTheSubjectAndRecordsANote(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	_, subject, reviewer := mustReviewSubject(t, store, projectID)
	reviewing := mustStartReviewer(t, store, reviewer)

	reviewed := mustDo(t, func() (*Assignment, error) {
		return store.RecordReviewResult(ReviewResultParams{
			ReviewerID: reviewing.ID,
			Verdict:    VerdictChangesRequested,
			Epoch:      reviewing.ClaimEpoch,
			Actor:      agentActor("charlie"),
			Note:       "the change is needed",
		})
	})
	if reviewed.State != StateCompleted {
		t.Errorf("the reviewer is %q after the verdict, want %q", reviewed.State, StateCompleted)
	}

	// The subject did NOT move...
	after := mustGet(t, store, subject.ID)
	if after.State != StateCompleted {
		t.Errorf("the subject is %q after the changes-requested verdict, want %q",
			after.State, StateCompleted)
	}
	// ...but the verdict is on its ledger.
	subjectEvents := eventTypes(t, store, subject.ID)
	if subjectEvents[len(subjectEvents)-1] != EventNote {
		t.Errorf("the subject's last event is %q, want %q",
			subjectEvents[len(subjectEvents)-1], EventNote)
	}
}

// TestAPIReviewerCannotReviewItsOwnWork proves the independence check.
//
// A reviewer delegated BY the work it reviews has the subject on its
// parent chain, and that is refused.
func TestAPIReviewerCannotReviewItsOwnWork(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	root := mustCreateRoot(t, store, projectID, func(p *CreateRootParams) {
		p.EligibleRecipients = []string{"alpha", "charlie"}
		p.MaxDepth = 2
	})
	subject := mustDelegate(t, store, root, func(p *DelegateParams) {
		p.Recipient = "alpha"
		p.IdempotencyKey = "idem:self-review-subject:" + t.Name()
	})
	subject = mustAdvance(t, store, subject, StateCompleted)

	// The reviewer is delegated BY the subject.
	reviewer := mustCreateReviewer(t, store, subject, subject, func(p *DelegateParams) {
		p.IdempotencyKey = "idem:self-review"
	})
	reviewing := mustStartReviewer(t, store, reviewer)

	if _, err := store.RecordReviewResult(ReviewResultParams{
		ReviewerID: reviewing.ID,
		Verdict:    VerdictPassed,
		Epoch:      reviewing.ClaimEpoch,
		Actor:      agentActor("charlie"),
	}); !errors.Is(err, ErrConstraint) {
		t.Fatalf("a review of the reviewer's own work returned %v, want %v", err, ErrConstraint)
	}
}

// TestAPIReviewOfAnIncompleteSubjectIsRefused proves the verdict cannot
// be recorded before the work is done.
func TestAPIReviewOfAnIncompleteSubjectIsRefused(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	root := mustCreateRoot(t, store, projectID, func(p *CreateRootParams) {
		p.EligibleRecipients = []string{"alpha", "charlie"}
		p.MaxDepth = 2
	})
	subject := mustDelegate(t, store, root, func(p *DelegateParams) {
		p.Recipient = "alpha"
		p.IdempotencyKey = "idem:unfinished:" + t.Name()
	})
	subject = mustAdvance(t, store, subject, StateInProgress)
	reviewer := mustCreateReviewer(t, store, root, subject)
	reviewing := mustStartReviewer(t, store, reviewer)

	if _, err := store.RecordReviewResult(ReviewResultParams{
		ReviewerID: reviewing.ID,
		Verdict:    VerdictPassed,
		Epoch:      reviewing.ClaimEpoch,
		Actor:      agentActor("charlie"),
	}); !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("a review of an unfinished subject returned %v, want %v",
			err, ErrIllegalTransition)
	}
}

// TestAPIAStaleReviewerIsFencedOut proves a reviewer whose claim was
// reclaimed cannot record a verdict.
func TestAPIAStaleReviewerIsFencedOut(t *testing.T) {
	store, _, clock, projectID := newTestStore(t)
	root := mustCreateRoot(t, store, projectID, func(p *CreateRootParams) {
		p.EligibleRecipients = []string{"alpha", "charlie", "delta"}
		p.MaxDepth = 2
	})
	subject := mustDelegate(t, store, root, func(p *DelegateParams) {
		p.Recipient = "alpha"
		p.IdempotencyKey = "idem:stale:" + t.Name()
	})
	subject = mustAdvance(t, store, subject, StateCompleted)
	reviewer := mustCreateReviewer(t, store, root, subject)

	claimed := mustStartReviewer(t, store, reviewer)

	// The reviewer's lease expires and somebody else takes it.
	clock.Advance(601 * time.Second)
	mustDo(t, func() (*Assignment, error) {
		return store.Claim(ClaimParams{
			ID: reviewer.ID, ExpectedEpoch: claimed.ClaimEpoch, Owner: "delta", LeaseSeconds: 600,
			Actor: agentActor("delta"),
		})
	})

	if _, err := store.RecordReviewResult(ReviewResultParams{
		ReviewerID: reviewer.ID,
		Verdict:    VerdictPassed,
		Epoch:      claimed.ClaimEpoch,
		Actor:      agentActor("charlie"),
	}); !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("the fenced-out reviewer's verdict returned %v, want %v", err, ErrStaleClaim)
	}
}

// ── the reads ────────────────────────────────────────────────────────────

// TestAPIInboxListsTheAddressedWork proves the inbox is addressed, and
// that the listing is a READ.
func TestAPIInboxListsTheAddressedWork(t *testing.T) {
	store, dm, _, projectID := newTestStore(t)
	root := mustCreateRoot(t, store, projectID, func(p *CreateRootParams) {
		p.EligibleRecipients = []string{"alpha", "beta"}
	})
	mustDelegate(t, store, root, func(p *DelegateParams) {
		p.Recipient = "beta"
		p.IdempotencyKey = "idem:inbox-child"
	})

	inbox, err := store.Inbox(InboxQuery{Recipient: "alpha"})
	if err != nil {
		t.Fatalf("read the inbox: %v", err)
	}
	if len(inbox) != 1 || inbox[0].ID != root.ID {
		t.Fatalf("the inbox holds %d assignments, want the one addressed to alpha", len(inbox))
	}

	// A read writes nothing.
	if n := countRows(t, dm, "agent_task_events"); n != 2 {
		t.Errorf("the inbox read left %d events, want 2 (one creation each)", n)
	}

	// The tree view.
	tree, err := store.ListByRoot(root.ID)
	if err != nil {
		t.Fatalf("read the tree: %v", err)
	}
	if len(tree) != 2 {
		t.Errorf("the tree holds %d assignments, want 2", len(tree))
	}
}
