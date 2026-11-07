package internal

// The resolver's status-carrying error wrapper — issue #178.
//
// ResolveWake's transactional body runs inside dm.WithTx, whose callback
// can only return an error, so the resolver's public status taxonomy
// (wake_not_found, lookup_failed, not_a_wake, update_failed, …) rides
// along with the error in a wakeResolveStatusError.
//
// The wrapper's contract is load-bearing:
//
//   - it must UNWRAP, or WithTx could neither recognise a
//     SQLITE_BUSY_SNAPSHOT (so the resolver would never recover from the
//     contention) nor honour a cancellation (so an abandoned attempt would
//     be replayed behind the caller's back).
//   - it must carry the status unchanged, so the public envelope's
//     machine-readable status survives the move into a transaction.

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/mattn/go-sqlite3"
)

func TestWakeResolveStatusError_PreservesCancellation(t *testing.T) {
	wrapped := &wakeResolveStatusError{
		status: "update_failed",
		err:    fmt.Errorf("%w: %w", context.Canceled, errors.New("abandoned")),
	}
	if !errors.Is(wrapped, context.Canceled) {
		t.Fatalf("the status wrapper must not swallow a cancellation: %v", wrapped)
	}
	if wrapped.Error() == "" {
		t.Fatal("the status wrapper must still render its cause")
	}
}

func TestWakeResolveStatusError_PreservesSnapshotConflictClassification(t *testing.T) {
	se := sqlite3.Error{Code: sqlite3.ErrBusy, ExtendedCode: sqlite3.ErrBusySnapshot}
	wrapped := &wakeResolveStatusError{status: "update_failed", err: se}

	if !IsBusySnapshotError(wrapped) {
		t.Fatalf("the status wrapper must not swallow a snapshot conflict: %v", wrapped)
	}
	if !IsBusyError(wrapped) {
		t.Fatalf("IsBusyError must remain true through the wrapper: %v", wrapped)
	}
}

func TestWakeResolveStatusError_CarriesTheStatus(t *testing.T) {
	wrapped := &wakeResolveStatusError{status: "not_a_wake", err: errors.New("refused")}
	if wrapped.status != "not_a_wake" {
		t.Fatalf("expected the status to survive, got %q", wrapped.status)
	}
	var target *wakeResolveStatusError
	if !errors.As(fmt.Errorf("outer: %w", wrapped), &target) {
		t.Fatal("the status wrapper must be recoverable with errors.As")
	}
	if target.status != "not_a_wake" {
		t.Fatalf("expected the status to survive wrapping, got %q", target.status)
	}
}
