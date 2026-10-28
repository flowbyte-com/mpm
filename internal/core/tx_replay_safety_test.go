package internal

// Safety review of the transaction-restart mechanism introduced by issue
// #158.
//
// THE QUESTION
// -------------
// Issue #158 made WithTx restart a transaction whose read snapshot went
// stale (SQLITE_BUSY_SNAPSHOT). A restart RE-RUNS THE CALLBACK. That is
// only safe if the callback is replay-safe: every side effect it produces
// must either (a) live inside the transaction, so the abandoned attempt's
// copy rolls back with it, or (b) be idempotent, so re-running it produces
// the same state rather than a second copy.
//
// The failure mode this review is guarding against is a callback that
// emits an audit row, writes a file, sends a notification, or appends to
// an accumulator OUTSIDE the transaction. On a replay those effects are
// NOT rolled back — they happen twice.
//
// THE STRUCTURAL ARGUMENT
// -----------------------
// Only ONE shape of callback can be replayed at all. SQLITE_BUSY_SNAPSHOT
// requires the transaction to hold an open READ SNAPSHOT that another
// connection has already committed past. In WAL mode a connection that
// holds the write lock blocks every other writer, so:
//
//   - a callback whose FIRST database statement is a READ can take a
//     snapshot, and a competing commit can invalidate it → replayable;
//   - a callback whose FIRST database statement is a WRITE acquires the
//     write lock immediately, and no other connection can commit until it
//     finishes → the snapshot can never go stale → structurally immune.
//
// The tests below verify both halves empirically: the replayable callbacks
// are forced through a real restart and diffed table-by-table against an
// uncontended control run, and the write-first shape is shown to be
// immune by the write lock itself.

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// tableRowCounts returns the row count of every user table in the
// database.
//
// EVERY table is counted, not a chosen subset: the point of the diff is to
// catch a duplicated side effect in a table nobody thought to check —
// `system_audit_log` is the obvious one (the audit writer uses the bare
// pool, so its rows are NOT inside the transaction), but the FTS shadow
// tables are just as good a duplicate detector, because every duplicated
// artifact row produces a duplicated index entry.
//
// Views are excluded (type='table' only); MPM's `lessons` view is a
// projection over `lessons_base`, which IS counted, so nothing is lost.
func tableRowCounts(t *testing.T, dm *DatabaseManager) map[string]int {
	t.Helper()
	rows, err := dm.db.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		t.Fatalf("enumerate tables: %v", err)
	}
	defer rows.Close()

	counts := map[string]int{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan table name: %v", err)
		}
		var n int
		// The name comes from sqlite_master, so it cannot carry a quote;
		// it is still quoted defensively because the table list is the
		// only untrusted part of this query.
		if err := dm.db.QueryRow(`SELECT COUNT(*) FROM "` + name + `"`).Scan(&n); err != nil {
			t.Fatalf("count rows of %q: %v", name, err)
		}
		counts[name] = n
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate tables: %v", err)
	}
	if len(counts) == 0 {
		t.Fatal("no tables found — the schema was not initialised")
	}
	return counts
}

// replayHarnessTables are the tables the review's own harness writes, not
// the substrate's. `tx_probe` is the probe table the contention harness
// uses to hold the WAL write lock, so the replayed run has one more row in
// it than the control run — an artefact of the measurement, not a
// duplicated side effect. It is excluded from the diff and named here so
// the exclusion is visible rather than a blanket filter.
var replayHarnessTables = map[string]bool{
	"tx_probe": true,
}

// diffTableCounts renders the difference between two count maps in a
// stable, readable form.
func diffTableCounts(want, got map[string]int) []string {
	var diffs []string
	names := map[string]bool{}
	for n := range want {
		names[n] = true
	}
	for n := range got {
		names[n] = true
	}
	ordered := make([]string, 0, len(names))
	for n := range names {
		if replayHarnessTables[n] {
			continue
		}
		ordered = append(ordered, n)
	}
	sort.Strings(ordered)
	for _, n := range ordered {
		w, okW := want[n]
		g, okG := got[n]
		if !okW {
			diffs = append(diffs, fmt.Sprintf("%s: absent in control, %d in replayed", n, g))
			continue
		}
		if !okG {
			diffs = append(diffs, fmt.Sprintf("%s: %d in control, absent in replayed", n, w))
			continue
		}
		if w != g {
			diffs = append(diffs, fmt.Sprintf("%s: control=%d replayed=%d", n, w, g))
		}
	}
	return diffs
}

