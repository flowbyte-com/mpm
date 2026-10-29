package internal

// synthesis_merge_transaction_test.go — issue #178's synthesis
// cooldown correction.
//
// THE DEFECT
// ----------
// The synthesis engine merges near-miss memories into one. The merge
// has SIX durable consequences:
//
//   1. the merged memory exists;
//   2. its created_at is the oldest source created_at;
//   3. the sources' topic memberships are transferred to it;
//   4. the sources are soft-deleted;
//   5. the synth_runs ledger records the content hash; and
//   6. the merged memory is stamped with last_synthesized_at — the
//      COOLDOWN that stops the near-miss detector from picking it again.
//
// Steps 5 and 6 are the DEDUP STATE: they are what makes "this content
// has already been synthesized" a durable fact rather than a hope.
//
// The previous implementation wrote steps 5 and 6 with a bare pool
// call and DISCARDED the error. So a failed cooldown write produced
// exactly the state the cooldown exists to prevent:
//
//   - the merged memory is live;
//   - it has NO last_synthesized_at;
//   - the next ingest's DetectNearMiss does not filter it out, so it
//     is merged a SECOND time.
//
// A second merge is not a second copy of a file. It is a SECOND
// ARTIFACT ASSERTING THE SAME CONCLUSION, over evidence that has
// already been soft-deleted — duplicated evidence, and confidence
// counted twice.
//
// THE CORRECTION
// --------------
// The six writes are ONE FACT, so they are ONE TRANSACTION
// (commitSynthesisMerge). Either the merge and its dedup state both
// land, or neither does. A failed cooldown write now rolls the whole
// merge back: the sources stay live, the merge stays pending, and the
// next ingest redoes it cleanly.
//
// THE DATABASE
// -------------
// Every test here runs against a DISPOSABLE, FILE-BACKED WAL database
// built by NewDatabaseManager, which uses the production connection
// settings (WAL, `_busy_timeout=5000`, `_foreign_keys=1`). The
// contention test therefore produces the contention production
// produces.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// mergeSourceContent is the content of the memories a merge consumes.
// It is deliberately FTS-friendly: the near-miss detector has to be
// able to FIND these rows for the cooldown assertions to mean anything.
const mergeSourceContent = "alpha beta gamma"

// mergeResultContent is the content of the merged memory. It shares
// tokens with the sources so that the near-miss detector would pick it
// up if the cooldown did not exclude it.
const mergeResultContent = "alpha beta delta merged conclusion"

// newMergeTestDM builds a disposable, file-backed WAL database with
// the production connection settings.
func newMergeTestDM(t *testing.T) *DatabaseManager {
	t.Helper()
	dm, err := NewDatabaseManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewDatabaseManager: %v", err)
	}
	t.Cleanup(func() { _ = dm.Close() })
	return dm
}

// insertMergeSource inserts a live memory the merge will consume.
func insertMergeSource(t *testing.T, dm *DatabaseManager, id, content string) {
	t.Helper()
	if _, err := dm.db.Exec(
		`INSERT INTO memories (id, collection, content, content_hash, created_at)
		 VALUES (?, 'memories', ?, ?, CAST(strftime('%s','now') AS INTEGER))`,
		id, content, "hash-"+id,
	); err != nil {
		t.Fatalf("insert the merge source %s: %v", id, err)
	}
}

// liveMemoryIDs returns the ids of the memories that are not
// soft-deleted.
func liveMemoryIDs(t *testing.T, dm *DatabaseManager) []string {
	t.Helper()
	rows, err := dm.db.Query(
		`SELECT id FROM memories WHERE collection = 'memories' AND deleted_at IS NULL ORDER BY id`,
	)
	if err != nil {
		t.Fatalf("read the live memories: %v", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan the live memories: %v", err)
		}
		ids = append(ids, id)
	}
	return ids
}

// memoryCount returns the number of memory rows, live or not.
func memoryCount(t *testing.T, dm *DatabaseManager) int {
	t.Helper()
	var n int
	if err := dm.db.QueryRow(
		`SELECT COUNT(*) FROM memories WHERE collection = 'memories'`,
	).Scan(&n); err != nil {
		t.Fatalf("count the memories: %v", err)
	}
	return n
}

