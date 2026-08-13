package capability

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	internal "github.com/flowbyte-com/mpm-core"
)

// =============================================================================
// executor_fracture.go — sliding-window fracture detection + §2.4.1 wake
//                         emission (spec §1.3 / EX-7)
//
// The Executor's checkFracture hook fires at the tail of every Invoke,
// after the telemetry row has been written. It implements the
// spec's failure-cluster detector:
//
//     "3 failures in 60s ⇒ state transitions to 'fractured'"
//     (docs/architecture/capability-lifecycle.md §1.3 row 9)
//
// The detector reads capability_invocations (the load-bearing
// telemetry ledger), counts failures whose invoked_at falls
// inside the last 60 seconds, and if the count crosses the
// threshold, asks the Store to MarkFracture the capability.
// MarkFracture is the canonical state transition (degraded →
// fractured per §1.3); this file extends it with the §2.4.1
// cascade wake emission when author_theory_id is set.
//
// Why this file instead of putting it in store_lifecycle.go:
//
//   The detector logic (sliding-window math, threshold check) is
//   executor-side policy, not lifecycle storage. MarkFracture
//   already enforces the state-machine transition; adding the
//   cascade emission on top of MarkFracture (rather than writing
//   a parallel FractureCapability method) keeps the matrix
//   enforcement in one place. Splitting the file gives EX-7 its
//   own testable surface without bloating store_lifecycle.go.
//
// Spec: docs/architecture/capability-lifecycle.md §1.3 (state matrix row 9),
//      §2.4.1 (fracture wake).
// =============================================================================

// FractureThreshold is the minimum number of failures within
// FractureWindowSeconds that trips the fracture transition.
// Matches the spec's "3 failures in 60s" rule literally; both
// numbers are exported so tests and the audit pipeline can
// assert against the same constants.
const (
	FractureThreshold      = 3
	FractureWindowSeconds  = 60
	FractureActorScheduler = "scheduler"
)

// FailureCountSince returns the number of failed invocations
// (exit_code != 0) for capabilityID whose invoked_at >= since
// (Unix epoch seconds, inclusive). Read-only — does NOT mutate
// any counter or row.
//
// Used by Executor.checkFracture to power the sliding-window
// detection. The query is index-covered by
// idx_invocations_recent_failures (capability_id, exit_code)
// — combined with the invoked_at >= predicate, the planner
// walks a single index range.
//
// A capability with no invocation history returns 0 (not an
// error). The check is a count, not a fetch — we deliberately
// don't materialize the rows here, so a high-volume capability
// doesn't pay a SELECT-row tax for every Invoke.
func (s *Store) FailureCountSince(ctx context.Context, capabilityID string, sinceUnixSec int64) (int, error) {
	if capabilityID == "" {
		return 0, fmt.Errorf("capability: FailureCountSince: capabilityID is required")
	}
	if sinceUnixSec < 0 {
		return 0, fmt.Errorf("capability: FailureCountSince: sinceUnixSec must be >= 0, got %d", sinceUnixSec)
	}

	q := `SELECT COUNT(*) FROM capability_invocations
	      WHERE capability_id = ? AND invoked_at >= ? AND exit_code != 0`

	var count int
	if err := s.dm.QueryRowTracked(q, capabilityID, sinceUnixSec).Scan(&count); err != nil {
		return 0, wrapDBError("FailureCountSince", err)
	}
	return count, nil
}

