package internal

// Regression suite for issue #178 — the ResolveWake SQLite contention
// defect.
//
// THE DEFECT
// ----------
// ResolveWake READS the wake row and then WRITES it. That read-then-write
// pair is what makes a DEFERRED transaction vulnerable: the read takes the
// transaction's read snapshot, and the write has to upgrade it to the
// write lock. In WAL mode ANY commit by another connection between those
// two points invalidates the snapshot, and the upgrade fails with
// SQLITE_BUSY_SNAPSHOT.
//
// SQLITE_BUSY_SNAPSHOT is NOT ordinary contention:
//
//   - it shares its PRIMARY code (SQLITE_BUSY = 5) and its MESSAGE
//     ("database is locked") with ordinary contention, so the two cannot
//     be told apart by text — only the EXTENDED code (517) tells them
//     apart;
//   - it is never serviced by the busy handler, so busy_timeout is never
//     consulted and the error returns in MICROSECONDS rather than after
//     the 5 s the pool is configured to wait;
//   - a statement-level retry can NEVER clear it, because the snapshot
//     belongs to the TRANSACTION and the transaction's snapshot does not
//     change between attempts.
//
// The pre-fix code bypassed BOTH retry layers: it opened its own deferred
// transaction with dm.db.Begin() and wrote with a raw tx.Exec. Two
// concurrent resolutions of DIFFERENT agent-owned wakes therefore lost one
// resolution with "resolve wake: update: database is locked" — the
// winner's commit invalidated the loser's read snapshot.
//
// THE FIX
// -------
// The transactional body runs inside dm.WithTx, the substrate's existing
// transaction layer, which restarts a transaction whose snapshot went stale
// (bounded by TxSnapshotMaxAttempts). The audit row stays OUTSIDE the
// transaction, so a restart cannot duplicate it.
//
// HOW THESE TESTS ARE DETERMINISTIC
// ----------------------------------
// Nothing here depends on a timing window or a sleep. A SQLite AUTHORIZER
// is installed on every pooled connection and pauses the resolver at
// PREPARE time — exactly the boundary between its read (which took the
// snapshot) and its write (which needs the snapshot to still be current).
// The test observes the arrival explicitly before it does anything else,
// so the interleaving is chosen, not raced for.
//
// The same gate is also the ANTI-SERIALISATION assertion: the tests hold
// BOTH resolvers at the UPDATE boundary before releasing either. A
// resolver that had been serialised behind a global lock could not reach
// the boundary while the other was parked there, so the test would time
// out. That is what distinguishes "genuine contention recovery" from
// "accidentally serialising all operations".

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/mattn/go-sqlite3"
)

// ── harness ───────────────────────────────────────────────────────────────

// resolveContentionHarness is the disposable fixture: a file-backed WAL
// DatabaseManager opened with the PRODUCTION DSN (so busy_timeout=5000
// and foreign_keys are exactly what production runs with), plus an
// INDEPENDENT competitor pool over the SAME file.
//
// WAL is not optional: SQLITE_BUSY_SNAPSHOT only exists in WAL mode. The
// competing writer needs its OWN pool, because a transaction on the same
// pool occupies one connection and the competing commit must come from a
// DIFFERENT connection to invalidate the first transaction's snapshot.
type resolveContentionHarness struct {
	dm         *DatabaseManager
	pool       *sql.DB
	competitor *sql.DB
}