// countWatchdogReason counts the watchdog records carrying the given
// retention reason.
func countWatchdogReason(t *testing.T, dm *DatabaseManager, reason string) int {
	t.Helper()
	n := 0
	for _, rec := range readWatchdogRecords(t, dm) {
		if rec["reason"] == reason {
			n++
		}
	}
	return n
}

// forceTransactionReplay runs fn while forcing the transaction that fn
// writes through to be RESTARTED exactly once.
//
// THE RECIPE
// ----------
//  1. A competing connection takes the WAL write lock with an
//     UNCOMMITTED write. The writer's first write attempt collides with
//     it and returns plain SQLITE_BUSY.
//  2. The retry counter rises — and it rises only AFTER the statement
//     level retry, which happens only AFTER the transaction has taken its
//     read snapshot. That is the ordering guarantee: releasing the holder
//     now guarantees the writer's snapshot is already established.
//  3. The holder commits. The WAL advances past the writer's snapshot.
//  4. The writer's next write attempt returns SQLITE_BUSY_SNAPSHOT, and
//     WithTx restarts the transaction.
//
// The caller must verify the restart actually happened — see
// countWatchdogReason: a case that did not restart proves nothing, so
// the test fails rather than passing vacuously.
func forceTransactionReplay(t *testing.T, dm *DatabaseManager, competitor *sql.DB, fn func() error) {
	t.Helper()

	baseline := dm.BusyRetryCount()
	release := holdWALWriteLock(t, competitor)

	errCh := make(chan error, 1)
	go func() { errCh <- fn() }()

	waitFor(t, 30*time.Second, "the writer to collide with the held write lock", func() bool {
		return dm.BusyRetryCount() > baseline
	})
	// Releasing the holder is what advances the WAL. Everything before
	// this point was a lock WAIT; everything after it can be a stale
	// SNAPSHOT.
	release()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("the write failed after the forced restart: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the write did not complete")
	}
}

// fileCensus records the size of every file in a directory, so a test can
// assert that a callback produced no external side effect.
//
// THE SCOPE
// ---------
// Two files in the database's own directory are observability streams and
// are expected to change: `watchdog.jsonl` (the operational record) and
// `mirror.jsonl` (the human-facing audit trail). They are excluded by
// name, and the exclusion is explicit here so it cannot quietly become a
// blanket filter.
//
// `system_audit_log` is NOT excluded — that is a TABLE, and the
// table-count diff covers it. The audit log is the one sink that writes
// through the bare pool, so its rows are never rolled back with the
// transaction.
func fileCensus(t *testing.T, dir string) map[string]int64 {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir %s: %v", dir, err)
	}
	out := map[string]int64{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			t.Fatalf("stat %s: %v", e.Name(), err)
		}
		out[e.Name()] = info.Size()
	}
	return out
}

// observabilityStreams are the files in the database's directory that are
// expected to change during a write. They are observability, not state.
var observabilityStreams = map[string]bool{
	"watchdog.jsonl": true,
	"mirror.jsonl":   true,
}

// isDatabaseFile reports whether a file in the database's directory is
// the SQLite database itself or one of its sidecars.
//
// They are the substrate, not an external side effect, and their byte
// counts are not comparable between two runs: a transaction that is
// restarted does more work, so its WAL grows by a different amount. That
// difference is expected and says nothing about duplication — the TABLE
// counts are what carry that meaning.
func isDatabaseFile(name string) bool {
	if strings.HasSuffix(name, "-wal") || strings.HasSuffix(name, "-shm") ||
		strings.HasSuffix(name, "-journal") {
		return true
	}
	return strings.HasSuffix(name, ".db")
}

