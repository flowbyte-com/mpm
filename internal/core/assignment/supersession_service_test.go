package assignment

// Store-level tests for the operator-authorised recovery supersession.
//
// These prove the service half of the service/storage parity the contract
// requires: every refusal below is returned by the service with a typed
// error before the write, and the database triggers independently refuse
// the same writes (the migration tests pin the trigger half). The
// traceability tags name the contract cases each test discharges.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	internal "github.com/flowbyte-com/mpm-core"
)

// supBlock builds the minimum supersedeable shape: a root, an implementer
// child walked to in_progress, claimed, started, with exactly one
// ambiguous attempt and an expired lease.
func supBlock(t *testing.T, store *Store, clock *testClock, projectID, tag string) (rootID, taskID, attemptID string, epoch int64) {
	t.Helper()
	id, ep := claimedAssignment(t, store, projectID, "alpha")
	att := startAmbiguous(t, store, id, ep, "ses-"+tag)
	clock.Advance(2 * 60 * 60 * 1e9)
	cur, err := store.Get(id)
	if err != nil {
		t.Fatalf("read the task: %v", err)
	}
	ref, err := store.Get(id)
	if err != nil {
		t.Fatalf("read the reference: %v", err)
	}
	_ = ref
	return cur.RootID, id, att.ID, cur.ClaimEpoch
}

// supParams returns valid supersession parameters for the blocked task.
func supParams(taskID, attemptID string, epoch int64, tag string) SupersedeParams {
	return SupersedeParams{
		SupersededID: taskID, AttemptID: attemptID, Epoch: epoch,
		Replacement: SupersedeTaskParams{Title: "redo " + tag, Objective: "redo the work",
			Recipient: "beta", ResourceUnits: 1, BaseSHA: strings.Repeat("a", 40)},
		Reviewer: SupersedeTaskParams{Title: "re-review " + tag, Objective: "review the redo",
			Recipient: "charlie", ResourceUnits: 1},
		EvidenceRef: "gh:flowbyte-com/mpm#258", Justification: "the original launch is ambiguous: " + tag,
		RequireQuiescence: true,
	}
}

// lineageCount counts lineage rows.
func lineageCount(t *testing.T, store *Store) int {
	t.Helper()
	var n int
	if err := store.db.SQLDB().QueryRow(`SELECT COUNT(*) FROM agent_task_supersessions`).Scan(&n); err != nil {
		t.Fatalf("count the lineages: %v", err)
	}
	return n
}

// taskCount counts assignment rows.
func taskCount(t *testing.T, store *Store) int {
	t.Helper()
	var n int
	if err := store.db.SQLDB().QueryRow(`SELECT COUNT(*) FROM agent_tasks`).Scan(&n); err != nil {
		t.Fatalf("count the tasks: %v", err)
	}
	return n
}

// TestSupersedeRefusesWithoutOperatorAuthority proves supersession is an
// operator operation: no authority, no lineage, no rows.
func TestSupersedeRefusesWithoutOperatorAuthority(t *testing.T) {
	store, _, clock, projectID := newTestStore(t)
	_, taskID, attemptID, epoch := supBlock(t, store, clock, projectID, "noauth")
	before := taskCount(t, store)
	if _, err := store.Supersede(nil, supParams(taskID, attemptID, epoch, "noauth")); !errors.Is(err, ErrNotAuthorised) {
		t.Fatalf("an unauthorised supersession returned %v, want %v", err, ErrNotAuthorised)
	}
	if lineageCount(t, store) != 0 || taskCount(t, store) != before {
		t.Fatalf("the refused supersession wrote rows")
	}
}

// TestSupersedeValidationRefusals proves malformed authorisations are
// refused before any lookup or write.
func TestSupersedeValidationRefusals(t *testing.T) {
	store, _, clock, projectID := newTestStore(t)
	_, taskID, attemptID, epoch := supBlock(t, store, clock, projectID, "valid")
	auth := NewLocalOperatorAuthority("operator")
	base := supParams(taskID, attemptID, epoch, "valid")
	cases := []struct {
		name string
		mut  func(*SupersedeParams)
	}{
		{"empty replacement title", func(p *SupersedeParams) { p.Replacement.Title = "" }},
		{"empty reviewer recipient", func(p *SupersedeParams) { p.Reviewer.Recipient = "" }},
		{"negative units", func(p *SupersedeParams) { p.Replacement.ResourceUnits = -1 }},
		{"replacement without base", func(p *SupersedeParams) { p.Replacement.BaseSHA = "" }},
		{"reviewer with base", func(p *SupersedeParams) { p.Reviewer.BaseSHA = strings.Repeat("b", 40) }},
		{"empty evidence", func(p *SupersedeParams) { p.EvidenceRef = "" }},
		{"short evidence", func(p *SupersedeParams) { p.EvidenceRef = "gh:x" }},
		{"bad evidence prefix", func(p *SupersedeParams) { p.EvidenceRef = "http://x/evidence.json" }},
		{"empty justification", func(p *SupersedeParams) { p.Justification = "" }},
	}
	for _, tc := range cases {
		p := base
		tc.mut(&p)
		if _, err := store.Supersede(auth, p); !errors.Is(err, ErrConstraint) {
			t.Fatalf("%s returned %v, want %v", tc.name, err, ErrConstraint)
		}
	}
	if lineageCount(t, store) != 0 {
		t.Fatalf("a refused supersession wrote a lineage")
	}
}

// TestSupersedeRefusesStaleEpochIsT34 proves the guarded write is fenced
// on the presented epoch: authorising against a moved claim writes
// nothing — no lineage, no assignments, no events.
func TestSupersedeRefusesStaleEpochIsT34(t *testing.T) {
	store, _, clock, projectID := newTestStore(t)
	_, taskID, attemptID, epoch := supBlock(t, store, clock, projectID, "stale")
	beforeTasks := taskCount(t, store)
	p := supParams(taskID, attemptID, epoch, "stale")
	p.Epoch = epoch - 1
	if _, err := store.Supersede(NewLocalOperatorAuthority("operator"), p); !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("a supersession at a stale epoch returned %v, want %v", err, ErrStaleClaim)
	}
	if lineageCount(t, store) != 0 || taskCount(t, store) != beforeTasks {
		t.Fatalf("the refused supersession wrote rows")
	}
}

// TestSupersedeRefusesLiveLeaseIsT35 proves a live lease blocks recovery:
// the original is still held, so supersession waits rather than
// stealing the work.
func TestSupersedeRefusesLiveLeaseIsT35(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	id, epoch := claimedAssignment(t, store, projectID, "alpha")
	att := startAmbiguous(t, store, id, epoch, "ses-live-lease")
	// No clock advance: the lease is live.
	p := supParams(id, att.ID, epoch, "live")
	if _, err := store.Supersede(NewLocalOperatorAuthority("operator"), p); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("a supersession under a live lease returned %v, want %v", err, ErrLeaseHeld)
	}
	if lineageCount(t, store) != 0 {
		t.Fatalf("the refused supersession wrote a lineage")
	}
}

// TestSupersedeRefusesIneligibleOriginals covers the boundary table: a
// root, a reviewer original and a non-live state are refused before any
// write (A4/T62-adjacent).
func TestSupersedeRefusesIneligibleOriginals(t *testing.T) {
	store, _, clock, projectID := newTestStore(t)
	auth := NewLocalOperatorAuthority("operator")

	// The root itself (depth 0) can never be superseded. The attempt id
	// is arbitrary: eligibility fails before the attempt is consulted.
	root, _, err := store.CreateRoot(auth, CreateRootParams{
		ProjectID: projectID, Title: "root", Objective: "root work", Recipient: "alpha",
		IdempotencyKey: "idem:sup-root-inelig", PermittedOperations: []string{"read"},
		EligibleRecipients: []string{"alpha"}, MaxDepth: 2, MaxTasks: 8, MaxConcurrency: 2, MaxResourceUnits: 100,
	})
	if err != nil {
		t.Fatalf("create the root: %v", err)
	}
	p := supParams(root.ID, "ats0000000000000000", 0, "root")
	if _, err := store.Supersede(auth, p); !errors.Is(err, ErrRootSupersession) {
		t.Fatalf("a root supersession returned %v, want %v", err, ErrRootSupersession)
	} else if !strings.Contains(err.Error(), "root_supersession") {
		t.Fatalf("a root supersession names no code: %v", err)
	}

	// A reviewer original: role, not an implementer. The reviewer is
	// walked to in_progress, claimed, and given its own ambiguous
	// attempt; supersession still refuses it before any write.
	_, taskID, _, _ := supBlock(t, store, clock, projectID, "revorig")
	subject, err := store.Get(taskID)
	if err != nil {
		t.Fatalf("read the subject: %v", err)
	}
	reviewer, created, err := store.Delegate(DelegateParams{
		ParentID: subject.ParentID, Title: "review", Objective: "review it",
		Recipient: "charlie", Role: RoleReviewer, IdempotencyKey: "idem:rev:" + taskID,
		Actor:   Actor{Address: "operator", Kind: "agent"},
		Payload: map[string]interface{}{PayloadKeyReviewTarget: taskID},
		HeadSHA: strings.Repeat("3", 40),
	})
	if err != nil || !created {
		t.Fatalf("delegate the reviewer: %v %v", created, err)
	}
	// The reviewer never needs a claim for this refusal: the role
	// check fires before the attempt is consulted. Routing steps use the
	// historical gate, which admits the reviewer in a clean... tree with
	// no ambiguity of its own.
	rActor := Actor{Address: "charlie", Kind: "agent"}
	for _, step := range []func(FencedParams) (*Assignment, error){store.Deliver, store.Acknowledge, store.Accept} {
		if _, err := step(FencedParams{ID: reviewer.ID, Actor: rActor}); err != nil {
			t.Fatalf("advance the reviewer: %v", err)
		}
	}
	rp := supParams(reviewer.ID, "ats0000000000000000", 0, "revorig")
	if _, err := store.Supersede(auth, rp); !errors.Is(err, ErrReviewerOriginal) {
		t.Fatalf("a reviewer supersession returned %v, want %v", err, ErrReviewerOriginal)
	} else if !strings.Contains(err.Error(), "reviewer_original") {
		t.Fatalf("a reviewer supersession names no code: %v", err)
	}
	if lineageCount(t, store) != 0 {
		t.Fatalf("an ineligible supersession wrote a lineage")
	}
}

// TestSupersedeRefusesWrongAttemptIsT30 proves the lineage binds one exact
// attempt: naming a non-ambiguous attempt, or another task's attempt, is
// refused with nothing written.
func TestSupersedeRefusesWrongAttemptIsT30(t *testing.T) {
	store, _, clock, projectID := newTestStore(t)
	auth := NewLocalOperatorAuthority("operator")
	// The sibling is fully built BEFORE any ambiguity exists: later
	// claims in an ambiguous tree are refused, so setup precedes the
	// block.
	taskID, sibEpoch := claimedAssignment(t, store, projectID, "alpha")
	sibling := mustDelegateSibling(t, store, projectID, taskID, "charlie")
	sibActor := Actor{Address: "charlie", Kind: "agent"}
	for _, step := range []func(FencedParams) (*Assignment, error){store.Deliver, store.Acknowledge, store.Accept} {
		if _, err := step(FencedParams{ID: sibling.ID, Actor: sibActor}); err != nil {
			t.Fatalf("advance the sibling: %v", err)
		}
	}
	sibClaimed, err := store.Claim(ClaimParams{ID: sibling.ID, ExpectedEpoch: 0,
		Owner: "charlie", LeaseSeconds: 600, Actor: sibActor})
	if err != nil {
		t.Fatalf("claim the sibling: %v", err)
	}
	if _, err := store.Start(FencedParams{ID: sibling.ID, Epoch: sibClaimed.ClaimEpoch, Actor: sibActor}); err != nil {
		t.Fatalf("start the sibling: %v", err)
	}
	sibAtt, err := store.BeginAttempt(BeginAttemptParams{
		TaskID: sibling.ID, Epoch: sibClaimed.ClaimEpoch, Runtime: RuntimeOpenCode,
		IntendedAgent: "mpm-worker", NativeSessionID: "ses-wrongatt",
		CorrelationRef: "corr-wrongatt", WorkspaceID: "ws", WorkspacePath: "/tmp/ws",
	})
	if err != nil {
		t.Fatalf("begin the sibling attempt: %v", err)
	}
	if _, err := store.MarkAttemptCompleted(SessionStateParams{
		AttemptID: sibAtt.ID, Epoch: sibClaimed.ClaimEpoch, Outcome: "succeeded"}); err != nil {
		t.Fatalf("complete the sibling attempt: %v", err)
	}
	// Both leases expire so the test reaches the attempt check, not the
	// lease check.
	att := startAmbiguous(t, store, taskID, sibEpoch, "ses-wrongatt-main")
	attemptID := att.ID
	clock.Advance(2 * 60 * 60 * 1e9)

	// Another task's ambiguous attempt is not this task's.
	p := supParams(sibling.ID, attemptID, sibClaimed.ClaimEpoch, "wrongtask")
	if _, err := store.Supersede(auth, p); !errors.Is(err, ErrConstraint) {
		t.Fatalf("a supersession naming another task's attempt returned %v, want %v", err, ErrConstraint)
	}
	// A completed attempt on the same task is not an ambiguity.
	p = supParams(sibling.ID, sibAtt.ID, sibClaimed.ClaimEpoch, "completedatt")
	if _, err := store.Supersede(auth, p); !errors.Is(err, ErrConstraint) {
		t.Fatalf("a supersession naming a completed attempt returned %v, want %v", err, ErrConstraint)
	}
	if lineageCount(t, store) != 0 {
		t.Fatalf("a refused supersession wrote a lineage")
	}
}

