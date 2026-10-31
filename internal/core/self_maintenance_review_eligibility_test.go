// self_maintenance_review_eligibility_test.go — the self-maintenance
// path must use the CANONICAL review eligibility and the TOTAL.
//
// WHAT THIS FILE PINS
// -------------------
// `RunSelfMaintenance()` reported the review backlog as
//
//	forReview, err := s.SpacedReinforcementReview(14, 100)
//	if err == nil {
//		stats.ForReview = len(forReview)
//	}
//
// and that was wrong in three ways at once:
//
//   - the OLD eligibility rule. `SpacedReinforcementReview` treats a
//     never-accessed memory as due IMMEDIATELY, and it ignores the
//     `memory_reviews` ledger entirely — so a memory the operator had
//     already reviewed was still counted. The canonical rule (see
//     review.go) uses the REVIEW CLOCK
//     COALESCE(reviewed_at, last_accessed_at, created_at), so a
//     brand-new memory is not due, a genuinely old one is, and an
//     operator's recorded review clears it.
//   - the CAP of 100. With more than 100 memories due, the number
//     reported was the length of a SAMPLE, not the backlog.
//   - a second copy of the review window, hard-coded to 14.
//
// The maintenance path is not an operator-facing surface in the same way
// the doctor is, but it REPORTS the backlog — and a number that
// disagrees with the doctor's is the same defect the doctor's count
// was.
//
// THE FIXTURE HAS TO SURVIVE THE OTHER MAINTENANCE STEPS.
//
// `RunSelfMaintenance` decays weights, prunes and consolidates BEFORE
// it counts, so a fixture whose memories are pruned by those steps
// would measure survival instead of eligibility. The seeds below are
// therefore built to survive:
//
//   - the due candidates are LONG-TERM (`is_long_term = 1`), which the
//     decay preserves and the never-accessed prune exempts;
//   - the candidates carry DISTINCT content, so the consolidation pass
//     cannot merge them into one.
//
// Every test runs against an isolated in-memory database. The live
// installation is never touched.
package internal

import (
	"fmt"
	"testing"
)

// seedReviewCandidate inserts a memory with an explicit age, weight and
// durability.
//
// ageDays > 0 backdates created_at by that many days; ageDays == 0
// means "created now". lastAccessedDays < 0 means "never accessed";
// otherwise last_accessed_at is backdated by that many days.
//
// longTerm marks the memory durable, which is what keeps the decay and
// the never-accessed prune away from it — so the test measures
// ELIGIBILITY rather than survival.
func seedReviewCandidate(t *testing.T, dm *DatabaseManager, id string, weight int, ageDays int, lastAccessedDays int, longTerm bool) {
	t.Helper()
	created := "CAST(strftime('%s','now') AS INTEGER)"
	if ageDays > 0 {
		created = fmt.Sprintf("CAST(strftime('%%s','now','-%d days') AS INTEGER)", ageDays)
	}
	var lastAccessed interface{}
	if lastAccessedDays >= 0 {
		if lastAccessedDays == 0 {
			lastAccessed = "CAST(strftime('%s','now') AS INTEGER)"
		} else {
			lastAccessed = fmt.Sprintf("CAST(strftime('%%s','now','-%d days') AS INTEGER)", lastAccessedDays)
		}
	}
	lt := 0
	if longTerm {
		lt = 1
	}
	_, err := dm.ExecTracked(`INSERT INTO memories
		(id, collection, content, tags, weight, is_long_term, is_prime_directive,
		 reinforcement_count, created_at, updated_at, last_accessed_at)
		VALUES (?, 'memories', ?, '[]', ?, ?, 0, 0, `+created+`, `+created+`, ?)`,
		0, id, "a seeded review candidate "+id, weight, lt, lastAccessed)
	if err != nil {
		t.Fatalf("seed the review candidate %q: %v", id, err)
	}
}

// seedDirective inserts a prime directive: standing policy, never a
// spaced-review candidate.
func seedDirective(t *testing.T, dm *DatabaseManager, id string, ageDays int) {
	t.Helper()
	created := "CAST(strftime('%s','now') AS INTEGER)"
	if ageDays > 0 {
		created = fmt.Sprintf("CAST(strftime('%%s','now','-%d days') AS INTEGER)", ageDays)
	}
	_, err := dm.ExecTracked(`INSERT INTO memories
		(id, collection, content, tags, weight, is_long_term, is_prime_directive,
		 reinforcement_count, created_at, updated_at)
		VALUES (?, 'directives', ?, '[]', 10, 0, 1, 0, `+created+`, `+created+`)`,
		0, id, "a seeded directive "+id)
	if err != nil {
		t.Fatalf("seed the directive %q: %v", id, err)
	}
}

// maintenanceForReview runs the self-maintenance path and returns the
// review backlog it reported.
func maintenanceForReview(t *testing.T, dm *DatabaseManager) int {
	t.Helper()
	stats, err := dm.RunSelfMaintenance()
	if err != nil {
		t.Fatalf("RunSelfMaintenance: %v", err)
	}
	count, ok := stats["for_spaced_reinforcement"].(int)
	if !ok {
		t.Fatalf("the maintenance stats carry no review backlog count: %#v", stats)
	}
	return count
}