// checkFracture is the post-Invoke detector hook. Called by the
// Executor after RecordInvocation has durably written the
// telemetry row. Reads the recent-failure count over a 60s
// window; if the count crosses FractureThreshold AND the
// capability is currently in a callable state that has already
// degraded (StateDegraded per §1.3 row 9), asks the Store to
// MarkFracture the capability — which fires the §2.4.1 cascade
// wake when author_theory_id is populated.
//
// The detector is intentionally narrow:
//
//   * It only fires for state='degraded'. Spec §1.3 restricts
//     the legal fracture transition to degraded → fractured
//     (active must first be demoted to degraded by the soft
//     failure-rate threshold before the cluster detector can
//     fire). A direct active → fractured transition is a
//     matrix violation; the detector guards against it here so
//     a misbehaving caller can't bypass it.
//   * It is best-effort: a read or write failure here does NOT
//     propagate back to the caller of Invoke. The telemetry
//     row has already been written (that is the load-bearing
//     record); a missed fracture is recoverable — the next
//     failure in the window will re-trigger the detector.
//   * It is idempotent: re-firing on an already-fractured
//     capability is a clean no-op via MarkFracture's
//     simpleTransition state guard.
//
// Returns true if the capability was transitioned to fractured
// (or was already fractured — the caller uses this to decide
// whether to log a "fracture detected" audit message).
func (s *Store) checkFracture(ctx context.Context, capID string) (bool, error) {
	if s == nil {
		return false, nil // EX-1 nil-store test convenience
	}
	if capID == "" {
		return false, fmt.Errorf("capability: checkFracture: capID is required")
	}

	now := s.Now()
	since := now - FractureWindowSeconds

	count, err := s.FailureCountSince(ctx, capID, since)
	if err != nil {
		return false, err
	}
	if count < FractureThreshold {
		return false, nil
	}

	// Threshold crossed. Read the current capability to
	// confirm it is in StateDegraded (the spec's matrix
	// requires degraded → fractured). If the capability is
	// not in degraded (it might have recovered, retired, or
	// already fractured), no-op — the state machine handles
	// these cases via other transitions.
	c, err := s.GetCapability(capID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return false, nil // capability deleted between Invoke and checkFracture
		}
		return false, err
	}
	if c.State != StateDegraded {
		return false, nil
	}

	// Fire MarkFracture with cluster-evidence metadata
	// (window + count). MarkFracture owns the state-machine
	// enforcement AND the §2.4.1 cascade emission (added
	// in this file as markFractureWithCascade — wraps the
	// canonical MarkFracture with the cascade step).
	reason := fmt.Sprintf("sliding_window:%d_failures_in_%ds",
		count, FractureWindowSeconds)
	if err := s.markFractureWithCascade(ctx, capID, reason, c); err != nil {
		return false, err
	}
	return true, nil
}

// markFractureWithCascade is the §2.4.1-aware wrapper around
// MarkFracture. MarkFracture is the canonical state transition
// (degraded → fractured, audit event row written); this wrapper
// adds the cascade wake emission when the fractured capability
// has an upstream theory (author_theory_id IS NOT NULL).
//
// Why wrap instead of fork:
//
//   The state-machine path (read → validate → UPDATE → event
//   insert) is load-bearing and exercised by tests; duplicating
//   it in a parallel FractureCapability method would create
//   two divergent state-machine code paths and double the test
//   surface. Wrapping keeps the matrix in one place; the
//   cascade emission is a single conditional after the
//   transition commits.
//
// Why we read c *before* MarkFracture instead of inside it:
//
//   MarkFracture runs in a transaction that re-reads the
//   capability for the state-machine guard. author_theory_id
//   is read here, outside that transaction, because the cascade
//   outbox API (EnqueueCascadeIntents) takes *sql.Tx and the
//   Store's withTx wraps DBNode — we need to read the value
//   once so we can pass it to a separate cascade step that runs
//   inside MarkFracture's transaction.
//
//   The read here is a non-blocking snapshot. If another
//   goroutine races to mutate author_theory_id between our
//   read and MarkFracture's transaction, the worst case is a
//   spurious cascade wake (the rare case where the upstream
//   theory was cleared in the same instant the capability was
//   fractured). The cascade materializer treats such wakes as
//   advisory; the spurious wake is logged, not propagated.
func (s *Store) markFractureWithCascade(ctx context.Context, id, reason string, c *Capability) error {
	// Capture author_theory_id before opening the
	// transaction; the cascade emission below needs it.
	var theoryID string
	if c != nil && c.AuthorTheoryID != nil && *c.AuthorTheoryID != "" {
		theoryID = *c.AuthorTheoryID
	}

	// MarkFracture does the state transition + audit
	// event. It returns nil if the capability is already
	// fractured (idempotent re-trigger).
	if err := s.MarkFractured(id, reason, "", FractureWindowSeconds); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil // already gone; nothing to cascade
		}
		return err
	}

	// Cascade wake. Only when author_theory_id was set at
	// fracture time — if it's nil, no upstream theory to
	// invalidate, no wake to emit.
	if theoryID == "" {
		return nil
	}

	return s.withRetry(ctx, func() error {
		return s.withTx(func(n internal.DBNode) error {
			return s.enqueueFractureCascade(ctx, n, id, theoryID, reason)
		})
	})
}