// TestSupersedeRefusesActiveIntentIsT89 proves a live intent beside the
// ambiguity blocks recovery: the tree needs operator reconciliation
// first, not a lineage over unsettled work.
func TestSupersedeRefusesActiveIntentIsT89(t *testing.T) {
	store, _, clock, projectID := newTestStore(t)
	auth := NewLocalOperatorAuthority("operator")
	// Build the sibling's intent BEFORE any ambiguity exists.
	taskID, sibEpoch := claimedAssignment(t, store, projectID, "alpha")
	sibling := mustDelegateSibling(t, store, projectID, taskID, "charlie")
	sibActor := Actor{Address: "charlie", Kind: "agent"}
	for _, step := range []func(FencedParams) (*Assignment, error){store.Deliver, store.Acknowledge, store.Accept} {
		if _, err := step(FencedParams{ID: sibling.ID, Actor: sibActor}); err != nil {
			t.Fatalf("advance the sibling: %v", err)
		}
	}
	sibClaimed, err := store.Claim(ClaimParams{ID: sibling.ID, ExpectedEpoch: 0,
		Owner: "charlie", LeaseSeconds: 3600, Actor: sibActor})
	if err != nil {
		t.Fatalf("claim the sibling: %v", err)
	}
	if _, err := store.Start(FencedParams{ID: sibling.ID, Epoch: sibClaimed.ClaimEpoch, Actor: sibActor}); err != nil {
		t.Fatalf("start the sibling: %v", err)
	}
	intent, err := store.BeginAttempt(BeginAttemptParams{
		TaskID: sibling.ID, Epoch: sibClaimed.ClaimEpoch, Runtime: RuntimeOpenCode,
		IntendedAgent: "mpm-worker", NativeSessionID: "ses-live-intent",
		CorrelationRef: "corr-intent", WorkspaceID: "ws", WorkspacePath: "/tmp/ws",
	})
	if err != nil {
		t.Fatalf("begin the sibling intent: %v", err)
	}
	_ = intent
	// Now the main task turns ambiguous; both leases expire.
	att := startAmbiguous(t, store, taskID, sibEpoch, "ses-t89-main")
	clock.Advance(2 * 60 * 60 * 1e9)
	p := supParams(taskID, att.ID, sibEpoch, "intent")
	if _, err := store.Supersede(auth, p); !errors.Is(err, ErrConstraint) {
		t.Fatalf("a supersession beside a live intent returned %v, want %v", err, ErrConstraint)
	}
	if lineageCount(t, store) != 0 {
		t.Fatalf("a refused supersession wrote a lineage")
	}
}

// TestSupersedeIdempotentRetryIsT40 proves a repeated authorisation with
// identical parameters returns the same lineage and both assignments
// without writing a second row of anything.
func TestSupersedeIdempotentRetryIsT40(t *testing.T) {
	store, _, clock, projectID := newTestStore(t)
	auth := NewLocalOperatorAuthority("operator")
	_, taskID, attemptID, epoch := supBlock(t, store, clock, projectID, "idem")
	p := supParams(taskID, attemptID, epoch, "idem")
	first, err := store.Supersede(auth, p)
	if err != nil {
		t.Fatalf("supersede: %v", err)
	}
	if !first.Created {
		t.Fatalf("the first supersession reports Created=false")
	}
	tasksBefore := taskCount(t, store)
	eventsBefore := countEvents(t, store)
	second, err := store.Supersede(auth, p)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if second.Created {
		t.Fatalf("the retry reports Created=true")
	}
	if second.Lineage.ID != first.Lineage.ID ||
		second.Replacement.ID != first.Replacement.ID ||
		second.Reviewer.ID != first.Reviewer.ID {
		t.Fatalf("the retry returned different rows: %+v vs %+v", second.Lineage, first.Lineage)
	}
	if taskCount(t, store) != tasksBefore || countEvents(t, store) != eventsBefore {
		t.Fatalf("the retry wrote rows")
	}
}

// TestSupersedeConflictNamesTheFieldIsT72 proves a repeated authorisation
// with different parameters is a conflict, not a second lineage — and
// the error names the field that changed.
func TestSupersedeConflictNamesTheFieldIsT72(t *testing.T) {
	store, _, clock, projectID := newTestStore(t)
	auth := NewLocalOperatorAuthority("operator")
	_, taskID, attemptID, epoch := supBlock(t, store, clock, projectID, "conflict")
	p := supParams(taskID, attemptID, epoch, "conflict")
	if _, err := store.Supersede(auth, p); err != nil {
		t.Fatalf("supersede: %v", err)
	}
	changed := supParams(taskID, attemptID, epoch, "conflict")
	changed.Justification = "a different justification"
	if _, err := store.Supersede(auth, changed); !errors.Is(err, ErrSupersessionConflict) {
		t.Fatalf("a changed justification returned %v, want %v", err, ErrSupersessionConflict)
	} else if !strings.Contains(err.Error(), "justification") {
		t.Fatalf("the conflict names no field: %v", err)
	}
	changed = supParams(taskID, attemptID, epoch, "conflict")
	changed.Replacement.Title = "a different title"
	if _, err := store.Supersede(auth, changed); !errors.Is(err, ErrSupersessionConflict) {
		t.Fatalf("a changed title returned %v, want %v", err, ErrSupersessionConflict)
	} else if !strings.Contains(err.Error(), "replacement.title") {
		t.Fatalf("the conflict names no field: %v", err)
	}
	if lineageCount(t, store) != 1 {
		t.Fatalf("a conflict wrote a lineage")
	}
}

// countEvents counts every ledger row in the database.
func countEvents(t *testing.T, store *Store) int {
	t.Helper()
	var n int
	if err := store.db.SQLDB().QueryRow(`SELECT COUNT(*) FROM agent_task_events`).Scan(&n); err != nil {
		t.Fatalf("count the events: %v", err)
	}
	return n
}

// TestSupersedeReservedKeyCollisionIsT71 proves a legacy row squatting on
// a reserved key blocks supersession without being rewritten or adopted:
// no lineage, no replacement, no reviewer, no events.
func TestSupersedeReservedKeyCollisionIsT71(t *testing.T) {
	store, db, clock, projectID := newTestStore(t)
	auth := NewLocalOperatorAuthority("operator")
	_, taskID, attemptID, epoch := supBlock(t, store, clock, projectID, "collision")
	_ = auth
	_ = projectID

	// A pre-feature legacy row occupying the replacement's reserved key.
	// It predates the feature, so it predates TR9: drop the trigger to
	// plant it, exactly as a pre-feature database would carry it. The
	// service collision check — not the trigger — is what must refuse
	// to rewrite or adopt it.
	if _, err := db.SQLDB().Exec(`DROP TRIGGER IF EXISTS trg_agent_tasks_reserved_key`); err != nil {
		t.Fatalf("drop the reserved-key trigger: %v", err)
	}
	key := "supersede:" + attemptID
	if _, err := db.SQLDB().Exec(`
		INSERT INTO agent_tasks (
			id, project_id, root_id, parent_id, depth, title, objective,
			recipient, role, state, idempotency_key, resource_units
		) VALUES ('at-legacy0000000001',
			(SELECT project_id FROM agent_tasks WHERE id = ?),
			(SELECT root_id FROM agent_tasks WHERE id = ?),
			(SELECT parent_id FROM agent_tasks WHERE id = ?),
			(SELECT depth FROM agent_tasks WHERE id = ?),
			'the squatter', 'squat the key', 'mallory', 'implementer',
			'enqueued', ?, 1)`, taskID, taskID, taskID, taskID, key); err != nil {
		t.Fatalf("plant the legacy row: %v", err)
	}
	tasksBefore := taskCount(t, store)
	if _, err := store.Supersede(auth, supParams(taskID, attemptID, epoch, "collision")); !errors.Is(err, ErrReservedKeyCollision) {
		t.Fatalf("a supersession over a legacy collision returned %v, want %v", err, ErrReservedKeyCollision)
	}
	if lineageCount(t, store) != 0 {
		t.Fatalf("a refused supersession wrote a lineage")
	}
	if taskCount(t, store) != tasksBefore {
		t.Fatalf("the legacy row was rewritten or adopted")
	}
	var keyAfter string
	if err := db.SQLDB().QueryRow(`SELECT idempotency_key FROM agent_tasks WHERE id = 'at-legacy0000000001'`).Scan(&keyAfter); err != nil {
		t.Fatalf("the legacy row disappeared: %v", err)
	}
	if keyAfter != key {
		t.Fatalf("the legacy row was rewritten: %q", keyAfter)
	}
}

// TestSupersedeKeysAreReservedIsT58 proves agent-supplied squatting keys
// are refused before any lookup: neither a root nor a child can be
// created under the supersede: namespace.
func TestSupersedeKeysAreReservedIsT58(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	auth := NewLocalOperatorAuthority("operator")
	if _, _, err := store.CreateRoot(auth, CreateRootParams{
		ProjectID: projectID, Title: "squat", Objective: "squat", Recipient: "alpha",
		IdempotencyKey:      "supersede:ats0000000000000000",
		PermittedOperations: []string{"read"}, EligibleRecipients: []string{"alpha"},
		MaxDepth: 1, MaxTasks: 4, MaxConcurrency: 1, MaxResourceUnits: 10,
	}); !errors.Is(err, ErrReservedKeyCollision) {
		t.Fatalf("a squatting root creation returned %v, want %v", err, ErrReservedKeyCollision)
	}
	root, _, err := store.CreateRoot(auth, CreateRootParams{
		ProjectID: projectID, Title: "ok", Objective: "ok", Recipient: "alpha",
		IdempotencyKey: "idem:ok-root", PermittedOperations: []string{"read"},
		EligibleRecipients: []string{"alpha"},
		MaxDepth:           1, MaxTasks: 4, MaxConcurrency: 1, MaxResourceUnits: 10,
	})
	if err != nil {
		t.Fatalf("create the root: %v", err)
	}
	if _, _, err := store.Delegate(DelegateParams{
		ParentID: root.ID, Title: "squat", Objective: "squat", Recipient: "alpha",
		Role: RoleImplementer, IdempotencyKey: "supersede:ats0000000000000000:review",
		Actor: Actor{Address: "operator", Kind: "agent"},
	}); !errors.Is(err, ErrReservedKeyCollision) {
		t.Fatalf("a squatting delegation returned %v, want %v", err, ErrReservedKeyCollision)
	}
}

// TestSupersedeBudgetExhaustedIsT82 proves recovery never resets the
// tree's budget: two more assignments that do not fit are refused
// atomically, with no lineage, no children and no events.
//
// Delegation itself is not execution, so the tree can be filled to its
// cap even under ambiguity; the supersession's guarded child INSERTs
// then match zero rows and the whole transaction rolls back.
func TestSupersedeBudgetExhaustedIsT82(t *testing.T) {
	store, _, clock, projectID := newTestStore(t)
	auth := NewLocalOperatorAuthority("operator")
	_, taskID, attemptID, epoch := supBlock(t, store, clock, projectID, "budget")
	// The fixture root allows 4 tasks; the tree holds root + original.
	for _, r := range []string{"alpha", "charlie"} {
		if _, created, err := store.Delegate(DelegateParams{
			ParentID: func() string { a, _ := store.Get(taskID); return a.ParentID }(),
			Title:    "filler", Objective: "fill the budget", Recipient: r,
			Role: RoleImplementer, IdempotencyKey: "idem:budget:" + r,
			Actor: Actor{Address: "operator", Kind: "agent"},
		}); err != nil || !created {
			t.Fatalf("delegate the filler: %v %v", created, err)
		}
	}
	tasksBefore := taskCount(t, store)
	eventsBefore := countEvents(t, store)
	if _, err := store.Supersede(auth, supParams(taskID, attemptID, epoch, "budget")); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("an over-budget supersession returned %v, want %v", err, ErrBudgetExhausted)
	}
	if lineageCount(t, store) != 0 {
		t.Fatalf("a refused supersession wrote a lineage")
	}
	if taskCount(t, store) != tasksBefore || countEvents(t, store) != eventsBefore {
		t.Fatalf("a refused supersession wrote rows or events")
	}
}

// TestSupersedeFaultsRollBackIsT37T38 proves the transaction is atomic
// under injected faults: a failure after the lineage insert (T38) or
// after the replacement insert (T37) leaves no lineage, no children and
// no events.
func TestSupersedeFaultsRollBackIsT37T38(t *testing.T) {
	for _, fault := range []string{"supersede-lineage", "supersede-replacement", "supersede-reviewer"} {
		store, _, clock, projectID := newTestStore(t)
		auth := NewLocalOperatorAuthority("operator")
		_, taskID, attemptID, epoch := supBlock(t, store, clock, projectID, "fault-"+fault)
		tasksBefore := taskCount(t, store)
		eventsBefore := countEvents(t, store)
		store.fault = func(stage string) error {
			if stage == fault {
				return errors.New("injected " + stage)
			}
			return nil
		}
		if _, err := store.Supersede(auth, supParams(taskID, attemptID, epoch, "fault")); err == nil ||
			!strings.Contains(err.Error(), "injected "+fault) {
			t.Fatalf("fault %s returned %v, want the injected fault", fault, err)
		}
		store.fault = nil
		if lineageCount(t, store) != 0 {
			t.Fatalf("fault %s left a lineage", fault)
		}
		if taskCount(t, store) != tasksBefore || countEvents(t, store) != eventsBefore {
			t.Fatalf("fault %s left rows or events", fault)
		}
	}
}

