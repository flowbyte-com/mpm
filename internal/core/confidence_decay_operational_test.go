package internal

// confidence_decay_operational_test.go — the OPERATIONAL contract of the
// bounded decay pass (issue #92).
//
// The trust-boundary rules live in confidence_decay_trust_boundary_test.go.
// This file covers the properties that make the pass safe to run
// unattended: what it selects, how it pages, how it resumes, how it
// behaves under contention and cancellation, what it writes, and whether
// the stored value ends up agreeing with the formula.
//
// Every test uses an isolated database and a FIXED clock, so the decay
// term is deterministic: no test depends on how long the machine took.

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// cancelAfterNContext is a context that reports cancellation after N
// Err() calls.
//
// The pass observes cancellation at the ARTIFACT BOUNDARY, so a counter
// is the honest way to interrupt it: it stops after a known number of
// artifacts have been processed, with no reliance on timing.
type cancelAfterNContext struct {
	n     int
	calls int
}

func (c *cancelAfterNContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *cancelAfterNContext) Done() <-chan struct{}       { return nil }
func (c *cancelAfterNContext) Value(interface{}) interface{} {
	return nil
}
func (c *cancelAfterNContext) Err() error {
	c.calls++
	if c.calls > c.n {
		return context.Canceled
	}
	return nil
}

// decayArtifactIDs reads the ids of the artifacts the pass considers due,
// in selection order.
func decayArtifactIDs(t *testing.T, dm *DatabaseManager, cutoff int64, limit int) []string {
	t.Helper()
	rows, err := dm.SQLDB().Query(
		confidenceDecayDueSelect+confidenceDecayDueWhere+` ORDER BY last_check ASC, id ASC LIMIT ?`,
		cutoff, limit)
	if err != nil {
		t.Fatalf("select due: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id, collection string
		var lastCheck int64
		if err := rows.Scan(&id, &collection, &lastCheck); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, id)
	}
	return out
}

// TestConfidenceDecay_StoredValueAgreesWithTheFormula is the acceptance
// criterion of issue #92 itself: after a pass, the stored confidence IS
// the value the formula derives.
//
// It is checked per artifact type, because the formula's initial value
// and its decay rate are per type. An artifact whose stored value still
// disagrees with the formula is exactly the divergence #92 is about.
func TestConfidenceDecay_StoredValueAgreesWithTheFormula(t *testing.T) {
	dm := NewTestDM(t)

	created := decayFixtureNow.Add(-200 * 24 * time.Hour)
	cases := []struct {
		id           string
		collection   string
		artifactType string
	}{
		{"mem-agree-mem", "memories", "memory"},
		{"mem-agree-theory", "theories", "theory"},
		{"mem-agree-decision", "decisions", "decision"},
	}
	for _, c := range cases {
		insertDecayMemoryEpoch(t, dm, c.id, c.collection, created, InitialConfidence(c.artifactType))
	}

	res, err := dm.RunConfidenceDecay(ConfidenceDecayOptions{Now: decayFixtureNow, Limit: 10})
	if err != nil {
		t.Fatalf("run decay: %v", err)
	}
	if res.Changed != len(cases) {
		t.Fatalf("result = %+v, want %d changed", res, len(cases))
	}

	for _, c := range cases {
		got, _ := storedDecayConfidence(t, dm, c.id)

		ev, lastPositiveAt, err := loadEvidenceForRecompute(dm, c.id, c.artifactType, decayFixtureNow)
		if err != nil {
			t.Fatalf("load evidence %s: %v", c.id, err)
		}
		want := computeConfidence(c.artifactType, ev, decayFixtureNow, lastPositiveAt, 0.005)
		if math.Abs(got-want) > 1e-9 {
			t.Errorf("%s: stored confidence %.12f, want the formula's %.12f",
				c.id, got, want)
		}
	}
}

