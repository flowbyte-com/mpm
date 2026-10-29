package capability

// Tests for the RETRY BUDGET of the capability Store — the interaction
// between the Store's own outer retry (withRetry) and the transaction
// layer's snapshot restart (internal.DatabaseManager.WithTx).
//
// THE SHAPE OF THE PROBLEM
// -------------------------
// The Store retries the WHOLE transaction:
//
//	err := s.withRetry(ctx, func() error {
//		return s.withTx(func(n internal.DBNode) error {   // → dm.WithTx
//			return s.writer.Write(ctx, n, payload)
//		})
//	})
//
// withRetry's budget is invocationRetryMaxAttempts (4). WithTx has its
// own budget for the SQLITE_BUSY_SNAPSHOT class (TxSnapshotMaxAttempts,
// 3 attempts). Because withRetry treated a busy error as retryable, the
// two budgets MULTIPLIED: 4 × 3 = 12 transaction attempts and 12 callback
// invocations, where the Store's own budget says 4.
//
// A multiplied budget is not merely wasteful — it is UNEXPECTED. The
// Store's doc comment says its budget "caps the worst-case latency at
// roughly 20s"; the real worst case was larger, and it grew whenever
// either constant grew. The two budgets belong to different layers and
// neither one knew about the other.
//
// ISSUE #164's CORRECTION
// -----------------------
// The layer that owns a busy class owns its budget:
//
//   - a STALE SNAPSHOT belongs to WithTx. It restarts the transaction
//     internally, up to its own budget, and each restart takes a FRESH
//     snapshot — so by the time the error reaches the Store, the
//     transaction layer has already spent its whole budget on that class.
//     Re-driving it bought no additional resilience and multiplied the
//     budget.
//   - an AMBIGUOUS COMMIT must never be re-driven at all: a failed
//     COMMIT does not say whether the write landed. If it did, re-running
//     the callback writes a SECOND capability_invocations row — with a
//     fresh UUID, so it is not a primary-key collision — and increments
//     the capability's probation counter a second time. Both writes look
//     successful; the corruption is silent.
//
// The measurements below are against a real, file-backed WAL database:
// SQLITE_BUSY_SNAPSHOT only exists in WAL mode, and an in-memory
// shared-cache database cannot produce it at all.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	internal "github.com/flowbyte-com/mpm-core"
)