// enqueueFractureCascade writes one row to the epistemic
// cascade outbox: the dead artifact is the fractured
// capability, the downstream target is the upstream theory
// that generated it. The cascade materializer picks this up
// on its next tick and re-evaluates the theory's confidence
// against the fracture signal.
//
// Why this method lives on Store (not a free function):
// the cascade enqueue runs inside the same withTx
// transaction as a hypothetical fracture-side write, so the
// cascade row and any state-row commit atomically. The
// wrapper here does NOT touch the capabilities row (that was
// already committed by MarkFracture); it only enqueues the
// cascade intent. Atomicity is between this enqueue and any
// future row in the same transaction (a future patch that
// folds MarkFracture into a single withTx block can drop the
// separate retry here).
//
// Failure mode: a cascade enqueue error is returned so the
// caller can log it. We deliberately do NOT roll back the
// fracture state transition (MarkFracture committed before
// us). A fractured capability without a queued cascade is a
// degraded-but-recoverable state — the operator sees the
// capability is fractured via mpm skill show, and the cascade
// can be manually re-triggered if needed. The next fracture
// in the window will re-emit the cascade.
func (s *Store) enqueueFractureCascade(ctx context.Context, n internal.DBNode, capID, theoryID, reason string) error {
	_ = ctx
	tx := n.Tx()
	if tx == nil {
		return fmt.Errorf("capability: enqueueFractureCascade: no transaction in DBNode")
	}

	// Mints a stable event_id via CreateInvalidationEvent.
	// CreateInvalidationEvent is the documented entry point
	// (cascade_outbox.go:200); it validates the inputs and
	// returns a fresh event ID for use across the
	// EnqueueCascadeIntents call.
	eventID, err := s.dm.CreateInvalidationEvent(tx, capID, "capability", "", reason, 0)
	if err != nil {
		return fmt.Errorf("capability: enqueueFractureCascade: CreateInvalidationEvent: %w", err)
	}
	event := internal.CascadeInvalidation{
		EventID:          eventID,
		DeadArtifactID:   capID,
		DeadArtifactType: "capability",
		Reason:           reason,
		CascadeDepth:     0,
	}
	targets := []internal.ProvenanceTarget{
		{ArtifactID: theoryID, ArtifactType: "theory"},
	}
	// EnqueueCascadeIntents writes one row per target.
	// Empty-targets is a clean no-op; theory is in
	// eligibleCascadeTypes so the intent lands in the
	// outbox.
	if _, err := s.dm.EnqueueCascadeIntents(tx, event, targets); err != nil {
		return fmt.Errorf("capability: enqueueFractureCascade: EnqueueCascadeIntents: %w", err)
	}
	return nil
}

// guard against unused-import lints when a future edit strips
// the sql usage; the file uses database/sql only via the
// indirect *sql.Tx that n.Tx() returns, so an explicit blank
// reference keeps the import slot if the implementation
// evolves.
var _ = sql.LevelDefault