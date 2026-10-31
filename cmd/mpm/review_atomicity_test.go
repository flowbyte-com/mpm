// review_atomicity_test.go — the review decision and the weight change
// are ONE operator act.
//
// WHAT THIS FILE PINS
// -------------------
// `mpm ops review --ack <id> --decision weaken` records the operator's
// review AND reduces the memory's weight. Those are two writes, and the
// command used to make them two SEPARATE ones:
//
//	RecordMemoryReview(id, decision, note)   ← write 1
//	WeakenMemory(id, 1)                      ← write 2
//
// If write 2 failed, write 1 had already landed: the memory had LEFT
// the doctor's backlog, and the weight the operator asked to reduce was
// untouched. The operator's decision was half applied, and the error
// message named only the half that worked.
//
// The same shape applied to `--ack-all`: a batch that failed on its
// third memory left the first two applied and reported nothing about
// the third. A partially applied batch was never reported as complete
// success — but it WAS left behind.
//
// The rule these tests pin:
//
//   - the review and the weight change are ONE transaction;
//   - the batch is atomic as a UNIT — every review and every
//     weakening, or none of them;
//   - a partially applied batch is never reported as complete success;
//   - the weight change goes through the EXISTING WeakenMemory path, so
//     there is no second, divergent weakening algorithm.
//
// EVERY test runs against an isolated workspace. The live installation
// is never touched, and no fault is injected into production.
package main

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ── the injection seams ───────────────────────────────────────────────────

// injectWeakeningFailure installs a trigger that makes the WEIGHT
// CHANGE fail for one memory, and nothing else.
//
// This is the honest way to inject a failure into the middle of the
// operation: the review write succeeds, and the weight UPDATE — the
// second statement — aborts. A failure between the two writes is
// exactly the defect this file exists for.
//
// The trigger is removed by dropping it, which is how the RECOVERY test
// proves that a failed operation leaves the system able to succeed
// afterwards.
func (f reviewFixture) injectWeakeningFailure(t *testing.T, memoryID string) {
	t.Helper()
	db, err := sql.Open("sqlite3", filepath.Join(f.ws, "src", "db", "mpm.db"))
	if err != nil {
		t.Fatalf("open the fixture database: %v", err)
	}
	defer db.Close()
	// A SQLite trigger cannot take bound parameters, so the id is
	// inlined. It is a test constant, and the quote is escaped anyway.
	_, err = db.Exec(`CREATE TRIGGER IF NOT EXISTS inject_weaken_failure
		BEFORE UPDATE ON memories
		WHEN NEW.id = '` + strings.ReplaceAll(memoryID, "'", "''") + `'
		BEGIN
			SELECT RAISE(ABORT, 'injected weakening failure');
		END`)
	if err != nil {
		t.Fatalf("install the failure injection: %v", err)
	}
}

