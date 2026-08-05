package capability

import (
	"errors"
	"fmt"
	"time"

	internal "github.com/flowbyte-com/mpm-core"
)

// Store is the persistence facade for the capability subsystem.
//
// Construction:
//
//	store := capability.NewStore(dm)                  // production
//	store := capability.NewStoreWithClock(dm, clockFn) // tests
//
// The Store is safe for concurrent use. It does not hold any
// per-capability state; every method call is independent.
type Store struct {
	dm     *internal.DatabaseManager
	clock  Clock
	writer invocationWriter // EX-2 test seam; defaults to realInvocationWriter
}

// Clock is the time source the Store uses for every audit field
// (state_changed_at, occurred_at, promoted_at, last_invoked_at, ...).
//
// Production code uses realClock. Tests inject a frozenClock via
// NewStoreWithClock to make observation-window logic, probation
// countdown, and observation-window pruning deterministic.
type Clock func() time.Time

func realClock() time.Time { return time.Now().UTC() }

// NewStore returns a Store backed by the given DatabaseManager with
// the production clock (time.Now).
func NewStore(dm *internal.DatabaseManager) *Store {
	return NewStoreWithClock(dm, realClock)
}

// NewStoreWithClock returns a Store with an injected clock. Test code
// passes a frozen or fast-forwarded clock; production code uses
// NewStore instead.
func NewStoreWithClock(dm *internal.DatabaseManager, clock Clock) *Store {
	if clock == nil {
		clock = realClock
	}
	s := &Store{dm: dm, clock: clock}
	// EX-2: default to the real SQLite writer. Tests that
	// need to inject a fake (for retry/contention tests)
	// assign Store.writer directly after construction.
	s.writer = &realInvocationWriter{store: s}
	return s
}

// withTx is the private seam that every Store method funnels through.
// It wraps internal.DatabaseManager.WithTx; the closure receives an
// internal.DBNode that delegates to the active transaction. Passing a
// DBNode to a helper makes the helper's queries participate in the
// same transaction — a failure in any helper rolls back every prior
// statement in this WithTx block.
func (s *Store) withTx(fn func(internal.DBNode) error) error {
	return s.dm.WithTx(fn)
}

// Now returns the current Unix-epoch seconds per the Store's clock.
// Used everywhere the Store writes an audit field. Returning through
// a method (rather than calling s.clock() inline) gives test code
// one override seam for all time-stamping.
func (s *Store) Now() int64 {
	return s.clock().Unix()
}

// ErrBothBroken is the sentinel returned from inside
// ExecuteRollbackWithShatter's transaction when the predecessor
// revival UPDATE returns rowcount == 0 (the target is fractured,
// not retired). The outer wrapper catches this via errors.Is and
// transitions to the §5.2.1 escalation path (a separate transaction).
var ErrBothBroken = errors.New("capability: rollback target is also broken")

// wrapDBError prefixes a capability-subsystem context tag onto the
// underlying database error. Keeps call sites terse while preserving
// the original error chain via %w so callers can errors.Is down to
// the underlying cause.
func wrapDBError(op string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("capability: %s: %w", op, err)
}