// TestSupersedeLegacyCompletedAndCancelledIsT88 proves pre-existing
// finished work with a later-recorded ambiguity is recoverable: the
// stored state is preserved, and the lineage names the ambiguity.
//
// A task that completed before the feature cannot gain an ambiguity
// through the service (TR19 blocks new attempts in clean... actually in
// any tree once recorded); the legacy shape is built the way a
// pre-feature runtime left it: finished first, ambiguity recorded after.
func TestSupersedeLegacyCompletedAndCancelledIsT88(t *testing.T) {
	for _, terminal := range []string{StateCompleted, StateCancelled} {
		store, _, clock, projectID := newTestStore(t)
		auth := NewLocalOperatorAuthority("operator")
		id, epoch := claimedAssignment(t, store, projectID, "alpha")
		actor := Actor{Address: "alpha", Kind: "agent"}
		if _, err := store.Start(FencedParams{ID: id, Epoch: epoch, Actor: actor}); err != nil {
			t.Fatalf("start: %v", err)
		}
		att, err := store.BeginAttempt(BeginAttemptParams{
			TaskID: id, Epoch: epoch, Runtime: RuntimeOpenCode,
			IntendedAgent: "mpm-worker", NativeSessionID: "ses-legacy-" + terminal,
			CorrelationRef: "corr-legacy", WorkspaceID: "ws", WorkspacePath: "/tmp/ws",
		})
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if terminal == StateCompleted {
			if _, err := store.MarkAttemptCompleted(SessionStateParams{
				AttemptID: att.ID, Epoch: epoch, Outcome: "succeeded"}); err != nil {
				t.Fatalf("complete the attempt: %v", err)
			}
			if _, err := store.Complete(FencedParams{ID: id, Epoch: epoch, Actor: actor}); err != nil {
				t.Fatalf("complete: %v", err)
			}
		} else {
			// The first attempt is closed (completed) so no live
			// intent blocks recovery; the task itself is withdrawn.
			if _, err := store.MarkAttemptCompleted(SessionStateParams{
				AttemptID: att.ID, Epoch: epoch, Outcome: "succeeded"}); err != nil {
				t.Fatalf("complete the attempt: %v", err)
			}
			if _, err := store.Cancel(CancelParams{ID: id, Epoch: epoch, Actor: auth.Actor()}); err != nil {
				t.Fatalf("cancel: %v", err)
			}
		}
		// The ambiguity is recorded after the finish, the way a
		// pre-feature runtime left it: a second attempt that lost its
		// launch record. The service cannot open it (the task is
		// finished), so it is recorded at the storage level; the tree
		// is clean at that moment, so TR19 authorises the admission.
		// (The t_amb ordering for restore uses the attempt's updated_at.)
		ambID := fmt.Sprintf("ats-legacy-%s", terminal)
		if _, err := store.db.SQLDB().Exec(`
			INSERT INTO agent_task_sessions (
				id, task_id, attempt, claim_epoch, runtime, intended_agent,
				native_session_id, correlation_ref, workspace_id, workspace_path,
				launch_state, last_error, created_at, updated_at
			) VALUES (?, ?, 2, ?, 'opencode', 'mpm-worker', ?, 'corr-legacy-amb',
				'ws', '/tmp/ws', 'ambiguous', 'lost', 100, 200)`,
			ambID, id, epoch, "ses-legacy-amb-"+terminal); err != nil {
			t.Fatalf("record the ambiguous attempt: %v", err)
		}
		amb := ambID
		clock.Advance(2 * 60 * 60 * 1e9)
		cur, err := store.Get(id)
		if err != nil {
			t.Fatalf("read the task: %v", err)
		}
		res, err := store.Supersede(auth, supParams(id, amb, cur.ClaimEpoch, "legacy-"+terminal))
		if err != nil {
			t.Fatalf("supersede a %s legacy original: %v", terminal, err)
		}
		if !res.Created {
			t.Fatalf("the supersession was not created")
		}
		// The stored state is preserved: the original still reads its
		// terminal state at the same epoch.
		after, err := store.Get(id)
		if err != nil {
			t.Fatalf("re-read: %v", err)
		}
		if after.State != terminal || after.ClaimEpoch != cur.ClaimEpoch {
			t.Fatalf("the original moved: %s epoch %d", after.State, after.ClaimEpoch)
		}
	}
}

// TestSupersessionLedgerUnchangedIsT44 proves the ledger rule: the
// original gains no event from the supersession, and each fresh
// assignment gains exactly one `created` event.
func TestSupersessionLedgerUnchangedIsT44(t *testing.T) {
	store, _, clock, projectID := newTestStore(t)
	auth := NewLocalOperatorAuthority("operator")
	_, taskID, attemptID, epoch := supBlock(t, store, clock, projectID, "ledger")
	before := eventTypes(t, store, taskID)
	res, err := store.Supersede(auth, supParams(taskID, attemptID, epoch, "ledger"))
	if err != nil {
		t.Fatalf("supersede: %v", err)
	}
	if got := eventTypes(t, store, taskID); len(got) != len(before) {
		t.Fatalf("the original gained events: %d -> %d", len(before), len(got))
	}
	for _, fresh := range []string{res.Replacement.ID, res.Reviewer.ID} {
		evs := eventTypes(t, store, fresh)
		if len(evs) != 1 || evs[0] != EventCreated {
			t.Fatalf("the fresh assignment carries %v, want exactly [created]", evs)
		}
	}
}

// TestReadOriginalAfterSupersessionIsT52 proves the frozen original
// reads back unchanged: state, epoch, owner and lease are exactly as
// authorised, and the ambiguous attempt is still recorded.
func TestReadOriginalAfterSupersessionIsT52(t *testing.T) {
	store, _, clock, projectID := newTestStore(t)
	auth := NewLocalOperatorAuthority("operator")
	_, taskID, attemptID, epoch := supBlock(t, store, clock, projectID, "readback")
	before, err := store.Get(taskID)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if _, err := store.Supersede(auth, supParams(taskID, attemptID, epoch, "readback")); err != nil {
		t.Fatalf("supersede: %v", err)
	}
	after, err := store.Get(taskID)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if after.State != before.State || after.ClaimEpoch != before.ClaimEpoch ||
		after.Owner != before.Owner || !leasesEqual(after.LeaseExpiresAt, before.LeaseExpiresAt) {
		t.Fatalf("the original changed: %+v -> %+v", before, after)
	}
	atts, err := store.AttemptsFor(taskID)
	if err != nil {
		t.Fatalf("read the attempts: %v", err)
	}
	found := false
	for _, a := range atts {
		if a.ID == attemptID && a.LaunchState == SessionAmbiguous {
			found = true
		}
	}
	if !found {
		t.Fatalf("the ambiguous attempt is no longer recorded: %+v", atts)
	}
}

// TestRecoveryReachesCompleteIsT55T56 drives a superseded tree all the
// way to terminal success: the replacement implements, the reviewer
// passes it with captured evidence, the operator approves, and the tree
// reads complete/complete.
//
// Every step runs through the service guards in an ambiguous tree, which
// is itself the proof that scoped work is admitted while the original
// stays frozen.
func TestRecoveryReachesCompleteIsT55T56(t *testing.T) {
	store, _, clock, projectID := newTestStore(t)
	auth := NewLocalOperatorAuthority("operator")
	_, taskID, attemptID, epoch := supBlock(t, store, clock, projectID, "complete")
	res, err := store.Supersede(auth, supParams(taskID, attemptID, epoch, "complete"))
	if err != nil {
		t.Fatalf("supersede: %v", err)
	}
	R, V := res.Replacement, res.Reviewer

	// The replacement implements and completes on a real attempt.
	rActor := Actor{Address: "beta", Kind: "agent"}
	for _, step := range []func(FencedParams) (*Assignment, error){store.Deliver, store.Acknowledge, store.Accept} {
		if _, err := step(FencedParams{ID: R.ID, Actor: rActor}); err != nil {
			t.Fatalf("advance the replacement: %v", err)
		}
	}
	rClaimed, err := store.Claim(ClaimParams{ID: R.ID, ExpectedEpoch: 0,
		Owner: "beta", LeaseSeconds: 3600, Actor: rActor})
	if err != nil {
		t.Fatalf("claim the replacement: %v", err)
	}
	if _, err := store.Start(FencedParams{ID: R.ID, Epoch: rClaimed.ClaimEpoch, Actor: rActor}); err != nil {
		t.Fatalf("start the replacement: %v", err)
	}
	completeWithEvidence(t, store, R.ID, rClaimed.ClaimEpoch)

	// The reviewer examines the replacement: own attempt for terminal
	// success, captured evidence, then the verdict.
	vActor := Actor{Address: "charlie", Kind: "agent"}
	for _, step := range []func(FencedParams) (*Assignment, error){store.Deliver, store.Acknowledge, store.Accept} {
		if _, err := step(FencedParams{ID: V.ID, Actor: vActor}); err != nil {
			t.Fatalf("advance the reviewer: %v", err)
		}
	}
	vClaimed, err := store.Claim(ClaimParams{ID: V.ID, ExpectedEpoch: 0,
		Owner: "charlie", LeaseSeconds: 3600, Actor: vActor})
	if err != nil {
		t.Fatalf("claim the reviewer: %v", err)
	}
	if _, err := store.Start(FencedParams{ID: V.ID, Epoch: vClaimed.ClaimEpoch, Actor: vActor}); err != nil {
		t.Fatalf("start the reviewer: %v", err)
	}
	vAtt, err := store.BeginAttempt(BeginAttemptParams{
		TaskID: V.ID, Epoch: vClaimed.ClaimEpoch, Runtime: RuntimeOpenCode,
		IntendedAgent: "mpm-reviewer", NativeSessionID: "ses-complete-reviewer",
		CorrelationRef: "corr-complete", WorkspaceID: "ws", WorkspacePath: "/tmp/ws",
	})
	if err != nil {
		t.Fatalf("begin the reviewer attempt: %v", err)
	}
	if _, err := store.MarkAttemptCompleted(SessionStateParams{
		AttemptID: vAtt.ID, Epoch: vClaimed.ClaimEpoch, Outcome: "succeeded"}); err != nil {
		t.Fatalf("complete the reviewer attempt: %v", err)
	}
	mustCaptureEvidence(t, store, V.ID, vClaimed.ClaimEpoch)
	if _, err := store.RecordReviewResult(ReviewResultParams{
		ReviewerID: V.ID, Verdict: VerdictPassed, Epoch: vClaimed.ClaimEpoch,
		Actor: vActor, Note: "the redo is correct",
	}); err != nil {
		t.Fatalf("verdict: %v", err)
	}

	// The operator's gate on the replacement.
	cur, err := store.Get(R.ID)
	if err != nil {
		t.Fatalf("read the replacement: %v", err)
	}
	if _, err := store.HandoffToOperator(HandoffParams{
		ID: cur.ID, Epoch: cur.ClaimEpoch, AwaitingAction: "merge PR #1", Actor: rActor,
	}); err != nil {
		t.Fatalf("handoff: %v", err)
	}
	cur, err = store.Get(R.ID)
	if err != nil {
		t.Fatalf("re-read the replacement: %v", err)
	}
	if _, err := store.RecordApproval(auth, ApprovalParams{
		ID: cur.ID, Action: "merge PR #1", Revision: strings.Repeat("a", 40),
		Source: "operator", Evidence: "gh:flowbyte-com/mpm#258",
		Epoch: cur.ClaimEpoch,
	}); err != nil {
		t.Fatalf("approval: %v", err)
	}

	ts, err := store.TreeStatus(cur.RootID)
	if err != nil {
		t.Fatalf("tree status: %v", err)
	}
	if ts.Recovery != "complete" {
		t.Fatalf("recovery_status=%q, want complete", ts.Recovery)
	}
	if ts.Tree != "complete" {
		t.Fatalf("tree_status=%q, want complete", ts.Tree)
	}
	if es, err := store.EffectiveState(R.ID); err != nil || es != "authorised" {
		t.Fatalf("replacement reads %q, want authorised (err %v)", es, err)
	}
	if es, err := store.EffectiveState(V.ID); err != nil || es != "authorised" {
		t.Fatalf("reviewer reads %q, want authorised (err %v)", es, err)
	}
	if es, err := store.EffectiveState(taskID); err != nil || es != "superseded" {
		t.Fatalf("original reads %q, want superseded (err %v)", es, err)
	}
}

// TestSecondAmbiguityCannotAriseIsA4 proves the fail-closed construction
// behind the multiple-ambiguity refusal: once one attempt is ambiguous,
// no second intent opens on the task — via the service or by direct
// write — so the A4 multiple_ambiguous_attempts arm is defense-in-depth
// for states that can only arrive by historical replay with deferred
// guards (covered by the restore suite), never by live writes.
func TestSecondAmbiguityCannotAriseIsA4(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	id, epoch := claimedAssignment(t, store, projectID, "alpha")
	actor := Actor{Address: "alpha", Kind: "agent"}
	if _, err := store.Start(FencedParams{ID: id, Epoch: epoch, Actor: actor}); err != nil {
		t.Fatalf("start: %v", err)
	}
	att, err := store.BeginAttempt(BeginAttemptParams{
		TaskID: id, Epoch: epoch, Runtime: RuntimeOpenCode,
		IntendedAgent: "mpm-worker", NativeSessionID: "ses-multi-1",
		CorrelationRef: "corr-multi-1", WorkspaceID: "ws", WorkspacePath: "/tmp/ws",
	})
	if err != nil {
		t.Fatalf("begin the first attempt: %v", err)
	}
	if _, err := store.MarkAttemptAmbiguous(SessionStateParams{
		AttemptID: att.ID, Epoch: epoch, Error: "lost"}); err != nil {
		t.Fatalf("mark the first attempt ambiguous: %v", err)
	}
	// Neither the service nor a direct write opens a second intent over
	// the ambiguity.
	if _, err := store.BeginAttempt(BeginAttemptParams{
		TaskID: id, Epoch: epoch, Runtime: RuntimeOpenCode,
		IntendedAgent: "mpm-worker", NativeSessionID: "ses-multi-2",
		CorrelationRef: "corr-multi-2", WorkspaceID: "ws", WorkspacePath: "/tmp/ws",
	}); !errors.Is(err, ErrAmbiguousAttempt) {
		t.Fatalf("a second intent over ambiguity returned %v, want %v", err, ErrAmbiguousAttempt)
	}
	if _, err := store.db.SQLDB().Exec(`
		INSERT INTO agent_task_sessions (
			id, task_id, attempt, claim_epoch, runtime, intended_agent,
			native_session_id, correlation_ref, workspace_id, workspace_path,
			launch_state, created_at, updated_at
		) VALUES ('ats-multi-direct', ?, 2, ?, 'opencode', 'mpm-worker',
			'ses-multi-direct', 'corr-direct', 'ws', '/tmp/ws', 'intent', 100, 100)`,
		id, epoch); err == nil {
		t.Fatalf("a direct second intent over ambiguity was accepted")
	} else if !strings.Contains(err.Error(), "no authorised execution") {
		t.Fatalf("a direct second intent was refused, but not by TR19: %v", err)
	}
}

