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

func TestExecuteRefusesAmbiguousAfterLeaseExpiryAndReclaim(t *testing.T) {
	store, _, clock, projectID := newTestStore(t)
	id, epoch := claimedAssignment(t, store, projectID, "alpha")
	att := startAmbiguous(t, store, id, epoch, "ses_expired_ambiguous")

	clock.Advance(2 * time.Hour)
	reclaimed, err := store.Claim(ClaimParams{
		ID: id, ExpectedEpoch: epoch, Owner: "mpm-dispatcher", LeaseSeconds: 300,
		Actor: Actor{Address: "alpha", Kind: "agent"},
	})
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if reclaimed.ClaimEpoch != epoch+1 {
		t.Fatalf("epoch = %d, want %d", reclaimed.ClaimEpoch, epoch+1)
	}

	rt := &fakeRuntime{allowed: map[string]bool{"mpm-worker": true}}
	_, err = store.Execute(context.Background(), rt, executeParams(id, reclaimed.ClaimEpoch, t.TempDir()))
	if !errors.Is(err, ErrAmbiguousAttempt) {
		t.Fatalf("err = %v, want ErrAmbiguousAttempt", err)
	}
	if rt.launches != 0 {
		t.Fatalf("runtime launched %d time(s) after a new claim", rt.launches)
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
	reclaimed, err := store.Claim(ClaimParams{
		ID: id, ExpectedEpoch: epoch, Owner: "mpm-dispatcher", LeaseSeconds: 300,
		Actor: Actor{Address: "alpha", Kind: "agent"},
	})
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	second, err := store.BeginAttempt(BeginAttemptParams{
		TaskID: id, Epoch: reclaimed.ClaimEpoch, Runtime: RuntimeOpenCode,
		IntendedAgent: "mpm-worker", NativeSessionID: "ses_later_failed",
		CorrelationRef: "corr-later", WorkspaceID: "ws", WorkspacePath: "/tmp/ws",
	})
	if err != nil {
		t.Fatalf("second attempt: %v", err)
	}
	if _, err := store.MarkAttemptLaunched(SessionStateParams{
		AttemptID: second.ID, Epoch: reclaimed.ClaimEpoch,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkAttemptFailed(SessionStateParams{
		AttemptID: second.ID, Epoch: reclaimed.ClaimEpoch, Error: "a later definitive failure",
	}); err != nil {
		t.Fatal(err)
	}

	rt := &fakeRuntime{allowed: map[string]bool{"mpm-worker": true}}
	_, err = store.Execute(context.Background(), rt, executeParams(id, reclaimed.ClaimEpoch, t.TempDir()))
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
	reclaimed, err := store.Claim(ClaimParams{
		ID: id, ExpectedEpoch: epoch, Owner: "mpm-dispatcher", LeaseSeconds: 300,
		Actor: Actor{Address: "alpha", Kind: "agent"},
	})
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	second, err := store.BeginAttempt(BeginAttemptParams{
		TaskID: id, Epoch: reclaimed.ClaimEpoch, Runtime: RuntimeOpenCode,
		IntendedAgent: "mpm-worker", NativeSessionID: "ses_later_completed",
		CorrelationRef: "corr-later-completed", WorkspaceID: "ws", WorkspacePath: "/tmp/ws",
	})
	if err != nil {
		t.Fatalf("second attempt: %v", err)
	}
	if _, err := store.MarkAttemptLaunched(SessionStateParams{
		AttemptID: second.ID, Epoch: reclaimed.ClaimEpoch,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkAttemptCompleted(SessionStateParams{
		AttemptID: second.ID, Epoch: reclaimed.ClaimEpoch, Outcome: "succeeded",
	}); err != nil {
		t.Fatal(err)
	}

	rt := &fakeRuntime{allowed: map[string]bool{"mpm-worker": true}}
	_, err = store.Execute(context.Background(), rt, executeParams(id, reclaimed.ClaimEpoch, t.TempDir()))
	if !errors.Is(err, ErrAmbiguousAttempt) {
		t.Fatalf("err = %v, want ErrAmbiguousAttempt", err)
	}
	if rt.launches != 0 {
		t.Fatalf("runtime launched %d time(s) despite the earlier ambiguity", rt.launches)
	}
}
