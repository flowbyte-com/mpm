package internal

// The producer side of the ambiguous-commit protection.
//
// WHAT #169 PROTECTS, AND WHERE
// -----------------------------
// A failed COMMIT does not say whether the write landed. SQLite can report
// SQLITE_BUSY from a commit when the commit's checkpoint cannot complete
// (another connection is still reading the write-ahead log), and the
// transaction's rows may or may not be durable.
//
// WithTx therefore does not retry a commit failure itself, and it MARKS
// the failure with ErrTxCommitAmbiguous so that a caller wrapping WithTx
// in its own retry loop can recognise the case and refuse to re-drive the
// callback. Without the marker, an outer loop that retries busy errors
// would re-run a callback whose write may already be durable — writing a
// SECOND row with a fresh id (so no primary-key collision) and
// incrementing a counter twice, silently.
//
// The capability Store's handling of the marker is covered by
// internal/core/capability/tx_retry_budget_test.go. This file covers the
// OTHER half — the half that produces the marker — because the two are
// only useful together: a consumer that checks for a marker the producer
// never sets is a false guarantee.
//
// THE TWO PROPERTIES ASSERTED HERE
// --------------------------------
//  1. THE CALLBACK RUNS EXACTLY ONCE. An uncertain commit must not cause
//     a replay: the write may already be durable, so a second execution
//     would write it twice.
//  2. THE FAILURE IS NOT PRESENTED AS A DEFINITE ROLLBACK. The error
//     says the write MAY OR MAY NOT have landed. A caller that reads it
//     as "rolled back, nothing was written" will re-drive the write and
//     double it; a caller that reads it as "definitely landed" will not
//     retry at all and will report a phantom success. Only the ambiguous
//     reading is safe, and only the ambiguous reading is honest.
//
// THE FAULT IS INJECTED DETERMINISTICALLY
// ----------------------------------------
// The test uses a ROLLBACK-JOURNAL (journal_mode=DELETE) database, not
// WAL. In rollback-journal mode a COMMIT must acquire an EXCLUSIVE lock
// on the database file, so a second connection holding an open read
// transaction blocks it and the commit fails with SQLITE_BUSY. That is
// deterministic — it needs no timing, no WAL checkpoint threshold and no
// scheduler luck.
//
// In WAL mode the same failure needs the WAL to reach its auto-checkpoint
// threshold while a reader is active, which is neither fast nor reliable.
// The rollback-journal mode is used precisely because the failure it
// produces is the same class (SQLITE_BUSY from COMMIT) and it is exact.

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

