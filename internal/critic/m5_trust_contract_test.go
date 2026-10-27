//go:build m5contract

package critic

// m5_trust_contract_test.go — CONTRACT test for the critic's contribution to
// defect D2.
//
// THESE TESTS FAIL AGAINST TODAY'S PRODUCTION BEHAVIOUR. THAT IS THE POINT.
//
// They assert the behaviour mpm-event/0.1 §3.7 (M5) requires and that the
// critic does not yet provide: neither the adapter nor the critic promotes,
// and trust is always observation.
//
// ── WHY THIS IS NOT IN A GREEN CI GATE ────────────────────────────────────
//
// The `m5contract` build tag excludes this file from every default build.
// `make test`, `make test-race`, `make test-core-precommit` and both CI test
// jobs invoke `go test -tags fts5` with no additional tags, so none of them
// compiles this file.
//
// That exclusion is MECHANICALLY GUARDED by
// internal/core/m5_trust_gate_test.go (always compiled, always green), which
// proves the contract files are absent from the default build, present under
// the tag, and that neither the Makefile nor any CI workflow enables it.
//
// ── HOW TO RUN ────────────────────────────────────────────────────────────
//
//	CGO_CFLAGS=-DSQLITE_ENABLE_FTS5=1 CGO_LDFLAGS=-lm \
//	  go test -tags 'fts5 m5contract' -count=1 -run 'TestM5Contract' -v \
//	  ./internal/critic/...
//
// ── HOW IT IS RETIRED ─────────────────────────────────────────────────────
//
// Session A's adjudication A-1 decides which of two resolutions governs:
//
//   (a) the critic may only write to a PENDING surface — then this test is
//       INVERTED to assert the pending behaviour in the fix's PR; or
//   (b) the critic's existing automatic lesson-writing is recorded as a
//       known pre-existing exception — then this test is DELETED and the
//       contract is amended to say so explicitly.
//
// It is never left failing in a green gate.

import (
	"context"
	"testing"

	mpmcore "github.com/flowbyte-com/mpm-core"
)

// TestM5Contract_CriticFindingIsNotTrustedWithoutReview covers defect D2.
//
// REPRODUCTION (today, green in the characterization suite):
// WeakTheoryHunt emits `mpm_lessons save`. Applying that finding writes a
// lesson that is immediately retrievable through lessons_fts with a full
// confidence value. lessons_base has no status column, so there is nowhere
// to record that the finding has not been reviewed.
//
// EXPECTED CORRECT BEHAVIOUR:
// a critic-authored finding must not be retrievable as trusted knowledge
// until it has been reviewed. It must be pending, or absent.
//
// EXISTING IMPLEMENTATION RESPONSIBLE:
// internal/critic/hunts.go:297-309 (WeakTheoryHunt) and :106-118
// (SurvivalAsymmetryHunt) emit Tool "mpm_lessons", Action "save"; the lesson
// surface is internal/core/db.go:5366 (AddLesson) over a lessons_base table
// with no status column.
//
// MINIMAL PROPOSED CORRECTION: Session A's adjudication A-1. Either the
// critic writes to a pending surface (needs a status column on lessons_base —
// a schema change, and therefore A's), or the behaviour is recorded as an
// accepted exception and this test is deleted.
//
// NEEDS SESSION A'S APPROVAL: YES.
func TestM5Contract_CriticFindingIsNotTrustedWithoutReview(t *testing.T) {
	a, dm, _ := m5NewAudit(t)
	m5SeedWeakTheory(t, dm)

	findings, err := (&WeakTheoryHunt{MaxConfidence: 0.6}).Run(context.Background(), a)
	if err != nil {
		t.Fatalf("WeakTheoryHunt.Run: %v", err)
	}
	if len(findings) == 0 {
		t.Fatalf("expected the weak-theory hunt to emit a finding")
	}
	f := findings[0]

	fact, _ := f.Payload["fact"].(string)
	lessonType, _ := f.Payload["type"].(string)
	tags, _ := f.Payload["tags"].([]string)
	if fact == "" {
		t.Fatalf("critic finding carried no fact: %v", f.Payload)
	}

	// Apply the finding through the real write path.
	if _, err := dm.AddLesson(fact, mpmcore.LessonType(lessonType), tags, ""); err != nil {
		t.Fatalf("AddLesson (applying the critic finding): %v", err)
	}

	// EXPECTED: the critic-authored lesson is NOT trusted yet. Either it
	// carries a pending state, or it is not retrievable at all.
	const marker = "m5contractunreviewedmarker"
	marked := fact + " " + marker + "."
	if _, err := dm.AddLesson(marked, mpmcore.LessonType(lessonType), tags, ""); err != nil {
		t.Fatalf("AddLesson (marked): %v", err)
	}

	var ftsHits int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM lessons_fts WHERE lessons_fts MATCH ?`, marker).
		Scan(&ftsHits); err != nil {
		t.Fatalf("query lessons_fts: %v", err)
	}
	if ftsHits != 0 {
		t.Errorf("expected a critic-authored lesson to NOT be retrievable as "+
			"trusted knowledge without review (defect D2), but lessons_fts "+
			"returned %d hit(s) — the critic wrote into the trusted surface "+
			"with no review step", ftsHits)
	}
}