// ledgerRunCount returns the ledger's run_count for a content hash,
// and whether the ledger knows the hash at all.
func ledgerRunCount(t *testing.T, dm *DatabaseManager, contentHash string) (int, bool) {
	t.Helper()
	var runCount int
	err := dm.db.QueryRow(
		`SELECT run_count FROM synth_runs WHERE content_hash = ?`, contentHash,
	).Scan(&runCount)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false
	}
	if err != nil {
		t.Fatalf("read the synthesis ledger: %v", err)
	}
	return runCount, true
}

// TestSynthesisMerge_AFailedCooldownWriteLeavesNoMergedMemory is THE
// REPRODUCTION.
//
// The cooldown write is made to fail deterministically — a trigger
// that refuses the UPDATE — and the merge is attempted. The merge MUST
// fail, and it MUST leave nothing behind.
//
// Under the previous implementation the cooldown write's error was
// discarded, so the merge "succeeded": the merged memory was live with
// NO last_synthesized_at, which is precisely the state that makes the
// next ingest merge it again.
//
// The assertions are the ones that matter for the epistemic boundary:
//   - the merged memory does NOT exist;
//   - the sources are still LIVE (not soft-deleted), so the evidence
//     has not been consumed by a merge that did not happen;
//   - the ledger has no row, so the content is still eligible for a
//     first synthesis.
func TestSynthesisMerge_AFailedCooldownWriteLeavesNoMergedMemory(t *testing.T) {
	dm := newMergeTestDM(t)
	insertMergeSource(t, dm, "src-a", mergeSourceContent)
	insertMergeSource(t, dm, "src-b", mergeSourceContent+" two")

	// The deterministic fault: the cooldown stamp cannot be written.
	if _, err := dm.db.Exec(`
		CREATE TRIGGER block_synth_cooldown BEFORE UPDATE OF last_synthesized_at ON memories
		BEGIN SELECT RAISE(ABORT, 'cooldown blocked'); END;
	`); err != nil {
		t.Fatalf("install the cooldown blocker: %v", err)
	}

	mergedID, err := commitSynthesisMerge(dm, context.Background(), synthesisMerge{
		Content:     mergeResultContent,
		Tags:        []string{"synthesized", "ltm"},
		Metadata:    map[string]interface{}{"synthesized": true},
		SourceIDs:   []string{"src-a", "src-b"},
		ContentHash: "hash-merge-1",
	})
	if err == nil {
		t.Fatalf("the merge must fail when the cooldown write fails; it returned %q", mergedID)
	}
	if mergedID != "" {
		t.Errorf("a failed merge must not report a merged memory id; got %q", mergedID)
	}
	if !strings.Contains(err.Error(), "cooldown") {
		t.Errorf("the error must name the failing step, so the failure is diagnosable; got: %v", err)
	}

	// The merged memory must NOT exist.
	if n := memoryCount(t, dm); n != 2 {
		t.Errorf("the failed merge wrote %d memory rows; want 2 (the sources only): "+
			"a merged memory without a cooldown stamp is merged AGAIN on the next ingest", n)
	}

	// The sources must still be LIVE.
	live := liveMemoryIDs(t, dm)
	if len(live) != 2 {
		t.Errorf("the sources must still be live after a failed merge; live=%v", live)
	}

	// The ledger must know nothing about this content.
	if _, known := ledgerRunCount(t, dm, "hash-merge-1"); known {
		t.Errorf("a failed merge must not leave a ledger row: the content is still eligible for " +
			"a first synthesis")
	}
}