// TestConfidenceDecay_RepeatRunWithNoElapsedTimeFindsNothing pins the
// idempotence property: a second pass at the SAME clock has nothing to
// do.
//
// The watermark is what makes this true, not the clock: the artifacts
// were just checked, so they are not due again until MinAge has elapsed.
func TestConfidenceDecay_RepeatRunWithNoElapsedTimeFindsNothing(t *testing.T) {
	dm := NewTestDM(t)
	created := decayFixtureNow.Add(-200 * 24 * time.Hour)
	for i := 0; i < 3; i++ {
		insertDecayMemoryEpoch(t, dm, "mem-rep-"+string(rune('a'+i)), "memories", created, InitialConfidence("memory"))
	}

	first, err := dm.RunConfidenceDecay(ConfidenceDecayOptions{Now: decayFixtureNow, Limit: 10})
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if first.Due != 3 || first.Changed != 3 {
		t.Fatalf("first run = %+v, want due=3 changed=3", first)
	}

	second, err := dm.RunConfidenceDecay(ConfidenceDecayOptions{Now: decayFixtureNow, Limit: 10})
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if second.Due != 0 || second.Scanned != 0 || second.Changed != 0 {
		t.Errorf("second run at the same clock = %+v, want nothing due", second)
	}

	// And the values did not move a second time.
	conf, wm := storedDecayConfidence(t, dm, "mem-rep-a")
	if wm == nil || *wm != decayFixtureNow.Unix() {
		t.Errorf("watermark = %v, want %d", wm, decayFixtureNow.Unix())
	}
	ev, lastPositiveAt, err := loadEvidenceForRecompute(dm, "mem-rep-a", "memory", decayFixtureNow)
	if err != nil {
		t.Fatalf("load evidence: %v", err)
	}
	want := computeConfidence("memory", ev, decayFixtureNow, lastPositiveAt, 0.005)
	if math.Abs(conf-want) > 1e-9 {
		t.Errorf("confidence moved on the second run: %.12f, want %.12f", conf, want)
	}
}

// TestConfidenceDecay_PagesThroughTheBacklogInOrder pins the paging
// contract: a pass takes the OLDEST due artifacts first, up to its
// limit, and the next pass takes the next page.
//
// This is what makes the pass a bounded maintenance step rather than a
// full-store rewrite, and the ordering is what makes it fair: the
// artifacts that have gone longest without a check are served first.
func TestConfidenceDecay_PagesThroughTheBacklogInOrder(t *testing.T) {
	dm := NewTestDM(t)

	// Five artifacts, each a day older than the last, so "oldest first"
	// is a strict order rather than a tie broken by id.
	ids := []string{"mem-page-1", "mem-page-2", "mem-page-3", "mem-page-4", "mem-page-5"}
	for i, id := range ids {
		created := decayFixtureNow.Add(-time.Duration(200-i) * 24 * time.Hour)
		insertDecayMemoryEpoch(t, dm, id, "memories", created, InitialConfidence("memory"))
	}

	cutoff := decayFixtureNow.Add(-24 * time.Hour).Unix()
	wantOrder := decayArtifactIDs(t, dm, cutoff, 10)
	if len(wantOrder) != 5 {
		t.Fatalf("fixture: %d artifacts are due, want 5", len(wantOrder))
	}

	page1, err := dm.RunConfidenceDecay(ConfidenceDecayOptions{Now: decayFixtureNow, Limit: 2})
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	if page1.Due != 5 || page1.Scanned != 2 || page1.Changed != 2 {
		t.Fatalf("page 1 = %+v, want due=5 scanned=2 changed=2", page1)
	}
	if page1.Deferred != 3 {
		t.Errorf("page 1 deferred = %d, want 3 — the backlog must be visible", page1.Deferred)
	}
	// The two oldest were served.
	for _, id := range wantOrder[:2] {
		if _, wm := storedDecayConfidence(t, dm, id); wm == nil {
			t.Errorf("%s was not processed by page 1", id)
		}
	}
	for _, id := range wantOrder[2:] {
		if _, wm := storedDecayConfidence(t, dm, id); wm != nil {
			t.Errorf("%s was processed by page 1, but it is not in the first page", id)
		}
	}

	// The next page takes the next two. The clock advances by a minute
	// so the served artifacts' watermarks do not make them due again.
	page2, err := dm.RunConfidenceDecay(ConfidenceDecayOptions{Now: decayFixtureNow.Add(time.Minute), Limit: 2})
	if err != nil {
		t.Fatalf("page 2: %v", err)
	}
	if page2.Scanned != 2 || page2.Changed != 2 {
		t.Fatalf("page 2 = %+v, want scanned=2 changed=2", page2)
	}
	for _, id := range wantOrder[:4] {
		if _, wm := storedDecayConfidence(t, dm, id); wm == nil {
			t.Errorf("%s was not processed by page 2", id)
		}
	}

	// The last page finishes the backlog.
	page3, err := dm.RunConfidenceDecay(ConfidenceDecayOptions{Now: decayFixtureNow.Add(2 * time.Minute), Limit: 2})
	if err != nil {
		t.Fatalf("page 3: %v", err)
	}
	if page3.Scanned != 1 || page3.Changed != 1 || page3.Deferred != 0 {
		t.Fatalf("page 3 = %+v, want scanned=1 changed=1 deferred=0", page3)
	}
	for _, id := range ids {
		if _, wm := storedDecayConfidence(t, dm, id); wm == nil {
			t.Errorf("%s was never processed", id)
		}
	}
}