// diffFileCensus renders the difference between two file censuses,
// ignoring the observability streams.
func diffFileCensus(want, got map[string]int64) []string {
	var diffs []string
	names := map[string]bool{}
	for n := range want {
		names[n] = true
	}
	for n := range got {
		names[n] = true
	}
	ordered := make([]string, 0, len(names))
	for n := range names {
		if observabilityStreams[n] || isDatabaseFile(n) {
			continue
		}
		ordered = append(ordered, n)
	}
	sort.Strings(ordered)
	for _, n := range ordered {
		w, okW := want[n]
		g, okG := got[n]
		switch {
		case !okW:
			diffs = append(diffs, fmt.Sprintf("%s: absent in control, %d bytes in replayed", n, g))
		case !okG:
			diffs = append(diffs, fmt.Sprintf("%s: %d bytes in control, absent in replayed", n, w))
		case w != g:
			diffs = append(diffs, fmt.Sprintf("%s: control=%d bytes replayed=%d bytes", n, w, g))
		}
	}
	return diffs
}

// fileGrowth lists the files that grew during a write, with their growth
// in bytes. The observability streams are excluded.
func fileGrowth(before, after map[string]int64) []string {
	var grown []string
	for n, size := range after {
		if observabilityStreams[n] || isDatabaseFile(n) {
			continue
		}
		if prev, ok := before[n]; !ok || prev != size {
			grown = append(grown, fmt.Sprintf("%s (+%d bytes)", n, size-before[n]))
		}
	}
	sort.Strings(grown)
	return grown
}