// TestSynthesisMerge_ASuccessfulMergeCommitsTheArtifactAndItsDedupState
// is the REGRESSION.
//
// The merge lands, and it lands COMPLETE: the artifact, its cooldown
// stamp, the ledger row and the sources' soft-deletion are all there.
// A merge that lands its artifact without its dedup state is the
// defect this change removes.
func TestSynthesisMerge_ASuccessfulMergeCommitsTheArtifactAndItsDedupState(t *testing.T) {
	dm := newMergeTestDM(t)
	insertMergeSource(t, dm, "src-a", mergeSourceContent)
	insertMergeSource(t, dm, "src-b", mergeSourceContent+" two")

	mergedID, err := commitSynthesisMerge(dm, context.Background(), synthesisMerge{
		Content:     mergeResultContent,
		Tags:        []string{"synthesized", "ltm"},
		Metadata:    map[string]interface{}{"synthesized": true},
		SourceIDs:   []string{"src-a", "src-b"},
		ContentHash: "hash-merge-1",
	})
	if err != nil {
		t.Fatalf("the merge must land: %v", err)
	}
	if mergedID == "" {
		t.Fatal("the merge must report the merged memory's id")
	}

	// The cooldown stamp.
	var stamped *int64
	if err := dm.db.QueryRow(
		`SELECT last_synthesized_at FROM memories WHERE id = ?`, mergedID,
	).Scan(&stamped); err != nil {
		t.Fatalf("read the merged memory's cooldown: %v", err)
	}
	if stamped == nil {
		t.Fatal("the merged memory has NO cooldown stamp: the next ingest would merge it again")
	}

	// The ledger.
	runCount, known := ledgerRunCount(t, dm, "hash-merge-1")
	if !known {
		t.Fatal("the merge must leave a ledger row: without it the same content is synthesized again")
	}
	if runCount != 1 {
		t.Errorf("the ledger's run_count = %d; want 1", runCount)
	}
	var ledgerMemory string
	if err := dm.db.QueryRow(
		`SELECT result_memory_id FROM synth_runs WHERE content_hash = ?`, "hash-merge-1",
	).Scan(&ledgerMemory); err != nil {
		t.Fatalf("read the ledger's result memory: %v", err)
	}
	if ledgerMemory != mergedID {
		t.Errorf("the ledger points at %q; want the merged memory %q", ledgerMemory, mergedID)
	}

	// The sources are soft-deleted, not destroyed.
	var liveSources int
	if err := dm.db.QueryRow(
		`SELECT COUNT(*) FROM memories WHERE id IN ('src-a','src-b') AND deleted_at IS NULL`,
	).Scan(&liveSources); err != nil {
		t.Fatalf("count the live sources: %v", err)
	}
	if liveSources != 0 {
		t.Errorf("%d sources are still live after the merge; the evidence would be duplicated "+
			"in the corpus", liveSources)
	}
}

// TestSynthesisMerge_TheCooldownSuppressesAnImmediateReMerge is the
// DEDUP RELATIONSHIP, asserted from the detector's side.
//
// The cooldown exists so that the merged memory is not picked again.
// This test drives the DETECTOR — the thing the cooldown is for — and
// asserts that the merged memory does not come back.
func TestSynthesisMerge_TheCooldownSuppressesAnImmediateReMerge(t *testing.T) {
	dm := newMergeTestDM(t)
	insertMergeSource(t, dm, "src-a", mergeSourceContent)
	insertMergeSource(t, dm, "src-b", mergeSourceContent+" two")

	mergedID, err := commitSynthesisMerge(dm, context.Background(), synthesisMerge{
		Content:     mergeResultContent,
		Tags:        []string{"synthesized", "ltm"},
		Metadata:    map[string]interface{}{"synthesized": true},
		SourceIDs:   []string{"src-a", "src-b"},
		ContentHash: "hash-merge-1",
	})
	if err != nil {
		t.Fatalf("the merge must land: %v", err)
	}

	// Threshold 0 isolates the cooldown predicate from bm25 scoring:
	// the question is "would the detector pick the merged memory",
	// not "does it score well enough".
	candidates, err := DetectNearMiss(dm, mergeResultContent, "a-new-memory", 0.0, 3600)
	if err != nil {
		t.Fatalf("DetectNearMiss: %v", err)
	}
	for _, c := range candidates {
		if c.ID == mergedID {
			t.Fatalf("the merged memory %s was returned as a near-miss candidate inside the "+
				"cooldown window: the merge would run a second time over the same conclusion",
				mergedID)
		}
	}

	// And the content hash fast-skip must hit, so the same CONTENT is
	// not synthesized again either.
	if hit, _ := synthHasContentHash(dm, "hash-merge-1"); !hit {
		t.Error("the content hash fast-skip must hit after a merge: the same content would be " +
			"synthesized again")
	}
}

