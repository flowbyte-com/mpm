// The PUBLIC-HANDLER half of the issue #178 ResolveWake contention
// regression suite.
//
// The core-module suite (internal/core/wake_resolve_contention_test.go)
// pins the resolver's behaviour. This file pins the PUBLIC surface: the
// `mpm_wakes` tool registry handler with action "resolve".
//
// Why the public surface needs its own suite: the fix moved the
// resolver's transactional body into dm.WithTx, whose callback can only
// return an error — so the resolver's machine-readable status had to
// travel with that error and be unpacked afterwards. The envelope the
// agent sees ({success, resolved, status, wake_id, reason,
// result_reference}) is therefore at risk in a way the core-module suite
// cannot see, because it never goes through the handler.
//
// The harness cannot be shared with the core module: a _test.go file is
// not importable from a sibling package, so this file carries its own
// copy of the deterministic statement-boundary gate (the same shape as
// internal/core/tools/scratchpad_write_budget_test.go's WAL harness).
package tools

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"
)

// ── harness ───────────────────────────────────────────────────────────────

// resolveContentionWALDB opens a file-backed WAL database with the
// production DSN and returns a manager over it, plus a second pool on the
// same file that can act as the competing writer.
//
// WAL is not optional: SQLITE_BUSY_SNAPSHOT only exists in WAL mode.
func resolveContentionWALDB(t *testing.T, poolConns int) (*mpminternal.DatabaseManager, *sql.DB, *sql.DB) {
	t.Helper()
	mpminternal.PinIsolatedWorkspace(t)

	dir := t.TempDir()
	path := filepath.Join(dir, "mpm.db")

	pool, err := sql.Open("sqlite3", mpminternal.SqliteWriteDSN(path))
	require.NoError(t, err, "sql.Open")
	t.Cleanup(func() { _ = pool.Close() })
	pool.SetMaxOpenConns(poolConns)
	pool.SetMaxIdleConns(poolConns)

	_, err = pool.Exec("PRAGMA journal_mode = WAL")
	require.NoError(t, err, "PRAGMA journal_mode = WAL")

	dm := mpminternal.NewDatabaseManagerForDB(pool)
	require.NoError(t, dm.InitSchema(), "InitSchema")

	competitor, err := sql.Open("sqlite3", mpminternal.SqliteWriteDSN(path))
	require.NoError(t, err, "competitor sql.Open")
	t.Cleanup(func() { _ = competitor.Close() })

	return dm, pool, competitor
}

// resolveContentionBarrier pauses the resolver's UPDATE at PREPARE time.
//
// SQLite's authorizer callback fires while a statement is being prepared.
// For SQLITE_TRANSACTION the arg1 is "BEGIN"/"COMMIT"/"ROLLBACK"; for
// SQLITE_INSERT / SQLITE_UPDATE / SQLITE_DELETE the arg1 is the TABLE
// name and arg2 is the column name.
//
// Each pooled connection pauses at most ONCE, so a transaction RESTART
// (which runs on the same connection) is not gated — that is what lets a
// recovery test observe the restart completing.
type resolveContentionBarrier struct {
	arrived chan struct{}
	release chan struct{}
}