// removeWeakeningFailure drops the injected trigger.
func (f reviewFixture) removeWeakeningFailure(t *testing.T) {
	t.Helper()
	db, err := sql.Open("sqlite3", filepath.Join(f.ws, "src", "db", "mpm.db"))
	if err != nil {
		t.Fatalf("open the fixture database: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`DROP TRIGGER IF EXISTS inject_weaken_failure`); err != nil {
		t.Fatalf("remove the failure injection: %v", err)
	}
}

// reviewCount reads the number of recorded reviews for a memory.
func (f reviewFixture) reviewCount(t *testing.T, id string) int {
	t.Helper()
	db, err := sql.Open("sqlite3", filepath.Join(f.ws, "src", "db", "mpm.db"))
	if err != nil {
		t.Fatalf("open the fixture database: %v", err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM memory_reviews WHERE memory_id = ?`, id).Scan(&n); err != nil {
		t.Fatalf("read the review count of %q: %v", id, err)
	}
	return n
}

// totalReviewCount reads the number of recorded reviews in the ledger.
func (f reviewFixture) totalReviewCount(t *testing.T) int {
	t.Helper()
	db, err := sql.Open("sqlite3", filepath.Join(f.ws, "src", "db", "mpm.db"))
	if err != nil {
		t.Fatalf("open the fixture database: %v", err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM memory_reviews`).Scan(&n); err != nil {
		t.Fatalf("read the review ledger: %v", err)
	}
	return n
}

// ── the atomicity ─────────────────────────────────────────────────────────

// TestReviewWeakenIsAtomic_AFailedWeakeningLeavesNoReview is THE test.
//
// The weakening fails. The review must NOT have been recorded: the
// memory must still be in the backlog, and the weight must be exactly
// what it was.
//
// The pre-fix command recorded the review first, so this test fails on
// the pre-fix code with "the review was recorded even though the
// weakening failed" — which is the defect, stated as an assertion.
func TestReviewWeakenIsAtomic_AFailedWeakeningLeavesNoReview(t *testing.T) {
	f := newReviewFixture(t)
	f.seedMemory(t, "candidate", 7, 90, nil)
	f.injectWeakeningFailure(t, "candidate")

	before := f.weightOf(t, "candidate")
	out, code := f.run(t, "ops", "review", "--ack", "candidate", "--decision", "weaken")
	if code == 0 {
		t.Fatalf("the acknowledgment reported success even though the weakening failed:\n%s", out)
	}
	if got := f.reviewCount(t, "candidate"); got != 0 {
		t.Errorf("the review was recorded even though the weakening failed: %d review row(s)", got)
	}
	if after := f.weightOf(t, "candidate"); after != before {
		t.Errorf("the weight changed even though the operation failed: %d -> %d", before, after)
	}
	if ids := f.dueIDs(t); !holdsID(ids, "candidate") {
		t.Errorf("the memory left the backlog even though the operation failed: %v", ids)
	}
}

// TestReviewWeakenIsAtomic_AFailureRecovers pins that a failed
// operation leaves the system able to succeed afterwards.
//
// The failure is removed — the injected trigger is dropped — and the
// same acknowledgment must then work. A half-applied state would make
// the retry fail or report a second review.
func TestReviewWeakenIsAtomic_AFailureRecovers(t *testing.T) {
	f := newReviewFixture(t)
	f.seedMemory(t, "candidate", 7, 90, nil)
	f.injectWeakeningFailure(t, "candidate")

	if _, code := f.run(t, "ops", "review", "--ack", "candidate", "--decision", "weaken"); code == 0 {
		t.Fatalf("the acknowledgment reported success even though the weakening failed")
	}
	f.removeWeakeningFailure(t)

	before := f.weightOf(t, "candidate")
	if _, code := f.run(t, "ops", "review", "--ack", "candidate", "--decision", "weaken"); code != 0 {
		t.Fatalf("the retry after a failed operation did not succeed")
	}
	if got := f.reviewCount(t, "candidate"); got != 1 {
		t.Errorf("the retry did not record exactly one review: %d", got)
	}
	if after := f.weightOf(t, "candidate"); after >= before {
		t.Errorf("the retry did not weaken the memory: %d -> %d", before, after)
	}
}

// TestReviewWeakenIsAtomic_AKeptReviewChangesNothing pins the "keep"
// decision: the review is recorded, and NOTHING else moves.
//
// A "keep" is a decision not to change the memory, so the atomic
// operation must not touch the weight, the reinforcement count or the
// content.
func TestReviewWeakenIsAtomic_AKeptReviewChangesNothing(t *testing.T) {
	f := newReviewFixture(t)
	f.seedMemory(t, "candidate", 7, 90, nil)

	before := f.weightOf(t, "candidate")
	if _, code := f.run(t, "ops", "review", "--ack", "candidate", "--decision", "keep"); code != 0 {
		t.Fatalf("the acknowledgment failed")
	}
	if got := f.reviewCount(t, "candidate"); got != 1 {
		t.Errorf("a `keep` review did not record exactly one review: %d", got)
	}
	if after := f.weightOf(t, "candidate"); after != before {
		t.Errorf("a `keep` review changed the weight: %d -> %d", before, after)
	}
	if ids := f.dueIDs(t); holdsID(ids, "candidate") {
		t.Errorf("a `keep` review did not clear the backlog: %v", ids)
	}
}

// TestReviewWeakenIsAtomic_ASuccessfulWeakeningRecordsBoth pins that
// the successful case records BOTH halves.
//
// The weight change must be the EXISTING one: a weaken of 1 reduces the
// reinforcement count by 1 and the weight by (delta+1)/2 = 1, with a
// floor of 1. A divergent implementation would show up here as a
// different weight.
func TestReviewWeakenIsAtomic_ASuccessfulWeakeningRecordsBoth(t *testing.T) {
	f := newReviewFixture(t)
	f.seedMemory(t, "candidate", 7, 90, nil)

	before := f.weightOf(t, "candidate")
	if _, code := f.run(t, "ops", "review", "--ack", "candidate", "--decision", "weaken"); code != 0 {
		t.Fatalf("the acknowledgment failed")
	}
	if got := f.reviewCount(t, "candidate"); got != 1 {
		t.Errorf("a `weaken` review did not record exactly one review: %d", got)
	}
	after := f.weightOf(t, "candidate")
	if after != before-1 {
		t.Errorf("a `weaken` review did not apply the existing weakening semantics: %d -> %d (expected %d)",
			before, after, before-1)
	}
}

// TestReviewWeakenIsAtomic_ContentionLeavesNothingBehind pins the
// SQLite-contention case.
//
// Another connection holds the write lock, so the operation cannot
// complete. It must fail, and it must leave NOTHING behind: no review,
// no weight change. A gate that reports success while the write was
// refused is the #178 class of defect.
func TestReviewWeakenIsAtomic_ContentionLeavesNothingBehind(t *testing.T) {
	f := newReviewFixture(t)
	f.seedMemory(t, "candidate", 7, 90, nil)

	// Hold the write lock from another connection for longer than the
	// database's busy timeout (5s), so the write is refused rather
	// than merely slow.
	locker, err := sql.Open("sqlite3", filepath.Join(f.ws, "src", "db", "mpm.db"))
	if err != nil {
		t.Fatalf("open the fixture database: %v", err)
	}
	defer locker.Close()
	if _, err := locker.Exec(`BEGIN EXCLUSIVE`); err != nil {
		t.Skipf("this host does not allow an exclusive lock: %v", err)
	}
	defer func() { _, _ = locker.Exec(`ROLLBACK`) }()
	go func() {
		time.Sleep(8 * time.Second)
		_, _ = locker.Exec(`ROLLBACK`)
	}()

	before := f.weightOf(t, "candidate")
	out, code := f.run(t, "ops", "review", "--ack", "candidate", "--decision", "weaken")
	if code == 0 {
		t.Fatalf("the acknowledgment reported success while the database was contended:\n%s", out)
	}
	if got := f.reviewCount(t, "candidate"); got != 0 {
		t.Errorf("a review was recorded even though the write was refused: %d", got)
	}
	if after := f.weightOf(t, "candidate"); after != before {
		t.Errorf("the weight changed even though the write was refused: %d -> %d", before, after)
	}
}

// ── the batch ─────────────────────────────────────────────────────────────

// TestReviewAckAllIsAtomic_AFailureLeavesNothingApplied pins the batch
// contract.
//
// The batch is atomic as a UNIT: the second memory's weakening fails,
// and NOT ONE review may be recorded — not for the first memory, not
// for the second, not for the third.
//
// The pre-fix command recorded each review in its own write and stopped
// at the first error, so the first memory's review was left behind: a
// partially applied batch that the operator was never told about.
func TestReviewAckAllIsAtomic_AFailureLeavesNothingApplied(t *testing.T) {
	f := newReviewFixture(t)
	f.seedMemory(t, "batch-a", 7, 90, nil)
	f.seedMemory(t, "batch-b", 7, 90, nil)
	f.seedMemory(t, "batch-c", 7, 90, nil)
	f.injectWeakeningFailure(t, "batch-b")

	beforeA := f.weightOf(t, "batch-a")
	beforeB := f.weightOf(t, "batch-b")
	beforeC := f.weightOf(t, "batch-c")

	out, code := f.run(t, "ops", "review", "--ack-all", "--decision", "weaken")
	if code == 0 {
		t.Fatalf("the batch reported complete success even though one weakening failed:\n%s", out)
	}
	if got := f.totalReviewCount(t); got != 0 {
		t.Errorf("the batch left %d review row(s) behind after a failure; the batch must be atomic", got)
	}
	for id, before := range map[string]int{"batch-a": beforeA, "batch-b": beforeB, "batch-c": beforeC} {
		if after := f.weightOf(t, id); after != before {
			t.Errorf("%s changed weight even though the batch failed: %d -> %d", id, before, after)
		}
	}
}

// TestReviewAckAllIsAtomic_ASuccessRecordsEveryReview is the other half
// of the batch contract: a successful batch records EVERY review, and
// reports the count it actually applied.
//
// The count in the JSON envelope must match the number of reviews in
// the ledger. A report that says "reviewed 3" while the ledger holds 1
// is the "complete success for partially applied work" defect, in the
// reporting direction.
func TestReviewAckAllIsAtomic_ASuccessRecordsEveryReview(t *testing.T) {
	f := newReviewFixture(t)
	f.seedMemory(t, "batch-a", 7, 90, nil)
	f.seedMemory(t, "batch-b", 7, 90, nil)
	f.seedMemory(t, "batch-c", 7, 90, nil)

	out, code := f.run(t, "ops", "review", "--ack-all", "--decision", "keep", "--json")
	if code != 0 {
		t.Fatalf("the batch failed:\n%s", out)
	}
	if got := f.totalReviewCount(t); got != 3 {
		t.Errorf("a successful batch recorded %d review(s), expected 3", got)
	}
	if !contains(out, `"count":3`) {
		t.Errorf("the report did not state the count it actually applied:\n%s", out)
	}
}

// holdsID reports whether the slice holds the id.
//
// `contains` in this package is a substring shim, so the slice check
// needs its own name.
func holdsID(ids []string, id string) bool {
	for _, got := range ids {
		if got == id {
			return true
		}
	}
	return false
}