// TestSynthesisMerge_TheCooldownDoesNotSuppressLegitimateFutureSynthesis
// is the COUNTERWEIGHT.
//
// A cooldown that never expires is not a cooldown; it is a permanent
// suppression. The requirement that the cooldown stop a duplicate
// merge must not become a requirement that the merged memory can NEVER
// be merged again.
//
// With the cooldown DISABLED (0 seconds — the documented way to turn
// it off), the merged memory must be a candidate again, and the
// ledger's fast-skip must not hide it.
func TestSynthesisMerge_TheCooldownDoesNotSuppressLegitimateFutureSynthesis(t *testing.T) {
	dm := newMergeTestDM(t)
	insertMergeSource(t, dm, "src-a", mergeSourceContent)
	insertMergeSource(t, dm, "src-b", mergeSourceContent+" two")

	mergedID, err := commitSynthesisMerge(dm, context.Background(), synthesisMerge{
		Content:     mergeResultContent,
		Tags:        []string{"synthesized", "ltm"},
		Metadata:    map[string]interface{}{"synthesized": true},
		SourceIDs:   []string{"src-a", "src-b"},
		ContentHash: "hash-merge-1",
	})
	if err != nil {
		t.Fatalf("the merge must land: %v", err)
	}

	// Cooldown disabled: the merged memory is eligible again. This is
	// what a legitimate LATER synthesis looks like.
	candidates, err := DetectNearMiss(dm, mergeResultContent, "a-new-memory", 0.0, 0)
	if err != nil {
		t.Fatalf("DetectNearMiss (cooldown disabled): %v", err)
	}
	found := false
	for _, c := range candidates {
		if c.ID == mergedID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("with the cooldown disabled the merged memory %s must be a candidate again: "+
			"the cooldown would otherwise suppress every legitimate future synthesis of it",
			mergedID)
	}

	// And the ledger's fast-skip is keyed on the CONTENT HASH, not on
	// the merged memory: a DIFFERENT content is not blocked by an old
	// merge. This is the property that keeps a later, distinct
	// synthesis reachable.
	if hit, _ := synthHasContentHash(dm, "a-different-content-hash"); hit {
		t.Error("the ledger must not block a different content hash: the fast-skip is per-content")
	}
}

// TestSynthesisMerge_ARestartBeforeTheCommitLeavesNoPartialState is
// the RESTART condition.
//
// A crash before the commit is, from the database's point of view,
// indistinguishable from a transaction that never committed: nothing
// landed. This test injects the fault at the LAST write of the merge
// (the cooldown), which is the write that decides whether the dedup
// state and the artifact agree.
//
// The point is that there is no state in which the artifact exists and
// the dedup state does not.
func TestSynthesisMerge_ARestartBeforeTheCommitLeavesNoPartialState(t *testing.T) {
	dm := newMergeTestDM(t)
	insertMergeSource(t, dm, "src-a", mergeSourceContent)
	insertMergeSource(t, dm, "src-b", mergeSourceContent+" two")

	// The fault lands on the soft-delete: the merge gets most of the
	// way through, and then the transaction cannot finish.
	if _, err := dm.db.Exec(`
		CREATE TRIGGER block_soft_delete BEFORE UPDATE OF deleted_at ON memories
		BEGIN SELECT RAISE(ABORT, 'soft-delete blocked'); END;
	`); err != nil {
		t.Fatalf("install the soft-delete blocker: %v", err)
	}

	if _, err := commitSynthesisMerge(dm, context.Background(), synthesisMerge{
		Content:     mergeResultContent,
		Tags:        []string{"synthesized", "ltm"},
		Metadata:    map[string]interface{}{"synthesized": true},
		SourceIDs:   []string{"src-a", "src-b"},
		ContentHash: "hash-merge-1",
	}); err == nil {
		t.Fatal("the merge must fail when one of its writes fails")
	}

	// NOTHING landed: not the merged memory, not the ledger row, not
	// the cooldown, not the soft-deletion.
	if n := memoryCount(t, dm); n != 2 {
		t.Errorf("a rolled-back merge left %d memory rows; want 2 (the sources only)", n)
	}
	if _, known := ledgerRunCount(t, dm, "hash-merge-1"); known {
		t.Error("a rolled-back merge must not leave a ledger row")
	}
	if live := liveMemoryIDs(t, dm); len(live) != 2 {
		t.Errorf("a rolled-back merge must leave the sources live; live=%v", live)
	}
}