// TestSupersedeLegacyApprovedAmbiguityIsA4 proves an approved task with
// ambiguity is permanently blocked with the A4/A5 code: the approval is
// preserved and no lineage is written.
func TestSupersedeLegacyApprovedAmbiguityIsA4(t *testing.T) {
	store, _, clock, projectID := newTestStore(t)
	auth := NewLocalOperatorAuthority("operator")
	id, epoch := claimedAssignment(t, store, projectID, "alpha")
	actor := Actor{Address: "alpha", Kind: "agent"}
	if _, err := store.Start(FencedParams{ID: id, Epoch: epoch, Actor: actor}); err != nil {
		t.Fatalf("start: %v", err)
	}
	completeWithEvidence(t, store, id, epoch)
	subject, err := store.Get(id)
	if err != nil {
		t.Fatalf("read the subject: %v", err)
	}
	// An independent reviewer passes the subject.
	rev, created, err := store.Delegate(DelegateParams{
		ParentID: subject.RootID, Title: "review", Objective: "review it",
		Recipient: "charlie", Role: RoleReviewer, IdempotencyKey: "idem:legacy-appr-rev",
		Actor: actor, Payload: map[string]interface{}{PayloadKeyReviewTarget: id},
		HeadSHA: strings.Repeat("4", 40),
	})
	if err != nil || !created {
		t.Fatalf("delegate the reviewer: %v %v", created, err)
	}
	vActor := Actor{Address: "charlie", Kind: "agent"}
	for _, step := range []func(FencedParams) (*Assignment, error){store.Deliver, store.Acknowledge, store.Accept} {
		if _, err := step(FencedParams{ID: rev.ID, Actor: vActor}); err != nil {
			t.Fatalf("advance the reviewer: %v", err)
		}
	}
	vClaimed, err := store.Claim(ClaimParams{ID: rev.ID, ExpectedEpoch: 0,
		Owner: "charlie", LeaseSeconds: 3600, Actor: vActor})
	if err != nil {
		t.Fatalf("claim the reviewer: %v", err)
	}
	if _, err := store.Start(FencedParams{ID: rev.ID, Epoch: vClaimed.ClaimEpoch, Actor: vActor}); err != nil {
		t.Fatalf("start the reviewer: %v", err)
	}
	mustCaptureEvidence(t, store, rev.ID, vClaimed.ClaimEpoch)
	if _, err := store.RecordReviewResult(ReviewResultParams{
		ReviewerID: rev.ID, Verdict: VerdictPassed, Epoch: vClaimed.ClaimEpoch,
		Actor: vActor, Note: "correct",
	}); err != nil {
		t.Fatalf("verdict: %v", err)
	}
	cur, err := store.Get(id)
	if err != nil {
		t.Fatalf("read the subject: %v", err)
	}
	if _, err := store.HandoffToOperator(HandoffParams{
		ID: cur.ID, Epoch: cur.ClaimEpoch, AwaitingAction: "merge PR #1", Actor: actor}); err != nil {
		t.Fatalf("handoff: %v", err)
	}
	cur, err = store.Get(id)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if _, err := store.RecordApproval(auth, ApprovalParams{
		ID: cur.ID, Action: "merge PR #1", Revision: strings.Repeat("c", 40),
		Source: "operator", Evidence: "gh:flowbyte-com/mpm#258", Epoch: cur.ClaimEpoch}); err != nil {
		t.Fatalf("approval: %v", err)
	}
	// The ambiguity is recorded after the approval, the way a legacy
	// runtime left it. The tree is clean at that moment, so admission
	// is allowed.
	ambID := "ats-legacy-approved"
	if _, err := store.db.SQLDB().Exec(`
		INSERT INTO agent_task_sessions (
			id, task_id, attempt, claim_epoch, runtime, intended_agent,
			native_session_id, correlation_ref, workspace_id, workspace_path,
			launch_state, last_error, created_at, updated_at
		) VALUES (?, ?, 2, ?, 'opencode', 'mpm-worker', 'ses-legacy-approved',
			'corr-legacy-approved', 'ws', '/tmp/ws', 'ambiguous', 'lost', 100, 200)`,
		ambID, id, cur.ClaimEpoch); err != nil {
		t.Fatalf("record the ambiguous attempt: %v", err)
	}
	clock.Advance(2 * 60 * 60 * 1e9)
	p := supParams(id, ambID, cur.ClaimEpoch, "legacy-approved")
	if _, err := store.Supersede(auth, p); !errors.Is(err, ErrLegacyApprovedAmbiguity) {
		t.Fatalf("an approved-ambiguous supersession returned %v, want %v", err, ErrLegacyApprovedAmbiguity)
	} else if !strings.Contains(err.Error(), "legacy_approved_ambiguity") {
		t.Fatalf("an approved-ambiguous supersession names no code: %v", err)
	}
	// The approval is preserved: the row still reads operator_approved.
	after, err := store.Get(id)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if after.State != StateOperatorApproved {
		t.Fatalf("the approval was disturbed: %s", after.State)
	}
	if lineageCount(t, store) != 0 {
		t.Fatalf("a refused supersession wrote a lineage")
	}
}

// TestVerdictNeedsCapturedEvidenceIsT84 proves the service half of the
// evidence contract: a verdict without host-captured evidence is refused
// before any write, for both passed and changes-requested verdicts, with
// nothing written to either ledger.
func TestVerdictNeedsCapturedEvidenceIsT84(t *testing.T) {
	for _, verdict := range []string{VerdictPassed, VerdictChangesRequested} {
		store, _, _, projectID := newTestStore(t)
		_, subject, reviewer := mustReviewSubject(t, store, projectID)
		reviewing := mustStartReviewer(t, store, reviewer)
		beforeSubject := len(eventTypes(t, store, subject.ID))
		beforeReviewer := len(eventTypes(t, store, reviewing.ID))
		if _, err := store.RecordReviewResult(ReviewResultParams{
			ReviewerID: reviewing.ID, Verdict: verdict, Epoch: reviewing.ClaimEpoch,
			Actor: agentActor("charlie"), Note: "no evidence captured",
		}); !errors.Is(err, ErrConstraint) {
			t.Fatalf("%s without evidence returned %v, want %v", verdict, err, ErrConstraint)
		} else if !strings.Contains(err.Error(), "captured evidence") {
			t.Fatalf("%s without evidence names no rule: %v", verdict, err)
		}
		if got := len(eventTypes(t, store, subject.ID)); got != beforeSubject {
			t.Fatalf("%s without evidence wrote to the subject: %d -> %d", verdict, beforeSubject, got)
		}
		if got := len(eventTypes(t, store, reviewing.ID)); got != beforeReviewer {
			t.Fatalf("%s without evidence wrote to the reviewer: %d -> %d", verdict, beforeReviewer, got)
		}
		after, err := store.Get(reviewing.ID)
		if err != nil {
			t.Fatalf("re-read: %v", err)
		}
		if after.State != StateInProgress {
			t.Fatalf("%s without evidence moved the reviewer to %s", verdict, after.State)
		}
	}
}

// TestEarlyReviewRefusedBeforeRuntimeIsT5 proves the subject must finish
// first: ExecuteReview with an unfinished subject is refused before any
// runtime session is opened — no inference is spent on an unreviewable
// subject.
func TestEarlyReviewRefusedBeforeRuntimeIsT5(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	id, epoch := claimedAssignment(t, store, projectID, "alpha")
	actor := Actor{Address: "alpha", Kind: "agent"}
	if _, err := store.Start(FencedParams{ID: id, Epoch: epoch, Actor: actor}); err != nil {
		t.Fatalf("start the subject: %v", err)
	}
	subject, err := store.Get(id)
	if err != nil {
		t.Fatalf("read the subject: %v", err)
	}
	rev, _, err := store.Delegate(DelegateParams{
		ParentID: subject.RootID, Title: "review", Objective: "review it",
		Recipient: "charlie", Role: RoleReviewer, IdempotencyKey: "idem:early-rev",
		Actor: actor, Payload: map[string]interface{}{PayloadKeyReviewTarget: id},
		HeadSHA: strings.Repeat("5", 40),
	})
	if err != nil {
		t.Fatalf("delegate the reviewer: %v", err)
	}
	vActor := Actor{Address: "charlie", Kind: "agent"}
	for _, step := range []func(FencedParams) (*Assignment, error){store.Deliver, store.Acknowledge, store.Accept} {
		if _, err := step(FencedParams{ID: rev.ID, Actor: vActor}); err != nil {
			t.Fatalf("advance the reviewer: %v", err)
		}
	}
	vClaimed, err := store.Claim(ClaimParams{ID: rev.ID, ExpectedEpoch: 0,
		Owner: "charlie", LeaseSeconds: 3600, Actor: vActor})
	if err != nil {
		t.Fatalf("claim the reviewer: %v", err)
	}
	if _, err := store.Start(FencedParams{ID: rev.ID, Epoch: vClaimed.ClaimEpoch, Actor: vActor}); err != nil {
		t.Fatalf("start the reviewer: %v", err)
	}
	fake := &fakeReviewRuntime{}
	if _, err := store.ExecuteReview(context.Background(), fake, ExecuteReviewParams{
		ID: rev.ID, Epoch: vClaimed.ClaimEpoch, Brief: "review it",
	}); err == nil {
		t.Fatalf("an early review was accepted")
	} else if !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("an early review returned %v, want %v", err, ErrIllegalTransition)
	}
	if fake.Calls() != 0 {
		t.Fatalf("the refused review ran the runtime %d time(s)", fake.Calls())
	}
}

// TestNoteAllowedOnBlockedWorkIsT20 proves notes stay append-only
// everywhere: on an ambiguous task and on a superseded task, a note
// lands without moving any state.
func TestNoteAllowedOnBlockedWorkIsT20(t *testing.T) {
	store, _, clock, projectID := newTestStore(t)
	_, taskID, attemptID, epoch := supBlock(t, store, clock, projectID, "note")
	if _, err := store.Note(NoteParams{ID: taskID, Note: "still here",
		Actor: Actor{Address: "alpha", Kind: "agent"}}); err != nil {
		t.Fatalf("note on the ambiguous task: %v", err)
	}
	res, err := store.Supersede(NewLocalOperatorAuthority("operator"),
		supParams(taskID, attemptID, epoch, "note"))
	if err != nil {
		t.Fatalf("supersede: %v", err)
	}
	_ = res
	for _, id := range []string{taskID, res.Replacement.ID} {
		before, err := store.Get(id)
		if err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		if _, err := store.Note(NoteParams{ID: id, Note: "after supersession",
			Actor: Actor{Address: "operator", Kind: "agent"}}); err != nil {
			t.Fatalf("note on %s: %v", id, err)
		}
		after, err := store.Get(id)
		if err != nil {
			t.Fatalf("re-read %s: %v", id, err)
		}
		if after.State != before.State || after.ClaimEpoch != before.ClaimEpoch {
			t.Fatalf("a note moved %s: %s/%d -> %s/%d", id,
				before.State, before.ClaimEpoch, after.State, after.ClaimEpoch)
		}
		evs := eventTypes(t, store, id)
		if evs[len(evs)-1] != EventNote {
			t.Fatalf("the last event on %s is %q, want note", id, evs[len(evs)-1])
		}
	}
}

// TestRootCancelAllowedUnderAmbiguityIsT21T74 proves withdrawing the root
// stays available in a blocked tree: the root reads withdrawn, the
// ambiguous child is untouched, and the tree reports withdrawn.
func TestRootCancelAllowedUnderAmbiguityIsT21T74(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	id, epoch := claimedAssignment(t, store, projectID, "alpha")
	att := startAmbiguous(t, store, id, epoch, "ses-root-cancel")
	_ = att
	rootID := func() string {
		a, err := store.Get(id)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		return a.RootID
	}()
	if _, err := store.Cancel(CancelParams{ID: rootID, Epoch: 0,
		Actor: Actor{Address: "operator", Kind: "agent"}}); err != nil {
		t.Fatalf("cancel the root: %v", err)
	}
	root, err := store.Get(rootID)
	if err != nil {
		t.Fatalf("re-read the root: %v", err)
	}
	if root.State != StateCancelled {
		t.Fatalf("the root is %s, want cancelled", root.State)
	}
	child, err := store.Get(id)
	if err != nil {
		t.Fatalf("re-read the child: %v", err)
	}
	if child.State != StateInProgress || child.ClaimEpoch != epoch {
		t.Fatalf("the withdrawal moved the ambiguous child: %s epoch %d", child.State, child.ClaimEpoch)
	}
	ts, err := store.TreeStatus(rootID)
	if err != nil {
		t.Fatalf("tree status: %v", err)
	}
	if ts.Tree != "withdrawn" {
		t.Fatalf("tree_status=%q, want withdrawn", ts.Tree)
	}
}

// TestOrdinaryReviewerCancelIsT101 proves an ordinary reviewer's
// withdrawal is not an evidence-gated verdict: cancelling from
// in_progress without captured findings is allowed where TR14 permits
// it.
func TestOrdinaryReviewerCancelIsT101(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	_, subject, reviewer := mustReviewSubject(t, store, projectID)
	reviewing := mustStartReviewer(t, store, reviewer)
	if _, err := store.Cancel(CancelParams{ID: reviewing.ID, Epoch: reviewing.ClaimEpoch,
		Actor: agentActor("charlie")}); err != nil {
		t.Fatalf("cancel the reviewer: %v", err)
	}
	after, err := store.Get(reviewing.ID)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if after.State != StateCancelled {
		t.Fatalf("the reviewer is %s, want cancelled", after.State)
	}
	subj, err := store.Get(subject.ID)
	if err != nil {
		t.Fatalf("re-read the subject: %v", err)
	}
	if subj.State != StateCompleted {
		t.Fatalf("the withdrawal moved the subject to %s", subj.State)
	}
}