// TestWithTxReplay_ProducesNoDuplicatedSideEffects is the core safety
// result.
//
// For every production write path whose callback READS before it WRITES —
// the only shape that can be replayed — the test:
//
//  1. runs the write in a CONTROL database with no contention and records
//     every table's row count;
//  2. runs the SAME write in a second database with a real transaction
//     restart forced, and records every table's row count again;
//  3. asserts a `busy_snapshot` watchdog record exists, so the restart
//     is PROVEN rather than assumed;
//  4. asserts the two count maps are IDENTICAL.
//
// Step 4 is the safety property. If the restart duplicated an audit row,
// an out-of-transaction side effect, or an artifact, the replayed run
// would show more rows in some table. If it LOST something — because the
// callback was not actually idempotent — the replayed run would show
// fewer.
//
// The two runs use separate database files so the diff is a clean
// per-table comparison rather than a delta against a shared baseline.
func TestWithTxReplay_ProducesNoDuplicatedSideEffects(t *testing.T) {
	cases := []struct {
		name string
		// setup prepares state the write path needs and returns the write
		// closure. It runs before the contention window, identically in
		// both runs. Returning the closure keeps per-case state in a
		// closure instead of a package-level variable.
		setup func(t *testing.T, dm *DatabaseManager) func(t *testing.T, dm *DatabaseManager) error
		// write performs the production write when there is no setup.
		write func(t *testing.T, dm *DatabaseManager) error
	}{
		{
			// RecordDecision's callback (epistemology_tools.go:490) reads
			// first — the dedup probe inside saveMemoryRow — and then
			// writes the decision memory and its source citations.
			name: "decision",
			write: func(t *testing.T, dm *DatabaseManager) error {
				_, err := dm.RecordDecision(
					"the context of the decision",
					"the choice",
					"the rationale",
					"the outcome",
					[]string{"tag"},
					nil,
					ActiveContext{},
				)
				return err
			},
		},
		{
			// ProposeTheory's callback (epistemology_tools.go:196) has the
			// same shape: dedup probe, then the theory memory and its
			// citations.
			name: "theory",
			write: func(t *testing.T, dm *DatabaseManager) error {
				_, err := dm.ProposeTheory(
					"a hypothesis written under a forced restart",
					"the validation criteria",
					nil,
					nil,
					[]string{"tag"},
				)
				return err
			},
		},
		{
			// LogChangelogEntryWithAssertions' callback
			// (changelog_mcp.go:287) writes a changelog memory plus one
			// evidence row per confirmation and contradiction.
			name: "changelog",
			write: func(t *testing.T, dm *DatabaseManager) error {
				_, err := dm.LogChangelogEntryWithAssertions(
					"a changelog entry written under a forced restart",
					// The changelog surface requires a full 40-character
					// SHA-1; it is validated, not derived.
					"0123456789abcdef0123456789abcdef01234567",
					nil,
					nil,
					nil,
				)
				return err
			},
		},
		{
			// The work-transition callbacks (db.go:7153 and siblings) call
			// AppendWorkEvent, whose FIRST statement is
			// `SELECT COALESCE(MAX(event_index), -1) + 1` — a read.
			name: "work-event",
			setup: func(t *testing.T, dm *DatabaseManager) func(t *testing.T, dm *DatabaseManager) error {
				work, err := dm.CreateWorkWithContext("a work item", "the content", "session-1", ActiveContext{})
				if err != nil {
					t.Fatalf("create work: %v", err)
				}
				id := work.ID
				return func(t *testing.T, dm *DatabaseManager) error {
					_, err := dm.CompleteWorkWithContext(id, "done under a forced restart", ActiveContext{})
					return err
				}
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			// CONTROL — no contention.
			ctlDM, _ := newWALTestDM(t)
			ctlDir := filepath.Dir(ctlDM.watchdogPath)
			ctlFilesBefore := fileCensus(t, ctlDir)
			ctlWrite := tc.write
			if tc.setup != nil {
				ctlWrite = tc.setup(t, ctlDM)
			}
			if err := ctlWrite(t, ctlDM); err != nil {
				t.Fatalf("control run failed: %v", err)
			}
			control := tableRowCounts(t, ctlDM)
			controlFiles := fileCensus(t, ctlDir)

			// REPLAYED — a real transaction restart, forced.
			repDM, competitor := newWALTestDM(t)
			repDir := filepath.Dir(repDM.watchdogPath)
			repFilesBefore := fileCensus(t, repDir)
			repWrite := tc.write
			if tc.setup != nil {
				repWrite = tc.setup(t, repDM)
			}
			forceTransactionReplay(t, repDM, competitor, func() error {
				return repWrite(t, repDM)
			})

			// The restart must be PROVEN. Without this assertion the
			// comparison below would pass even if no restart happened —
			// a vacuous pass.
			if n := countWatchdogReason(t, repDM, watchdogReasonBusySnapshot); n == 0 {
				t.Fatalf("no %q watchdog record: the transaction was never restarted, so this case proves nothing", watchdogReasonBusySnapshot)
			}

			replayed := tableRowCounts(t, repDM)
			if diffs := diffTableCounts(control, replayed); len(diffs) > 0 {
				t.Fatalf("the restart changed the durable state:\n  %s", strings.Join(diffs, "\n  "))
			}

			// EXTERNAL ACTIONS. A file the callback wrote would not be
			// rolled back with the transaction, so a replay would append
			// to it twice. The census covers every file in the database's
			// directory, so a new file is caught too.
			controlFileGrowth := fileGrowth(ctlFilesBefore, controlFiles)
			replayFileGrowth := fileGrowth(repFilesBefore, fileCensus(t, repDir))
			if len(controlFileGrowth) != len(replayFileGrowth) {
				t.Fatalf("the restart produced a different set of external side effects:\n  control: %v\n  replayed: %v",
					controlFileGrowth, replayFileGrowth)
			}
			for i := range controlFileGrowth {
				if controlFileGrowth[i] != replayFileGrowth[i] {
					t.Fatalf("the restart produced a different external side effect:\n  control: %v\n  replayed: %v",
						controlFileGrowth, replayFileGrowth)
				}
			}
		})
	}
}