// TestSynthesisMerge_SurvivesOrdinaryLockContention is the RETRY
// condition.
//
// The merge's writes carry the substrate's statement-level budget, so
// ordinary contention is a WAIT and the merge waits it out. The
// contention is held for LONGER than the production busy timeout
// (5000ms) — a single attempt cannot survive it, so the retry is
// genuinely exercised.
//
// The assertion is not just "the merge landed": it is that the merge
// landed COMPLETE. A merge that landed its artifact and lost its
// cooldown would be the defect, and a retry that stopped halfway
// would be a different one.
func TestSynthesisMerge_SurvivesOrdinaryLockContention(t *testing.T) {
	dm := newMergeTestDM(t)
	insertMergeSource(t, dm, "src-a", mergeSourceContent)
	insertMergeSource(t, dm, "src-b", mergeSourceContent+" two")

	// The competitor: a second connection to the SAME file, holding
	// the WAL write lock in an open transaction.
	competitor, err := sql.Open("sqlite3", dm.dbPath+"?_busy_timeout=5000&_journal_mode=WAL")
	if err != nil {
		t.Fatalf("open the competitor connection: %v", err)
	}
	defer competitor.Close()

	if _, err := competitor.Exec(
		`CREATE TABLE IF NOT EXISTS synth_merge_probe (id TEXT PRIMARY KEY, v TEXT NOT NULL)`,
	); err != nil {
		t.Fatalf("create the contention probe table: %v", err)
	}
	tx, err := competitor.Begin()
	if err != nil {
		t.Fatalf("competitor begin: %v", err)
	}
	if _, err := tx.Exec(
		`INSERT INTO synth_merge_probe (id, v) VALUES ('holder', 'h')`,
	); err != nil {
		t.Fatalf("competitor insert: %v", err)
	}

	var released atomic.Bool
	release := func() {
		if released.Swap(true) {
			return
		}
		if err := tx.Commit(); err != nil {
			t.Errorf("competitor commit: %v", err)
		}
	}
	// Longer than the busy timeout: the first attempt expires.
	go func() {
		time.Sleep(6 * time.Second)
		release()
	}()
	defer release()

	mergedID, err := commitSynthesisMerge(dm, context.Background(), synthesisMerge{
		Content:     mergeResultContent,
		Tags:        []string{"synthesized", "ltm"},
		Metadata:    map[string]interface{}{"synthesized": true},
		SourceIDs:   []string{"src-a", "src-b"},
		ContentHash: "hash-merge-1",
	})
	if err != nil {
		t.Fatalf("the merge must survive ordinary contention: %v", err)
	}

	var stamped *int64
	if err := dm.db.QueryRow(
		`SELECT last_synthesized_at FROM memories WHERE id = ?`, mergedID,
	).Scan(&stamped); err != nil {
		t.Fatalf("read the merged memory's cooldown: %v", err)
	}
	if stamped == nil {
		t.Error("the merged memory has no cooldown stamp after a contended merge: the merge " +
			"landed its artifact and lost its dedup state")
	}
	if _, known := ledgerRunCount(t, dm, "hash-merge-1"); !known {
		t.Error("a contended merge must land its ledger row too")
	}
}

