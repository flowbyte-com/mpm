// doctor_bootstrap_directives_test.go — HAL-9001 Finding 5.
//
// A clean install's Doctor report contradicted itself:
//
//	✓  Stored embeddings      6 memories, all provider-generated
//	⚠  Embedding integrity    6 live memories have no vector; none are stored
//	⚠  Review backlog         6 memories due for spaced review (>5 considered backlog)
//
// Three separate claims were wrong:
//
//  1. The coverage row counted the `embedding_source` LABEL, whose
//     column default is 'provider'. A row holding no vector at all was
//     therefore counted as provider-covered — the coverage row was the
//     one lying, not the integrity row.
//
//  2. The five baseline bootstrap directives were counted as memories
//     with missing vectors. A directive is standing policy read at wake
//     time, never a vector-search candidate, so its absent vector is an
//     expected policy choice and not an indexing gap.
//
//  3. The same five directives were counted as overdue spaced-review
//     work. Their `last_accessed_at` is never written (the directive
//     read path does not touch it), so that backlog could never be
//     cleared — clearing it would mean "reviewing" the bootstrap policy.
//
// These tests pin the corrected behaviour: the coverage row measures
// the stored body, directive rows are DISCLOSED rather than counted as
// gaps, and a genuine review candidate stays visible.
package main