// TestConfidenceDecay_CancelledPassIsResumable pins the interruption
// contract: a cancelled pass stops at an artifact boundary, the artifacts
// it finished keep their watermark, and the ones it did not reach stay
// due for the next pass.
//
// Nothing is half-written: the artifact in flight always completes.
func TestConfidenceDecay_CancelledPassIsResumable(t *testing.T) {
	dm := NewTestDM(t)
	created := decayFixtureNow.Add(-200 * 24 * time.Hour)
	ids := []string{"mem-cancel-1", "mem-cancel-2", "mem-cancel-3", "mem-cancel-4"}
	for _, id := range ids {
		insertDecayMemoryEpoch(t, dm, id, "memories", created, InitialConfidence("memory"))
	}

	// Cancel at the third boundary: two artifacts are processed, then the
	// pass stops.
	ctx := &cancelAfterNContext{n: 2}
	res, err := dm.RunConfidenceDecay(ConfidenceDecayOptions{Now: decayFixtureNow, Limit: 10, Ctx: ctx})
	if err != nil {
		t.Fatalf("cancelled run: %v", err)
	}
	if !res.Cancelled {
		t.Errorf("result = %+v, want cancelled", res)
	}
	if res.Scanned != 2 || res.Changed != 2 {
		t.Errorf("result = %+v, want scanned=2 changed=2 before the cancellation", res)
	}
	if res.Due != 4 {
		t.Errorf("due = %d, want 4 — the backlog must still be visible", res.Due)
	}

	// Two artifacts were processed and two were not.
	processed, unprocessed := 0, 0
	for _, id := range ids {
		if _, wm := storedDecayConfidence(t, dm, id); wm != nil {
			processed++
		} else {
			unprocessed++
		}
	}
	if processed != 2 || unprocessed != 2 {
		t.Errorf("processed=%d unprocessed=%d, want 2 and 2", processed, unprocessed)
	}

	// The pass resumes: the artifacts that were not reached are still
	// due, and a follow-up pass finishes them.
	next, err := dm.RunConfidenceDecay(ConfidenceDecayOptions{Now: decayFixtureNow.Add(time.Minute), Limit: 10})
	if err != nil {
		t.Fatalf("resume run: %v", err)
	}
	if next.Scanned != 2 || next.Changed != 2 {
		t.Errorf("resume = %+v, want scanned=2 changed=2", next)
	}
	for _, id := range ids {
		if _, wm := storedDecayConfidence(t, dm, id); wm == nil {
			t.Errorf("%s was never processed across the two passes", id)
		}
	}
}