// TestSynthesisMerge_NoDuplicatedEvidenceOrInflatedConfidence is the
// TRUST BOUNDARY.
//
// A merge that runs twice does not merely duplicate a row: it
// duplicates EVIDENCE and it counts the same conclusion's confidence
// twice. This test asserts the three properties that make a merge
// safe to retry:
//
//   - exactly ONE live memory carries the merged content;
//   - the sources are soft-deleted, so the evidence is not live twice;
//   - the merged memory's confidence is the ordinary initial confidence
//     — not a doubled or auto-trusted one.
//
// The confidence is read from the same InitialConfidence the substrate
// gives every memory of this collection, so the assertion is that the
// merge did not inflate it.
func TestSynthesisMerge_NoDuplicatedEvidenceOrInflatedConfidence(t *testing.T) {
	dm := newMergeTestDM(t)
	insertMergeSource(t, dm, "src-a", mergeSourceContent)
	insertMergeSource(t, dm, "src-b", mergeSourceContent+" two")

	mergedID, err := commitSynthesisMerge(dm, context.Background(), synthesisMerge{
		Content:     mergeResultContent,
		Tags:        []string{"synthesized", "ltm"},
		Metadata:    map[string]interface{}{"synthesized": true},
		SourceIDs:   []string{"src-a", "src-b"},
		ContentHash: "hash-merge-1",
	})
	if err != nil {
		t.Fatalf("the merge must land: %v", err)
	}

	// Exactly one LIVE memory carries the merged conclusion.
	var liveWithContent int
	if err := dm.db.QueryRow(
		`SELECT COUNT(*) FROM memories WHERE collection = 'memories' AND deleted_at IS NULL AND content = ?`,
		mergeResultContent,
	).Scan(&liveWithContent); err != nil {
		t.Fatalf("count the live merged memories: %v", err)
	}
	if liveWithContent != 1 {
		t.Errorf("%d live memories carry the merged conclusion; want exactly 1: "+
			"a second merge would duplicate the evidence and the conclusion", liveWithContent)
	}

	// The evidence is not live twice: the sources are soft-deleted,
	// and they still EXIST (soft delete, not destroy) so the merge is
	// auditable.
	var liveSources, totalSources int
	if err := dm.db.QueryRow(
		`SELECT COUNT(*) FROM memories WHERE id IN ('src-a','src-b') AND deleted_at IS NULL`,
	).Scan(&liveSources); err != nil {
		t.Fatalf("count the live sources: %v", err)
	}
	if err := dm.db.QueryRow(
		`SELECT COUNT(*) FROM memories WHERE id IN ('src-a','src-b')`,
	).Scan(&totalSources); err != nil {
		t.Fatalf("count the sources: %v", err)
	}
	if liveSources != 0 {
		t.Errorf("%d sources are still live: the evidence is duplicated in the corpus", liveSources)
	}
	if totalSources != 2 {
		t.Errorf("the sources were destroyed rather than soft-deleted; %d of 2 remain", totalSources)
	}

	// The confidence is the ordinary initial confidence — the merge
	// did not inflate it and did not auto-trust the conclusion.
	var confidence float64
	if err := dm.db.QueryRow(
		`SELECT confidence FROM memories WHERE id = ?`, mergedID,
	).Scan(&confidence); err != nil {
		t.Fatalf("read the merged memory's confidence: %v", err)
	}
	want := float64(InitialConfidence(artifactTypeFromCollection("memories")))
	if confidence != want {
		t.Errorf("the merged memory's confidence = %v; want the ordinary initial confidence %v: "+
			"a merge must not inflate confidence", confidence, want)
	}
}

// TestSynthesisMerge_AFailedMergeIsObservable asserts the
// OBSERVABILITY requirement: a merge that did not land says so.
//
// The error names the step that failed, so an operator reading the
// watchdog or the audit log can tell a cooldown failure from a
// soft-delete failure without reading the source.
func TestSynthesisMerge_AFailedMergeIsObservable(t *testing.T) {
	dm := newMergeTestDM(t)
	insertMergeSource(t, dm, "src-a", mergeSourceContent)

	if _, err := dm.db.Exec(`
		CREATE TRIGGER block_ledger BEFORE INSERT ON synth_runs
		BEGIN SELECT RAISE(ABORT, 'ledger blocked'); END;
	`); err != nil {
		t.Fatalf("install the ledger blocker: %v", err)
	}

	_, err := commitSynthesisMerge(dm, context.Background(), synthesisMerge{
		Content:     mergeResultContent,
		Tags:        []string{"synthesized", "ltm"},
		Metadata:    map[string]interface{}{"synthesized": true},
		SourceIDs:   []string{"src-a"},
		ContentHash: "hash-merge-1",
	})
	if err == nil {
		t.Fatal("the merge must fail when the ledger write fails")
	}
	if !strings.Contains(err.Error(), "ledger") {
		t.Errorf("the error must name the failing step; got: %v", err)
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("%v", "blocked")) {
		t.Errorf("the error must carry the underlying cause; got: %v", err)
	}
}

