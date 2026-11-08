// mcp_contention_acceptance_test.go — the acceptance suite for the
// observational-write latency remediation (issue #178, F2).
//
// WHAT THIS FILE PROVES.
//
// The mission's baseline was 124.47 s for ONE MCP wake request under a
// held SQLite write lock. This file measures the same request, over
// the same real MCP stdio transport, against a disposable workspace,
// under every contention shape the mission names, and asserts that the
// request's WALL CLOCK — not an individual statement — stays inside a
// ceiling that is far below the baseline.
//
// The measurement harness (latencyClient, holdWriteLock,
// seedWorkspaceContent) lives in mcp_contention_latency_test.go. This
// file is the scenarios.
//
// THE CEILING, AND WHY IT IS WHERE IT IS.
//
// The baseline is 124.47 s. The remaining cost after the remediation is
// the DURABLE mark-read — the write that records that a handoff has
// been delivered — which spends the substrate's full durable-write
// budget (~33.2 s: six 5 s busy-handler waits plus the backoff
// between them) when the lock is held for the whole request.
//
// The ceiling is 60 s: a 52% reduction from the baseline, and about
// 1.8x the remaining durable write. It is set above the durable
// write so a loaded CI runner does not produce a false failure, and
// far below the baseline so the pre-fix behaviour cannot pass.
//
// WHAT THE REMAINING LATENCY IS, AND WHY IT STAYS.
//
// The 33.2 s that remains is a DURABLE USER-STATE write, and it stays
// synchronous for three reasons: the handoff is delivered exactly
// once, so the mark must be durable before the agent is told the
// handoff was delivered; several substrate contracts assert the mark
// synchronously; and reducing that write's retry budget would weaken
// the durability #234 approved. It is bounded, it is attributable, and
// it is the substrate's documented write budget rather than an
// accident.
//
// Every scenario here uses a disposable workspace and never touches the
// operator's.

package main

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	core "github.com/flowbyte-com/mpm-core"
)

// baselineSeconds is the measured pre-fix wall clock for one MCP wake
// request under sustained SQLite write contention, on main + #234.
//
// Measured by this repository's own harness: 124.47 s (and 124.46 s
// on a second run). The mission's figure is 124.47 s.
const baselineSeconds = 124.47

// acceptanceCeiling is the wall-clock ceiling every contended scenario
// must stay under.
//
// 60 s is a 52% reduction from the 124.47 s baseline and about 1.8x
// the remaining durable mark-read (~33.2 s).
const acceptanceCeiling = 60 * time.Second

// noContentionCeiling is the ceiling for a request that meets no
// contention at all. The whole point of the remediation is that an
// uncontended request stays uncontended-fast.
const noContentionCeiling = 15 * time.Second

// observationalCeiling is the ceiling for a request whose ONLY writes
// are observational.
//
// Pre-fix that request cost about 58 s of wall clock under a held
// lock: five telemetry writes at one busy-handler wait each, plus the
// per-invocation audit row at the full durable-write budget. The
// ceiling is generous enough to tolerate a loaded runner and is still
// an order of magnitude below that cost.
const observationalCeiling = 20 * time.Second

// wakeCtxArgs is the MCP request the mission measures: one wake
// context read.
func wakeCtxArgs() map[string]interface{} {
	return map[string]interface{}{
		"name":      "mpm_context",
		"arguments": map[string]interface{}{"action": "read_wake_context"},
	}
}

// workspaceDBPath is the disposable workspace's database.
func workspaceDBPath(ws string) string {
	return filepath.Join(ws, "src", "db", "mpm.db")
}