// TestConfidenceDecay_AlreadyCancelledPassDoesNothing pins the other
// end of the cancellation contract: a context cancelled before the pass
// starts means the pass does no work at all.
func TestConfidenceDecay_AlreadyCancelledPassDoesNothing(t *testing.T) {
	dm := NewTestDM(t)
	created := decayFixtureNow.Add(-200 * 24 * time.Hour)
	insertDecayMemoryEpoch(t, dm, "mem-pre-cancel", "memories", created, InitialConfidence("memory"))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res, err := dm.RunConfidenceDecay(ConfidenceDecayOptions{Now: decayFixtureNow, Limit: 10, Ctx: ctx})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Cancelled || res.Scanned != 0 {
		t.Errorf("result = %+v, want cancelled with nothing scanned", res)
	}
	conf, wm := storedDecayConfidence(t, dm, "mem-pre-cancel")
	if wm != nil {
		t.Errorf("watermark = %v, want NULL — a cancelled pass must not mark work done", wm)
	}
	if conf != InitialConfidence("memory") {
		t.Errorf("confidence = %v, want it unchanged", conf)
	}
}

// TestConfidenceDecay_ContendedArtifactFailsAndStaysDue pins the
// behaviour under SQLite contention.
//
// A competing writer holds the WAL write lock for the whole pass, so the
// artifact's write cannot proceed. The pass must:
//   - report the artifact as FAILED rather than silently dropping it;
//   - leave its stored value untouched;
//   - leave its watermark untouched, so the next pass retries it.
//
// The fixture uses a file-backed WAL database with the production DSN,
// because that is the only configuration where lock contention of this
// shape exists at all.
func TestConfidenceDecay_ContendedArtifactFailsAndStaysDue(t *testing.T) {
	dm, competitor := newWALTestDM(t)

	created := decayFixtureNow.Add(-200 * 24 * time.Hour)
	insertDecayMemoryEpoch(t, dm, "mem-contended", "memories", created, InitialConfidence("memory"))

	// The competing connection takes the WAL write lock and holds it.
	holderTx, err := competitor.Begin()
	if err != nil {
		t.Fatalf("holder begin: %v", err)
	}
	if _, err := holderTx.Exec(`INSERT INTO tx_probe (id, v) VALUES ('holder', 'h')`); err != nil {
		t.Fatalf("holder insert: %v", err)
	}

	res, err := dm.RunConfidenceDecay(ConfidenceDecayOptions{Now: decayFixtureNow, Limit: 10})
	// The pass itself does not fail as a whole: one artifact failed, the
	// rest of the pass is unaffected.
	if err != nil {
		t.Fatalf("run decay: %v", err)
	}
	if res.Failed != 1 {
		t.Errorf("result = %+v, want one failed artifact", res)
	}
	if len(res.Failures) != 1 {
		t.Fatalf("result = %+v, want one failure message", res)
	}
	conf, wm := storedDecayConfidence(t, dm, "mem-contended")
	if wm != nil {
		t.Errorf("watermark = %v, want NULL — a failed artifact must stay due", wm)
	}
	if conf != InitialConfidence("memory") {
		t.Errorf("confidence = %v, want it untouched", conf)
	}

	// Release the lock; the next pass succeeds and the artifact is
	// finally decayed.
	if err := holderTx.Commit(); err != nil {
		t.Fatalf("holder commit: %v", err)
	}
	next, err := dm.RunConfidenceDecay(ConfidenceDecayOptions{Now: decayFixtureNow.Add(time.Minute), Limit: 10})
	if err != nil {
		t.Fatalf("retry run: %v", err)
	}
	if next.Failed != 0 || next.Changed != 1 {
		t.Fatalf("retry = %+v, want failed=0 changed=1", next)
	}
	after, wm := storedDecayConfidence(t, dm, "mem-contended")
	if wm == nil {
		t.Error("watermark was not advanced by the successful pass")
	}
	if !(after < InitialConfidence("memory")) {
		t.Errorf("confidence = %v, want it decayed", after)
	}
}