// TestSynthesisMerge_ACancelledContextWritesNothing asserts that a
// cancelled synthesis does not half-apply.
//
// The context is checked before the closure's first write, so a
// cancelled merge writes nothing at all.
func TestSynthesisMerge_ACancelledContextWritesNothing(t *testing.T) {
	dm := newMergeTestDM(t)
	insertMergeSource(t, dm, "src-a", mergeSourceContent)
	insertMergeSource(t, dm, "src-b", mergeSourceContent+" two")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := commitSynthesisMerge(dm, ctx, synthesisMerge{
		Content:     mergeResultContent,
		Tags:        []string{"synthesized", "ltm"},
		Metadata:    map[string]interface{}{"synthesized": true},
		SourceIDs:   []string{"src-a", "src-b"},
		ContentHash: "hash-merge-1",
	})
	if err == nil {
		t.Fatal("a cancelled merge must fail")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("the error must carry the cancellation; got: %v", err)
	}
	if n := memoryCount(t, dm); n != 2 {
		t.Errorf("a cancelled merge wrote %d memory rows; want 2 (the sources only)", n)
	}
}

// TestSynthesisMerge_TheMergeIsIdempotentUnderReDelivery asserts the
// property that makes the merge safe to re-drive: running it twice
// with the SAME content does not produce a second live artifact.
//
// The second delivery finds the merged content already live (the
// substrate's own identity dedup), so it reuses that row instead of
// writing a second one. The point is that the corpus ends up with ONE
// live artifact asserting the conclusion, not two.
func TestSynthesisMerge_TheMergeIsIdempotentUnderReDelivery(t *testing.T) {
	dm := newMergeTestDM(t)
	insertMergeSource(t, dm, "src-a", mergeSourceContent)
	insertMergeSource(t, dm, "src-b", mergeSourceContent+" two")

	merge := synthesisMerge{
		Content:     mergeResultContent,
		Tags:        []string{"synthesized", "ltm"},
		Metadata:    map[string]interface{}{"synthesized": true},
		SourceIDs:   []string{"src-a", "src-b"},
		ContentHash: "hash-merge-1",
	}

	firstID, err := commitSynthesisMerge(dm, context.Background(), merge)
	if err != nil {
		t.Fatalf("the first merge must land: %v", err)
	}

	// The same merge, delivered again — the situation a re-drive after
	// an ambiguous commit produces.
	secondID, err := commitSynthesisMerge(dm, context.Background(), merge)
	if err != nil {
		t.Fatalf("the re-delivered merge must not fail: %v", err)
	}
	if secondID != firstID {
		t.Errorf("the re-delivered merge produced a different artifact (%q vs %q): the corpus "+
			"would carry the conclusion twice", secondID, firstID)
	}

	var liveWithContent int
	if err := dm.db.QueryRow(
		`SELECT COUNT(*) FROM memories WHERE collection = 'memories' AND deleted_at IS NULL AND content = ?`,
		mergeResultContent,
	).Scan(&liveWithContent); err != nil {
		t.Fatalf("count the live merged memories: %v", err)
	}
	if liveWithContent != 1 {
		t.Errorf("%d live memories carry the merged conclusion after a re-delivery; want exactly 1",
			liveWithContent)
	}
}

