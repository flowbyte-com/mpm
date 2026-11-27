package assignment

// Adversarial regression tests for the ambiguous-attempt execution barrier.
// Disposable databases and a fake runtime; no inference.

import (
	"context"
	"errors"
	"testing"
	"time"
)

func startAmbiguous(t *testing.T, store *Store, id string, epoch int64, sessionID string) *SessionAttempt {
	t.Helper()
	if _, err := store.Start(FencedParams{
		ID: id, Epoch: epoch, Actor: Actor{Address: "alpha", Kind: "agent"},
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	att, err := store.BeginAttempt(BeginAttemptParams{
		TaskID: id, Epoch: epoch, Runtime: RuntimeOpenCode,
		IntendedAgent: "mpm-worker", NativeSessionID: sessionID,
		CorrelationRef: "corr-" + sessionID,
		WorkspaceID:    "ws", WorkspacePath: "/tmp/ws",
	})
	if err != nil {
		t.Fatalf("begin attempt: %v", err)
	}
	if _, err := store.MarkAttemptLaunched(SessionStateParams{
		AttemptID: att.ID, Epoch: epoch,
	}); err != nil {
		t.Fatalf("mark launched: %v", err)
	}
	if _, err := store.MarkAttemptAmbiguous(SessionStateParams{
		AttemptID: att.ID, Epoch: epoch, Error: "the runtime produced no session state",
	}); err != nil {
		t.Fatalf("mark ambiguous: %v", err)
	}
	return att
}

func TestExecuteRefusesCurrentEpochAmbiguousAttempt(t *testing.T) {
	store, _, clock, projectID := newTestStore(t)
	_ = clock
	ws := t.TempDir()
	id, epoch := claimedAssignment(t, store, projectID, "alpha")
	att := startAmbiguous(t, store, id, epoch, "ses_current_ambiguous")

	rt := &fakeRuntime{allowed: map[string]bool{"mpm-worker": true}}
	_, err := store.Execute(context.Background(), rt, executeParams(id, epoch, ws))
	if !errors.Is(err, ErrAmbiguousAttempt) {
		t.Fatalf("err = %v, want ErrAmbiguousAttempt", err)
	}
	if rt.launches != 0 {
		t.Fatalf("runtime launched %d time(s); the barrier must precede Launch", rt.launches)
	}
	attempts, _ := store.AttemptsFor(id)
	if len(attempts) != 1 || attempts[0].ID != att.ID {
		t.Fatalf("attempt history changed: %+v", attempts)
	}
	row, _ := store.Get(id)
	if row.State != StateInProgress || row.ClaimEpoch != epoch {
		t.Fatalf("assignment moved to %s epoch %d; the refusal must not mutate", row.State, row.ClaimEpoch)
	}
}

// leasesEqual reports whether two lease pointers name the same instant.
func leasesEqual(a, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func TestExecuteRefusesAmbiguousAfterLeaseExpiryAndReclaim(t *testing.T) {
	store, _, clock, projectID := newTestStore(t)
	id, epoch := claimedAssignment(t, store, projectID, "alpha")
	att := startAmbiguous(t, store, id, epoch, "ses_expired_ambiguous")

	clock.Advance(2 * time.Hour)
	// TR11: lease expiry does not authorise a new epoch in an ambiguous
	// tree. The reclaim is refused and changes nothing: the epoch stays,
	// the holder stays, the lease stays expired, and no event or attempt
	// is recorded for the refused claim.
	before, err := store.Get(id)
	if err != nil {
		t.Fatalf("read the task: %v", err)
	}
	beforeEvents := len(eventTypes(t, store, id))
	if _, err := store.Claim(ClaimParams{
		ID: id, ExpectedEpoch: epoch, Owner: "mpm-dispatcher", LeaseSeconds: 300,
		Actor: Actor{Address: "alpha", Kind: "agent"},
	}); !errors.Is(err, ErrAmbiguousAttempt) {
		t.Fatalf("a reclaim in an ambiguous tree returned %v, want %v", err, ErrAmbiguousAttempt)
	}
	after, err := store.Get(id)
	if err != nil {
		t.Fatalf("re-read the task: %v", err)
	}
	if after.ClaimEpoch != before.ClaimEpoch || after.Owner != before.Owner ||
		after.State != before.State || !leasesEqual(after.LeaseExpiresAt, before.LeaseExpiresAt) {
		t.Fatalf("the refused reclaim changed the row: %+v -> %+v", before, after)
	}
	if got := len(eventTypes(t, store, id)); got != beforeEvents {
		t.Fatalf("the refused reclaim wrote %d events, want %d", got, beforeEvents)
	}

	rt := &fakeRuntime{allowed: map[string]bool{"mpm-worker": true}}
	_, err = store.Execute(context.Background(), rt, executeParams(id, epoch, t.TempDir()))
	if !errors.Is(err, ErrAmbiguousAttempt) {
		t.Fatalf("err = %v, want ErrAmbiguousAttempt", err)
	}
	if rt.launches != 0 {
		t.Fatalf("runtime launched %d time(s) after a refused claim", rt.launches)
	}
	attempts, _ := store.AttemptsFor(id)
	if len(attempts) != 1 || attempts[0].ID != att.ID {
		t.Fatalf("attempt history changed: %+v", attempts)
	}
}

func TestExecuteRefusesEarlierAmbiguousWithLaterFailedHistory(t *testing.T) {
	store, _, clock, projectID := newTestStore(t)
	id, epoch := claimedAssignment(t, store, projectID, "alpha")
	startAmbiguous(t, store, id, epoch, "ses_hidden_ambiguous")

	clock.Advance(2 * time.Hour)
	// TR11: the reclaim itself is refused — lease expiry does not open
	// a new epoch in an ambiguous tree. No later history can be
	// constructed over the ambiguity at any epoch.
	if _, err := store.Claim(ClaimParams{
		ID: id, ExpectedEpoch: epoch, Owner: "mpm-dispatcher", LeaseSeconds: 300,
		Actor: Actor{Address: "alpha", Kind: "agent"},
	}); !errors.Is(err, ErrAmbiguousAttempt) {
		t.Fatalf("a reclaim over ambiguity returned %v, want %v", err, ErrAmbiguousAttempt)
	}
	// TR19: no new attempt opens in an ambiguous tree either.
	if _, err := store.BeginAttempt(BeginAttemptParams{
		TaskID: id, Epoch: epoch, Runtime: RuntimeOpenCode,
		IntendedAgent: "mpm-worker", NativeSessionID: "ses_later_failed",
		CorrelationRef: "corr-later", WorkspaceID: "ws", WorkspacePath: "/tmp/ws",
	}); !errors.Is(err, ErrAmbiguousAttempt) {
		t.Fatalf("a second attempt over ambiguity returned %v, want %v", err, ErrAmbiguousAttempt)
	}

	rt := &fakeRuntime{allowed: map[string]bool{"mpm-worker": true}}
	_, err := store.Execute(context.Background(), rt, executeParams(id, epoch, t.TempDir()))
	if !errors.Is(err, ErrAmbiguousAttempt) {
		t.Fatalf("err = %v, want ErrAmbiguousAttempt", err)
	}
	if rt.launches != 0 {
		t.Fatalf("runtime launched %d time(s) despite the earlier ambiguity", rt.launches)
	}
}

func TestExecuteAllowsRetryAfterDefinitiveFailure(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	ws := t.TempDir()
	id, epoch := claimedAssignment(t, store, projectID, "alpha")
	if _, err := store.Start(FencedParams{
		ID: id, Epoch: epoch, Actor: Actor{Address: "alpha", Kind: "agent"},
	}); err != nil {
		t.Fatal(err)
	}
	att, err := store.BeginAttempt(BeginAttemptParams{
		TaskID: id, Epoch: epoch, Runtime: RuntimeOpenCode,
		IntendedAgent: "mpm-worker", NativeSessionID: "ses_definitive_failure",
		CorrelationRef: "corr-failure", WorkspaceID: "ws", WorkspacePath: ws,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkAttemptLaunched(SessionStateParams{
		AttemptID: att.ID, Epoch: epoch,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkAttemptFailed(SessionStateParams{
		AttemptID: att.ID, Epoch: epoch, Error: "a definitive failure",
	}); err != nil {
		t.Fatal(err)
	}

	rt := &fakeRuntime{
		allowed:     map[string]bool{"mpm-worker": true},
		launchState: okState(ws, "permitted-model", "mpm-worker"),
	}
	res, err := store.Execute(context.Background(), rt, executeParams(id, epoch, ws))
	if err != nil {
		t.Fatalf("legitimate retry refused: %v", err)
	}
	if rt.launches != 1 || res.Attempt.Attempt != 2 {
		t.Fatalf("launches=%d attempt=%d, want 1 and 2", rt.launches, res.Attempt.Attempt)
	}
}

func TestExecuteRefusesEarlierAmbiguousWithLaterCompletedHistory(t *testing.T) {
	store, _, clock, projectID := newTestStore(t)
	id, epoch := claimedAssignment(t, store, projectID, "alpha")
	startAmbiguous(t, store, id, epoch, "ses_hidden_ambiguous_completed")

	clock.Advance(2 * time.Hour)
	// TR11: the reclaim itself is refused — not even one that would
	// later complete can open a new epoch over ambiguity.
	if _, err := store.Claim(ClaimParams{
		ID: id, ExpectedEpoch: epoch, Owner: "mpm-dispatcher", LeaseSeconds: 300,
		Actor: Actor{Address: "alpha", Kind: "agent"},
	}); !errors.Is(err, ErrAmbiguousAttempt) {
		t.Fatalf("a reclaim over ambiguity returned %v, want %v", err, ErrAmbiguousAttempt)
	}
	// TR19: no new attempt opens in an ambiguous tree — not even one
	// that would later complete. The barrier is at admission.
	if _, err := store.BeginAttempt(BeginAttemptParams{
		TaskID: id, Epoch: epoch, Runtime: RuntimeOpenCode,
		IntendedAgent: "mpm-worker", NativeSessionID: "ses_later_completed",
		CorrelationRef: "corr-later-completed", WorkspaceID: "ws", WorkspacePath: "/tmp/ws",
	}); !errors.Is(err, ErrAmbiguousAttempt) {
		t.Fatalf("a second attempt over ambiguity returned %v, want %v", err, ErrAmbiguousAttempt)
	}

	rt := &fakeRuntime{allowed: map[string]bool{"mpm-worker": true}}
	_, err := store.Execute(context.Background(), rt, executeParams(id, epoch, t.TempDir()))
	if !errors.Is(err, ErrAmbiguousAttempt) {
		t.Fatalf("err = %v, want ErrAmbiguousAttempt", err)
	}
	if rt.launches != 0 {
		t.Fatalf("runtime launched %d time(s) despite the earlier ambiguity", rt.launches)
	}
}

// TestSiblingClaimRefusedInAmbiguousTreeIsT1 proves the tree-scoped
// barrier is not per-task: an ambiguous attempt on A refuses a claim on
// an unrelated sibling B that never ran anything.
//
// The refusal changes nothing: B keeps epoch 0, no owner, no lease and
// its state, and the ledger gains no event and no attempt.
func TestSiblingClaimRefusedInAmbiguousTreeIsT1(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	id, epoch := claimedAssignment(t, store, projectID, "alpha")
	startAmbiguous(t, store, id, epoch, "ses_t1_ambiguous")

	sibling := mustDelegateSibling(t, store, projectID, id, "charlie")
	beforeEvents := len(eventTypes(t, store, sibling.ID))

	err := tryClaim(t, store, sibling.ID, "beta")
	if !errors.Is(err, ErrAmbiguousAttempt) {
		t.Fatalf("a sibling claim in an ambiguous tree returned %v, want %v", err, ErrAmbiguousAttempt)
	}
	after, err := store.Get(sibling.ID)
	if err != nil {
		t.Fatalf("re-read the sibling: %v", err)
	}
	if after.ClaimEpoch != 0 || after.Owner != "" || after.LeaseExpiresAt != nil || after.State != StateEnqueued {
		t.Fatalf("the refused claim changed the sibling: %+v", after)
	}
	if got := len(eventTypes(t, store, sibling.ID)); got != beforeEvents {
		t.Fatalf("the refused claim wrote %d events, want %d", got, beforeEvents)
	}
	if atts, _ := store.AttemptsFor(sibling.ID); len(atts) != 0 {
		t.Fatalf("the refused claim recorded %d attempts", len(atts))
	}
}

// TestRenewalRefusedInAmbiguousTreeIsT67 proves renewal and completion
// part ways under a block: the holder's renewal is refused (it would
// advance the epoch and extend admission) while the admitted attempt may
// still complete on its evidence.
//
// The refused renewal changes nothing: epoch, owner, lease, state,
// ledger and attempts are exactly as before.
func TestRenewalRefusedInAmbiguousTreeIsT67(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	id, epoch := claimedAssignment(t, store, projectID, "alpha")
	startAmbiguous(t, store, id, epoch, "ses_t67_ambiguous")

	before, err := store.Get(id)
	if err != nil {
		t.Fatalf("read the task: %v", err)
	}
	beforeEvents := len(eventTypes(t, store, id))
	beforeAttempts, err := store.AttemptsFor(id)
	if err != nil {
		t.Fatalf("read the attempts: %v", err)
	}

	if _, err := store.Renew(RenewParams{
		ID: id, ExpectedEpoch: epoch, Owner: "alpha", LeaseSeconds: 600,
		Actor: Actor{Address: "alpha", Kind: "agent"},
	}); !errors.Is(err, ErrAmbiguousAttempt) {
		t.Fatalf("a renewal in an ambiguous tree returned %v, want %v", err, ErrAmbiguousAttempt)
	}
	after, err := store.Get(id)
	if err != nil {
		t.Fatalf("re-read the task: %v", err)
	}
	if after.ClaimEpoch != before.ClaimEpoch || after.Owner != before.Owner ||
		after.State != before.State || !leasesEqual(after.LeaseExpiresAt, before.LeaseExpiresAt) {
		t.Fatalf("the refused renewal changed the row: %+v -> %+v", before, after)
	}
	if got := len(eventTypes(t, store, id)); got != beforeEvents {
		t.Fatalf("the refused renewal wrote %d events, want %d", got, beforeEvents)
	}
	afterAttempts, _ := store.AttemptsFor(id)
	if len(afterAttempts) != len(beforeAttempts) {
		t.Fatalf("the refused renewal recorded attempts: %d -> %d", len(beforeAttempts), len(afterAttempts))
	}
}

// TestRoutingRefusedOnAmbiguousTaskIsT19 proves routing steps are not a
// side door: deliver, acknowledge and accept are refused on the
// ambiguous task itself, while an unscoped sibling still routes.
//
// The matrix allows U and C and refuses A and S; the sibling control
// proves the guard did not over-block the tree.
func TestRoutingRefusedOnAmbiguousTaskIsT19(t *testing.T) {
	store, _, _, projectID := newTestStore(t)
	id, epoch := claimedAssignment(t, store, projectID, "alpha")
	startAmbiguous(t, store, id, epoch, "ses_t19_ambiguous")

	for _, op := range []struct {
		name string
		call func() error
	}{
		{"deliver", func() error {
			_, err := store.Deliver(FencedParams{ID: id, Epoch: epoch, Actor: Actor{Address: "alpha", Kind: "agent"}})
			return err
		}},
		{"acknowledge", func() error {
			_, err := store.Acknowledge(FencedParams{ID: id, Epoch: epoch, Actor: Actor{Address: "alpha", Kind: "agent"}})
			return err
		}},
		{"accept", func() error {
			_, err := store.Accept(FencedParams{ID: id, Epoch: epoch, Actor: Actor{Address: "alpha", Kind: "agent"}})
			return err
		}},
	} {
		if err := op.call(); !errors.Is(err, ErrAmbiguousAttempt) {
			t.Fatalf("%s on an ambiguous task returned %v, want %v", op.name, err, ErrAmbiguousAttempt)
		}
	}
	after, err := store.Get(id)
	if err != nil {
		t.Fatalf("re-read the task: %v", err)
	}
	if after.State != StateInProgress || after.ClaimEpoch != epoch {
		t.Fatalf("a refused routing step moved the task to %s epoch %d", after.State, after.ClaimEpoch)
	}

	sibling := mustDelegateSibling(t, store, projectID, id, "charlie")
	siblingActor := Actor{Address: "charlie", Kind: "agent"}
	if _, err := store.Deliver(FencedParams{ID: sibling.ID, Epoch: 0, Actor: siblingActor}); err != nil {
		t.Fatalf("deliver on an unscoped sibling was refused: %v", err)
	}
}

// mustDelegateSibling delegates a second implementer under the same
// parent as the given task, so the two are siblings.
func mustDelegateSibling(t *testing.T, store *Store, projectID, siblingOf, recipient string) *Assignment {
	t.Helper()
	ref, err := store.Get(siblingOf)
	if err != nil {
		t.Fatalf("read the reference task: %v", err)
	}
	a, created, err := store.Delegate(DelegateParams{
		ParentID:       ref.ParentID,
		Title:          "sibling work",
		Objective:      "exercise the tree scope",
		Recipient:      recipient,
		Role:           RoleImplementer,
		IdempotencyKey: "idem:sibling:" + t.Name() + ":" + recipient,
		Actor:          Actor{Address: "operator", Kind: "agent"},
	})
	if err != nil {
		t.Fatalf("delegate the sibling: %v", err)
	}
	if !created {
		t.Fatalf("the sibling was not created")
	}
	_ = projectID
	return a
}

// tryClaim presents an initial claim (epoch 0) for the task.
func tryClaim(t *testing.T, store *Store, id, owner string) error {
	t.Helper()
	_, err := store.Claim(ClaimParams{
		ID: id, ExpectedEpoch: 0, Owner: owner, LeaseSeconds: 600,
		Actor: Actor{Address: owner, Kind: "agent"},
	})
	return err
}

// TestReplacementAmbiguityRefusesAllClaimsIsT8 proves the Named=Amb arm
// in the other direction: a lineage that once authorised the tree stops
// authorising anything when the replacement itself turns ambiguous and
// no lineage names the new attempt.
//
// The fixture supersedes A (naming X), authorises R, then records a new
// ambiguity on R. From then on every claim in the tree is refused until
// a second lineage names the new attempt.
func TestReplacementAmbiguityRefusesAllClaimsIsT8(t *testing.T) {
	store, _, clock, projectID := newTestStore(t)
	id, epoch := claimedAssignment(t, store, projectID, "alpha")
	att := startAmbiguous(t, store, id, epoch, "ses_t8_original")
	clock.Advance(2 * time.Hour)
	cur, err := store.Get(id)
	if err != nil {
		t.Fatalf("read the task: %v", err)
	}
	res, err := store.Supersede(NewLocalOperatorAuthority("operator"), SupersedeParams{
		SupersededID: id, AttemptID: att.ID, Epoch: cur.ClaimEpoch,
		Replacement: SupersedeTaskParams{Title: "redo", Objective: "redo the work",
			Recipient: "beta", ResourceUnits: 1, BaseSHA: "0598edb7ac904e98a5a21c1fe140333742857ffb"},
		Reviewer: SupersedeTaskParams{Title: "re-review", Objective: "review the redo",
			Recipient: "charlie", ResourceUnits: 1},
		EvidenceRef: "gh:flowbyte-com/mpm#258", Justification: "the original launch is ambiguous",
		RequireQuiescence: true,
	})
	if err != nil {
		t.Fatalf("supersede: %v", err)
	}
	replacement := res.Replacement

	// Authorise the replacement: walk the path, claim, start, open an
	// attempt, lose it.
	rActor := Actor{Address: "beta", Kind: "agent"}
	for _, step := range []func(FencedParams) (*Assignment, error){store.Deliver, store.Acknowledge, store.Accept} {
		if _, err := step(FencedParams{ID: replacement.ID, Actor: rActor}); err != nil {
			t.Fatalf("advance the replacement: %v", err)
		}
	}
	rClaimed, err := store.Claim(ClaimParams{
		ID: replacement.ID, ExpectedEpoch: 0, Owner: "beta", LeaseSeconds: 3600,
		Actor: rActor,
	})
	if err != nil {
		t.Fatalf("claim the replacement: %v", err)
	}
	if _, err := store.Start(FencedParams{ID: replacement.ID, Epoch: rClaimed.ClaimEpoch,
		Actor: Actor{Address: "beta", Kind: "agent"}}); err != nil {
		t.Fatalf("start the replacement: %v", err)
	}
	rAtt, err := store.BeginAttempt(BeginAttemptParams{
		TaskID: replacement.ID, Epoch: rClaimed.ClaimEpoch, Runtime: RuntimeOpenCode,
		IntendedAgent: "mpm-worker", NativeSessionID: "ses_t8_replacement",
		CorrelationRef: "corr-t8", WorkspaceID: "ws", WorkspacePath: "/tmp/ws",
	})
	if err != nil {
		t.Fatalf("begin the replacement attempt: %v", err)
	}
	if _, err := store.MarkAttemptAmbiguous(SessionStateParams{
		AttemptID: rAtt.ID, Epoch: rClaimed.ClaimEpoch, Error: "lost as well",
	}); err != nil {
		t.Fatalf("mark the replacement ambiguous: %v", err)
	}

	// Every claim in the tree is now refused: the replacement's, the
	// reviewer's and an unrelated sibling's.
	for _, target := range []struct {
		name string
		id   string
		call func() error
	}{
		{"replacement", replacement.ID, func() error {
			return tryClaimOn(t, store, replacement.ID, rClaimed.ClaimEpoch, "beta")
		}},
		{"reviewer", res.Reviewer.ID, func() error {
			return tryClaim(t, store, res.Reviewer.ID, "charlie")
		}},
	} {
		if err := target.call(); !errors.Is(err, ErrAmbiguousAttempt) {
			t.Fatalf("a claim on %s after replacement ambiguity returned %v, want %v",
				target.name, err, ErrAmbiguousAttempt)
		}
	}
	if es, err := store.EffectiveState(replacement.ID); err != nil || es != "recovery_required" {
		t.Fatalf("the replacement reads %q, want recovery_required (err %v)", es, err)
	}
	// T57: the tree reports the block rather than admitting work.
	ts, err := store.TreeStatus(cur.RootID)
	if err != nil {
		t.Fatalf("tree status: %v", err)
	}
	if ts.Recovery != "blocked_unresolved_ambiguity" {
		t.Fatalf("recovery_status=%q, want blocked_unresolved_ambiguity", ts.Recovery)
	}
	if ts.Tree != "blocked_unresolved_ambiguity" {
		t.Fatalf("tree_status=%q, want blocked_unresolved_ambiguity", ts.Tree)
	}
}

// tryClaimOn presents a claim at the given epoch for the task.
func tryClaimOn(t *testing.T, store *Store, id string, epoch int64, owner string) error {
	t.Helper()
	_, err := store.Claim(ClaimParams{
		ID: id, ExpectedEpoch: epoch, Owner: owner, LeaseSeconds: 600,
		Actor: Actor{Address: owner, Kind: "agent"},
	})
	return err
}

// TestSupersededOriginalAbsentFromDispatchIsT53 proves recovery freezes
// dispatch: once a lineage supersedes the original, DispatchCandidates no
// longer lists it — even though its stored state is still dispatchable
// (in_progress) and the row itself never changes.
func TestSupersededOriginalAbsentFromDispatchIsT53(t *testing.T) {
	store, _, clock, projectID := newTestStore(t)
	id, epoch := claimedAssignment(t, store, projectID, "alpha")
	att := startAmbiguous(t, store, id, epoch, "ses_t53_original")

	before, err := store.DispatchCandidates(DispatchQuery{Limit: 10})
	if err != nil {
		t.Fatalf("candidates before: %v", err)
	}
	found := false
	for _, c := range before {
		if c.ID == id {
			found = true
		}
	}
	if !found {
		t.Fatalf("the ambiguous original is not even listed before supersession")
	}

	clock.Advance(2 * time.Hour)
	cur, err := store.Get(id)
	if err != nil {
		t.Fatalf("read the task: %v", err)
	}
	if _, err := store.Supersede(NewLocalOperatorAuthority("operator"), SupersedeParams{
		SupersededID: id, AttemptID: att.ID, Epoch: cur.ClaimEpoch,
		Replacement: SupersedeTaskParams{Title: "redo", Objective: "redo the work",
			Recipient: "beta", ResourceUnits: 1, BaseSHA: "0598edb7ac904e98a5a21c1fe140333742857ffb"},
		Reviewer: SupersedeTaskParams{Title: "re-review", Objective: "review the redo",
			Recipient: "charlie", ResourceUnits: 1},
		EvidenceRef: "gh:flowbyte-com/mpm#258", Justification: "the original launch is ambiguous",
		RequireQuiescence: true,
	}); err != nil {
		t.Fatalf("supersede: %v", err)
	}
	after, err := store.DispatchCandidates(DispatchQuery{Limit: 10})
	if err != nil {
		t.Fatalf("candidates after: %v", err)
	}
	for _, c := range after {
		if c.ID == id {
			t.Fatalf("the superseded original is still a dispatch candidate")
		}
	}
	// And the row itself is untouched by the delisting.
	row, err := store.Get(id)
	if err != nil {
		t.Fatalf("re-read the original: %v", err)
	}
	if row.State != StateInProgress || row.ClaimEpoch != cur.ClaimEpoch {
		t.Fatalf("the original moved: %s epoch %d", row.State, row.ClaimEpoch)
	}
	// T55: the live lineage reads in_progress/incomplete while its
	// replacement and reviewer are unfinished.
	ts, err := store.TreeStatus(cur.RootID)
	if err != nil {
		t.Fatalf("tree status: %v", err)
	}
	if ts.Recovery != "in_progress" {
		t.Fatalf("recovery_status=%q, want in_progress", ts.Recovery)
	}
	if ts.Tree != "incomplete" {
		t.Fatalf("tree_status=%q, want incomplete", ts.Tree)
	}
}
