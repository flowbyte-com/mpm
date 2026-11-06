// Regression tests for the retention sweeps' retirement race (issue #178,
// defect W3).
//
// THE DEFECT
// ----------
// Both retention sweeps work in two steps:
//
//	1. a candidate SELECT on the POOL, outside any transaction, which
//	   picks the pending notification-kind / cron-kind wakes that are
//	   overdue;
//	2. a retirement UPDATE inside a transaction, one statement per
//	   candidate.
//
// Step 1 and step 2 were not atomic with respect to each other. The
// retirement UPDATE carried only `WHERE id = ?` — it did NOT re-check
// `fired = 0`. So a wake that another writer retired between the
// candidate SELECT and the retirement UPDATE was still updated by the
// sweep, and the sweep OVERWROTE that writer's `fired_at` and its
// `metadata`.
//
// The writer that matters is the scheduler settlement
// (internal/core/wake_settlement.go: MarkWakeFired), which records the
// moment the handler finished. The sweep's overwrite destroyed the only
// record of when the handler actually completed, and replaced the
// settlement's metadata with its own expiration audit note.
//
// THE FIX
// -------
// The retirement UPDATE now carries `AND fired = 0`, which makes it a
// compare-and-set on the retirement flag: the sweep retires a wake only
// if it is still pending. A wake retired in the meantime is SKIPPED —
// not overwritten, not counted as retired, and it does not abort the
// batch (the deferred Rollback would otherwise undo every candidate the
// sweep had already retired).
//
// HOW THESE TESTS ARE DETERMINISTIC
// ---------------------------------
// Nothing here depends on a timing window or a sleep. A SQLite
// AUTHORIZER is installed on every pooled connection of the sweep's
// pool; it pauses the sweep's retirement UPDATE at PREPARE time —
// exactly the boundary between the candidate SELECT (which has already
// completed, on the pool) and the UPDATE (which is inside the
// retirement transaction). The test observes the arrival explicitly
// before it does anything else, performs the competing retirement on a
// SECOND pool over the same file, and only then releases the sweep.
//
// Two pools are required. The competitor must commit from a different
// connection, or it would block behind the sweep's own transaction
// rather than racing it.
//
// The sweep's UPDATE is the only UPDATE on scheduled_wakes in the
// sweep's path (the candidate SELECT and the read-back are both
// SELECTs), so the barrier gates exactly the statement under test.
//
// The competitor is the REAL settlement path — core.MarkWakeFired —
// not a hand-written UPDATE, so the test proves the property against
// the code that actually runs in production.

package scheduler

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/mattn/go-sqlite3"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// ── the deterministic statement-boundary barrier ──────────────────────────

// sweepUpdateBarrier pauses the first UPDATE on scheduled_wakes issued
// by the pool it is installed on.
//
// SQLite's authorizer callback fires while a statement is being
// prepared. For SQLITE_TRANSACTION the arg1 is "BEGIN"/"COMMIT"/"ROLLBACK";
// for SQLITE_INSERT / SQLITE_UPDATE / SQLITE_DELETE the arg1 is the TABLE
// name and arg2 is the column name.
//
// The barrier fires ONCE. The sweep's candidate SELECT and its
// read-back are SELECTs, so the only UPDATE the sweep issues on
// scheduled_wakes is the retirement statement under test.
type sweepUpdateBarrier struct {
	arrived chan struct{}
	release chan struct{}

	once sync.Once
}