// TestWriteFirstTransactionCannotExperienceAStaleSnapshot proves the
// structural half of the argument: a transaction whose FIRST statement is
// a WRITE cannot hit SQLITE_BUSY_SNAPSHOT, because it holds the WAL write
// lock, and while it holds that lock no other connection can write — so
// no other connection can commit, so the WAL cannot advance, so no
// snapshot can go stale.
//
// This is what makes most of the WithTx callbacks safe by construction
// rather than by inspection. The proof is direct: the competing write is
// ATTEMPTED while the write-first transaction is open, and it is refused
// — so the WAL never advances past any reader's snapshot, and the stale
// snapshot class cannot arise.
//
// It is also the reason the restart cannot cause a double-write in those
// callbacks: they are never replayed.
func TestWriteFirstTransactionCannotExperienceAStaleSnapshot(t *testing.T) {
	dm, competitor := newWALTestDM(t)

	tx, err := dm.db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()

	node := &txNode{tx: tx, dm: dm}

	// The FIRST statement is a WRITE — it takes the WAL write lock.
	if _, err := node.ExecTracked(`INSERT INTO tx_probe (id, v) VALUES ('first-write', 'w')`, 0); err != nil {
		t.Fatalf("first write: %v", err)
	}

	// Now a competing connection tries to write. It must be REFUSED: the
	// write lock is held, so the WAL cannot advance.
	compTx, err := competitor.Begin()
	if err != nil {
		t.Fatalf("competitor begin: %v", err)
	}
	if _, err := compTx.Exec(`INSERT INTO tx_probe (id, v) VALUES ('competitor', 'c')`); err == nil {
		_ = compTx.Rollback()
		t.Fatal("the competing write SUCCEEDED while the write-first transaction held the lock — the structural argument is wrong")
	} else if !IsBusyError(err) {
		t.Fatalf("expected the competing write to fail with a busy error, got %v", err)
	}
	// The competing transaction cannot commit either: its write never
	// landed, so there is nothing to advance the WAL with.
	_ = compTx.Rollback()

	// The reader's snapshot therefore never went stale: a subsequent
	// read-then-write in the SAME transaction is unaffected.
	var n int
	if err := node.QueryRowTracked(`SELECT COUNT(*) FROM tx_probe`).Scan(&n); err != nil {
		t.Fatalf("read after the refused commit: %v", err)
	}
	if _, err := node.ExecTracked(`INSERT INTO tx_probe (id, v) VALUES ('second-write', 'w')`, 0); err != nil {
		t.Fatalf("second write: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// TestReplayReachesNoAuditRowOutsideTheTransaction is the narrowest
// statement of the safety property, and the one that matters most for
// observability: the audit log is written through the BARE pool, so its
// rows are never rolled back. If a replayed callback emitted an audit row
// per attempt, the audit log would grow by one row per replay — and the
// replay would be invisible in every other table.
//
// The decision write path is used because it is a real replayable
// callback. The assertion is that the audit log holds the SAME number of
// rows after the forced restart as after the uncontended control.
func TestReplayReachesNoAuditRowOutsideTheTransaction(t *testing.T) {
	ctlDM, _ := newWALTestDM(t)
	if _, err := ctlDM.RecordDecision("ctx", "choice", "why", "result", nil, nil, ActiveContext{}); err != nil {
		t.Fatalf("control run failed: %v", err)
	}
	ctlAudit := tableRowCounts(t, ctlDM)["system_audit_log"]

	repDM, competitor := newWALTestDM(t)
	forceTransactionReplay(t, repDM, competitor, func() error {
		_, err := repDM.RecordDecision("ctx", "choice", "why", "result", nil, nil, ActiveContext{})
		return err
	})
	if n := countWatchdogReason(t, repDM, watchdogReasonBusySnapshot); n == 0 {
		t.Fatalf("no %q watchdog record: the transaction was never restarted", watchdogReasonBusySnapshot)
	}
	repAudit := tableRowCounts(t, repDM)["system_audit_log"]

	if repAudit != ctlAudit {
		t.Fatalf("the audit log gained rows across the restart: control=%d replayed=%d — an out-of-transaction side effect was duplicated", ctlAudit, repAudit)
	}
}