import (
	"fmt"
	"strings"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// insertBootstrapRow inserts a row into the memories table directly.
//
// The insert deliberately mirrors the production path: `embedding` is
// left NULL and `embedding_source` is left at its column default
// ('provider'), which is exactly the state that made the coverage row
// claim provider coverage for a row that holds no vector.
func insertBootstrapRow(t *testing.T, dm *mpminternal.DatabaseManager, id, collection string, primeDirective int, weight int) {
	t.Helper()
	insertBootstrapRowAged(t, dm, id, collection, primeDirective, weight, 0)
}

// insertBootstrapRowAged inserts a bootstrap row with an explicit
// creation time.
//
// ageDays > 0 backdates the row's created_at by that many days. It
// exists because the review rule changed (2026-10-30): a memory's
// review clock is COALESCE(reviewed_at, last_accessed_at, created_at),
// so a memory created NOW and never accessed is NOT due for review —
// it is brand new. Under the previous rule every never-accessed memory
// was due immediately, which is how a just-created memory could be
// "30 days stale".
//
// A fixture that wants a memory to be DUE has to make it old.
func insertBootstrapRowAged(t *testing.T, dm *mpminternal.DatabaseManager, id, collection string, primeDirective int, weight int, ageDays int) {
	t.Helper()
	createdAt := fmt.Sprintf("CAST(strftime('%%s','now','-%d days') AS INTEGER)", ageDays)
	_, err := dm.SQLDB().Exec(
		`INSERT INTO memories
			(id, collection, content, embedding, embedding_source, weight,
			 is_long_term, is_prime_directive, reinforcement_count, created_at, updated_at)
		 VALUES (?, ?, ?, NULL, 'provider', ?, 0, ?, 0, `+createdAt+`, `+createdAt+`)`,
		id, collection, "bootstrap row "+id, weight, primeDirective)
	if err != nil {
		t.Fatalf("insert %q: %v", id, err)
	}
}

// TestDoctorStoredEmbeddings_DoesNotClaimCoverageForVectorlessRows pins
// the correction of the coverage row.
//
// A memory that holds no vector must not be reported as
// "provider-generated". The `embedding_source` label defaults to
// 'provider' and is not set on the ordinary insert path, so counting
// the label counted vector-less rows as covered.
func TestDoctorStoredEmbeddings_DoesNotClaimCoverageForVectorlessRows(t *testing.T) {
	dm := newTestDMForCmd(t)
	svc := NewDoctorService(dm)

	insertBootstrapRow(t, dm, "memory-no-vector", "memories", 0, 5)

	check := svc.checkEmbeddings()
	if strings.Contains(check.Message, "all provider-generated") {
		t.Errorf("the coverage row claims provider coverage for a memory that holds no "+
			"vector; message: %q", check.Message)
	}
	if check.Status != "WARN" {
		t.Errorf("a memory with no vector must be reported as a shortfall; status = %q, "+
			"want WARN (message: %q)", check.Status, check.Message)
	}
	if !strings.Contains(check.Message, "without embedding") {
		t.Errorf("the coverage row does not report the missing vector; message: %q", check.Message)
	}
}

// TestDoctorStoredEmbeddings_CountsRealVectors pins the positive case:
// a memory that DOES hold a vector is reported as covered.
func TestDoctorStoredEmbeddings_CountsRealVectors(t *testing.T) {
	dm := newTestDMForCmd(t)
	svc := NewDoctorService(dm)

	_, err := dm.SQLDB().Exec(
		`INSERT INTO memories
			(id, collection, content, embedding, embedding_source, weight,
			 is_long_term, is_prime_directive, reinforcement_count, created_at, updated_at)
		 VALUES ('memory-with-vector', 'memories', 'a vectorised memory', ?, 'provider', 5,
		        0, 0, 0, CAST(strftime('%s','now') AS INTEGER), CAST(strftime('%s','now') AS INTEGER))`,
		"[0.1,0.2,0.3]")
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	check := svc.checkEmbeddings()
	if check.Status != "PASS" {
		t.Errorf("a memory holding a vector must pass the coverage row; status = %q (message: %q)",
			check.Status, check.Message)
	}
	if !strings.Contains(check.Message, "all provider-generated") {
		t.Errorf("a memory holding a provider vector must be reported as provider-generated; message: %q",
			check.Message)
	}
}

// TestDoctorStoredEmbeddings_DirectivesAreDisclosedNotCounted pins the
// directive handling on the coverage row.
//
// A directive row must be DISCLOSED (the operator can see why the
// database holds rows without vectors) and must not be counted as an
// ordinary memory — its missing vector is a policy choice, not a gap.
func TestDoctorStoredEmbeddings_DirectivesAreDisclosedNotCounted(t *testing.T) {
	dm := newTestDMForCmd(t)
	svc := NewDoctorService(dm)

	insertBootstrapRow(t, dm, "directive-1", "directives", 1, 10)

	check := svc.checkEmbeddings()

	disclosed := strings.Join(check.Details, " ")
	if !strings.Contains(disclosed, "1 bootstrap directive") {
		t.Errorf("the coverage row does not disclose the directive row; details: %q", disclosed)
	}
	if strings.Contains(check.Message, "all provider-generated") {
		t.Errorf("the coverage row claims provider coverage for a directive that holds no "+
			"vector; message: %q", check.Message)
	}
	if !strings.Contains(check.Message, "directive") {
		t.Errorf("the coverage row does not mention the directive population; message: %q", check.Message)
	}
}

// TestDoctorEmbeddingIntegrity_DirectiveRowsAreNotCountedAsGaps pins
// the directive split on the integrity row.
//
// A directive's absent vector is disclosed but not counted as a
// missing vector, and a genuine gap in an ordinary memory still WARNs.
func TestDoctorEmbeddingIntegrity_DirectiveRowsAreNotCountedAsGaps(t *testing.T) {
	dm := newTestDMForCmd(t)
	svc := NewDoctorService(dm)

	insertBootstrapRow(t, dm, "directive-1", "directives", 1, 10)
	insertBootstrapRow(t, dm, "memory-1", "memories", 0, 5)

	check := svc.checkEmbeddingIntegrity()

	if !strings.Contains(check.Message, "1 live memories have no vector") {
		t.Errorf("the integrity row counted the directive as a gap; message: %q", check.Message)
	}
	if strings.Contains(check.Message, "2 live memories have no vector") {
		t.Errorf("the integrity row counted the directive as a gap; message: %q", check.Message)
	}
	if check.Status != "WARN" {
		t.Errorf("a genuine gap must still WARN; status = %q (message: %q)", check.Status, check.Message)
	}

	disclosed := strings.Join(check.Details, " ")
	if !strings.Contains(disclosed, "1 bootstrap directive") {
		t.Errorf("the integrity row does not disclose the directive rows; details: %q", disclosed)
	}
}

// TestDoctorEmbeddingIntegrity_DirectiveRowAloneIsNotAGap pins the
// clean-install case: with only directives present, the substrate is
// not carrying an indexing gap.
//
// The vector statistics are empty, so the row is informational — the
// directives are disclosed in the details instead.
func TestDoctorEmbeddingIntegrity_DirectiveRowAloneIsNotAGap(t *testing.T) {
	dm := newTestDMForCmd(t)
	svc := NewDoctorService(dm)

	insertBootstrapRow(t, dm, "directive-1", "directives", 1, 10)

	check := svc.checkEmbeddingIntegrity()
	if check.Status == "WARN" || check.Status == "FAIL" {
		t.Errorf("directives alone must not report an indexing gap; status = %q (message: %q)",
			check.Status, check.Message)
	}
	disclosed := strings.Join(check.Details, " ")
	if !strings.Contains(disclosed, "1 bootstrap directive") {
		t.Errorf("the integrity row does not disclose the directive rows; details: %q", disclosed)
	}
}

// TestDoctorReviewBacklog_ExcludesDirectivesKeepsRealCandidates pins
// the review-queue correction.
//
// The directive must not be offered as spaced-review work, and the
// genuine high-weight memory must still be offered — the fix excludes
// directives, it does not silence the queue.
func TestDoctorReviewBacklog_ExcludesDirectivesKeepsRealCandidates(t *testing.T) {
	dm := newTestDMForCmd(t)
	svc := NewDoctorService(dm)

	// Both rows are backdated: the directive is excluded from the
	// review queue, and the ordinary memory is old enough to be due.
	// A memory created NOW is NOT due — see insertBootstrapRowAged.
	insertBootstrapRowAged(t, dm, "directive-1", "directives", 1, 10, 90)
	insertBootstrapRowAged(t, dm, "memory-1", "memories", 0, 10, 90)

	items, err := dm.MemoriesDueForReview(14, 100)
	if err != nil {
		t.Fatalf("MemoriesDueForReview: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("the review queue holds %d candidates, want 1 (the ordinary memory only); candidates: %v",
			len(items), items)
	}
	if items[0].ID != "memory-1" {
		t.Errorf("the review candidate is %q, want the ordinary memory \"memory-1\"", items[0].ID)
	}

	check := svc.checkReviewBacklog()
	if !strings.Contains(check.Message, "1 memories due for spaced review") {
		t.Errorf("the review backlog must count the genuine candidate only; message: %q", check.Message)
	}
}

// TestDoctorReviewBacklog_DirectivesOnlyIsNotABacklog pins the
// clean-install case: a freshly bootstrapped store must not report a
// spaced-review backlog that can never be cleared.
func TestDoctorReviewBacklog_DirectivesOnlyIsNotABacklog(t *testing.T) {
	dm := newTestDMForCmd(t)
	svc := NewDoctorService(dm)

	insertBootstrapRow(t, dm, "directive-1", "directives", 1, 10)

	check := svc.checkReviewBacklog()
	if check.Status != "PASS" {
		t.Errorf("a bootstrap directive must not create a review backlog; status = %q (message: %q)",
			check.Status, check.Message)
	}
	if !strings.Contains(check.Message, "no memories due for review") {
		t.Errorf("the review backlog row does not report an empty queue; message: %q", check.Message)
	}
}