// TestConfidenceDecay_ConcurrentEvidenceIsNotLost pins the concurrency
// property that matters for correctness: an evidence row committed while
// the pass is working on the same artifact is never lost, and the pass
// never leaves a stored value that contradicts the evidence.
//
// The shape here is a competing writer that holds the WAL write lock
// across the pass and commits an evidence row for the artifact the pass
// is trying to write. The pass cannot complete that artifact while the
// lock is held, so it fails it and leaves it due — and the follow-up
// pass, which sees the committed evidence, computes from it.
func TestConfidenceDecay_ConcurrentEvidenceIsNotLost(t *testing.T) {
	dm, competitor := newWALTestDM(t)

	created := decayFixtureNow.Add(-200 * 24 * time.Hour)
	insertDecayMemoryEpoch(t, dm, "mem-concurrent", "memories", created, InitialConfidence("memory"))

	// The competing writer holds the write lock and has an evidence row
	// for the artifact, uncommitted.
	holderTx, err := competitor.Begin()
	if err != nil {
		t.Fatalf("holder begin: %v", err)
	}
	if _, err := holderTx.Exec(`
		INSERT INTO evidence (id, artifact_id, artifact_type, type, source_group,
		                      strength, independence_factor, created_by, created_at, expires_at, notes, reference_url)
		VALUES ('ev-concurrent', 'mem-concurrent', 'memory', 'observation', 'test',
		        1.0, 1.0, 'competitor', ?, NULL, NULL, NULL)`,
		decayFixtureNow.Add(-time.Hour).Unix(),
	); err != nil {
		t.Fatalf("holder evidence insert: %v", err)
	}

	res, err := dm.RunConfidenceDecay(ConfidenceDecayOptions{Now: decayFixtureNow, Limit: 10})
	if err != nil {
		t.Fatalf("run decay: %v", err)
	}
	if res.Failed != 1 {
		t.Errorf("result = %+v, want the contended artifact to fail", res)
	}
	if conf, wm := storedDecayConfidence(t, dm, "mem-concurrent"); wm != nil || conf != InitialConfidence("memory") {
		t.Errorf("the contended artifact was partially written: conf=%v watermark=%v", conf, wm)
	}

	// The competing writer commits.
	if err := holderTx.Commit(); err != nil {
		t.Fatalf("holder commit: %v", err)
	}

	// The next pass sees the committed evidence and derives from it.
	next, err := dm.RunConfidenceDecay(ConfidenceDecayOptions{Now: decayFixtureNow.Add(time.Minute), Limit: 10})
	if err != nil {
		t.Fatalf("retry run: %v", err)
	}
	if next.Failed != 0 || next.Changed != 1 {
		t.Fatalf("retry = %+v, want failed=0 changed=1", next)
	}
	got, _ := storedDecayConfidence(t, dm, "mem-concurrent")
	want := computeConfidence("memory", []evidenceInput{{
		Strength: 1.0, Independence: 1.0, CreatedAt: decayFixtureNow.Add(-time.Hour),
	}}, decayFixtureNow.Add(time.Minute), decayFixtureNow.Add(-time.Hour), 0.005)
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("confidence = %.12f, want %.12f — the concurrent evidence was not used", got, want)
	}
}

// TestConfidenceDecay_UnchangedArtifactsProduceNoWrites pins the write
// amplification contract.
//
// An artifact whose derived value has not moved must cost exactly one
// write — the watermark — and nothing else: no confidence write, no
// confidence_history row. The history ledger is the thing that grows
// without bound, so it is the thing worth measuring.
func TestConfidenceDecay_UnchangedArtifactsProduceNoWrites(t *testing.T) {
	dm := NewTestDM(t)

	// An artifact whose confidence was JUST recomputed to the value the
	// formula would derive now: the next pass finds nothing to change.
	created := decayFixtureNow.Add(-200 * 24 * time.Hour)
	insertDecayMemoryEpoch(t, dm, "mem-nochange", "memories", created, InitialConfidence("memory"))
	if _, err := dm.RunConfidenceDecay(ConfidenceDecayOptions{Now: decayFixtureNow, Limit: 10}); err != nil {
		t.Fatalf("first run: %v", err)
	}
	historyBefore := decayHistoryCount(t, dm, "mem-nochange")
	if historyBefore != 1 {
		t.Fatalf("history rows after the first pass = %d, want 1", historyBefore)
	}

	// Move the clock to exactly the derived value's instant? No — the
	// value drifts with the clock, so instead run the pass again at the
	// SAME clock: the watermark makes it not due, which is the
	// idempotence property, already covered. To exercise the
	// "unchanged" path the watermark must be due but the value must not
	// move, so the watermark is cleared and the clock is held.
	if _, err := dm.SQLDB().Exec(
		`UPDATE memories SET confidence_checked_at = NULL WHERE id = 'mem-nochange'`); err != nil {
		t.Fatalf("clear watermark: %v", err)
	}
	res, err := dm.RunConfidenceDecay(ConfidenceDecayOptions{Now: decayFixtureNow, Limit: 10})
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if res.Changed != 0 || res.Unchanged != 1 {
		t.Errorf("result = %+v, want changed=0 unchanged=1", res)
	}
	if n := decayHistoryCount(t, dm, "mem-nochange"); n != historyBefore {
		t.Errorf("history rows = %d, want %d — an unchanged artifact must not grow the ledger", n, historyBefore)
	}
}