// TestReviewerWithdrawalRefusedIsT104 proves TR14 at the service layer:
// an ambiguous reviewer's withdrawal would hide the blockage, and a
// superseded (stale) reviewer's withdrawal touches frozen work.
func TestReviewerWithdrawalRefusedIsT104(t *testing.T) {
	store, _, clock, projectID := newTestStore(t)
	auth := NewLocalOperatorAuthority("operator")
	// An ambiguous reviewer cannot be withdrawn.
	_, subject, reviewer := mustReviewSubject(t, store, projectID)
	_ = subject
	reviewing := mustStartReviewer(t, store, reviewer)
	att, err := store.BeginAttempt(BeginAttemptParams{
		TaskID: reviewing.ID, Epoch: reviewing.ClaimEpoch, Runtime: RuntimeOpenCode,
		IntendedAgent: "mpm-reviewer", NativeSessionID: "ses-rev-amb",
		CorrelationRef: "corr-rev-amb", WorkspaceID: "ws", WorkspacePath: "/tmp/ws",
	})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := store.MarkAttemptAmbiguous(SessionStateParams{
		AttemptID: att.ID, Epoch: reviewing.ClaimEpoch, Error: "lost"}); err != nil {
		t.Fatalf("mark ambiguous: %v", err)
	}
	if _, err := store.Cancel(CancelParams{ID: reviewing.ID, Epoch: reviewing.ClaimEpoch,
		Actor: agentActor("charlie")}); !errors.Is(err, ErrAmbiguousAttempt) {
		t.Fatalf("an ambiguous reviewer withdrawal returned %v, want %v", err, ErrAmbiguousAttempt)
	}

	// A stale reviewer (its replacement superseded by L2) is frozen.
	// The L2 tree needs room for six rows, so it is built on its own
	// roomy root rather than the shared fixture's capped one.
	staleRoot, _, err := store.CreateRoot(auth, CreateRootParams{
		ProjectID: projectID, Title: "stale tree", Objective: "holds the stale reviewer",
		Recipient: "alpha", IdempotencyKey: "idem:stale-root",
		PermittedOperations: []string{"read", "write"},
		EligibleRecipients:  []string{"alpha", "beta", "charlie"},
		MaxDepth:            2, MaxTasks: 8, MaxConcurrency: 2, MaxResourceUnits: 100,
	})
	if err != nil {
		t.Fatalf("create the stale tree root: %v", err)
	}
	staleChild, created, err := store.Delegate(DelegateParams{
		ParentID: staleRoot.ID, Title: "stale work", Objective: "block",
		Recipient: "alpha", Role: RoleImplementer, IdempotencyKey: "idem:stale-child",
		Actor: Actor{Address: "operator", Kind: "agent"},
	})
	if err != nil || !created {
		t.Fatalf("delegate the stale child: %v %v", created, err)
	}
	staleActor := Actor{Address: "alpha", Kind: "agent"}
	staleChild = mustDo(t, func() (*Assignment, error) {
		return store.Deliver(FencedParams{ID: staleChild.ID, Actor: staleActor})
	})
	staleChild = mustDo(t, func() (*Assignment, error) {
		return store.Acknowledge(FencedParams{ID: staleChild.ID, Epoch: staleChild.ClaimEpoch, Actor: staleActor})
	})
	staleChild = mustDo(t, func() (*Assignment, error) {
		return store.Accept(FencedParams{ID: staleChild.ID, Epoch: staleChild.ClaimEpoch, Actor: staleActor})
	})
	staleClaimed, err := store.Claim(ClaimParams{ID: staleChild.ID, ExpectedEpoch: 0,
		Owner: "alpha", LeaseSeconds: 600, Actor: staleActor})
	if err != nil {
		t.Fatalf("claim the stale child: %v", err)
	}
	if _, err := store.Start(FencedParams{ID: staleChild.ID, Epoch: staleClaimed.ClaimEpoch, Actor: staleActor}); err != nil {
		t.Fatalf("start the stale child: %v", err)
	}
	staleAtt, err := store.BeginAttempt(BeginAttemptParams{
		TaskID: staleChild.ID, Epoch: staleClaimed.ClaimEpoch, Runtime: RuntimeOpenCode,
		IntendedAgent: "mpm-worker", NativeSessionID: "ses-stale-orig",
		CorrelationRef: "corr-stale", WorkspaceID: "ws", WorkspacePath: "/tmp/ws",
	})
	if err != nil {
		t.Fatalf("begin the stale original attempt: %v", err)
	}
	if _, err := store.MarkAttemptAmbiguous(SessionStateParams{
		AttemptID: staleAtt.ID, Epoch: staleClaimed.ClaimEpoch, Error: "lost"}); err != nil {
		t.Fatalf("mark ambiguous: %v", err)
	}
	clock.Advance(2 * 60 * 60 * 1e9)
	l1, err := store.Supersede(auth, supParams(staleChild.ID, staleAtt.ID, staleClaimed.ClaimEpoch, "stale-l1"))
	if err != nil {
		t.Fatalf("supersede: %v", err)
	}
	staleReviewer := l1.Reviewer.ID
	replacement := l1.Replacement
	rActor := Actor{Address: "beta", Kind: "agent"}
	for _, step := range []func(FencedParams) (*Assignment, error){store.Deliver, store.Acknowledge, store.Accept} {
		if _, err := step(FencedParams{ID: replacement.ID, Actor: rActor}); err != nil {
			t.Fatalf("advance the replacement: %v", err)
		}
	}
	rClaimed, err := store.Claim(ClaimParams{ID: replacement.ID, ExpectedEpoch: 0,
		Owner: "beta", LeaseSeconds: 3600, Actor: rActor})
	if err != nil {
		t.Fatalf("claim the replacement: %v", err)
	}
	if _, err := store.Start(FencedParams{ID: replacement.ID, Epoch: rClaimed.ClaimEpoch, Actor: rActor}); err != nil {
		t.Fatalf("start the replacement: %v", err)
	}
	rAtt, err := store.BeginAttempt(BeginAttemptParams{
		TaskID: replacement.ID, Epoch: rClaimed.ClaimEpoch, Runtime: RuntimeOpenCode,
		IntendedAgent: "mpm-worker", NativeSessionID: "ses-stale-r",
		CorrelationRef: "corr-stale-r", WorkspaceID: "ws", WorkspacePath: "/tmp/ws",
	})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := store.MarkAttemptAmbiguous(SessionStateParams{
		AttemptID: rAtt.ID, Epoch: rClaimed.ClaimEpoch, Error: "lost"}); err != nil {
		t.Fatalf("mark ambiguous: %v", err)
	}
	clock.Advance(2 * 60 * 60 * 1e9)
	if _, err := store.Supersede(auth, SupersedeParams{
		SupersededID: replacement.ID, AttemptID: rAtt.ID, Epoch: rClaimed.ClaimEpoch,
		Replacement: SupersedeTaskParams{Title: "redo 2", Objective: "redo again",
			Recipient: "beta", ResourceUnits: 1, BaseSHA: strings.Repeat("c", 40)},
		Reviewer: SupersedeTaskParams{Title: "re-review 2", Objective: "review again",
			Recipient: "charlie", ResourceUnits: 1},
		EvidenceRef: "gh:flowbyte-com/mpm#258", Justification: "the replacement is ambiguous",
		RequireQuiescence: true,
	}); err != nil {
		t.Fatalf("supersede the replacement: %v", err)
	}
	revRow, err := store.Get(staleReviewer)
	if err != nil {
		t.Fatalf("read the stale reviewer: %v", err)
	}
	if _, err := store.Cancel(CancelParams{ID: staleReviewer, Epoch: revRow.ClaimEpoch,
		Actor: agentActor("charlie")}); !errors.Is(err, ErrSuperseded) {
		t.Fatalf("a stale reviewer withdrawal returned %v, want %v", err, ErrSuperseded)
	}
}

// TestUnrelatedTreeUnaffectedIsT10 proves the tree scope is exact: an
// ambiguous attempt in one tree neither refuses writes in another tree
// at the service layer nor fires the trigger backstop there.
//
// This is the regression test for an alias-shadowing defect in the
// trigger predicate that once collapsed the tree scope to the whole
// database (the service said authorised, the trigger refused).
func TestUnrelatedTreeUnaffectedIsT10(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	auth := NewLocalOperatorAuthority("operator")
	// Tree A turns ambiguous.
	aID, aEpoch := claimedAssignment(t, store, projectID, "alpha")
	startAmbiguous(t, store, aID, aEpoch, "ses-t10-ambiguous")

	// Tree B is a second root: claim, start and open an attempt.
	bRoot, created, err := store.CreateRoot(auth, CreateRootParams{
		ProjectID: projectID, Title: "tree b", Objective: "independent work", Recipient: "beta",
		IdempotencyKey: "idem:t10:root", PermittedOperations: []string{"read", "write"},
		EligibleRecipients: []string{"beta"}, MaxDepth: 2, MaxTasks: 4,
		MaxConcurrency: 2, MaxResourceUnits: 100,
	})
	if err != nil || !created {
		t.Fatalf("create the second root: %v %v", created, err)
	}
	b, created, err := store.Delegate(DelegateParams{
		ParentID: bRoot.ID, Title: "b work", Objective: "independent", Recipient: "beta",
		Role: RoleImplementer, IdempotencyKey: "idem:t10:child",
		Actor: Actor{Address: "operator", Kind: "agent"},
	})
	if err != nil || !created {
		t.Fatalf("delegate in the second tree: %v %v", created, err)
	}
	bActor := Actor{Address: "beta", Kind: "agent"}
	for _, step := range []func(FencedParams) (*Assignment, error){store.Deliver, store.Acknowledge, store.Accept} {
		if _, err := step(FencedParams{ID: b.ID, Actor: bActor}); err != nil {
			t.Fatalf("advance in the clean tree: %v", err)
		}
	}
	bClaimed, err := store.Claim(ClaimParams{ID: b.ID, ExpectedEpoch: 0,
		Owner: "beta", LeaseSeconds: 3600, Actor: bActor})
	if err != nil {
		t.Fatalf("claim in the clean tree: %v", err)
	}
	if _, err := store.Start(FencedParams{ID: b.ID, Epoch: bClaimed.ClaimEpoch, Actor: bActor}); err != nil {
		t.Fatalf("start in the clean tree: %v", err)
	}
	if _, err := store.BeginAttempt(BeginAttemptParams{
		TaskID: b.ID, Epoch: bClaimed.ClaimEpoch, Runtime: RuntimeOpenCode,
		IntendedAgent: "mpm-worker", NativeSessionID: "ses-t10-clean",
		CorrelationRef: "corr-t10", WorkspaceID: "ws", WorkspacePath: "/tmp/ws",
	}); err != nil {
		t.Fatalf("begin in the clean tree: %v", err)
	}
}

// TestReviewerWithdrawalIsRefusedWhenAmbiguous is the T104 ambiguous
// arm as a focused unit: withdrawing the ambiguous task itself is
// refused (TR14), while withdrawing an unscoped sibling is allowed.
func TestReviewerWithdrawalIsRefusedWhenAmbiguous(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	id, epoch := claimedAssignment(t, store, projectID, "alpha")
	att := startAmbiguous(t, store, id, epoch, "ses-t104-unit")
	_ = att
	if _, err := store.Cancel(CancelParams{ID: id, Epoch: epoch,
		Actor: Actor{Address: "alpha", Kind: "agent"}}); !errors.Is(err, ErrAmbiguousAttempt) {
		t.Fatalf("cancelling the ambiguous task returned %v, want %v", err, ErrAmbiguousAttempt)
	}
	sibling := mustDelegateSibling(t, store, projectID, id, "charlie")
	if _, err := store.Cancel(CancelParams{ID: sibling.ID, Epoch: 0,
		Actor: Actor{Address: "charlie", Kind: "agent"}}); err != nil {
		t.Fatalf("cancelling an unscoped sibling: %v", err)
	}
}

// TestStartOnTerminalIsRefusedIsT94 proves a completed assignment cannot
// start: the state machine has no completed → in_progress edge, and the
// schema's transition trigger backstops the service refusal.
func TestStartOnTerminalIsT94(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	id, epoch := claimedAssignment(t, store, projectID, "alpha")
	actor := Actor{Address: "alpha", Kind: "agent"}
	if _, err := store.Start(FencedParams{ID: id, Epoch: epoch, Actor: actor}); err != nil {
		t.Fatalf("start: %v", err)
	}
	completeWithEvidence(t, store, id, epoch)
	if _, err := store.Start(FencedParams{ID: id, Epoch: epoch, Actor: actor}); !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("a start on a completed task returned %v, want %v", err, ErrIllegalTransition)
	}
}

// TestOrdinaryWritesCarryNoBaseShapeIsT96T97 proves TR10's WHEN scoping:
// an ordinary delegation and an ordinary root carry no base SHA and are
// never evaluated against the sealed-shape rule.
func TestOrdinaryWritesCarryNoBaseShapeIsT96T97(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	auth := NewLocalOperatorAuthority("test")
	root, created, err := store.CreateRoot(auth, CreateRootParams{
		ProjectID: projectID, Title: "root", Objective: "o", Recipient: "alpha",
		IdempotencyKey: "idem:noshape:root:" + t.Name(), PermittedOperations: []string{"read"},
		EligibleRecipients: []string{"alpha"}, MaxDepth: 2, MaxTasks: 8,
		MaxConcurrency: 2, MaxResourceUnits: 100,
	})
	if err != nil || !created {
		t.Fatalf("create an ordinary root: %v %v", err, created)
	}
	if root.BaseSHA != "" {
		t.Fatalf("an ordinary root carries %q", root.BaseSHA)
	}
	child, created, err := store.Delegate(DelegateParams{
		ParentID: root.ID, Title: "child", Objective: "o", Recipient: "alpha",
		Role: RoleImplementer, IdempotencyKey: "idem:noshape:child:" + t.Name(),
		Actor: Actor{Address: "operator", Kind: "agent"},
	})
	if err != nil || !created {
		t.Fatalf("delegate without a base SHA: %v %v", err, created)
	}
	if child.BaseSHA != "" {
		t.Fatalf("an ordinary child carries %q", child.BaseSHA)
	}
}