// TestSelfMaintenanceReviewCount_MatchesTheCanonicalRule pins the
// agreement, case by case.
//
// Each case below is a case the OLD rule and the canonical rule
// DISAGREE on:
//
//   - a never-accessed memory created NOW is NOT due. The old rule
//     treated every never-accessed memory as due immediately.
//   - a genuinely OLD memory IS due.
//   - a REVIEWED memory is not due. The old rule ignored the ledger.
//   - a directive is never due.
//
// If the maintenance path used the old rule, the count would differ
// from the canonical count, and the test says so.
func TestSelfMaintenanceReviewCount_MatchesTheCanonicalRule(t *testing.T) {
	dm := newTestDM(t)

	// A never-accessed memory created NOW: not due.
	seedReviewCandidate(t, dm, "brand-new", 8, 0, -1, false)
	// A genuinely OLD memory, never accessed: due. It is long-term so
	// the maintenance's own prune does not take it before the count.
	seedReviewCandidate(t, dm, "genuinely-old", 8, 90, -1, true)
	// A directive: never a review candidate, however old.
	seedDirective(t, dm, "a-directive", 90)
	// A low-weight, non-long-term memory: not worth reviewing. It was
	// accessed long ago, so the never-accessed prune leaves it alone.
	seedReviewCandidate(t, dm, "lightweight", 2, 90, 90, false)

	maintained := maintenanceForReview(t, dm)

	canonical, err := dm.CountMemoriesDueForReview(ReviewDueAfterDays)
	if err != nil {
		t.Fatalf("the canonical count: %v", err)
	}
	if maintained != canonical {
		t.Errorf("the maintenance path reported %d memories due for review; the canonical rule says %d. "+
			"Two definitions of review eligibility is the defect this test exists for.",
			maintained, canonical)
	}
	if maintained != 1 {
		t.Errorf("the maintenance path reported %d; expected 1 (the genuinely old memory)", maintained)
	}
}

// TestSelfMaintenanceReviewCount_AReviewedMemoryIsNotDue pins the
// reviewed case.
//
// The operator reviewed the memory, so it left the backlog. The OLD
// rule ignored the `memory_reviews` ledger, so it would still count
// the memory.
func TestSelfMaintenanceReviewCount_AReviewedMemoryIsNotDue(t *testing.T) {
	dm := newTestDM(t)
	seedReviewCandidate(t, dm, "already-reviewed", 8, 90, -1, true)

	if err := dm.RecordMemoryReview("already-reviewed", ReviewDecisionKeep, ""); err != nil {
		t.Fatalf("record the review: %v", err)
	}

	maintained := maintenanceForReview(t, dm)
	canonical, err := dm.CountMemoriesDueForReview(ReviewDueAfterDays)
	if err != nil {
		t.Fatalf("the canonical count: %v", err)
	}
	if canonical != 0 {
		t.Fatalf("a reviewed memory must leave the backlog; the canonical count says %d", canonical)
	}
	if maintained != 0 {
		t.Errorf("the maintenance path counted a REVIEWED memory: %d; the canonical rule says 0", maintained)
	}
}

// TestSelfMaintenanceReviewCount_IsTheTotalNotASample pins the
// sample-vs-total defect.
//
// 150 due memories: the OLD path took the first 100 and reported 100.
// The maintenance path must report 150 — the backlog, not a slice of
// it.
func TestSelfMaintenanceReviewCount_IsTheTotalNotASample(t *testing.T) {
	dm := newTestDM(t)
	const due = 150
	for i := 0; i < due; i++ {
		seedReviewCandidate(t, dm, fmt.Sprintf("due-%03d", i), 8, 90, -1, true)
	}

	maintained := maintenanceForReview(t, dm)
	canonical, err := dm.CountMemoriesDueForReview(ReviewDueAfterDays)
	if err != nil {
		t.Fatalf("the canonical count: %v", err)
	}
	if canonical != due {
		t.Fatalf("the canonical count says %d, expected %d", canonical, due)
	}
	if maintained != due {
		t.Errorf("the maintenance path reported %d memories due for review; the backlog is %d. "+
			"A capped sample reported as the total is the defect this test exists for.",
			maintained, due)
	}
	if maintained == 100 {
		t.Errorf("the maintenance path reported the OLD sample cap (100), not the backlog")
	}
}

// TestSelfMaintenanceReviewCount_AgreesWithTheDoctor pins that the
// maintenance path and the operator-facing surfaces cannot disagree.
//
// The doctor's backlog check and the dashboard's readiness row both
// use the canonical count. The maintenance path must report the same
// number, or the operator sees two different backlogs.
func TestSelfMaintenanceReviewCount_AgreesWithTheDoctor(t *testing.T) {
	dm := newTestDM(t)
	seedReviewCandidate(t, dm, "doctor-a", 8, 90, -1, true)
	seedReviewCandidate(t, dm, "doctor-b", 8, 90, -1, true)
	seedReviewCandidate(t, dm, "doctor-c", 8, 91, -1, true)

	maintained := maintenanceForReview(t, dm)
	canonical, err := dm.CountMemoriesDueForReview(ReviewDueAfterDays)
	if err != nil {
		t.Fatalf("the canonical count: %v", err)
	}
	if canonical != 3 {
		t.Fatalf("the canonical count says %d, expected 3", canonical)
	}
	if maintained != canonical {
		t.Errorf("the maintenance path reported %d; the canonical backlog is %d", maintained, canonical)
	}
}