func newResolveContentionHarness(t *testing.T, poolConns int) *resolveContentionHarness {
	t.Helper()
	PinIsolatedWorkspace(t)

	dir := t.TempDir()
	path := filepath.Join(dir, "mpm.db")

	pool, err := sql.Open("sqlite3", SqliteWriteDSN(path))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	pool.SetMaxOpenConns(poolConns)
	pool.SetMaxIdleConns(poolConns)

	if _, err := pool.Exec("PRAGMA journal_mode = WAL"); err != nil {
		t.Fatalf("PRAGMA journal_mode = WAL: %v", err)
	}

	dm := NewDatabaseManagerForDB(pool)
	if err := dm.InitSchema(); err != nil {
		t.Fatalf("InitSchema: %v", err)
	}

	competitor, err := sql.Open("sqlite3", SqliteWriteDSN(path))
	if err != nil {
		t.Fatalf("competitor sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = competitor.Close() })

	return &resolveContentionHarness{dm: dm, pool: pool, competitor: competitor}
}

// ── the deterministic statement-boundary gate ────────────────────────────

// resolveBarrier pauses the resolver's UPDATE at PREPARE time.
//
// SQLite's authorizer callback fires while a statement is being prepared.
// For SQLITE_TRANSACTION the arg1 is "BEGIN"/"COMMIT"/"ROLLBACK"; for
// SQLITE_INSERT / SQLITE_UPDATE / SQLITE_DELETE the arg1 is the TABLE
// name and arg2 is the column name.
//
// Two modes:
//
//   - oncePerConnection (the default): each pooled connection pauses at
//     most ONE update. A transaction RESTART runs on the same connection,
//     so its update is not gated — which is what lets a recovery test
//     observe the restart completing.
//   - perAttempt: the gate is re-armed by every BEGIN, so the gate pauses
//     ONCE PER TRANSACTION ATTEMPT. That is what makes the retry-budget
//     test possible: the restarted transaction is gated again, exactly
//     like the first one.
type resolveBarrier struct {
	arrived chan struct{}
	release chan struct{}

	perAttempt bool
}

func newResolveBarrier() *resolveBarrier {
	return &resolveBarrier{
		arrived: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func newPerAttemptBarrier() *resolveBarrier {
	b := newResolveBarrier()
	b.perAttempt = true
	return b
}

// authorizerFor returns the authorizer to install on ONE pooled
// connection.
func (b *resolveBarrier) authorizerFor() func(int, string, string, string) int {
	var (
		mu    sync.Mutex
		armed = !b.perAttempt
	)
	return func(action int, arg1, arg2, dbName string) int {
		switch {
		case action == sqlite3.SQLITE_TRANSACTION && arg1 == "BEGIN":
			// A new transaction attempt: re-arm the gate for its UPDATE.
			if b.perAttempt {
				mu.Lock()
				armed = true
				mu.Unlock()
			}
		case action == sqlite3.SQLITE_UPDATE && arg1 == "scheduled_wakes":
			mu.Lock()
			shouldPause := armed
			if shouldPause {
				armed = false
			}
			mu.Unlock()
			if shouldPause {
				b.arrived <- struct{}{}
				<-b.release
			}
		}
		return sqlite3.SQLITE_OK
	}
}

// releaseOne lets exactly one paused attempt proceed.
func (b *resolveBarrier) releaseOne() { b.release <- struct{}{} }

// installResolveBarrier registers the barrier's authorizer on EVERY
// physical connection the pool can hand out.
//
// The pool's connection count is fixed and its idle connections are
// retained, so pinning `conns` handles at once guarantees that all `conns`
// distinct physical connections the pool owns are registered — the
// resolver cannot draw an unregistered connection.
func installResolveBarrier(t *testing.T, pool *sql.DB, b *resolveBarrier, conns int) {
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

// ── fixtures ─────────────────────────────────────────────────────────────

// seedResolveContentionWake inserts an agent-owned wake row and returns
// its id.
func seedResolveContentionWake(t *testing.T, dm *DatabaseManager, kind string) string {
	t.Helper()
	id := fmt.Sprintf("wk-contention-%s-%d", kind, time.Now().UnixNano())
	if _, err := dm.ExecTracked(
		`INSERT INTO scheduled_wakes (id, target_time, reason, fired, created_by, metadata)
		 VALUES (?, ?, ?, 0, ?, ?)`,
		5, id, time.Now().Unix()+3600, "contention fixture", "contention-fixture",
		fmt.Sprintf(`{"kind":%q}`, kind),
	); err != nil {
		t.Fatalf("seed wake %s: %v", id, err)
	}
	return id
}

// seedResolveContentionWakeWithMetadata inserts a wake row with RAW
// metadata, so the legacy / malformed shapes can be exercised.
func seedResolveContentionWakeWithMetadata(t *testing.T, dm *DatabaseManager, metadata string) string {
	t.Helper()
	id := fmt.Sprintf("wk-contention-legacy-%d", time.Now().UnixNano())
	var metaVal interface{}
	if metadata != "<NULL>" {
		metaVal = metadata
	}
	if _, err := dm.ExecTracked(
		`INSERT INTO scheduled_wakes (id, target_time, reason, fired, created_by, metadata)
		 VALUES (?, ?, ?, 0, ?, ?)`,
		5, id, time.Now().Unix()+3600, "contention fixture", "contention-fixture", metaVal,
	); err != nil {
		t.Fatalf("seed wake %s: %v", id, err)
	}
	return id
}

type resolveWakeState struct {
	Fired   int
	FiredAt sql.NullInt64
	FiredBy sql.NullString
}

func readResolveWakeState(t *testing.T, dm *DatabaseManager, id string) resolveWakeState {
	t.Helper()
	var s resolveWakeState
	err := dm.SQLDB().QueryRow(
		`SELECT fired, fired_at, fired_by FROM scheduled_wakes WHERE id = ?`, id,
	).Scan(&s.Fired, &s.FiredAt, &s.FiredBy)
	if err != nil {
		t.Fatalf("read wake %s: %v", id, err)
	}
	return s
}

func resolverAuditCount(t *testing.T, dm *DatabaseManager, wakeID string) int {
	t.Helper()
	var n int
	err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM system_audit_log
		 WHERE component = 'wake-resolver' AND json_extract(context, '$.wake_id') = ?`,
		wakeID,
	).Scan(&n)
	if err != nil {
		t.Fatalf("count resolver audit rows for %s: %v", wakeID, err)
	}
	return n
}

// resolveOutcome is one resolver's result.
type resolveOutcome struct {
	wakeID    string
	resolved  bool
	status    string
	err       error
	startedAt time.Time
	endedAt   time.Time
}

// commitCompetingWrite advances the WAL from the competitor pool. The
// commit must happen AFTER the resolver took its snapshot and BEFORE its
// write executes — that is the whole interleaving.
func (h *resolveContentionHarness) commitCompetingWrite(t *testing.T, wakeID string) {
	t.Helper()
	if _, err := h.competitor.Exec(
		`UPDATE scheduled_wakes SET fired = 1, fired_at = ?, fired_by = 'contending-writer' WHERE id = ?`,
		time.Now().Unix(), wakeID,
	); err != nil {
		t.Fatalf("competing commit: %v", err)
	}
}

// ── CASE 1: concurrent resolution of DISTINCT wakes ──────────────────────

// TestResolveWake_ConcurrentDistinctWakesBothResolve is the headline
// regression.
//
// Two agent-owned wakes, resolved concurrently, on two independent
// connections of the SAME manager. Before the fix exactly one of them
// failed with "resolve wake: update: database is locked" — the winner's
// commit invalidated the loser's read snapshot. After the fix both
// resolve.
//
// The gate holds BOTH resolvers at their UPDATE boundary before either is
// released. That is what makes the test deterministic AND what proves the
// fix RECOVERS rather than SERIALISES: a resolver that had been
// serialised behind a global lock could not reach the boundary while the
// other was parked there, so this test would time out instead of passing.
func TestResolveWake_ConcurrentDistinctWakesBothResolve(t *testing.T) {
	h := newResolveContentionHarness(t, 2)

	wakeA := seedResolveContentionWake(t, h.dm, "notification")
	wakeB := seedResolveContentionWake(t, h.dm, "notification")

	// PRECONDITION: both wakes start pending.
	for _, id := range []string{wakeA, wakeB} {
		s := readResolveWakeState(t, h.dm, id)
		if s.Fired != 0 || s.FiredAt.Valid || s.FiredBy.Valid {
			t.Fatalf("precondition: wake %s must start pending, got %+v", id, s)
		}
	}

	barrier := newResolveBarrier()
	installResolveBarrier(t, h.pool, barrier, 2)

	results := make(chan resolveOutcome, 2)
	for _, id := range []string{wakeA, wakeB} {
		go func(id string) {
			started := time.Now()
			resolved, status, err := h.dm.ResolveWake(id, "reconciled", "")
			results <- resolveOutcome{
				wakeID:    id,
				resolved:  resolved,
				status:    status,
				err:       err,
				startedAt: started,
				endedAt:   time.Now(),
			}
		}(id)
	}

	// Both callers reach the transaction boundary. If the implementation
	// had serialised the two resolutions, the second would never arrive
	// here while the first is parked — the wait below is the
	// anti-serialisation assertion.
	for i := 0; i < 2; i++ {
		select {
		case <-barrier.arrived:
		case <-time.After(30 * time.Second):
			t.Fatalf("only %d of 2 resolvers reached the UPDATE boundary — "+
				"the resolutions were serialised instead of recovered", i)
		}
	}

	// Release both. Neither has a competing writer of its own; they
	// invalidate each other.
	barrier.releaseOne()
	barrier.releaseOne()

	outs := make([]resolveOutcome, 0, 2)
	for i := 0; i < 2; i++ {
		select {
		case o := <-results:
			outs = append(outs, o)
		case <-time.After(60 * time.Second):
			t.Fatal("a resolver never returned")
		}
	}

	for _, o := range outs {
		if o.err != nil {
			t.Fatalf("resolving %s must survive the contention, got %v", o.wakeID, o.err)
		}
		if !o.resolved || o.status != "resolved" {
			t.Fatalf("resolving %s must report resolved, got resolved=%v status=%q",
				o.wakeID, o.resolved, o.status)
		}
	}

	// The two resolutions genuinely OVERLAPPED: the second resolver
	// started before the first one finished. A serialised implementation
	// could not overlap them.
	if !(outs[0].startedAt.Before(outs[1].endedAt) && outs[1].startedAt.Before(outs[0].endedAt)) {
		t.Fatalf("the two resolutions did not overlap: %+v / %+v", outs[0], outs[1])
	}

	// Each wake is retired exactly once, with the resolver's attribution,
	// and carries exactly ONE audit row.
	for _, o := range outs {
		s := readResolveWakeState(t, h.dm, o.wakeID)
		if s.Fired != 1 || !s.FiredAt.Valid || s.FiredBy.String != "wake-resolver" {
			t.Fatalf("wake %s must be retired by the resolver, got %+v", o.wakeID, s)
		}
		if n := resolverAuditCount(t, h.dm, o.wakeID); n != 1 {
			t.Fatalf("wake %s must have exactly 1 audit row, got %d", o.wakeID, n)
		}
	}
}

// ── CASE 2: concurrent resolution of the SAME wake ───────────────────────

// TestResolveWake_ConcurrentSameWakeResolvesExactlyOnce covers the
// same-wake race.
//
// Two resolvers on the SAME wake must not produce a double resolution:
// exactly one reports "resolved" and writes the audit row, the other
// reports "already_resolved" and writes NONE. The `fired = 0` guard in the
// UPDATE is what makes that true, and the transaction restart is what
// makes it true under contention: the loser's restarted transaction reads
// fired=1 and takes the idempotent path.
func TestResolveWake_ConcurrentSameWakeResolvesExactlyOnce(t *testing.T) {
	h := newResolveContentionHarness(t, 2)

	wakeID := seedResolveContentionWake(t, h.dm, "notification")

	barrier := newResolveBarrier()
	installResolveBarrier(t, h.pool, barrier, 2)

	results := make(chan resolveOutcome, 2)
	for i := 0; i < 2; i++ {
		go func() {
			started := time.Now()
			resolved, status, err := h.dm.ResolveWake(wakeID, "reconciled", "")
			results <- resolveOutcome{
				wakeID:    wakeID,
				resolved:  resolved,
				status:    status,
				err:       err,
				startedAt: started,
				endedAt:   time.Now(),
			}
		}()
	}

	for i := 0; i < 2; i++ {
		select {
		case <-barrier.arrived:
		case <-time.After(30 * time.Second):
			t.Fatalf("only %d of 2 resolvers reached the UPDATE boundary", i)
		}
	}
	barrier.releaseOne()
	barrier.releaseOne()

	var resolvedCount, alreadyCount int
	for i := 0; i < 2; i++ {
		select {
		case o := <-results:
			if o.err != nil {
				t.Fatalf("a same-wake resolution must not fail, got %v", o.err)
			}
			switch o.status {
			case "resolved":
				resolvedCount++
			case "already_resolved":
				alreadyCount++
			default:
				t.Fatalf("unexpected status %q", o.status)
			}
		case <-time.After(60 * time.Second):
			t.Fatal("a resolver never returned")
		}
	}

	if resolvedCount != 1 || alreadyCount != 1 {
		t.Fatalf("expected exactly one resolution and one idempotent no-op, got %d/%d",
			resolvedCount, alreadyCount)
	}

	s := readResolveWakeState(t, h.dm, wakeID)
	if s.Fired != 1 || s.FiredBy.String != "wake-resolver" {
		t.Fatalf("the wake must be retired exactly once, got %+v", s)
	}
	if n := resolverAuditCount(t, h.dm, wakeID); n != 1 {
		t.Fatalf("a double resolution must not double the audit trail; got %d rows", n)
	}
}

// ── CASE 3 + 9: contention beyond the retry budget ───────────────────────

// TestResolveWake_ContentionBeyondTheRetryBudgetSurfacesTheFailure forces
// the resolver to lose EVERY attempt.
//
// The competing writer commits after each attempt's snapshot and before
// each attempt's write, so every attempt fails with a stale snapshot.
// The transaction layer's budget (TxSnapshotMaxAttempts) is finite, so the
// resolver must surface a classified failure rather than retry forever.
//
// The test also pins the failure's classification: the error is a
// SQLITE_BUSY_SNAPSHOT (extended code 517), reported under the resolver's
// "update_failed" status, with the substrate's retry counter advanced.
func TestResolveWake_ContentionBeyondTheRetryBudgetSurfacesTheFailure(t *testing.T) {
	h := newResolveContentionHarness(t, 2)

	wakeID := seedResolveContentionWake(t, h.dm, "notification")
	other := seedResolveContentionWake(t, h.dm, "notification")

	// The per-attempt barrier gates EVERY transaction attempt, so each
	// attempt can be defeated individually.
	barrier := newPerAttemptBarrier()
	installResolveBarrier(t, h.pool, barrier, 2)

	retriesBefore := h.dm.BusyRetryCount()

	results := make(chan resolveOutcome, 1)
	go func() {
		started := time.Now()
		resolved, status, err := h.dm.ResolveWake(wakeID, "reconciled", "")
		results <- resolveOutcome{
			wakeID:    wakeID,
			resolved:  resolved,
			status:    status,
			err:       err,
			startedAt: started,
			endedAt:   time.Now(),
		}
	}()

	// Invalidate the snapshot of EVERY attempt the resolver makes. The
	// loop runs exactly TxSnapshotMaxAttempts times: if the resolver made
	// one more attempt it would park at the barrier and never return, and
	// the wait below would time out.
	attempts := 0
	for i := 0; i < TxSnapshotMaxAttempts; i++ {
		select {
		case <-barrier.arrived:
		case <-time.After(30 * time.Second):
			t.Fatalf("attempt %d never reached the UPDATE boundary", i+1)
		}
		attempts++
		h.commitCompetingWrite(t, other)
		barrier.releaseOne()
	}
	if attempts != TxSnapshotMaxAttempts {
		t.Fatalf("expected %d attempts, got %d", TxSnapshotMaxAttempts, attempts)
	}

	var out resolveOutcome
	select {
	case out = <-results:
	case <-time.After(60 * time.Second):
		t.Fatal("the resolver never returned — the retry budget is unbounded")
	}

	if out.err == nil {
		t.Fatal("the resolution must fail once the retry budget is exhausted")
	}
	if out.resolved {
		t.Fatal("a failed resolution must not report resolved")
	}
	if out.status != "update_failed" {
		t.Fatalf("expected status update_failed, got %q", out.status)
	}

	// The failure is classified by the SQLite EXTENDED code, not by the
	// message: SQLITE_BUSY_SNAPSHOT (517), not ordinary contention (5).
	var se sqlite3.Error
	if !errors.As(out.err, &se) {
		t.Fatalf("expected a sqlite3.Error, got %T: %v", out.err, out.err)
	}
	if se.ExtendedCode != sqlite3.ErrBusySnapshot {
		t.Fatalf("expected SQLITE_BUSY_SNAPSHOT (extended %d), got %s",
			sqlite3.ErrBusySnapshot, describeSQLErr(out.err))
	}
	if !IsBusySnapshotError(out.err) {
		t.Fatalf("IsBusySnapshotError must be true: %s", describeSQLErr(out.err))
	}

	// The retry counter proves the retry path was actually taken (rather
	// than the resolver having been serialised, or having failed for some
	// unrelated reason).
	if h.dm.BusyRetryCount() <= retriesBefore {
		t.Fatalf("the substrate's busy-retry counter must advance, %d -> %d",
			retriesBefore, h.dm.BusyRetryCount())
	}
}

// ── CASE 4: cancellation during contention ───────────────────────────────

// TestResolveWake_CancellationDuringContentionStopsTheReplay covers the
// cancellation contract.
//
// The resolver has NO context parameter — the public surface
// (handleResolveWake) never had one — so "cancellation" for the resolver
// means the substrate's abandonment contract: work that a caller has
// given up on must not be replayed behind its back.
//
// The fix introduced a status-carrying error wrapper around the
// resolver's callback error, and that wrapper is the risk: if it did not
// UNWRAP, WithTx would neither recognise the snapshot conflict (so the
// resolver would never recover) nor honour a cancellation (so an
// abandoned attempt would be replayed). The wrapper's own contract is
// pinned in wake_resolve_status_error_test.go; this test pins the
// transaction-layer behaviour the resolver now depends on.
func TestResolveWake_CancellationDuringContentionStopsTheReplay(t *testing.T) {
	t.Run("a cancelled attempt is not replayed under contention", func(t *testing.T) {
		h := newResolveContentionHarness(t, 2)
		other := seedResolveContentionWake(t, h.dm, "notification")

		barrier := newResolveBarrier()
		installResolveBarrier(t, h.pool, barrier, 2)

		type txOutcome struct {
			err      error
			attempts int
		}
		results := make(chan txOutcome, 1)
		go func() {
			attempts := 0
			err := h.dm.WithTx(func(node DBNode) error {
				attempts++
				var n int
				if scanErr := node.QueryRowTracked(
					`SELECT COUNT(*) FROM scheduled_wakes`).Scan(&n); scanErr != nil {
					return scanErr
				}
				// The write is paused at the statement boundary; the
				// competing commit invalidates this attempt's snapshot
				// while it waits.
				res, execErr := node.ExecTracked(
					`UPDATE scheduled_wakes SET fired = 1, fired_at = ?, fired_by = 'probe' WHERE id = ?`,
					5, time.Now().Unix(), other)
				if execErr == nil {
					_ = res
					return nil
				}
				// A real snapshot conflict AND a cancellation: the
				// cancellation must win, exactly as the resolver's own
				// error shape would.
				return fmt.Errorf("%w: %w", context.Canceled, execErr)
			})
			results <- txOutcome{err: err, attempts: attempts}
		}()

		select {
		case <-barrier.arrived:
		case <-time.After(30 * time.Second):
			t.Fatal("the attempt never reached the UPDATE boundary")
		}
		// The competing commit invalidates the attempt's snapshot while it
		// is parked at the boundary.
		h.commitCompetingWrite(t, other)
		barrier.releaseOne()

		var out txOutcome
		select {
		case out = <-results:
		case <-time.After(60 * time.Second):
			t.Fatal("the transaction never returned")
		}

		if out.err == nil {
			t.Fatal("expected the cancelled attempt's error to surface")
		}
		if !errors.Is(out.err, context.Canceled) {
			t.Fatalf("expected context.Canceled to be preserved, got %v", out.err)
		}
		if out.attempts != 1 {
			t.Fatalf("a cancelled attempt must not be replayed; attempts = %d", out.attempts)
		}
	})
}

// ── CASE 5: no partial audit or settlement ───────────────────────────────

// TestResolveWake_FailedResolutionLeavesNoPartialAuditOrSettlement asserts
// that a resolution which loses the contention leaves NOTHING behind.
//
// The write and the audit row are two separate effects: the write is
// inside the transaction, the audit row is outside it and only written
// after a successful commit. A failed resolution must produce neither a
// half-retired wake nor an audit row for a retirement that did not
// happen.
func TestResolveWake_FailedResolutionLeavesNoPartialAuditOrSettlement(t *testing.T) {
	h := newResolveContentionHarness(t, 2)

	wakeID := seedResolveContentionWake(t, h.dm, "notification")
	other := seedResolveContentionWake(t, h.dm, "notification")

	// The per-attempt barrier gates EVERY transaction attempt, so each
	// attempt can be defeated individually.
	barrier := newPerAttemptBarrier()
	installResolveBarrier(t, h.pool, barrier, 2)

	results := make(chan resolveOutcome, 1)
	go func() {
		resolved, status, err := h.dm.ResolveWake(wakeID, "reconciled", "")
		results <- resolveOutcome{wakeID: wakeID, resolved: resolved, status: status, err: err}
	}()

	for i := 0; i < TxSnapshotMaxAttempts; i++ {
		select {
		case <-barrier.arrived:
		case <-time.After(30 * time.Second):
			t.Fatalf("attempt %d never reached the UPDATE boundary", i+1)
		}
		h.commitCompetingWrite(t, other)
		barrier.releaseOne()
	}

	select {
	case out := <-results:
		if out.err == nil {
			t.Fatal("the resolution must fail once the retry budget is exhausted")
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the resolver never returned")
	}

	s := readResolveWakeState(t, h.dm, wakeID)
	if s.Fired != 0 || s.FiredAt.Valid || s.FiredBy.Valid {
		t.Fatalf("a failed resolution must not retire the wake, got %+v", s)
	}
	if n := resolverAuditCount(t, h.dm, wakeID); n != 0 {
		t.Fatalf("a failed resolution must not write an audit row, got %d", n)
	}
}

// ── CASE 6: the uncontended path ─────────────────────────────────────────

// TestResolveWake_UncontendedResolutionSucceeds is the NEGATIVE CONTROL.
//
// Without contention the resolver must behave exactly as it always did:
// one resolution, one audit row, the resolver's attribution. It proves
// the contention machinery is not what makes the happy path work.
func TestResolveWake_UncontendedResolutionSucceeds(t *testing.T) {
	h := newResolveContentionHarness(t, 2)

	wakeID := seedResolveContentionWake(t, h.dm, "notification")

	resolved, status, err := h.dm.ResolveWake(wakeID, "reconciled", "ref-1")
	if err != nil {
		t.Fatalf("an uncontended resolution must succeed, got %v", err)
	}
	if !resolved || status != "resolved" {
		t.Fatalf("expected resolved/\"resolved\", got %v/%q", resolved, status)
	}

	s := readResolveWakeState(t, h.dm, wakeID)
	if s.Fired != 1 || !s.FiredAt.Valid || s.FiredBy.String != "wake-resolver" {
		t.Fatalf("the wake must be retired by the resolver, got %+v", s)
	}
	if n := resolverAuditCount(t, h.dm, wakeID); n != 1 {
		t.Fatalf("exactly one audit row, got %d", n)
	}

	// The idempotent re-resolve: no second audit row.
	resolved, status, err = h.dm.ResolveWake(wakeID, "reconciled", "ref-1")
	if err != nil {
		t.Fatalf("a re-resolve must not fail, got %v", err)
	}
	if !resolved || status != "already_resolved" {
		t.Fatalf("expected resolved/\"already_resolved\", got %v/%q", resolved, status)
	}
	if n := resolverAuditCount(t, h.dm, wakeID); n != 1 {
		t.Fatalf("a re-resolve must not write a second audit row, got %d", n)
	}
}

// ── CASE 7: scheduler-owned wake refusal ─────────────────────────────────

// TestResolveWake_SchedulerOwnedWakeIsRefused pins the ownership gate.
//
// The contention fix must not weaken the wake-ownership policy: a
// scheduler-owned wake is still refused, still with the "not_a_wake"
// status, still without any state change — and the refusal must NOT be
// retried (it is not a contention event).
func TestResolveWake_SchedulerOwnedWakeIsRefused(t *testing.T) {
	h := newResolveContentionHarness(t, 2)

	for _, kind := range []string{"cascade_summary", "drill", "gc", "snapshot", "broadcast", "critic_audit"} {
		wakeID := seedResolveContentionWake(t, h.dm, kind)

		resolved, status, err := h.dm.ResolveWake(wakeID, "reconciled", "")
		if err == nil {
			t.Fatalf("the scheduler-owned kind %q must be refused", kind)
		}
		if resolved {
			t.Fatalf("the scheduler-owned kind %q must not report resolved", kind)
		}
		if status != "not_a_wake" {
			t.Fatalf("expected status not_a_wake for %q, got %q", kind, status)
		}

		s := readResolveWakeState(t, h.dm, wakeID)
		if s.Fired != 0 || s.FiredAt.Valid || s.FiredBy.Valid {
			t.Fatalf("a refused wake must be untouched, got %+v", s)
		}
		if n := resolverAuditCount(t, h.dm, wakeID); n != 0 {
			t.Fatalf("a refused wake must not write an audit row, got %d", n)
		}
	}
}

// ── CASE 8: malformed and legacy metadata ────────────────────────────────

// TestResolveWake_MalformedAndLegacyMetadata pins the metadata probe
// through the resolver.
//
// The resolver's kind probe must keep collapsing every legacy shape to
// "no kind" — an untagged wake is a notification wake, which the agent
// owns — while a non-string kind is still a kind the agent does not own.
//
// (The same shapes are pinned against the probe itself in
// wake_legacy_metadata_safety_test.go; this test pins them through the
// RESOLVER, which is where the contention fix touched the code.)
func TestResolveWake_MalformedAndLegacyMetadata(t *testing.T) {
	h := newResolveContentionHarness(t, 2)

	untagged := []struct {
		name     string
		metadata string
	}{
		{"null metadata", "<NULL>"},
		{"empty metadata", ""},
		{"empty object", "{}"},
		{"null kind", `{"kind":null}`},
		{"empty kind", `{"kind":""}`},
		{"malformed json", "not json at all"},
		{"truncated json", `{"kind":`},
		{"notification kind", `{"kind":"notification"}`},
		{"cascade kind", `{"kind":"cascade"}`},
		{"cron kind", `{"kind":"cron"}`},
	}
	for _, tc := range untagged {
		t.Run("untagged/"+tc.name, func(t *testing.T) {
			wakeID := seedResolveContentionWakeWithMetadata(t, h.dm, tc.metadata)
			resolved, status, err := h.dm.ResolveWake(wakeID, "reconciled", "")
			if err != nil {
				t.Fatalf("%s must resolve, got %v", tc.name, err)
			}
			if !resolved || status != "resolved" {
				t.Fatalf("%s must report resolved, got %v/%q", tc.name, resolved, status)
			}
			if n := resolverAuditCount(t, h.dm, wakeID); n != 1 {
				t.Fatalf("%s must write exactly one audit row, got %d", tc.name, n)
			}
		})
	}

	refused := []struct {
		name     string
		metadata string
	}{
		{"numeric kind", `{"kind":123}`},
		{"boolean kind", `{"kind":true}`},
		{"array kind", `{"kind":["notification"]}`},
		{"object kind", `{"kind":{"name":"notification"}}`},
		{"unknown kind", `{"kind":"reminder"}`},
		{"scheduler kind", `{"kind":"cascade_summary"}`},
	}
	for _, tc := range refused {
		t.Run("refused/"+tc.name, func(t *testing.T) {
			wakeID := seedResolveContentionWakeWithMetadata(t, h.dm, tc.metadata)
			resolved, status, err := h.dm.ResolveWake(wakeID, "reconciled", "")
			if err == nil {
				t.Fatalf("%s must be refused", tc.name)
			}
			if resolved {
				t.Fatalf("%s must not report resolved", tc.name)
			}
			if status != "not_a_wake" {
				t.Fatalf("expected status not_a_wake for %s, got %q", tc.name, status)
			}
			s := readResolveWakeState(t, h.dm, wakeID)
			if s.Fired != 0 {
				t.Fatalf("%s must leave the wake pending, got %+v", tc.name, s)
			}
			if n := resolverAuditCount(t, h.dm, wakeID); n != 0 {
				t.Fatalf("%s must not write an audit row, got %d", tc.name, n)
			}
		})
	}
}

// ── CASE 9 (continued): the classification of the contention itself ──────

// TestResolveWake_ContentionIsClassifiedAsSQLiteBusySnapshot pins the
// ERROR CLASSIFICATION of the contention event, at the SQL level.
//
// The reported production symptom is a message — "database is locked" —
// and that message is shared with ordinary lock contention. The
// distinction matters because the two have different remedies: ordinary
// contention is waitable and the statement-level retry is correct for it;
// a stale snapshot is not waitable and only a transaction restart clears
// it.
func TestResolveWake_ContentionIsClassifiedAsSQLiteBusySnapshot(t *testing.T) {
	h := newResolveContentionHarness(t, 2)

	wakeID := seedResolveContentionWake(t, h.dm, "notification")
	other := seedResolveContentionWake(t, h.dm, "notification")

	// The raw shape the resolver's write takes: read, then write.
	tx, err := h.pool.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()

	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM scheduled_wakes`).Scan(&n); err != nil {
		t.Fatalf("snapshot read: %v", err)
	}

	h.commitCompetingWrite(t, other)

	start := time.Now()
	_, execErr := tx.Exec(
		`UPDATE scheduled_wakes SET fired = 1, fired_at = ?, fired_by = 'probe' WHERE id = ?`,
		time.Now().Unix(), wakeID,
	)
	elapsed := time.Since(start)

	if execErr == nil {
		t.Fatal("expected the write to fail under a stale snapshot")
	}
	var se sqlite3.Error
	if !errors.As(execErr, &se) {
		t.Fatalf("expected a sqlite3.Error, got %T: %v", execErr, execErr)
	}
	if se.ExtendedCode != sqlite3.ErrBusySnapshot {
		t.Fatalf("expected SQLITE_BUSY_SNAPSHOT (extended %d), got %s",
			sqlite3.ErrBusySnapshot, describeSQLErr(execErr))
	}
	if se.Code != sqlite3.ErrBusy {
		t.Fatalf("expected the primary code to stay SQLITE_BUSY, got %d", se.Code)
	}
	if !IsBusySnapshotError(execErr) {
		t.Fatalf("IsBusySnapshotError must be true: %s", describeSQLErr(execErr))
	}
	if !IsBusyError(execErr) {
		t.Fatalf("IsBusyError must remain true for a snapshot conflict: %s", describeSQLErr(execErr))
	}
	// A snapshot conflict never consults the busy handler, so it cannot
	// wait out busy_timeout. If this were ordinary contention the error
	// would take ~5 s to arrive.
	if elapsed >= 2*time.Second {
		t.Fatalf("the failure waited %v — that is a LOCK WAIT, not a snapshot conflict", elapsed)
	}
}