// newWALStore returns a Store over a FILE-backed WAL database, plus a
// second pool on the same file to act as a competing writer.
//
// The production DSN is used so busy_timeout and foreign-key enforcement
// are exactly what production runs with.
func newWALStore(t *testing.T) (*Store, *sql.DB) {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "mpm.db")

	db, err := sql.Open("sqlite3", internal.SqliteWriteDSN(path))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if _, err := db.Exec("PRAGMA journal_mode = WAL"); err != nil {
		t.Fatalf("PRAGMA journal_mode = WAL: %v", err)
	}

	dm := internal.NewDatabaseManagerForDB(db)
	if err := dm.InitSchema(); err != nil {
		t.Fatalf("InitSchema: %v", err)
	}

	competitor, err := sql.Open("sqlite3", internal.SqliteWriteDSN(path))
	if err != nil {
		t.Fatalf("competitor sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = competitor.Close() })

	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS tx_probe (id TEXT PRIMARY KEY, v TEXT NOT NULL)`); err != nil {
		t.Fatalf("create tx_probe: %v", err)
	}

	store := NewStoreWithClock(dm, nil)
	return store, competitor
}

// countBusySnapshotRecords counts the watchdog records with the given
// reason. Every transaction attempt that hit a stale snapshot emits
// exactly one, so the count IS the number of such attempts.
func countBusySnapshotRecords(t *testing.T, dm *internal.DatabaseManager, reason string) int {
	t.Helper()
	path := dm.WatchdogPath()
	if path == "" {
		return 0
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("read watchdog %s: %v", path, err)
	}
	n := 0
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec map[string]interface{}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if rec["reason"] == reason {
			n++
		}
	}
	return n
}

// TestWithRetry_DoesNotMultiplyTheSnapshotBudget is the budget
// measurement, after issue #164's correction.
//
// Every attempt of the callback conflicts: the competing connection
// commits between the transaction's read (its snapshot) and its write,
// so every transaction hits SQLITE_BUSY_SNAPSHOT.
//
// BEFORE the correction: the callback ran 4 × 3 = 12 times.
//
// AFTER the correction: the Store's outer loop refuses to re-drive a
// class the transaction layer already owns, so the callback runs exactly
// TxSnapshotMaxAttempts times — the transaction layer's budget, and no
// more.
func TestWithRetry_DoesNotMultiplyTheSnapshotBudget(t *testing.T) {
	store, competitor := newWALStore(t)

	attempts := 0
	err := store.withRetry(context.Background(), func() error {
		return store.withTx(func(n internal.DBNode) error {
			attempts++
			// The READ that establishes the transaction's snapshot.
			var c int
			if err := n.QueryRowTracked(`SELECT COUNT(*) FROM tx_probe`).Scan(&c); err != nil {
				return err
			}
			// The competing commit lands between the read and the write:
			// this is exactly SQLITE_BUSY_SNAPSHOT.
			if _, cerr := competitor.Exec(
				`INSERT INTO tx_probe (id, v) VALUES (?, 'y')`,
				fmt.Sprintf("c%d-%d", attempts, time.Now().UnixNano()),
			); cerr != nil {
				return cerr
			}
			_, werr := n.ExecTracked(`INSERT INTO tx_probe (id, v) VALUES ('writer', 'z')`, 0)
			return werr
		})
	})
	if err == nil {
		t.Fatal("expected the exhausted budget to surface an error")
	}
	if !internal.IsBusySnapshotError(err) {
		t.Fatalf("expected the snapshot conflict to surface, got %v", err)
	}

	dm := store.dm
	records := countBusySnapshotRecords(t, dm, "busy_snapshot")
	if records == 0 {
		t.Fatal("no busy_snapshot watchdog record: the transaction was never restarted, so this measurement says nothing")
	}

	// The transaction layer's budget, and no more.
	if attempts != internal.TxSnapshotMaxAttempts {
		t.Fatalf("the callback ran %d times, want %d — the outer retry is re-driving "+
			"a class the transaction layer already owns", attempts, internal.TxSnapshotMaxAttempts)
	}
	if records != internal.TxSnapshotMaxAttempts {
		t.Fatalf("the transaction made %d stale-snapshot attempts, want %d",
			records, internal.TxSnapshotMaxAttempts)
	}
	t.Logf("callback invocations=%d, transaction attempts=%d (inner budget %d, outer budget %d)",
		attempts, records, internal.TxSnapshotMaxAttempts, invocationRetryMaxAttempts)
}

// countingWriter is an invocationWriter that counts its calls and
// returns a fixed error. It is the seam the Store documents for exactly
// this purpose: "Tests that need to inject a fake (for retry/contention
// tests) assign Store.writer directly after construction."
type countingWriter struct {
	calls atomic.Int64
	err   error
}

func (w *countingWriter) Write(ctx context.Context, n internal.DBNode, payload *TelemetryPayload) error {
	w.calls.Add(1)
	return w.err
}

// TestWithRetry_DoesNotRedriveAnAmbiguousCommit asserts the idempotence
// fix.
//
// A failed COMMIT does not say whether the write landed. If the outer
// loop re-drives the callback, the telemetry row is written a second
// time — with a fresh UUID, so it is not a primary-key collision — and
// the capability's probation counter is incremented twice. Both writes
// look successful.
//
// The writer here returns an error that WRAPS internal.ErrTxCommitAmbiguous,
// which is exactly what WithTx now returns when its commit fails. The
// callback must run ONCE.
func TestWithRetry_DoesNotRedriveAnAmbiguousCommit(t *testing.T) {
	store, _ := newWALStore(t)

	writer := &countingWriter{
		err: fmt.Errorf("%w: %w", internal.ErrTxCommitAmbiguous, errors.New("database is locked")),
	}
	store.writer = writer

	err := store.withRetry(context.Background(), func() error {
		return store.withTx(func(n internal.DBNode) error {
			return writer.Write(context.Background(), n, &TelemetryPayload{
				CapabilityID: "cap-x",
				StartedAt:    1,
				FinishedAt:   2,
				ExitCode:     0,
			})
		})
	})
	if err == nil {
		t.Fatal("expected the ambiguous commit to surface")
	}
	if !errors.Is(err, internal.ErrTxCommitAmbiguous) {
		t.Fatalf("expected ErrTxCommitAmbiguous to be preserved, got %v", err)
	}
	if calls := writer.calls.Load(); calls != 1 {
		t.Fatalf("the callback ran %d times, want 1 — an ambiguous commit must never be re-driven, "+
			"because the write may already have landed", calls)
	}
}

// TestWithRetry_RedrivesOrdinaryContention asserts the correction did
// NOT take the Store's ordinary-contention budget away.
//
// The outer loop exists for the class the statement-level retry could
// not clear: ordinary lock contention. It must still re-drive that
// class, and it must still re-drive it up to its own budget.
func TestWithRetry_RedrivesOrdinaryContention(t *testing.T) {
	store, _ := newWALStore(t)

	writer := &countingWriter{
		err: errors.New("SQLITE_BUSY: database is locked"),
	}
	store.writer = writer

	err := store.withRetry(context.Background(), func() error {
		return store.withTx(func(n internal.DBNode) error {
			return writer.Write(context.Background(), n, &TelemetryPayload{
				CapabilityID: "cap-x",
				StartedAt:    1,
				FinishedAt:   2,
				ExitCode:     0,
			})
		})
	})
	if err == nil {
		t.Fatal("expected the busy error to surface")
	}
	// The transaction layer restarts its own budget first, and then the
	// outer loop re-drives the whole transaction up to its budget. The
	// point of the assertion is that the outer loop still fires at all:
	// more than one attempt means the ordinary class is still retried.
	if calls := writer.calls.Load(); calls < 2 {
		t.Fatalf("the callback ran %d times, want more than one — the outer loop must still "+
			"re-drive ordinary contention", calls)
	}
}