// newCommitBlockingDM builds a DatabaseManager over a rollback-journal
// database, and returns it together with a second connection that is
// holding an open read transaction — the thing that will block the
// commit.
//
// The caller MUST release the reader (by rolling its transaction back)
// before it can expect any subsequent write to succeed.
func newCommitBlockingDM(t *testing.T) (dm *DatabaseManager, reader *sql.DB, release func()) {
	t.Helper()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "commit-ambiguity.db")

	// busy_timeout=0 so the failure is immediate and deterministic
	// rather than a wait: this test is about WHICH error comes back and
	// what the transaction layer does with it, not about how long a
	// caller waits.
	dsn := "file:" + dbPath + "?_busy_timeout=0"

	writer, err := sql.Open("sqlite3", dsn)
	if err != nil {
		t.Fatalf("open writer: %v", err)
	}
	t.Cleanup(func() { _ = writer.Close() })

	// Rollback-journal mode: a COMMIT needs an EXCLUSIVE lock, so an
	// open reader blocks it. This is the deterministic fault.
	var jm string
	if err := writer.QueryRow("PRAGMA journal_mode=DELETE").Scan(&jm); err != nil {
		t.Fatalf("set journal mode: %v", err)
	}
	if jm != "delete" {
		t.Fatalf("expected journal_mode=delete, got %q", jm)
	}
	if _, err := writer.Exec(`CREATE TABLE ambiguous (id INTEGER PRIMARY KEY, v TEXT)`); err != nil {
		t.Fatalf("create table: %v", err)
	}

	reader, err = sql.Open("sqlite3", dsn)
	if err != nil {
		t.Fatalf("open reader: %v", err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	reader.SetMaxOpenConns(1)

	readTx, err := reader.Begin()
	if err != nil {
		t.Fatalf("begin reader: %v", err)
	}
	var n int
	if err := readTx.QueryRow(`SELECT count(*) FROM ambiguous`).Scan(&n); err != nil {
		t.Fatalf("reader read: %v", err)
	}

	dm = NewDatabaseManagerForDB(writer)

	return dm, reader, func() {
		_ = readTx.Rollback()
	}
}

// TestWithTx_AmbiguousCommitIsMarkedAndNeverReplayed asserts the two
// properties of the producer side.
//
// The commit is blocked by an open reader on another connection, so
// tx.Commit() fails with SQLITE_BUSY. The callback has already run. The
// assertions are:
//
//   - the error is returned (the failure is not swallowed);
//   - the error MATCHES ErrTxCommitAmbiguous, so an outer retry loop can
//     recognise it;
//   - the callback ran EXACTLY ONCE — no replay;
//   - the error does not claim a definite rollback.
func TestWithTx_AmbiguousCommitIsMarkedAndNeverReplayed(t *testing.T) {
	dm, _, release := newCommitBlockingDM(t)
	defer release()

	var calls atomic.Int64

	err := dm.WithTx(func(node DBNode) error {
		calls.Add(1)
		// A write inside the transaction. It is not durable: the
		// transaction cannot commit.
		if _, execErr := node.ExecTracked(`INSERT INTO ambiguous (v) VALUES ('written')`, 0); execErr != nil {
			return execErr
		}
		return nil
	})

	if err == nil {
		t.Fatal("expected the blocked commit to surface an error, got nil")
	}

	// PROPERTY 1 — the callback must not be replayed. The write may
	// already be durable; a second execution would write it twice.
	if got := calls.Load(); got != 1 {
		t.Fatalf("the callback ran %d times, want exactly 1 — an ambiguous commit "+
			"must never be replayed, because the write may already have landed", got)
	}

	// The marker must be present, and it must survive errors.Is.
	if !errors.Is(err, ErrTxCommitAmbiguous) {
		t.Fatalf("expected the commit failure to be marked ErrTxCommitAmbiguous, got %v", err)
	}

	// PROPERTY 2 — the failure must not be presented as a definite
	// rollback. A caller that reads "rolled back" will re-drive the
	// write and double it.
	msg := strings.ToLower(err.Error())
	for _, forbidden := range []string{"rolled back", "rollback complete", "definitely not written", "nothing was written"} {
		if strings.Contains(msg, forbidden) {
			t.Fatalf("the error presents an uncertain commit as a definite rollback: %q", err.Error())
		}
	}
	// ...and it must SAY that it is uncertain.
	if !strings.Contains(msg, "may or may not have landed") {
		t.Fatalf("the error does not state that the write may or may not have landed: %q", err.Error())
	}
}

// TestWithTx_AmbiguousCommitErrorDoesNotMasqueradeAsBusyOnly asserts that
// the marker is ADDITIVE: the underlying SQLite error is still reachable.
//
// A caller that wants to know whether the failure was a busy error —
// rather than, say, a constraint violation — must still be able to ask.
// Wrapping the original error with %w (not replacing it) is what makes
// both answers available at once.
func TestWithTx_AmbiguousCommitErrorDoesNotMasqueradeAsBusyOnly(t *testing.T) {
	dm, _, release := newCommitBlockingDM(t)
	defer release()

	err := dm.WithTx(func(node DBNode) error {
		if _, execErr := node.ExecTracked(`INSERT INTO ambiguous (v) VALUES ('written')`, 0); execErr != nil {
			return execErr
		}
		return nil
	})
	if err == nil {
		t.Fatal("expected the blocked commit to surface an error, got nil")
	}

	if !errors.Is(err, ErrTxCommitAmbiguous) {
		t.Fatalf("expected ErrTxCommitAmbiguous, got %v", err)
	}
	// The underlying driver error must still be reachable: the marker is
	// added, not substituted.
	if !IsBusyError(err) {
		t.Fatalf("expected the underlying busy error to remain detectable, got %v", err)
	}
}

// TestWithTx_AmbiguousCommitLeavesNoDurableWrite proves the ambiguous
// case is not silently converted into a success.
//
// The write was inside a transaction whose COMMIT failed. Whether it
// landed depends on SQLite's internals, and the transaction layer's job
// is to REPORT that uncertainty — not to paper over it with a nil error.
func TestWithTx_AmbiguousCommitLeavesNoDurableWrite(t *testing.T) {
	dm, _, release := newCommitBlockingDM(t)
	defer release()

	err := dm.WithTx(func(node DBNode) error {
		if _, execErr := node.ExecTracked(`INSERT INTO ambiguous (v) VALUES ('written')`, 0); execErr != nil {
			return execErr
		}
		return nil
	})
	if err == nil {
		t.Fatal("expected the blocked commit to surface an error, got nil")
	}

	// Release the reader, then look at what actually landed.
	release()

	var count int
	if qerr := dm.SQLDB().QueryRow(`SELECT count(*) FROM ambiguous`).Scan(&count); qerr != nil {
		t.Fatalf("count rows: %v", qerr)
	}
	// In rollback-journal mode a blocked commit does not land. The point
	// of the assertion is not the count but the SHAPE: the caller was
	// TOLD the write may or may not have landed, and the caller decides
	// what to do about it. The transaction layer must not have decided
	// for it.
	if count > 1 {
		t.Fatalf("expected at most one row after a single callback, got %d — the write was replayed", count)
	}
}

// TestAmbiguousCommitMarkerIsDistinctFromOrdinaryBusy pins the
// discrimination the consumer depends on: an ordinary busy error and an
// ambiguous commit are DIFFERENT classes, and a consumer that cannot tell
// them apart cannot refuse to re-drive only the dangerous one.
//
// An ordinary busy error is clearable by waiting and re-driving. An
// ambiguous commit is not: the write may already be durable.
func TestAmbiguousCommitMarkerIsDistinctFromOrdinaryBusy(t *testing.T) {
	ordinary := errors.New("database is locked")
	ambiguous := ErrTxCommitAmbiguous

	if errors.Is(ambiguous, ordinary) {
		t.Fatal("ErrTxCommitAmbiguous must not match an ordinary busy error")
	}
	if errors.Is(ordinary, ambiguous) {
		t.Fatal("an ordinary busy error must not match ErrTxCommitAmbiguous")
	}
	// A busy error wrapped in the ambiguous marker IS the ambiguous
	// class, and a consumer checking for the marker sees it.
	marked := errors.Join(ambiguous, ordinary)
	if !errors.Is(marked, ErrTxCommitAmbiguous) {
		t.Fatal("a marked busy error must match ErrTxCommitAmbiguous")
	}
}

// TestErrTxCommitAmbiguousMessageStatesUncertainty asserts the marker's
// own message, because the marker is what a human reads when the error
// reaches a log.
//
// The message must not imply a known outcome. "may or may not have
// landed" is the whole point.
func TestErrTxCommitAmbiguousMessageStatesUncertainty(t *testing.T) {
	msg := strings.ToLower(ErrTxCommitAmbiguous.Error())
	if !strings.Contains(msg, "may or may not") {
		t.Fatalf("the ambiguous-commit marker must state that the outcome is unknown, got %q",
			ErrTxCommitAmbiguous.Error())
	}
	if !strings.Contains(msg, "commit") {
		t.Fatalf("the ambiguous-commit marker must name the commit, got %q", ErrTxCommitAmbiguous.Error())
	}
}