func newSweepUpdateBarrier() *sweepUpdateBarrier {
	return &sweepUpdateBarrier{
		// Buffered so the authorizer never blocks on the send: the
		// test reads `arrived` after the sweep goroutine has parked.
		arrived: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
}

// authorizerFor returns the authorizer to install on ONE pooled
// connection.
func (b *sweepUpdateBarrier) authorizerFor() func(int, string, string, string) int {
	return func(action int, arg1, arg2, dbName string) int {
		if action == sqlite3.SQLITE_UPDATE && arg1 == "scheduled_wakes" {
			b.once.Do(func() {
				b.arrived <- struct{}{}
				<-b.release
			})
		}
		return sqlite3.SQLITE_OK
	}
}

// releaseOne lets the paused statement proceed.
func (b *sweepUpdateBarrier) releaseOne() { b.release <- struct{}{} }

// installSweepBarrier registers the barrier's authorizer on EVERY
// physical connection the pool can hand out.
//
// The pool's connection count is fixed and its idle connections are
// retained, so pinning `conns` handles at once guarantees that all
// `conns` distinct physical connections the pool owns are registered —
// the sweep cannot draw an unregistered connection.
func installSweepBarrier(t *testing.T, pool *sql.DB, b *sweepUpdateBarrier, conns int) {
	t.Helper()

	handles := make([]*sql.Conn, 0, conns)
	for i := 0; i < conns; i++ {
		h, err := pool.Conn(context.Background())
		if err != nil {
			t.Fatalf("pin connection %d: %v", i, err)
		}
		handles = append(handles, h)
	}
	for i, h := range handles {
		auth := b.authorizerFor()
		err := h.Raw(func(dc any) error {
			sc, ok := dc.(*sqlite3.SQLiteConn)
			if !ok {
				return fmt.Errorf("driver connection is %T, not *sqlite3.SQLiteConn", dc)
			}
			sc.RegisterAuthorizer(auth)
			return nil
		})
		if err != nil {
			t.Fatalf("register authorizer on connection %d: %v", i, err)
		}
	}
	for _, h := range handles {
		if err := h.Close(); err != nil {
			t.Fatalf("release pinned connection: %v", err)
		}
	}
}

// ── the harness ───────────────────────────────────────────────────────────

// sweepRaceHarness is the disposable fixture: a file-backed WAL
// database with the production schema, opened as TWO independent pools
// over the same file.
//
// WAL is not optional: the competing writer needs its own connection,
// and the competing commit must come from a DIFFERENT connection than
// the one holding the sweep's transaction.
type sweepRaceHarness struct {
	// sweepDB is the pool the sweep runs on. It carries the barrier.
	sweepDB *sql.DB
	// otherDB is the pool the competing retirement runs on. It carries
	// no authorizer, so the competitor is never gated.
	otherDB *sql.DB
}

func newSweepRaceHarness(t *testing.T, conns int) *sweepRaceHarness {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "mpm.db")
	dsn := path + "?_busy_timeout=5000&_journal_mode=WAL&_synchronous=NORMAL"

	sweepDB, err := sql.Open("sqlite3", dsn)
	if err != nil {
		t.Fatalf("open sweep pool: %v", err)
	}
	t.Cleanup(func() { _ = sweepDB.Close() })
	sweepDB.SetMaxOpenConns(conns)
	sweepDB.SetMaxIdleConns(conns)

	otherDB, err := sql.Open("sqlite3", dsn)
	if err != nil {
		t.Fatalf("open competitor pool: %v", err)
	}
	t.Cleanup(func() { _ = otherDB.Close() })
	otherDB.SetMaxOpenConns(conns)
	otherDB.SetMaxIdleConns(conns)

	if _, err := sweepDB.Exec(testSchema); err != nil {
		t.Fatalf("install schema: %v", err)
	}
	if _, err := sweepDB.Exec(`PRAGMA journal_mode = WAL`); err != nil {
		t.Fatalf("PRAGMA journal_mode = WAL: %v", err)
	}
	return &sweepRaceHarness{sweepDB: sweepDB, otherDB: otherDB}
}

// seedStaleWake inserts a pending wake that is eligible for the sweep.
func (h *sweepRaceHarness) seedStaleWake(t *testing.T, id string, target time.Time, kind string) {
	t.Helper()
	metadata := "{}"
	if kind != "" {
		metadata = fmt.Sprintf(`{"kind":%q}`, kind)
	}
	if _, err := h.sweepDB.Exec(
		`INSERT INTO scheduled_wakes (id, target_time, reason, fired, created_by, metadata)
		 VALUES (?, ?, ?, 0, 'test', ?)`,
		id, target.Unix(), "test-wake "+id, metadata,
	); err != nil {
		t.Fatalf("seed wake %s: %v", id, err)
	}
}

// wakeRow is the complete evidence of a wake's retirement: the flag,
// the timestamp, and the metadata that carries the audit note.
type wakeRow struct {
	fired    int
	firedAt  int64
	metadata string
}

func readWakeRow(t *testing.T, db *sql.DB, id string) wakeRow {
	t.Helper()
	var r wakeRow
	err := db.QueryRow(
		`SELECT fired, COALESCE(fired_at, 0), COALESCE(metadata, '{}') FROM scheduled_wakes WHERE id = ?`,
		id,
	).Scan(&r.fired, &r.firedAt, &r.metadata)
	if err != nil {
		t.Fatalf("read wake %s: %v", id, err)
	}
	return r
}

// ── the tests ─────────────────────────────────────────────────────────────