// TestConfidenceDecay_DatabaseGrowthIsBounded pins the growth contract
// empirically: a pass over N artifacts must not grow the database by
// more than a small multiple of N rows' worth of pages.
//
// The ledger is the unbounded thing (one row per CHANGED artifact), so
// the assertion is that the history ledger grows exactly in proportion
// to the artifacts whose value actually moved, and that the database
// file itself does not grow with the number of passes.
func TestConfidenceDecay_DatabaseGrowthIsBounded(t *testing.T) {
	// A FILE-backed database: the growth of the database file is the
	// thing under test, and an in-memory database has no file to grow.
	dm, _ := newWALTestDM(t)
	path := decayDatabaseFile(t, dm)
	if path == "" {
		t.Fatal("no file-backed database path; growth cannot be measured")
	}

	created := decayFixtureNow.Add(-200 * 24 * time.Hour)
	const n = 50
	for i := 0; i < n; i++ {
		insertDecayMemoryEpoch(t, dm, fmt.Sprintf("mem-growth-%02d", i), "memories", created, InitialConfidence("memory"))
	}

	sizeBefore := decayFileSize(t, path)
	res, err := dm.RunConfidenceDecay(ConfidenceDecayOptions{Now: decayFixtureNow, Limit: n})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Changed != n {
		t.Fatalf("result = %+v, want %d changed", res, n)
	}

	// The ledger grew by exactly one row per changed artifact.
	if got := decayHistoryCount(t, dm, fmt.Sprintf("mem-growth-%02d", 0)); got != 1 {
		t.Errorf("history rows = %d, want 1", got)
	}
	var total int
	if err := dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM confidence_history`).Scan(&total); err != nil {
		t.Fatalf("count history: %v", err)
	}
	if total != n {
		t.Errorf("total history rows = %d, want %d", total, n)
	}

	// A second pass at the same clock finds nothing due and writes
	// nothing, so the database does not grow.
	sizeAfterFirst := decayFileSize(t, path)
	if _, err := dm.RunConfidenceDecay(ConfidenceDecayOptions{Now: decayFixtureNow, Limit: n}); err != nil {
		t.Fatalf("second run: %v", err)
	}
	sizeAfterSecond := decayFileSize(t, path)
	if sizeAfterSecond > sizeAfterFirst {
		t.Errorf("the database grew on a no-op pass: %d -> %d bytes", sizeAfterFirst, sizeAfterSecond)
	}

	// And the growth of the first pass is bounded: a WAL-backed store
	// that wrote a row per artifact plus one history row per artifact
	// cannot grow by more than a generous multiple of that.
	if sizeBefore > 0 && sizeAfterFirst > sizeBefore+int64(n)*4096 {
		t.Errorf("the pass grew the database by %d bytes for %d artifacts — the write "+
			"amplification is not bounded", sizeAfterFirst-sizeBefore, n)
	}
}

// TestConfidenceDecay_LessonsAreNotParticipants pins the eligibility
// decision: a lesson is not decayed.
//
// The reason is structural, not a preference: the maintenance watermark
// lives on `memories`, and `lessons_base` has no metadata column either,
// so a lesson cannot carry the trust-boundary state the pass reads. The
// pass therefore leaves the lesson population alone rather than half-
// managing it.
func TestConfidenceDecay_LessonsAreNotParticipants(t *testing.T) {
	dm := NewTestDM(t)

	if _, err := dm.SQLDB().Exec(`
		INSERT INTO lessons (id, type, content, tags, reinforcement_count, source_session_id, created, content_hash, confidence)
		VALUES ('lesson-decay-1', 'insight', 'a lesson that is not decayed', '[]', 1, NULL, ?, 'hash-1', 0.7)`,
		decayFixtureNow.Add(-200*24*time.Hour).Format(time.RFC3339),
	); err != nil {
		t.Fatalf("insert lesson: %v", err)
	}

	res, err := dm.RunConfidenceDecay(ConfidenceDecayOptions{Now: decayFixtureNow, Limit: 10})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Due != 0 || res.Scanned != 0 {
		t.Errorf("result = %+v, want no lesson in the participant set", res)
	}
	var conf float64
	if err := dm.SQLDB().QueryRow(`SELECT confidence FROM lessons WHERE id = 'lesson-decay-1'`).Scan(&conf); err != nil {
		t.Fatalf("read lesson confidence: %v", err)
	}
	if math.Abs(conf-0.7) > 1e-9 {
		t.Errorf("lesson confidence = %v, want it unchanged at 0.7", conf)
	}
}

// TestConfidenceDecay_DryRunCountsMatchTheRealPass pins the dry-run
// contract: the preview must report what the real pass will do.
//
// This is the property an operator relies on to decide whether to run
// the pass at all, so a mismatch is a defect in the preview, not a
// rounding difference.
func TestConfidenceDecay_DryRunCountsMatchTheRealPass(t *testing.T) {
	dm := NewTestDM(t)
	created := decayFixtureNow.Add(-200 * 24 * time.Hour)
	// One artifact that will move, one that will not.
	insertDecayMemoryEpoch(t, dm, "mem-dry-a", "memories", created, InitialConfidence("memory"))
	insertDecayMemoryEpoch(t, dm, "mem-dry-b", "memories", created, InitialConfidence("memory"))
	if _, err := dm.RunConfidenceDecay(ConfidenceDecayOptions{Now: decayFixtureNow, Limit: 10}); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	// Clear one watermark so only one is due.
	if _, err := dm.SQLDB().Exec(
		`UPDATE memories SET confidence_checked_at = NULL WHERE id = 'mem-dry-b'`); err != nil {
		t.Fatalf("clear watermark: %v", err)
	}

	dry, err := dm.RunConfidenceDecay(ConfidenceDecayOptions{Now: decayFixtureNow.Add(time.Hour), Limit: 10, DryRun: true})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	// The dry run must not have written anything.
	if _, wm := storedDecayConfidence(t, dm, "mem-dry-b"); wm != nil {
		t.Errorf("the dry run advanced a watermark: %v", *wm)
	}

	real, err := dm.RunConfidenceDecay(ConfidenceDecayOptions{Now: decayFixtureNow.Add(time.Hour), Limit: 10})
	if err != nil {
		t.Fatalf("real run: %v", err)
	}
	if dry.Due != real.Due || dry.Scanned != real.Scanned ||
		dry.Changed != real.Changed || dry.Unchanged != real.Unchanged {
		t.Errorf("dry run = %+v, real pass = %+v — the preview must match the pass", dry, real)
	}
}

// decayDatabaseFile returns the path of the SQLite file backing the
// manager, or "" when the database is in memory.
//
// It asks SQLite rather than the manager, because a manager built over
// an already-open pool (the shape the WAL harness uses) does not know
// the path itself.
func decayDatabaseFile(t *testing.T, dm *DatabaseManager) string {
	t.Helper()
	rows, err := dm.SQLDB().Query(`PRAGMA database_list`)
	if err != nil {
		t.Fatalf("PRAGMA database_list: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var seq int
		var name, file string
		if err := rows.Scan(&seq, &name, &file); err != nil {
			t.Fatalf("scan database_list: %v", err)
		}
		if name == "main" {
			return file
		}
	}
	return ""
}

// decayFileSize returns the size of a file, or 0 when it does not exist.
func decayFileSize(t *testing.T, path string) int64 {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0
		}
		t.Fatalf("stat %s: %v", filepath.Base(path), err)
	}
	return st.Size()
}