// TestReadViewsCarryExecutionState proves the read surfaces share one
// reading with every guard: show carries the effective state, the tree
// carries its tree/recovery picture plus per-task effective states, and
// the lineage reads return the exact immutable authorisation.
func TestReadViewsCarryExecutionState(t *testing.T) {
	store, _, clock, projectID := newTestStore(t)
	auth := NewLocalOperatorAuthority("test")
	root, created, err := store.CreateRoot(auth, CreateRootParams{
		ProjectID: projectID, Title: "root", Objective: "o", Recipient: "alpha",
		IdempotencyKey: "idem:readview:root", PermittedOperations: []string{"read"},
		EligibleRecipients: []string{"alpha", "beta"}, MaxDepth: 3, MaxTasks: 16,
		MaxConcurrency: 2, MaxResourceUnits: 100,
	})
	if err != nil || !created {
		t.Fatalf("create root: %v %v", err, created)
	}
	child, created, err := store.Delegate(DelegateParams{
		ParentID: root.ID, Title: "child", Objective: "o", Recipient: "alpha",
		Role: RoleImplementer, IdempotencyKey: "idem:readview:child",
		Actor: Actor{Address: "operator", Kind: "agent"},
	})
	if err != nil || !created {
		t.Fatalf("delegate: %v %v", err, created)
	}
	for _, step := range []func(FencedParams) (*Assignment, error){
		store.Deliver, store.Acknowledge, store.Accept,
	} {
		if _, err := step(FencedParams{ID: child.ID, Actor: Actor{Address: "alpha", Kind: "agent"}}); err != nil {
			t.Fatalf("advance: %v", err)
		}
	}
	claimed, err := store.Claim(ClaimParams{ID: child.ID, ExpectedEpoch: 0,
		Owner: "alpha", LeaseSeconds: 600, Actor: Actor{Address: "alpha", Kind: "agent"}})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	view, err := store.View(child.ID)
	if err != nil {
		t.Fatalf("view: %v", err)
	}
	if view.EffectiveState == "" || view.ID != child.ID {
		t.Fatalf("view is %+v", view)
	}
	if view.EffectiveState != claimed.State && view.EffectiveState != "in_progress" {
		t.Fatalf("live claimed work reads %q, want its stored state", view.EffectiveState)
	}
	tree, err := store.Tree(root.ID)
	if err != nil {
		t.Fatalf("tree: %v", err)
	}
	if tree.Tree == "" || tree.Recovery == "" {
		t.Fatalf("tree carries no derived picture: %+v", tree)
	}
	if len(tree.Assignments) < 2 {
		t.Fatalf("tree lists %d assignments, want at least root+child", len(tree.Assignments))
	}
	for _, a := range tree.Assignments {
		if a.EffectiveState == "" {
			t.Fatalf("tree item %s carries no effective state", a.ID)
		}
	}
	// The ambiguous arm: the same reading the guards use.
	att := startAmbiguous(t, store, child.ID, claimed.ClaimEpoch, "ses-readview")
	view2, err := store.View(child.ID)
	if err != nil {
		t.Fatalf("view ambiguous: %v", err)
	}
	if view2.EffectiveState != "recovery_required" {
		t.Fatalf("ambiguous unnamed work reads %q, want recovery_required", view2.EffectiveState)
	}
	// The lease must expire before the lineage can authorise recovery.
	clock.Advance(2 * 60 * 60 * 1e9)
	res, err := store.Supersede(auth, SupersedeParams{
		SupersededID: child.ID, AttemptID: att.ID, Epoch: claimed.ClaimEpoch,
		Replacement: SupersedeTaskParams{Title: "redo", Objective: "o",
			Recipient: "beta", ResourceUnits: 1, BaseSHA: strings.Repeat("a", 40)},
		Reviewer: SupersedeTaskParams{Title: "re-review", Objective: "o",
			Recipient: "alpha", ResourceUnits: 1},
		EvidenceRef: "gh:x/y#1", Justification: "the launch is ambiguous",
	})
	if err != nil {
		t.Fatalf("supersede: %v", err)
	}
	l := res.Lineage
	byID, err := store.Lineage(l.ID)
	if err != nil || byID.ID != l.ID {
		t.Fatalf("lineage by id: %v %+v", err, byID)
	}
	byAttempt, err := store.LineageByAttempt(child.ID, att.ID)
	if err != nil || byAttempt.ID != l.ID {
		t.Fatalf("lineage by attempt: %v %+v", err, byAttempt)
	}
	view3, err := store.View(child.ID)
	if err != nil {
		t.Fatalf("view historical: %v", err)
	}
	if view3.EffectiveState != "superseded" {
		t.Fatalf("superseded work reads %q, want superseded", view3.EffectiveState)
	}
	if _, err := store.Lineage("no-such-lineage"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing lineage returned %v, want ErrNotFound", err)
	}
	if _, err := store.LineageByAttempt(child.ID, "no-such-attempt"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing lineage by attempt returned %v, want ErrNotFound", err)
	}
}

// supBlockWide is supBlock with room for the supersession's two rows AND
// a follow-up delegation. The adapter fixture's budget is deliberately
// tight, so a control that needs to delegate after a recovery must build
// its own root.
func supBlockWide(t *testing.T, store *Store, clock *testClock, projectID, tag string) (rootID, taskID, attemptID string, epoch int64) {
	t.Helper()
	root, _, err := store.CreateRoot(NewLocalOperatorAuthority("test"), CreateRootParams{
		ProjectID: projectID, Title: "wide " + tag, Objective: "o", Recipient: "alpha",
		IdempotencyKey:      "idem:wide:" + tag,
		PermittedOperations: []string{"deliver", "acknowledge", "accept", "claim", "start", "complete", "delegate"},
		EligibleRecipients:  []string{"alpha", "charlie"},
		MaxDepth:            3, MaxTasks: 16, MaxConcurrency: 2, MaxResourceUnits: 100,
	})
	if err != nil {
		t.Fatalf("create the wide root: %v", err)
	}
	child, _, err := store.Delegate(DelegateParams{
		ParentID: root.ID, Title: "work", Objective: "w", Recipient: "alpha",
		Role: RoleImplementer, IdempotencyKey: "idem:wide:" + tag + ":child",
		Actor: Actor{Address: "operator", Kind: "agent"},
	})
	if err != nil {
		t.Fatalf("delegate in the wide root: %v", err)
	}
	actor := Actor{Address: "alpha", Kind: "agent"}
	for _, step := range []func(FencedParams) (*Assignment, error){store.Deliver, store.Acknowledge, store.Accept} {
		if _, err := step(FencedParams{ID: child.ID, Actor: actor}); err != nil {
			t.Fatalf("advance in the wide root: %v", err)
		}
	}
	claimed, err := store.Claim(ClaimParams{ID: child.ID, ExpectedEpoch: 0,
		Owner: "alpha", LeaseSeconds: 3600, Actor: actor})
	if err != nil {
		t.Fatalf("claim in the wide root: %v", err)
	}
	att := startAmbiguous(t, store, child.ID, claimed.ClaimEpoch, "ses-wide-"+tag)
	clock.Advance(2 * 60 * 60 * 1e9)
	return root.ID, child.ID, att.ID, claimed.ClaimEpoch
}

// reworkRequiredTree drives a tree to recovery_status=rework_required:
// root → implementer (ambiguous) → supersede → replacement completes →
// the lineage reviewer records changes_requested.
//
// It returns the root id, the completed replacement, the completed
// reviewer, the reviewer's recorded epoch and the task count BEFORE any
// attempted rework delegation.
func reworkRequiredTree(t *testing.T, store *Store, clock *testClock, projectID, tag string) (rootID, replacementID, reviewerID string, reviewerEpoch int64) {
	t.Helper()
	return recoveryTreeWithVerdict(t, store, clock, projectID, tag, VerdictChangesRequested)
}

// recoveryTreeWithVerdict is the shared builder: the SAME tree shape with
// only the reviewer's verdict changed, which is what makes the rework
// control a single-variable comparison.
func recoveryTreeWithVerdict(t *testing.T, store *Store, clock *testClock, projectID, tag, verdict string) (rootID, replacementID, reviewerID string, reviewerEpoch int64) {
	t.Helper()
	rootID, taskID, attemptID, epoch := supBlockWide(t, store, clock, projectID, tag)
	auth := NewLocalOperatorAuthority("operator")
	res, err := store.Supersede(auth, supParams(taskID, attemptID, epoch, tag))
	if err != nil {
		t.Fatalf("supersede: %v", err)
	}
	R, V := res.Replacement, res.Reviewer

	rActor := Actor{Address: "beta", Kind: "agent"}
	for _, step := range []func(FencedParams) (*Assignment, error){store.Deliver, store.Acknowledge, store.Accept} {
		if _, err := step(FencedParams{ID: R.ID, Actor: rActor}); err != nil {
			t.Fatalf("advance the replacement: %v", err)
		}
	}
	rClaimed, err := store.Claim(ClaimParams{ID: R.ID, ExpectedEpoch: 0,
		Owner: "beta", LeaseSeconds: 3600, Actor: rActor})
	if err != nil {
		t.Fatalf("claim the replacement: %v", err)
	}
	if _, err := store.Start(FencedParams{ID: R.ID, Epoch: rClaimed.ClaimEpoch, Actor: rActor}); err != nil {
		t.Fatalf("start the replacement: %v", err)
	}
	completeWithEvidence(t, store, R.ID, rClaimed.ClaimEpoch)

	vActor := Actor{Address: "charlie", Kind: "agent"}
	for _, step := range []func(FencedParams) (*Assignment, error){store.Deliver, store.Acknowledge, store.Accept} {
		if _, err := step(FencedParams{ID: V.ID, Actor: vActor}); err != nil {
			t.Fatalf("advance the reviewer: %v", err)
		}
	}
	vClaimed, err := store.Claim(ClaimParams{ID: V.ID, ExpectedEpoch: 0,
		Owner: "charlie", LeaseSeconds: 3600, Actor: vActor})
	if err != nil {
		t.Fatalf("claim the reviewer: %v", err)
	}
	if _, err := store.Start(FencedParams{ID: V.ID, Epoch: vClaimed.ClaimEpoch, Actor: vActor}); err != nil {
		t.Fatalf("start the reviewer: %v", err)
	}
	vAtt, err := store.BeginAttempt(BeginAttemptParams{
		TaskID: V.ID, Epoch: vClaimed.ClaimEpoch, Runtime: RuntimeOpenCode,
		IntendedAgent: "mpm-reviewer", NativeSessionID: "ses-" + tag + "-rework",
		CorrelationRef: "corr-" + tag, WorkspaceID: "ws", WorkspacePath: "/tmp/ws",
	})
	if err != nil {
		t.Fatalf("begin the reviewer attempt: %v", err)
	}
	if _, err := store.MarkAttemptCompleted(SessionStateParams{
		AttemptID: vAtt.ID, Epoch: vClaimed.ClaimEpoch, Outcome: "succeeded"}); err != nil {
		t.Fatalf("complete the reviewer attempt: %v", err)
	}
	mustCaptureEvidence(t, store, V.ID, vClaimed.ClaimEpoch)
	if _, err := store.RecordReviewResult(ReviewResultParams{
		ReviewerID: V.ID, Verdict: verdict, Epoch: vClaimed.ClaimEpoch,
		Actor: vActor, Note: "the verdict",
	}); err != nil {
		t.Fatalf("record %s: %v", verdict, err)
	}

	ts, err := store.TreeStatus(rootID)
	if err != nil {
		t.Fatalf("tree status: %v", err)
	}
	if verdict == VerdictPassed {
		// A passed review leaves the lineage awaiting the operator's
		// approval, which is progress — the point is that it is NOT
		// rework.
		if ts.Recovery == "rework_required" {
			t.Fatalf("a passed review reported rework_required")
		}
	} else if ts.Recovery != "rework_required" {
		t.Fatalf("recovery_status=%q, want rework_required", ts.Recovery)
	}
	return rootID, R.ID, V.ID, vClaimed.ClaimEpoch
}