// TestSweepDoesNotOverwriteAConcurrentSettlement is the W3 regression.
//
// A notification wake is selected as a sweep candidate. Before the
// sweep's retirement UPDATE runs, the scheduler settles the wake — the
// real settlement path, core.MarkWakeFired — which records the moment
// the handler finished. The sweep must then SKIP the wake: it must not
// change `fired_at`, and it must not stamp its own expiration note over
// the settlement's metadata.
//
// Without the `AND fired = 0` guard the sweep's UPDATE matches the row
// by id alone and overwrites both fields.
func TestSweepDoesNotOverwriteAConcurrentSettlement(t *testing.T) {
	h := newSweepRaceHarness(t, 2)
	barrier := newSweepUpdateBarrier()
	installSweepBarrier(t, h.sweepDB, barrier, 2)

	now := time.Now()
	// The sweep is given a clock one hour ahead of the settlement's.
	// That is not realism for its own sake: it makes the `fired_at`
	// assertion decisive even if both writers land in the same wall-
	// clock second. The wake is seeded well inside the 7-day window
	// relative to BOTH clocks, so it is a candidate either way.
	sweepNow := now.Add(time.Hour)

	h.seedStaleWake(t, "wk-raced", now.Add(-30*24*time.Hour), "notification")

	type sweepResult struct {
		retired int
		err     error
	}
	done := make(chan sweepResult, 1)
	go func() {
		retired, err := SweepOverdueNotificationWakes(context.Background(), h.sweepDB, sweepNow)
		done <- sweepResult{retired: retired, err: err}
	}()

	// Wait for the sweep to park at the retirement UPDATE. The
	// candidate SELECT has already run at this point.
	select {
	case <-barrier.arrived:
	case <-time.After(30 * time.Second):
		t.Fatalf("the sweep never reached the retirement UPDATE")
	}

	// The competing writer: the REAL scheduler settlement, on the
	// other pool.
	if err := mpminternal.MarkWakeFired(h.otherDB, "wk-raced", ""); err != nil {
		t.Fatalf("settle the wake concurrently: %v", err)
	}
	settled := readWakeRow(t, h.otherDB, "wk-raced")
	if settled.fired != 1 {
		t.Fatalf("the settlement must retire the wake; fired=%d", settled.fired)
	}
	if settled.firedAt == sweepNow.Unix() {
		t.Fatalf("the fixture is degenerate: the settlement and the sweep share a timestamp")
	}

	// Let the sweep proceed.
	barrier.releaseOne()

	var res sweepResult
	select {
	case res = <-done:
	case <-time.After(30 * time.Second):
		t.Fatalf("the sweep did not return")
	}
	if res.err != nil {
		t.Fatalf("a candidate retired concurrently must not fail the sweep: %v", res.err)
	}
	if res.retired != 0 {
		t.Errorf("the sweep retired %d wakes, want 0: the candidate was already retired", res.retired)
	}

	after := readWakeRow(t, h.otherDB, "wk-raced")
	if after.firedAt != settled.firedAt {
		t.Errorf("the sweep overwrote the settlement's fired_at: %d -> %d",
			settled.firedAt, after.firedAt)
	}
	if after.metadata != settled.metadata {
		t.Errorf("the sweep overwrote the settlement's metadata:\n  before=%s\n  after =%s",
			settled.metadata, after.metadata)
	}
	if reason := extractMetadataString(after.metadata, ExpirationMetadataKey+".reason"); reason != "" {
		t.Errorf("the sweep stamped its expiration note over the settlement's metadata: %s", after.metadata)
	}
}