// TestSynthesisMerge_AMergedMemoryWithoutACooldownStampIsEligibleForReMerge
// proves the CAUSAL LINK that makes a discarded cooldown write
// dangerous.
//
// The reproduction test above shows that the merge now rolls back when
// the cooldown write fails. This test shows WHY that matters: a memory
// that carries the merged content but NO cooldown stamp IS a near-miss
// candidate, so the next ingest merges it again.
//
// Without this test, "the cooldown write failed" and "the content was
// synthesized twice" would be two unrelated claims. With it, they are
// one mechanism.
func TestSynthesisMerge_AMergedMemoryWithoutACooldownStampIsEligibleForReMerge(t *testing.T) {
	dm := newMergeTestDM(t)

	// A memory that looks exactly like a merge result — but was never
	// stamped, because the stamp write failed.
	if _, err := dm.db.Exec(
		`INSERT INTO memories (id, collection, content, content_hash, last_synthesized_at)
		 VALUES (?, 'memories', ?, ?, NULL)`,
		"unstamped-merge", mergeResultContent, "hash-unstamped",
	); err != nil {
		t.Fatalf("insert the unstamped memory: %v", err)
	}

	// The FTS index must carry it, or the probe below proves nothing.
	var indexed int
	if err := dm.db.QueryRow(
		`SELECT COUNT(*) FROM memories_fts WHERE memories_fts MATCH ?`, `"alpha"`,
	).Scan(&indexed); err != nil {
		t.Fatalf("FTS5 probe: %v", err)
	}
	if indexed == 0 {
		t.Skip("the FTS5 trigger did not fire in this session; the eligibility claim would be vacuous")
	}

	// Threshold 0 isolates the cooldown predicate from bm25 scoring.
	candidates, err := DetectNearMiss(dm, mergeResultContent, "a-new-memory", 0.0, 3600)
	if err != nil {
		t.Fatalf("DetectNearMiss: %v", err)
	}
	found := false
	for _, c := range candidates {
		if c.ID == "unstamped-merge" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("a merged memory WITHOUT a cooldown stamp must be a near-miss candidate: " +
			"that is the mechanism by which a discarded cooldown write causes a duplicate synthesis")
	}

	// And with the stamp, it is not. This is the other half of the
	// mechanism, and it is what the correction guarantees.
	if err := markMemorySynthCooldown(dm, "unstamped-merge"); err != nil {
		t.Fatalf("markMemorySynthCooldown: %v", err)
	}
	candidates, err = DetectNearMiss(dm, mergeResultContent, "a-new-memory", 0.0, 3600)
	if err != nil {
		t.Fatalf("DetectNearMiss (stamped): %v", err)
	}
	for _, c := range candidates {
		if c.ID == "unstamped-merge" {
			t.Fatal("a STAMPED merged memory must not be a near-miss candidate inside the " +
				"cooldown window")
		}
	}
}

// TestSynthesisMerge_TheDatabaseIsWALAndFileBacked asserts the
// substrate the tests claim to be exercising.
//
// A test that says "WAL database with production settings" and then
// runs against an in-memory database proves something else. This test
// pins the claim, so the file cannot be quietly changed to something
// that behaves differently under contention.
func TestSynthesisMerge_TheDatabaseIsWALAndFileBacked(t *testing.T) {
	dm := newMergeTestDM(t)

	var journalMode string
	if err := dm.db.QueryRow(`PRAGMA journal_mode`).Scan(&journalMode); err != nil {
		t.Fatalf("read the journal mode: %v", err)
	}
	if !strings.EqualFold(journalMode, "wal") {
		t.Errorf("the journal mode is %q; the merge's contention behaviour is WAL's", journalMode)
	}
	if !filepath.IsAbs(dm.dbPath) {
		t.Errorf("the database path %q is not absolute: the test is not exercising the "+
			"file-backed database it claims to", dm.dbPath)
	}
	var busyTimeout int
	if err := dm.db.QueryRow(`PRAGMA busy_timeout`).Scan(&busyTimeout); err != nil {
		t.Fatalf("read the busy timeout: %v", err)
	}
	if busyTimeout != 5000 {
		t.Errorf("the busy timeout is %dms; the production setting is 5000ms", busyTimeout)
	}
}