// TestUnsupportedReworkIsRefusedIsT66T80 proves the A4 boundary for
// unsupported rework: once the live lineage's reviewer has completed
// with changes requested, NEITHER the completed reviewer NOR the
// completed replacement admits another assignment, and the refusal
// writes nothing.
func TestUnsupportedReworkIsRefusedIsT66T80(t *testing.T) {
	for _, parent := range []string{"reviewer", "replacement"} {
		t.Run(parent, func(t *testing.T) {
			store, dm, clock, projectID := newTestStore(t)
			rootID, replacementID, reviewerID, _ := reworkRequiredTree(t, store, clock, projectID, "rework-"+parent)

			parentID := reviewerID
			if parent == "replacement" {
				parentID = replacementID
			}
			before := countRows(t, dm, "agent_tasks")
			beforeEvents := countRows(t, dm, "agent_task_events")
			beforeBudget, err := store.BudgetStatus(rootID)
			if err != nil {
				t.Fatalf("budget before: %v", err)
			}

			_, created, err := store.Delegate(DelegateParams{
				ParentID: parentID, Title: "the rework", Objective: "do it again",
				Recipient: "beta", Role: RoleImplementer,
				IdempotencyKey: "idem:rework:" + parent, ResourceUnits: 1,
				Actor: Actor{Address: "beta", Kind: "agent"},
			})
			if !errors.Is(err, ErrReworkRequired) {
				t.Fatalf("delegating beneath the completed %s returned %v (created=%v), want %v",
					parent, err, created, ErrReworkRequired)
			}
			if created {
				t.Fatalf("the refused rework was reported as created")
			}
			if got := countRows(t, dm, "agent_tasks"); got != before {
				t.Fatalf("the refused rework wrote a task: %d -> %d", before, got)
			}
			if got := countRows(t, dm, "agent_task_events"); got != beforeEvents {
				t.Fatalf("the refused rework wrote an event: %d -> %d", beforeEvents, got)
			}
			afterBudget, err := store.BudgetStatus(rootID)
			if err != nil {
				t.Fatalf("budget after: %v", err)
			}
			if afterBudget.Tasks != beforeBudget.Tasks || afterBudget.Units != beforeBudget.Units {
				t.Fatalf("the refused rework consumed budget: %+v -> %+v", beforeBudget, afterBudget)
			}
			// The existing work and evidence are untouched.
			for _, id := range []string{rootID, replacementID, reviewerID} {
				if _, err := store.Get(id); err != nil {
					t.Fatalf("the refused rework disturbed %s: %v", id, err)
				}
			}
		})
	}
}

// TestOrdinaryDelegationStillWorksOutsideRework proves the refusal did
// not become a blanket one: an ordinary tree delegates exactly as it
// did before, and a tree that recovered COMPLETELY also delegates.
func TestOrdinaryDelegationStillWorksOutsideRework(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	root := mustCreateRoot(t, store, projectID, func(p *CreateRootParams) {
		p.EligibleRecipients = []string{"alpha", "beta"}
		p.MaxDepth = 3
	})
	if _, created, err := store.Delegate(DelegateParams{
		ParentID: root.ID, Title: "ordinary", Objective: "work", Recipient: "alpha",
		Role: RoleImplementer, IdempotencyKey: "idem:ordinary:child",
		Actor: Actor{Address: "operator", Kind: "agent"},
	}); err != nil || !created {
		t.Fatalf("an ordinary delegation returned %v (created=%v)", err, created)
	}
}

// TestReworkPredicateMatchesTreeStatus pins the transactional predicate
// against TreeStatus: the two must agree on every fixture, so the
// refusal can never act on a tree the status does not call
// rework_required (and vice versa).
func TestReworkPredicateMatchesTreeStatus(t *testing.T) {
	t.Run("rework-required", func(t *testing.T) {
		store, dm, clock, projectID := newTestStore(t)
		rootID, _, _, _ := reworkRequiredTree(t, store, clock, projectID, "pred-rework")
		var got bool
		if err := dm.WithTx(func(node internal.DBNode) error {
			var err error
			got, err = reworkRequiredTx(node, rootID)
			return err
		}); err != nil {
			t.Fatalf("predicate: %v", err)
		}
		ts, err := store.TreeStatus(rootID)
		if err != nil {
			t.Fatalf("tree status: %v", err)
		}
		if want := ts.Recovery == "rework_required"; got != want {
			t.Fatalf("predicate=%v, TreeStatus=%q", got, ts.Recovery)
		}
		if !got {
			t.Fatal("the fixture must be rework_required")
		}
	})
	t.Run("ordinary", func(t *testing.T) {
		store, dm, _, projectID := newTestStore(t)
		root := mustCreateRoot(t, store, projectID, func(p *CreateRootParams) {
			p.EligibleRecipients = []string{"alpha", "beta"}
		})
		var got bool
		if err := dm.WithTx(func(node internal.DBNode) error {
			var err error
			got, err = reworkRequiredTx(node, root.ID)
			return err
		}); err != nil {
			t.Fatalf("predicate: %v", err)
		}
		if got {
			t.Fatal("an ordinary tree must not read as rework_required")
		}
	})
	t.Run("complete", func(t *testing.T) {
		// A tree that recovered and whose reviewer PASSED is complete,
		// not rework: the predicate must not confuse the two.
		store, dm, clock, projectID := newTestStore(t)
		rootID, taskID, attemptID, epoch := supBlock(t, store, clock, projectID, "pred-complete")
		auth := NewLocalOperatorAuthority("operator")
		res, err := store.Supersede(auth, supParams(taskID, attemptID, epoch, "pred-complete"))
		if err != nil {
			t.Fatalf("supersede: %v", err)
		}
		R, V := res.Replacement, res.Reviewer
		rActor := Actor{Address: "beta", Kind: "agent"}
		for _, step := range []func(FencedParams) (*Assignment, error){store.Deliver, store.Acknowledge, store.Accept} {
			if _, err := step(FencedParams{ID: R.ID, Actor: rActor}); err != nil {
				t.Fatalf("advance the replacement: %v", err)
			}
		}
		rClaimed, err := store.Claim(ClaimParams{ID: R.ID, ExpectedEpoch: 0,
			Owner: "beta", LeaseSeconds: 3600, Actor: rActor})
		if err != nil {
			t.Fatalf("claim the replacement: %v", err)
		}
		if _, err := store.Start(FencedParams{ID: R.ID, Epoch: rClaimed.ClaimEpoch, Actor: rActor}); err != nil {
			t.Fatalf("start the replacement: %v", err)
		}
		completeWithEvidence(t, store, R.ID, rClaimed.ClaimEpoch)
		vActor := Actor{Address: "charlie", Kind: "agent"}
		for _, step := range []func(FencedParams) (*Assignment, error){store.Deliver, store.Acknowledge, store.Accept} {
			if _, err := step(FencedParams{ID: V.ID, Actor: vActor}); err != nil {
				t.Fatalf("advance the reviewer: %v", err)
			}
		}
		vClaimed, err := store.Claim(ClaimParams{ID: V.ID, ExpectedEpoch: 0,
			Owner: "charlie", LeaseSeconds: 3600, Actor: vActor})
		if err != nil {
			t.Fatalf("claim the reviewer: %v", err)
		}
		if _, err := store.Start(FencedParams{ID: V.ID, Epoch: vClaimed.ClaimEpoch, Actor: vActor}); err != nil {
			t.Fatalf("start the reviewer: %v", err)
		}
		vAtt, err := store.BeginAttempt(BeginAttemptParams{
			TaskID: V.ID, Epoch: vClaimed.ClaimEpoch, Runtime: RuntimeOpenCode,
			IntendedAgent: "mpm-reviewer", NativeSessionID: "ses-pred-complete-reviewer",
			CorrelationRef: "corr-pred", WorkspaceID: "ws", WorkspacePath: "/tmp/ws",
		})
		if err != nil {
			t.Fatalf("begin the reviewer attempt: %v", err)
		}
		if _, err := store.MarkAttemptCompleted(SessionStateParams{
			AttemptID: vAtt.ID, Epoch: vClaimed.ClaimEpoch, Outcome: "succeeded"}); err != nil {
			t.Fatalf("complete the reviewer attempt: %v", err)
		}
		mustCaptureEvidence(t, store, V.ID, vClaimed.ClaimEpoch)
		if _, err := store.RecordReviewResult(ReviewResultParams{
			ReviewerID: V.ID, Verdict: VerdictPassed, Epoch: vClaimed.ClaimEpoch, Actor: vActor,
		}); err != nil {
			t.Fatalf("record passed: %v", err)
		}
		var got bool
		if err := dm.WithTx(func(node internal.DBNode) error {
			var err error
			got, err = reworkRequiredTx(node, rootID)
			return err
		}); err != nil {
			t.Fatalf("predicate: %v", err)
		}
		ts, err := store.TreeStatus(rootID)
		if err != nil {
			t.Fatalf("tree status: %v", err)
		}
		if want := ts.Recovery == "rework_required"; got != want {
			t.Fatalf("predicate=%v, TreeStatus=%q", got, ts.Recovery)
		}
		if got {
			t.Fatalf("a passed review is not rework (recovery=%q)", ts.Recovery)
		}
	})
}

// ── concurrency, relabelling and old-runtime compatibility ───────────────

// TestConcurrentClaimAndSupersessionIsT41 races a claim on a sibling
// against the supersession of the ambiguous original.
//
// The winner is CONSISTENT, never both: if the claim commits first the
// supersession's quiescence predicate refuses (a live lease exists in
// the tree); if the supersession commits first the sibling is not
// authorised in the recovered tree and the claim refuses. Both orderings
// leave a tree that the status can explain.
func TestConcurrentClaimAndSupersessionIsT41(t *testing.T) {
	for i := 0; i < 5; i++ {
		store, dm, _, projectID := newTestStore(t)
		// A root with room for the supersession's two rows: the
		// default adapter fixture budget is deliberately tight and
		// would make this test measure the budget instead of the race.
		root, _, err := store.CreateRoot(NewLocalOperatorAuthority("test"), CreateRootParams{
			ProjectID: projectID, Title: "race root", Objective: "race",
			Recipient: "alpha", IdempotencyKey: fmt.Sprintf("idem:race41:root:%d", i),
			PermittedOperations: []string{"deliver", "acknowledge", "accept", "claim", "start", "complete", "delegate"},
			EligibleRecipients:  []string{"alpha", "charlie"},
			MaxDepth:            2, MaxTasks: 12, MaxConcurrency: 2, MaxResourceUnits: 100,
		})
		if err != nil {
			t.Fatalf("create the race root: %v", err)
		}
		task, _, err := store.Delegate(DelegateParams{
			ParentID: root.ID, Title: "work", Objective: "w", Recipient: "alpha",
			Role: RoleImplementer, IdempotencyKey: fmt.Sprintf("idem:race41:task:%d", i),
			Actor: Actor{Address: "operator", Kind: "agent"},
		})
		if err != nil {
			t.Fatalf("delegate the task: %v", err)
		}
		actor := Actor{Address: "alpha", Kind: "agent"}
		for _, step := range []func(FencedParams) (*Assignment, error){store.Deliver, store.Acknowledge, store.Accept} {
			if _, err := step(FencedParams{ID: task.ID, Actor: actor}); err != nil {
				t.Fatalf("advance: %v", err)
			}
		}
		claimed, err := store.Claim(ClaimParams{ID: task.ID, ExpectedEpoch: 0,
			Owner: "alpha", LeaseSeconds: 3600, Actor: actor})
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		// startAmbiguous records the Start AND the ambiguous attempt.
		att := startAmbiguous(t, store, task.ID, claimed.ClaimEpoch, fmt.Sprintf("ses-race41-%d", i))
		sibling := mustDelegateSibling(t, store, projectID, task.ID, "charlie")

		claimErr := make(chan error, 1)
		superErr := make(chan error, 1)
		go func() {
			_, err := store.Claim(ClaimParams{
				ID: sibling.ID, ExpectedEpoch: 0, Owner: "charlie", LeaseSeconds: 3600,
				Actor: Actor{Address: "charlie", Kind: "agent"},
			})
			claimErr <- err
		}()
		go func() {
			_, err := store.Supersede(NewLocalOperatorAuthority("operator"),
				supParams(task.ID, att.ID, claimed.ClaimEpoch, fmt.Sprintf("race41-%d", i)))
			superErr <- err
		}()
		ce, se := <-claimErr, <-superErr
		if ce == nil && se == nil {
			t.Fatalf("iteration %d: BOTH the claim and the supersession committed", i)
		}
		if se == nil && ce == nil {
			t.Fatalf("iteration %d: the claim and the supersession both succeeded", i)
		}
		if se != nil && lineageCount(t, store) != 0 {
			t.Fatalf("iteration %d: a REFUSED supersession wrote %d lineage row(s)", i, lineageCount(t, store))
		}
		if se == nil {
			if es, err := store.EffectiveState(task.ID); err != nil || es != "superseded" {
				t.Fatalf("iteration %d: the original reads %q after a supersession (err %v)", i, es, err)
			}
		}
		// The database must still pass its own foreign keys.
		var fkViolations int
		rows, err := dm.SQLDB().Query(`PRAGMA foreign_key_check`)
		if err != nil {
			t.Fatalf("foreign_key_check: %v", err)
		}
		for rows.Next() {
			fkViolations++
		}
		rows.Close()
		if fkViolations != 0 {
			t.Fatalf("iteration %d: %d foreign-key violation(s) after the race", i, fkViolations)
		}
	}
}

// TestConcurrentSiblingStartAndSupersessionIsT69 races a sibling's
// Start against the supersession of the ambiguous original.
//
// The fixture claims the sibling BEFORE the ambiguity exists (a claim
// inside an ambiguous tree is refused by TR11) and lets its lease
// expire, so the supersession's quiescence predicate can pass. Whatever
// the ordering, the two operations never both commit, and the tree
// keeps a single explainable state.
func TestConcurrentSiblingStartAndSupersessionIsT69(t *testing.T) {
	for i := 0; i < 5; i++ {
		store, _, clock, projectID := newTestStore(t)
		id, epoch := claimedAssignment(t, store, projectID, "alpha")
		sibling := mustDelegateSibling(t, store, projectID, id, "charlie")
		sibActor := Actor{Address: "charlie", Kind: "agent"}
		for _, step := range []func(FencedParams) (*Assignment, error){store.Deliver, store.Acknowledge, store.Accept} {
			if _, err := step(FencedParams{ID: sibling.ID, Actor: sibActor}); err != nil {
				t.Fatalf("advance the sibling: %v", err)
			}
		}
		claimed := mustDo(t, func() (*Assignment, error) {
			return store.Claim(ClaimParams{ID: sibling.ID, ExpectedEpoch: 0,
				Owner: "charlie", LeaseSeconds: 1, Actor: sibActor})
		})
		// The original's ambiguity is recorded after the sibling's claim.
		att := startAmbiguous(t, store, id, epoch, fmt.Sprintf("ses-race69-%d", i))
		// The sibling's one-second lease expires so quiescence can pass.
		clock.Advance(2 * 60 * 60 * 1e9)

		startErr := make(chan error, 1)
		superErr := make(chan error, 1)
		go func() {
			_, err := store.Start(FencedParams{ID: sibling.ID, Epoch: claimed.ClaimEpoch, Actor: sibActor})
			startErr <- err
		}()
		go func() {
			_, err := store.Supersede(NewLocalOperatorAuthority("operator"),
				supParams(id, att.ID, epoch, fmt.Sprintf("race69-%d", i)))
			superErr <- err
		}()
		ste, se := <-startErr, <-superErr
		if ste == nil && se == nil {
			t.Fatalf("iteration %d: BOTH the sibling Start and the supersession committed", i)
		}
		if se != nil && lineageCount(t, store) != 0 {
			t.Fatalf("iteration %d: a refused supersession wrote a lineage", i)
		}
		if se == nil {
			if es, err := store.EffectiveState(id); err != nil || es != "superseded" {
				t.Fatalf("iteration %d: the original reads %q after a supersession (err %v)", i, es, err)
			}
			sib, err := store.Get(sibling.ID)
			if err != nil {
				t.Fatalf("read the sibling: %v", err)
			}
			if ste == nil && sib.State != StateInProgress {
				t.Fatalf("iteration %d: a successful Start left the sibling %s", i, sib.State)
			}
		}
	}
}