// TestSweepSkipsAConcurrentlyRetiredCandidateAndContinuesTheBatch is
// the batch-continuation guarantee.
//
// One candidate is retired by another writer while the sweep is parked.
// The sweep must skip it AND still retire the remaining candidates.
//
// The second half matters as much as the first: a zero-row guarded
// UPDATE is the "lost the race" outcome, and the substrate's own CAS
// idiom treats it as a benign no-op. Treating it as an error would
// abort the batch, and the deferred Rollback would undo every
// candidate the sweep had already retired.
func TestSweepSkipsAConcurrentlyRetiredCandidateAndContinuesTheBatch(t *testing.T) {
	h := newSweepRaceHarness(t, 2)
	barrier := newSweepUpdateBarrier()
	installSweepBarrier(t, h.sweepDB, barrier, 2)

	now := time.Now()
	sweepNow := now.Add(time.Hour)

	// The candidate SELECT orders by target_time ASC, so the wake
	// seeded with the OLDEST target is retired first — that is the one
	// the barrier parks on and the one the competitor retires.
	h.seedStaleWake(t, "wk-first", now.Add(-90*24*time.Hour), "notification")
	h.seedStaleWake(t, "wk-second", now.Add(-30*24*time.Hour), "notification")

	done := make(chan int, 1)
	errc := make(chan error, 1)
	go func() {
		retired, err := SweepOverdueNotificationWakes(context.Background(), h.sweepDB, sweepNow)
		if err != nil {
			errc <- err
			return
		}
		done <- retired
	}()

	select {
	case <-barrier.arrived:
	case <-time.After(30 * time.Second):
		t.Fatalf("the sweep never reached the retirement UPDATE")
	}

	if err := mpminternal.MarkWakeFired(h.otherDB, "wk-first", ""); err != nil {
		t.Fatalf("settle the wake concurrently: %v", err)
	}
	settled := readWakeRow(t, h.otherDB, "wk-first")

	barrier.releaseOne()

	var retired int
	select {
	case err := <-errc:
		t.Fatalf("a candidate retired concurrently must not fail the sweep: %v", err)
	case retired = <-done:
	case <-time.After(30 * time.Second):
		t.Fatalf("the sweep did not return")
	}
	if retired != 1 {
		t.Errorf("the sweep retired %d wakes, want 1: only the second candidate was retirable", retired)
	}

	// The concurrently retired candidate is untouched.
	after := readWakeRow(t, h.otherDB, "wk-first")
	if after.firedAt != settled.firedAt || after.metadata != settled.metadata {
		t.Errorf("the sweep altered the concurrently retired wake:\n  before=%+v\n  after =%+v",
			settled, after)
	}

	// The remaining candidate WAS retired, with the sweep's audit note.
	second := readWakeRow(t, h.otherDB, "wk-second")
	if second.fired != 1 {
		t.Errorf("wk-second: fired=%d after the sweep, want 1", second.fired)
	}
	if second.firedAt != sweepNow.Unix() {
		t.Errorf("wk-second: fired_at=%d, want %d", second.firedAt, sweepNow.Unix())
	}
	if reason := extractMetadataString(second.metadata, ExpirationMetadataKey+".reason"); reason != ExpirationReasonValue {
		t.Errorf("wk-second: %s.reason=%q, want %q (metadata: %s)",
			ExpirationMetadataKey, reason, ExpirationReasonValue, second.metadata)
	}
}

// TestCronSweepDoesNotOverwriteAConcurrentSettlement is the same
// guarantee for the 1-hour cron-kind sweep, whose retirement UPDATE
// had the identical missing guard.
func TestCronSweepDoesNotOverwriteAConcurrentSettlement(t *testing.T) {
	h := newSweepRaceHarness(t, 2)
	barrier := newSweepUpdateBarrier()
	installSweepBarrier(t, h.sweepDB, barrier, 2)

	now := time.Now()
	sweepNow := now.Add(time.Hour)

	h.seedStaleWake(t, "wk-cron-raced", now.Add(-30*24*time.Hour), "cron")

	done := make(chan int, 1)
	errc := make(chan error, 1)
	go func() {
		retired, err := SweepOverdueCronWakes(context.Background(), h.sweepDB, sweepNow, 10)
		if err != nil {
			errc <- err
			return
		}
		done <- retired
	}()

	select {
	case <-barrier.arrived:
	case <-time.After(30 * time.Second):
		t.Fatalf("the cron sweep never reached the retirement UPDATE")
	}

	if err := mpminternal.MarkWakeFired(h.otherDB, "wk-cron-raced", ""); err != nil {
		t.Fatalf("settle the wake concurrently: %v", err)
	}
	settled := readWakeRow(t, h.otherDB, "wk-cron-raced")

	barrier.releaseOne()

	select {
	case err := <-errc:
		t.Fatalf("a candidate retired concurrently must not fail the cron sweep: %v", err)
	case retired := <-done:
		if retired != 0 {
			t.Errorf("the cron sweep retired %d wakes, want 0: the candidate was already retired", retired)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("the cron sweep did not return")
	}

	after := readWakeRow(t, h.otherDB, "wk-cron-raced")
	if after.firedAt != settled.firedAt {
		t.Errorf("the cron sweep overwrote the settlement's fired_at: %d -> %d",
			settled.firedAt, after.firedAt)
	}
	if after.metadata != settled.metadata {
		t.Errorf("the cron sweep overwrote the settlement's metadata:\n  before=%s\n  after =%s",
			settled.metadata, after.metadata)
	}
}