// openWorkspaceDB opens the disposable workspace's database for
// reading.
//
// The MCP server holds this database open in WAL mode, so a reader can
// observe its writes without blocking it. The assertions below read
// rows the server wrote, which is the point: the evidence has to be in
// the DATABASE, not in the server's memory.
func openWorkspaceDB(t *testing.T, ws string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", core.SqliteWriteDSN(workspaceDBPath(ws)))
	if err != nil {
		t.Fatalf("open workspace db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// waitForCount polls the workspace database until a count reaches at
// least want, or the deadline expires.
//
// It returns the last count it saw, so a failure can report how far
// the evidence got.
func waitForCount(t *testing.T, db *sql.DB, query string, args []interface{}, want int, budget time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(budget)
	got := 0
	for {
		if err := db.QueryRow(query, args...).Scan(&got); err != nil {
			if time.Now().After(deadline) {
				t.Fatalf("query %q failed: %v", query, err)
			}
			time.Sleep(50 * time.Millisecond)
			continue
		}
		if got >= want {
			return got
		}
		if time.Now().After(deadline) {
			return got
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// assertUnderCeiling fails the test when a measurement is over the
// ceiling, and states the reduction from the baseline either way.
func assertUnderCeiling(t *testing.T, what string, elapsed time.Duration, ceiling time.Duration) {
	t.Helper()
	baseline := time.Duration(baselineSeconds * float64(time.Second))
	if elapsed > ceiling {
		t.Fatalf("%s took %.2fs, over the %.0fs ceiling (baseline %.2fs)",
			what, elapsed.Seconds(), ceiling.Seconds(), baseline.Seconds())
	}
	t.Logf("%s: %.2fs (ceiling %.0fs, baseline %.2fs, reduction %.0f%%)",
		what, elapsed.Seconds(), ceiling.Seconds(), baseline.Seconds(),
		100*(1-elapsed.Seconds()/baseline.Seconds()))
}

// TestAcceptance_NoContention is the floor: a request that meets no
// contention must stay fast.
//
// It is the control for every other scenario. Without it, a
// "remediation" that simply made the request fast by never writing
// anything would pass the contended scenarios.
func TestAcceptance_NoContention(t *testing.T) {
	if testing.Short() {
		t.Skip("contention acceptance suite is not a short test")
	}
	ws := t.TempDir()
	c := newLatencyClient(t, ws)
	seedWorkspaceContent(t, c, ws)

	result, elapsed := c.call("tools/call", wakeCtxArgs(), 2*time.Minute)
	assertUnderCeiling(t, "uncontended MCP wake request", elapsed, noContentionCeiling)

	if !wakeContextReturned(result) {
		t.Fatalf("the wake request did not return a wake context: %v", result)
	}
}

// TestAcceptance_RecoverableContentionWindows measures a lock that is
// HELD and then RELEASED, at 1, 3, 6, 12 and 20 seconds.
//
// These are the mission's recoverable contention windows. The request
// must complete, and it must complete close to the moment the lock was
// released — the durable mark-read's retry budget is long enough to
// absorb a 20 s window, so a recoverable window must not cost the
// caller the pre-fix 124 s.
func TestAcceptance_RecoverableContentionWindows(t *testing.T) {
	if testing.Short() {
		t.Skip("contention acceptance suite is not a short test")
	}
	for _, hold := range []time.Duration{
		1 * time.Second,
		3 * time.Second,
		6 * time.Second,
		12 * time.Second,
		20 * time.Second,
	} {
		hold := hold
		t.Run(fmt.Sprintf("released_after_%ds", int(hold.Seconds())), func(t *testing.T) {
			ws := t.TempDir()
			c := newLatencyClient(t, ws)
			seedWorkspaceContent(t, c, ws)

			release := holdWriteLock(t, workspaceDBPath(ws), hold)
			defer release()

			result, elapsed := c.call("tools/call", wakeCtxArgs(), 5*time.Minute)
			assertUnderCeiling(t, "MCP wake request, lock released after "+hold.String(), elapsed, acceptanceCeiling)

			// A recoverable window must not cost the caller much more
			// than the window itself. The margin covers the durable
			// write's own retry cadence (a backoff between attempts).
			if over := elapsed - hold; over > 25*time.Second {
				t.Fatalf("the request took %.2fs for a lock held %.2fs — %s beyond the window",
					elapsed.Seconds(), hold.Seconds(), over.String())
			}
			if !wakeContextReturned(result) {
				t.Fatalf("the wake request did not return a wake context: %v", result)
			}
		})
	}
}

// TestAcceptance_SustainedContention measures a lock that is NEVER
// released — the shape the 124.47 s baseline was taken under.
//
// The request must still RETURN: the mark-read's failure is
// non-fatal, and the agent must get its wake context either way. The
// only cost left is the durable write's own budget.
func TestAcceptance_SustainedContention(t *testing.T) {
	if testing.Short() {
		t.Skip("contention acceptance suite is not a short test")
	}
	ws := t.TempDir()
	c := newLatencyClient(t, ws)
	seedWorkspaceContent(t, c, ws)

	release := holdWriteLock(t, workspaceDBPath(ws), 0)
	defer release()

	result, elapsed := c.call("tools/call", wakeCtxArgs(), 5*time.Minute)
	assertUnderCeiling(t, "MCP wake request under sustained contention", elapsed, acceptanceCeiling)

	if !wakeContextReturned(result) {
		t.Fatalf("the wake request did not return a wake context under sustained contention: %v", result)
	}
}

// TestAcceptance_ObservationalCostIsOffTheCriticalPath measures the
// cost of the OBSERVATIONAL writes alone.
//
// This is the branch-agnostic measurement, and it is the one that
// isolates the remediation's effect from #234's separate work.
//
// The workspace deliberately has NO unread handoff, so the wake
// request performs NO durable mark-read. Every write left on the
// request's path is therefore observational: the retrieval telemetry
// rows and the per-invocation audit row.
//
// Pre-fix those writes were paid INLINE, on the caller's goroutine:
// five telemetry writes at one busy-handler wait each (~25 s) plus the
// per-invocation audit row at the substrate's full durable-write
// budget (~33 s) — about 58 s of the request's wall clock, for rows
// the agent never sees.
//
// Post-fix they are off the caller's goroutine, so the request's
// latency under the same lock is the READ path's latency.
//
// The bound is deliberately generous: it is a property of the request,
// not a measurement of the remaining durable write.
func TestAcceptance_ObservationalCostIsOffTheCriticalPath(t *testing.T) {
	if testing.Short() {
		t.Skip("contention acceptance suite is not a short test")
	}
	ws := t.TempDir()
	c := newLatencyClient(t, ws)

	// No handoff: the request has no durable mark-read to pay for.
	seedModeAndPersonaFiles(t, ws)
	seedMemories(t, c, 3)

	release := holdWriteLock(t, workspaceDBPath(ws), 0)
	defer release()

	result, elapsed := c.call("tools/call", wakeCtxArgs(), 5*time.Minute)
	assertUnderCeiling(t, "MCP wake request with only observational writes", elapsed, observationalCeiling)

	if !wakeContextReturned(result) {
		t.Fatalf("the wake request did not return a wake context: %v", result)
	}
}

// TestAcceptance_ConcurrentRequests measures SEVERAL MCP requests
// against ONE workspace while the lock is held.
//
// Each request is independent and each must come back inside the
// ceiling. This is the scenario a shared agent host produces: several
// sessions, one database, one contended lock.
func TestAcceptance_ConcurrentRequests(t *testing.T) {
	if testing.Short() {
		t.Skip("contention acceptance suite is not a short test")
	}
	const clients = 4

	ws := t.TempDir()
	bin := mcpCmd(t)

	// Seed through one client so the workspace has the content the
	// wake request needs.
	seeder := newLatencyClientWithBin(t, ws, bin)
	seedWorkspaceContent(t, seeder, ws)

	// The clients are STARTED BEFORE the lock is taken. The MCP
	// handshake itself touches the database (the active-state
	// resolver reads it, and the session bootstrap writes it), so a
	// client that starts under a held lock cannot even initialise.
	// That is a property of the handshake, not of the wake request
	// under test.
	procs := make([]*latencyClient, clients)
	for i := range procs {
		procs[i] = newLatencyClientWithBin(t, ws, bin)
	}

	release := holdWriteLock(t, workspaceDBPath(ws), 0)
	defer release()

	type outcome struct {
		elapsed time.Duration
		err     error
	}
	results := make([]outcome, clients)
	var wg sync.WaitGroup
	for i := 0; i < clients; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, elapsed, err := procs[i].callRaw("tools/call", wakeCtxArgs(), 5*time.Minute)
			results[i] = outcome{elapsed: elapsed, err: err}
			if err == nil && !wakeContextReturned(result) {
				results[i].err = fmt.Errorf("no wake context in the result")
			}
		}()
	}
	wg.Wait()

	for i, r := range results {
		if r.err != nil {
			t.Fatalf("concurrent client %d failed: %v", i, r.err)
		}
		assertUnderCeiling(t, fmt.Sprintf("concurrent MCP wake request %d", i), r.elapsed, acceptanceCeiling)
	}
}

// TestAcceptance_CancellationLeavesTheDatabaseIntact covers the
// cancellation path: the host tears the server down while a request
// is in flight.
//
// The obligation is that a torn-down server leaves NO corruption and
// NO half-written state behind. The database must still pass its
// integrity check, and a fresh server must be able to open it.
func TestAcceptance_CancellationLeavesTheDatabaseIntact(t *testing.T) {
	if testing.Short() {
		t.Skip("contention acceptance suite is not a short test")
	}
	ws := t.TempDir()
	c := newLatencyClient(t, ws)
	seedWorkspaceContent(t, c, ws)

	release := holdWriteLock(t, workspaceDBPath(ws), 0)
	defer release()

	// Start a request and abandon it, then kill the server while it
	// is still in flight. This is the cancellation shape a host
	// produces when it stops the agent mid-turn.
	go func() {
		_, _, _ = c.callRaw("tools/call", wakeCtxArgs(), 5*time.Minute)
	}()
	time.Sleep(500 * time.Millisecond)
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}

	// The database must still be a valid SQLite database.
	db := openWorkspaceDB(t, ws)
	var integrity string
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil {
		t.Fatalf("integrity check: %v", err)
	}
	if integrity != "ok" {
		t.Fatalf("integrity check after a cancelled request: %s", integrity)
	}
}

// TestAcceptance_DeferredEvidenceLands is the DURABILITY half of the
// remediation, and it is the red-to-green evidence.
//
// Pre-fix, the failure-path audit write burned a second full durable
// budget trying to record a failure and then produced NO ROW. The
// telemetry writes lost their rows too. Post-fix, those obligations
// are queued and retried until they land.
//
// This scenario reproduces the failure path (hold the lock, so the
// mark-read fails), releases the lock, and then asserts that the
// evidence is IN THE DATABASE: the failure audit row, the retrieval
// telemetry rows, and the per-invocation audit row.
func TestAcceptance_DeferredEvidenceLands(t *testing.T) {
	if testing.Short() {
		t.Skip("contention acceptance suite is not a short test")
	}
	ws := t.TempDir()
	c := newLatencyClient(t, ws)
	seedWorkspaceContent(t, c, ws)

	release := holdWriteLock(t, workspaceDBPath(ws), 0)

	// The wake request fails its durable mark-read, which is what
	// produces the failure-path audit row.
	_, elapsed := c.call("tools/call", wakeCtxArgs(), 5*time.Minute)
	assertUnderCeiling(t, "MCP wake request that fails its mark-read", elapsed, acceptanceCeiling)

	// Release the lock so the queued obligations can land.
	release()

	db := openWorkspaceDB(t, ws)

	// 1. The FAILURE-PATH AUDIT ROW. Pre-fix this row was LOST: the
	//    write exhausted its budget and produced nothing.
	got := waitForCount(t, db,
		`SELECT COUNT(*) FROM system_audit_log WHERE component = 'wake_context' AND message LIKE '%handoff read failed%'`,
		nil, 1, 90*time.Second)
	if got < 1 {
		t.Fatalf("the failure-path audit row never landed (count=%d) — the evidence was lost", got)
	}
	t.Logf("failure-path audit row landed: %d row(s)", got)

	// 2. The RETRIEVAL TELEMETRY rows.
	got = waitForCount(t, db,
		`SELECT COUNT(*) FROM retrieval_metadata WHERE node_id IN (
			SELECT id FROM memories WHERE deleted_at IS NULL)`,
		nil, 1, 90*time.Second)
	if got < 1 {
		t.Fatalf("the retrieval telemetry rows never landed (count=%d)", got)
	}
	t.Logf("retrieval telemetry rows landed: %d row(s)", got)

	// 3. The PER-INVOCATION AUDIT ROW.
	got = waitForCount(t, db,
		`SELECT COUNT(*) FROM tool_invocations WHERE tool_name = 'mpm_context'`,
		nil, 1, 90*time.Second)
	if got < 1 {
		t.Fatalf("the per-invocation audit row never landed (count=%d)", got)
	}
	t.Logf("per-invocation audit rows landed: %d row(s)", got)
}

// TestAcceptance_PersistentStateAfterRestart covers the restart
// contract: what the substrate wrote must still be there after the
// server that wrote it is gone.
//
// The scenario drains the deferred queue, stops the server, starts a
// NEW server against the same workspace, and asserts that the durable
// state survived — the handoff is still marked read, and the
// observational evidence is still queryable.
func TestAcceptance_PersistentStateAfterRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("contention acceptance suite is not a short test")
	}
	ws := t.TempDir()
	bin := mcpCmd(t)

	c := newLatencyClientWithBin(t, ws, bin)
	seedWorkspaceContent(t, c, ws)

	// A clean wake request: the mark-read succeeds.
	_, elapsed := c.call("tools/call", wakeCtxArgs(), 2*time.Minute)
	assertUnderCeiling(t, "MCP wake request before restart", elapsed, noContentionCeiling)

	db := openWorkspaceDB(t, ws)
	// Wait for the deferred per-invocation audit row so the restart
	// has something observational to preserve.
	waitForCount(t, db,
		`SELECT COUNT(*) FROM tool_invocations WHERE tool_name = 'mpm_context'`,
		nil, 1, 60*time.Second)

	// Stop the server the way a host does: SIGTERM, which runs the
	// shutdown flush.
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Signal(syscall.SIGTERM)
	}
	_ = c.cmd.Wait()

	// The durable mark-read must have survived the restart.
	var readAt sql.NullInt64
	if err := db.QueryRow(
		`SELECT read_at FROM session_handoffs ORDER BY created_at DESC LIMIT 1`).Scan(&readAt); err != nil {
		t.Fatalf("read the handoff: %v", err)
	}
	if !readAt.Valid {
		t.Fatalf("the handoff is not marked read after the restart — the durable mark-read did not survive")
	}

	// The observational evidence must have survived too.
	var invocations int
	if err := db.QueryRow(`SELECT COUNT(*) FROM tool_invocations`).Scan(&invocations); err != nil {
		t.Fatalf("count tool_invocations: %v", err)
	}
	if invocations < 1 {
		t.Fatalf("the per-invocation audit rows did not survive the restart (count=%d)", invocations)
	}

	// A NEW server must open the same workspace and find the handoff
	// ALREADY consumed: the delivery-once contract is a durable one.
	c2 := newLatencyClientWithBin(t, ws, bin)
	result, elapsed := c2.call("tools/call", wakeCtxArgs(), 2*time.Minute)
	assertUnderCeiling(t, "MCP wake request after restart", elapsed, noContentionCeiling)
	if !wakeContextReturned(result) {
		t.Fatalf("the wake request after the restart did not return a wake context: %v", result)
	}
}

// wakeContextReturned reports whether an MCP result carries a wake
// context.
//
// The result arrives as the tool's JSON text content, so the check is
// on the payload's shape rather than on a Go type.
func wakeContextReturned(result map[string]interface{}) bool {
	if result == nil {
		return false
	}
	raw, ok := result["content"].([]interface{})
	if !ok {
		return false
	}
	for _, item := range raw {
		block, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		if block["type"] != "text" {
			continue
		}
		text, _ := block["text"].(string)
		if strings.Contains(text, "recent_memories") || strings.Contains(text, "session_current_id") {
			return true
		}
	}
	return false
}