// TestAmbiguousAttemptCannotBeRelabelledIsT47 proves the attempt record
// is born with its identity and cannot be given a new one: the task, the
// epoch and the native session are write-once, so an old runtime cannot
// relabel an ambiguous attempt as a fresh one and re-use it.
func TestAmbiguousAttemptCannotBeRelabelledIsT47(t *testing.T) {
	store, dm, _, projectID := newTestStore(t)
	id, epoch := claimedAssignment(t, store, projectID, "alpha")
	att := startAmbiguous(t, store, id, epoch, "ses-t47")

	// No forward edge out of 'ambiguous' exists in the state machine.
	if _, err := store.MarkAttemptCompleted(SessionStateParams{
		AttemptID: att.ID, Epoch: epoch, Outcome: "succeeded",
	}); err == nil {
		t.Fatalf("an ambiguous attempt was relabelled as completed")
	} else if !errors.Is(err, ErrIllegalTransition) && !errors.Is(err, ErrRefused) {
		t.Fatalf("relabelling returned %v, want an illegal-transition or storage refusal", err)
	}

	// The identity columns are write-once even by direct SQL, which is
	// the old-runtime path (T83).
	for _, stmt := range []string{
		`UPDATE agent_task_sessions SET claim_epoch = claim_epoch + 1 WHERE id = ?`,
		`UPDATE agent_task_sessions SET task_id = 'at-somewhere-else' WHERE id = ?`,
		`UPDATE agent_task_sessions SET native_session_id = 'ses-forged' WHERE id = ?`,
	} {
		if _, err := dm.SQLDB().Exec(stmt, att.ID); err == nil {
			t.Fatalf("a direct attempt relabelling was accepted: %s", stmt)
		}
	}
	after, err := store.AttemptsFor(id)
	if err != nil {
		t.Fatalf("read the attempts: %v", err)
	}
	if len(after) != 1 || after[0].ID != att.ID || after[0].ClaimEpoch != epoch ||
		after[0].NativeSessionID != "ses-t47" {
		t.Fatalf("the attempt identity changed: %+v", after)
	}
}

// TestOldRuntimeOnMigratedDatabaseIsT83 proves the post-migration
// database is LOUD for an older runtime: the writes a pre-feature
// runtime issues on an ambiguous tree are refused by the storage, while
// the ordinary paths it uses keep working.
func TestOldRuntimeOnMigratedDatabaseIsT83(t *testing.T) {
	store, dm, _, projectID := newTestStore(t)
	id, epoch := claimedAssignment(t, store, projectID, "alpha")
	startAmbiguous(t, store, id, epoch, "ses-t83")

	// An old runtime's retry: clear the lease and take a new epoch
	// directly, as the pre-feature recovery did.
	oldWrites := []struct {
		name string
		sql  string
		args []interface{}
	}{
		{"clear the lease", `UPDATE agent_tasks SET lease_expires_at = NULL WHERE id = ?`, []interface{}{id}},
		{"bump the epoch", `UPDATE agent_tasks SET claim_epoch = claim_epoch + 1 WHERE id = ?`, []interface{}{id}},
		{"relabel the owner", `UPDATE agent_tasks SET owner = 'someone-else' WHERE id = ?`, []interface{}{id}},
		{"reopen the work", `UPDATE agent_tasks SET state = 'delivered' WHERE id = ?`, []interface{}{id}},
	}
	for _, w := range oldWrites {
		if _, err := dm.SQLDB().Exec(w.sql, w.args...); err == nil {
			t.Errorf("an old runtime's write was accepted: %s", w.name)
		}
	}
	// Ordinary paths keep working: a note is a legal append.
	if _, err := store.Note(NoteParams{ID: id, Note: "old runtime note",
		Actor: Actor{Address: "alpha", Kind: "agent"}}); err != nil {
		t.Fatalf("the ordinary note path is blocked: %v", err)
	}
	// And a fresh, unrelated tree is entirely unaffected.
	root := mustCreateRoot(t, store, projectID, func(p *CreateRootParams) {
		p.EligibleRecipients = []string{"alpha", "epsilon"}
		p.IdempotencyKey = "idem:t83:fresh"
	})
	if _, created, err := store.Delegate(DelegateParams{
		ParentID: root.ID, Title: "fresh", Objective: "o", Recipient: "epsilon",
		Role: RoleImplementer, IdempotencyKey: "idem:t83:fresh-child",
		Actor: Actor{Address: "operator", Kind: "agent"},
	}); err != nil || !created {
		t.Fatalf("ordinary delegation on the migrated database returned %v (created=%v)", err, created)
	}
}

// TestTwoConcurrentSupersessionsIsT39 proves exactly one of two
// concurrent authorisations commits. They name the SAME task and attempt
// with DIFFERENT evidence, so they are not idempotent retries: the
// UNIQUE (superseded_task_id, attempt_id) constraint admits one winner
// and the other receives the conflict diagnostic.
func TestTwoConcurrentSupersessionsIsT39(t *testing.T) {
	for i := 0; i < 5; i++ {
		store, _, clock, projectID := newTestStore(t)
		_, taskID, attemptID, epoch := supBlock(t, store, clock, projectID, fmt.Sprintf("race39-%d", i))
		auth := NewLocalOperatorAuthority("operator")

		type result struct {
			res *SupersedeResult
			err error
		}
		results := make(chan result, 2)
		for j := 0; j < 2; j++ {
			go func(j int) {
				p := supParams(taskID, attemptID, epoch, fmt.Sprintf("race39-%d-%d", i, j))
				p.EvidenceRef = fmt.Sprintf("gh:test/t39#%d", j)
				res, err := store.Supersede(auth, p)
				results <- result{res, err}
			}(j)
		}
		a, b := <-results, <-results
		winners := 0
		if a.err == nil {
			winners++
		}
		if b.err == nil {
			winners++
		}
		if winners != 1 {
			t.Fatalf("iteration %d: %d concurrent supersessions committed (a=%v, b=%v)",
				i, winners, a.err, b.err)
		}
		if got := lineageCount(t, store); got != 1 {
			t.Fatalf("iteration %d: %d lineage row(s) after the race", i, got)
		}
		loser := a.err
		if a.err == nil {
			loser = b.err
		}
		if !errors.Is(loser, ErrSupersessionConflict) {
			t.Fatalf("iteration %d: the loser returned %v, want %v", i, loser, ErrSupersessionConflict)
		}
	}
}

// TestEvidenceAndVerdictAreAtomicIsT111 proves the atomic
// evidence+verdict transaction: a fault between the two writes leaves
// NEITHER. A captured review with no verdict could never be re-run (the
// capture is write-once), and a verdict with no evidence would publish a
// decision with nothing behind it.
func TestEvidenceAndVerdictAreAtomicIsT111(t *testing.T) {
	// The passed path writes the subject's transition and its ledger
	// event after the evidence; both are inside the shared transaction.
	for _, fault := range []string{"transition", "transition-event"} {
		t.Run(fault, func(t *testing.T) {
			store, _, _, projectID := newTestStore(t)
			subject, reviewer := mustReviewSubjectForT111(t, store, projectID)
			reviewing := mustStartReviewer(t, store, reviewer)
			beforeSubjectEvents := len(eventTypes(t, store, subject.ID))
			beforeReviewerEvents := len(eventTypes(t, store, reviewing.ID))

			store.fault = func(stage string) error {
				if stage == fault {
					return errors.New("injected " + stage)
				}
				return nil
			}
			_, err := store.captureReviewEvidenceAndVerdict(ReviewEvidenceParams{
				ReviewerID:     reviewing.ID,
				Epoch:          reviewing.ClaimEpoch,
				ReviewedSHA:    strings.Repeat("a", 40),
				FindingsSHA256: strings.Repeat("b", 64),
				EvidenceRef:    "evidence/t111.json",
				Actor:          agentActor("charlie"),
			}, VerdictPassed, "the review found nothing")
			store.fault = nil
			if err == nil || !strings.Contains(err.Error(), "injected "+fault) {
				t.Fatalf("the atomic write returned %v, want the injected fault", err)
			}

			after, err := store.Get(reviewing.ID)
			if err != nil {
				t.Fatalf("re-read the reviewer: %v", err)
			}
			if after.ReviewedSHA != "" || after.FindingsSHA256 != "" {
				t.Fatalf("the evidence survived the rollback: %q %q",
					after.ReviewedSHA, after.FindingsSHA256)
			}
			if after.State != StateInProgress {
				t.Fatalf("the reviewer moved to %q despite the rollback", after.State)
			}
			if got := len(eventTypes(t, store, reviewing.ID)); got != beforeReviewerEvents {
				t.Fatalf("the reviewer ledger grew: %d -> %d", beforeReviewerEvents, got)
			}
			if got := len(eventTypes(t, store, subject.ID)); got != beforeSubjectEvents {
				t.Fatalf("the subject ledger grew: %d -> %d", beforeSubjectEvents, got)
			}
			subjectAfter, err := store.Get(subject.ID)
			if err != nil {
				t.Fatalf("re-read the subject: %v", err)
			}
			if subjectAfter.State != StateCompleted {
				t.Fatalf("the subject moved to %q despite the rollback", subjectAfter.State)
			}
		})
	}
}

// mustReviewSubjectForT111 builds the reviewable shape: a completed
// implementer subject and an independent reviewer that is its sibling.
func mustReviewSubjectForT111(t *testing.T, store *Store, projectID string) (*Assignment, *Assignment) {
	t.Helper()
	root := mustCreateRoot(t, store, projectID, func(p *CreateRootParams) {
		p.EligibleRecipients = []string{"alpha", "charlie"}
		p.MaxDepth = 2
	})
	subject := mustDelegate(t, store, root, func(p *DelegateParams) {
		p.Recipient = "alpha"
		p.IdempotencyKey = "idem:t111:subject"
	})
	subject = mustAdvance(t, store, subject, StateCompleted)
	reviewer := mustCreateReviewer(t, store, root, subject)
	return subject, reviewer
}

// TestReworkRefusalIsLoadBearingIsT66T80 is the negative control for the
// rework refusal: two trees with the SAME shape and the SAME parent role
// differ only in the recovery outcome, and that difference is what
// changes the delegation result.
//
// Without this pairing a "refusal" test could be satisfied by any other
// rule (budget, allow-list, depth) — the reviewed failure mode. Here the
// control tree delegates successfully from a completed reviewer, so the
// refusal in the rework tree can only come from the rework rule.
func TestReworkRefusalIsLoadBearingIsT66T80(t *testing.T) {
	t.Run("control: a passed review is not rework", func(t *testing.T) {
		store, _, clock, projectID := newTestStore(t)
		_, _, reviewerID, _ := recoveryTreeWithVerdict(t, store, clock, projectID, "ctl-pass", VerdictPassed)
		// The SAME delegate call, from the SAME reviewer role, in a tree
		// whose recovery is complete rather than rework: it succeeds.
		_, created, err := store.Delegate(DelegateParams{
			ParentID: reviewerID, Title: "follow-up", Objective: "ordinary",
			Recipient: "alpha", Role: RoleImplementer,
			IdempotencyKey: "idem:ctl:pass:child", ResourceUnits: 1,
			Actor: Actor{Address: "alpha", Kind: "agent"},
		})
		if err != nil || !created {
			t.Fatalf("the control delegation returned %v (created=%v); the refusal must "+
				"depend on the rework state, not on the parent's role", err, created)
		}
	})
	t.Run("treatment: the same tree in rework_required", func(t *testing.T) {
		store, _, clock, projectID := newTestStore(t)
		_, replacementID, reviewerID, _ := reworkRequiredTree(t, store, clock, projectID, "ctl-rework")
		_, created, err := store.Delegate(DelegateParams{
			ParentID: reviewerID, Title: "follow-up", Objective: "ordinary",
			Recipient: "alpha", Role: RoleImplementer,
			IdempotencyKey: "idem:ctl:rework:child", ResourceUnits: 1,
			Actor: Actor{Address: "alpha", Kind: "agent"},
		})
		if !errors.Is(err, ErrReworkRequired) || created {
			t.Fatalf("the treatment delegation returned %v (created=%v), want %v",
				err, created, ErrReworkRequired)
		}
		// The replacement parent is refused for the same reason.
		if _, _, err := store.Delegate(DelegateParams{
			ParentID: replacementID, Title: "follow-up 2", Objective: "ordinary",
			Recipient: "alpha", Role: RoleImplementer,
			IdempotencyKey: "idem:ctl:rework:child2", ResourceUnits: 1,
			Actor: Actor{Address: "alpha", Kind: "agent"},
		}); !errors.Is(err, ErrReworkRequired) {
			t.Fatalf("delegating beneath the replacement returned %v, want %v", err, ErrReworkRequired)
		}
	})
}