func newResolveContentionBarrier() *resolveContentionBarrier {
	return &resolveContentionBarrier{
		arrived: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (b *resolveContentionBarrier) authorizerFor() func(int, string, string, string) int {
	var (
		mu    sync.Mutex
		armed = true
	)
	return func(action int, arg1, arg2, dbName string) int {
		if action == sqlite3.SQLITE_UPDATE && arg1 == "scheduled_wakes" {
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

func (b *resolveContentionBarrier) releaseOne() { b.release <- struct{}{} }

// installResolveContentionBarrier registers the barrier's authorizer on
// EVERY physical connection the pool can hand out.
func installResolveContentionBarrier(t *testing.T, pool *sql.DB, b *resolveContentionBarrier, conns int) {
	t.Helper()

	handles := make([]*sql.Conn, 0, conns)
	for i := 0; i < conns; i++ {
		h, err := pool.Conn(context.Background())
		require.NoError(t, err, "pin connection %d", i)
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
		require.NoError(t, err, "register authorizer on connection %d", i)
	}
	for _, h := range handles {
		require.NoError(t, h.Close(), "release pinned connection")
	}
}

// ── fixtures ─────────────────────────────────────────────────────────────

func seedResolveContentionWake(t *testing.T, dm *mpminternal.DatabaseManager, id, kind string) {
	t.Helper()
	_, err := dm.SQLDB().Exec(`
		INSERT INTO scheduled_wakes
			(id, target_time, reason, theory_id, recurring_rule, fired, fired_at, created_by, metadata)
		VALUES (?, ?, ?, NULL, NULL, 0, NULL, 'test-producer', ?)
	`, id, time.Now().Add(2*time.Hour).Unix(), "resolve-contention fixture "+id,
		fmt.Sprintf(`{"kind":%q}`, kind))
	require.NoError(t, err, "seed wake %s", id)
}

// resolveViaPublicTool drives the REAL public surface: the mpm_wakes
// tool registry handler, action "resolve".
func resolveViaPublicTool(t *testing.T, dm *mpminternal.DatabaseManager, wakeID, reason string) map[string]interface{} {
	t.Helper()
	res, err := handleMpmWakes(dm, defaultACForPatch(), map[string]interface{}{
		"action": "resolve",
		"params": map[string]interface{}{
			"wake_id": wakeID,
			"reason":  reason,
		},
	})
	require.NoError(t, err, "a machine-readable refusal is not a Go error")
	m, ok := res.(map[string]interface{})
	require.True(t, ok, "the resolve envelope must be a map; got %T", res)
	return m
}

// resolveContentionAuditCount counts the resolver's audit rows for a wake.
func resolveContentionAuditCount(t *testing.T, dm *mpminternal.DatabaseManager, wakeID string) int {
	t.Helper()
	var n int
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM system_audit_log
		 WHERE component = 'wake-resolver' AND json_extract(context, '$.wake_id') = ?`,
		wakeID).Scan(&n), "count resolver audit rows")
	return n
}

// ── the public-surface regression ────────────────────────────────────────

// TestHandleResolveWake_ConcurrentDistinctWakesBothSucceed drives the
// contention through the PUBLIC handler.
//
// Before the fix, one of the two resolutions came back as
// {success: false, status: "update_failed", error: "resolve wake: update:
// database is locked"} — the agent was told the wake was not resolved,
// and it was not. After the fix both envelopes report success.
func TestHandleResolveWake_ConcurrentDistinctWakesBothSucceed(t *testing.T) {
	dm, pool, _ := resolveContentionWALDB(t, 2)

	wakeA := "wk-contention-a"
	wakeB := "wk-contention-b"
	other := "wk-contention-other"
	seedResolveContentionWake(t, dm, wakeA, "notification")
	seedResolveContentionWake(t, dm, wakeB, "notification")
	seedResolveContentionWake(t, dm, other, "notification")

	barrier := newResolveContentionBarrier()
	installResolveContentionBarrier(t, pool, barrier, 2)

	type envelope struct {
		wakeID string
		m      map[string]interface{}
	}
	results := make(chan envelope, 2)
	for _, id := range []string{wakeA, wakeB} {
		go func(id string) {
			results <- envelope{wakeID: id, m: resolveViaPublicTool(t, dm, id, "reconciled")}
		}(id)
	}

	// Both callers reach the transaction boundary. A resolver that had
	// been serialised behind a global lock could not reach the boundary
	// while the other was parked there — so this wait is also the
	// anti-serialisation assertion.
	for i := 0; i < 2; i++ {
		select {
		case <-barrier.arrived:
		case <-time.After(30 * time.Second):
			t.Fatalf("only %d of 2 resolvers reached the UPDATE boundary — "+
				"the resolutions were serialised instead of recovered", i)
		}
	}
	barrier.releaseOne()
	barrier.releaseOne()

	seen := map[string]map[string]interface{}{}
	for i := 0; i < 2; i++ {
		select {
		case e := <-results:
			seen[e.wakeID] = e.m
		case <-time.After(60 * time.Second):
			t.Fatal("a resolver never returned")
		}
	}

	for _, id := range []string{wakeA, wakeB} {
		m, ok := seen[id]
		require.True(t, ok, "no envelope for %s", id)

		// The envelope shape is unchanged.
		require.Equal(t, true, m["success"], "envelope for %s: %v", id, m)
		require.Equal(t, "resolved", m["status"], "envelope for %s: %v", id, m)
		require.Equal(t, true, m["resolved"], "envelope for %s: %v", id, m)
		require.Equal(t, id, m["wake_id"], "envelope for %s: %v", id, m)
		require.Equal(t, "reconciled", m["reason"], "envelope for %s: %v", id, m)
		require.NotContains(t, m, "error", "envelope for %s: %v", id, m)

		// Exactly one audit row per successful resolution.
		require.Equal(t, 1, resolveContentionAuditCount(t, dm, id),
			"wake %s must have exactly one audit row", id)
	}
}

// TestHandleResolveWake_ConcurrentSameWakeResolvesExactlyOnce drives the
// same-wake race through the PUBLIC handler.
//
// The envelope must report one "resolved" and one "already_resolved" —
// and the audit trail must carry exactly one row, because a double
// resolution would double the audit trail.
func TestHandleResolveWake_ConcurrentSameWakeResolvesExactlyOnce(t *testing.T) {
	dm, pool, _ := resolveContentionWALDB(t, 2)

	wakeID := "wk-contention-same"
	seedResolveContentionWake(t, dm, wakeID, "notification")

	barrier := newResolveContentionBarrier()
	installResolveContentionBarrier(t, pool, barrier, 2)

	results := make(chan map[string]interface{}, 2)
	for i := 0; i < 2; i++ {
		go func() {
			results <- resolveViaPublicTool(t, dm, wakeID, "reconciled")
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
		case m := <-results:
			require.Equal(t, true, m["success"], "envelope: %v", m)
			switch m["status"] {
			case "resolved":
				resolvedCount++
			case "already_resolved":
				alreadyCount++
			default:
				t.Fatalf("unexpected status %v in %v", m["status"], m)
			}
		case <-time.After(60 * time.Second):
			t.Fatal("a resolver never returned")
		}
	}

	require.Equal(t, 1, resolvedCount, "exactly one resolution must be reported")
	require.Equal(t, 1, alreadyCount, "exactly one idempotent no-op must be reported")
	require.Equal(t, 1, resolveContentionAuditCount(t, dm, wakeID),
		"a double resolution must not double the audit trail")
}

// TestHandleResolveWake_SchedulerOwnedRefusalIsUnchangedUnderContention
// pins that the ownership gate still refuses under contention, and that
// the refusal is not retried (it is not a contention event).
func TestHandleResolveWake_SchedulerOwnedRefusalIsUnchangedUnderContention(t *testing.T) {
	dm, _, _ := resolveContentionWALDB(t, 2)

	wakeID := "wk-contention-scheduler-owned"
	seedResolveContentionWake(t, dm, wakeID, "cascade_summary")

	m := resolveViaPublicTool(t, dm, wakeID, "reconciled")

	require.Equal(t, false, m["success"], "envelope: %v", m)
	require.Equal(t, "not_a_wake", m["status"], "envelope: %v", m)
	require.Equal(t, wakeID, m["wake_id"], "envelope: %v", m)
	require.Contains(t, m["error"], "scheduler-owned", "envelope: %v", m)

	var fired int
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT fired FROM scheduled_wakes WHERE id = ?`, wakeID).Scan(&fired))
	require.Equal(t, 0, fired, "a refused wake must stay pending")
	require.Equal(t, 0, resolveContentionAuditCount(t, dm, wakeID),
		"a refused wake must not write an audit row")
}
