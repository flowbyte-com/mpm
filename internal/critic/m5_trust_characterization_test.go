package critic

// m5_trust_characterization_test.go — CHARACTERIZATION tests for the critic's
// contribution to the M5 trust defects.
//
// WHAT THIS FILE IS.
//
// These tests assert the CURRENT behaviour of the critic, including the
// defective behaviour. They PASS today. They pin the behaviour so that a
// later fix is a visible, deliberate change.
//
// The M5-compliant counterparts live in m5_trust_contract_test.go, which is
// gated behind the `m5contract` build tag and therefore FAILS today.
//
// DEFECT D2 — the critic creates TRUSTED lessons without explicit review.
//
// Two of the four hunts — SurvivalAsymmetryHunt (hunts.go:106-118) and
// WeakTheoryHunt (hunts.go:297-309) — emit `mpm_lessons save` findings on
// every cycle. A lesson is trusted knowledge in MPM: it lands in
// lessons_base, is indexed in lessons_fts, is served by the wake-context
// payload and is returned by retrieval. The findings carry no evidence and
// no review step, and lessons_base has no status column to record one.
//
// The core-side half of the same defect (the missing pending state on the
// lesson surface) is pinned in
// internal/core/m5_trust_characterization_test.go.
//
// DEFECT D4 — the critic's cycle marker defeats proposal idempotency.
//
// PoisonPillHunt tags its theory with cycle_N (hunts.go:373-378). Because
// the proposal identity hash includes json(tags)
// (internal/core/db.go:6269-6287), every fifth cycle produces a genuinely
// distinct proposal rather than a re-observation of the same one.
//
// ISOLATION.
//
// HOME, MPM_WORKSPACE and MPM_DB_PATH are pinned to disposable locations and
// the database is a disposable file-backed database at the canonical
// <workspace>/src/db/mpm.db path. The DB-safety AST guard
// (internal/core/test_db_safety_test.go) flags only an EMPTY string literal
// as the first argument to NewDatabaseManager — the identifier passed here
// is not an empty literal, so the guard correctly does not flag it and no
// whitelist entry is needed.
//
// NOTE ON THE SCHEMA. These tests deliberately use the REAL core schema
// (via mpmcore.NewDatabaseManager) rather than the minimal criticTestSchema
// fixture in critic_test.go, because the defect is a property of the real
// lesson surface — its columns, its FTS index and its confidence — and the
// minimal fixture has no lessons table at all.

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	mpmcore "github.com/flowbyte-com/mpm-core"
)

// m5IsolatedWorkspace pins HOME, MPM_WORKSPACE and MPM_DB_PATH to disposable
// locations and returns the disposable workspace root.
func m5IsolatedWorkspace(t *testing.T) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	ws := t.TempDir()
	t.Setenv("MPM_WORKSPACE", ws)
	t.Setenv("MPM_DB_PATH", filepath.Join(ws, "src", "db", "mpm.db"))
	return ws
}

// m5NewAudit opens a disposable core DatabaseManager and returns an Audit
// bound to it, with the real core schema installed.
func m5NewAudit(t *testing.T) (*Audit, *mpmcore.DatabaseManager, *fakeCLI) {
	t.Helper()
	ws := m5IsolatedWorkspace(t)
	dm, err := mpmcore.NewDatabaseManager(ws)
	if err != nil {
		t.Fatalf("m5NewAudit: NewDatabaseManager(%q): %v", ws, err)
	}
	t.Cleanup(func() { _ = dm.Close() })

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	cli := &fakeCLI{}
	a := &Audit{
		db:    dm.SQLDB(),
		log:   log,
		cycle: 7,
		cli:   cli,
		hunts: []Hunt{
			&SurvivalAsymmetryHunt{},
			&StaleMemoryHunt{MaxAge: 30 * 24 * 1e9},
			&WeakTheoryHunt{MaxConfidence: 0.6},
		},
	}
	return a, dm, cli
}

// m5SeedWeakTheory creates a pending theory with confidence 0.5, which is
// below WeakTheoryHunt's 0.6 threshold and therefore triggers the hunt.
func m5SeedWeakTheory(t *testing.T, dm *mpmcore.DatabaseManager) string {
	t.Helper()
	res, err := dm.ProposeTheory(
		"A pending hypothesis the critic should notice.",
		"Demonstrate the claim.",
		nil, nil, []string{"m5", "probe"},
	)
	if err != nil {
		t.Fatalf("ProposeTheory: %v", err)
	}
	id, _ := res["id"].(string)
	if id == "" {
		t.Fatalf("ProposeTheory returned no id: %v", res)
	}
	return id
}

// TestM5Characterization_CriticEmitsTrustedLessonWithoutReview pins the
// critic half of defect D2.
//
// REPRODUCTION (today):
// a WeakTheoryHunt over a pending theory emits a finding whose target is
// `mpm_lessons save` — the trusted lesson surface. The finding carries no
// evidence, no citation and no review step; it is a fact, a type and tags.
//
// Applying that finding to the real core schema writes a lesson that is
// immediately retrievable through lessons_fts with a full confidence value.
// There is no pending state to mark, because lessons_base has no status
// column.
//
// WHY IT MATTERS. mpm-event/0.1 §3.7 (M5) requires that neither the adapter
// nor the critic promotes, and that trust is always observation. The critic
// writes trusted knowledge today, with no review, on every cycle.
//
// EXISTING IMPLEMENTATION RESPONSIBLE:
// internal/critic/hunts.go:297-309 (WeakTheoryHunt) and :106-118
// (SurvivalAsymmetryHunt) emit Tool "mpm_lessons", Action "save". The
// lesson surface itself is internal/core/db.go:5366 (AddLesson) over a
// lessons_base table with no status column.
//
// MINIMAL PROPOSED CORRECTION: Session A's adjudication A-1 — either the
// critic may only write to a PENDING surface (which needs a status column on
// lessons_base, a schema change), or A records the behaviour as a known
// pre-existing exception and the contract is amended to say so.
//
// NEEDS SESSION A'S APPROVAL: YES — this is the M5 tension referred to A.
func TestM5Characterization_CriticEmitsTrustedLessonWithoutReview(t *testing.T) {
	a, dm, _ := m5NewAudit(t)
	m5SeedWeakTheory(t, dm)

	findings, err := (&WeakTheoryHunt{MaxConfidence: 0.6}).Run(context.Background(), a)
	if err != nil {
		t.Fatalf("WeakTheoryHunt.Run: %v", err)
	}
	if len(findings) == 0 {
		t.Fatalf("expected the weak-theory hunt to emit a finding — " +
			"the characterization of defect D2 is stale")
	}

	f := findings[0]

	// CURRENT BEHAVIOUR: the finding targets the TRUSTED lesson surface.
	if f.Tool != "mpm_lessons" || f.Action != "save" {
		t.Errorf("expected the critic to target mpm_lessons/save, got %s/%s — "+
			"the characterization of defect D2 is stale", f.Tool, f.Action)
	}

	// CURRENT BEHAVIOUR: the finding carries no evidence and no review
	// step. Its payload is a fact, a type and tags — nothing else.
	for _, forbidden := range []string{
		"evidence", "evidence_ids", "source_ids", "review",
		"review_status", "status", "confidence",
	} {
		if _, present := f.Payload[forbidden]; present {
			t.Errorf("expected the critic finding to carry NO %q field — "+
				"the characterization of defect D2 is stale", forbidden)
		}
	}

	// CURRENT BEHAVIOUR: applying the finding writes an IMMEDIATELY
	// TRUSTED lesson — retrievable through the FTS index that backs
	// retrieval, with a full confidence value.
	fact, _ := f.Payload["fact"].(string)
	lessonType, _ := f.Payload["type"].(string)
	tags, _ := f.Payload["tags"].([]string)
	if fact == "" {
		t.Fatalf("critic finding carried no fact: %v", f.Payload)
	}
	if _, err := dm.AddLesson(fact, mpmcore.LessonType(lessonType), tags, ""); err != nil {
		t.Fatalf("AddLesson (applying the critic finding): %v", err)
	}

	// The marker token is a single alphanumeric word on purpose:
	// lessons_fts is an external-content FTS5 table over the `lessons`
	// view with columns (content, tags), and an unquoted multi-word MATCH
	// term is parsed as a column filter and fails with "no such column".
	const marker = "m5criticunreviewedmarker"
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
	if ftsHits != 1 {
		t.Errorf("expected the critic-authored lesson to be immediately "+
			"retrievable through lessons_fts (1 hit), got %d — the "+
			"characterization of defect D2 is stale", ftsHits)
	}

	var baseRows int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM lessons_base WHERE content LIKE ?`, "%"+marker+"%").
		Scan(&baseRows); err != nil {
		t.Fatalf("query lessons_base: %v", err)
	}
	if baseRows != 1 {
		t.Errorf("expected exactly 1 critic-authored lesson row, got %d — "+
			"the characterization of defect D2 is stale", baseRows)
	}
}

// TestM5Characterization_CycleMarkerDefeatsProposalIdentity pins the critic
// half of defect D4.
//
// INVERTED 2026-10-27 by Session B's P3. This test previously asserted
// that the poison pill's cycle marker DID defeat the proposal identity.
// That was the defect, and P3 fixed it: the cycle marker is gone from the
// poison pill's hypothesis and its tags, so the same target now yields the
// SAME proposal in every cycle.
//
// WHY THE CHARACTERIZATION WAS INVERTED RATHER THAN DELETED: the
// two-file discipline from PR #117 — a characterization test pins the
// CURRENT behaviour and is inverted by the fix's PR; the matching contract
// test (behind the m5contract build tag) is deleted by the fix's PR.
//
// WHAT REMAINS OPEN: the SUBSTRATE half of D4 is untouched. A caller that
// explicitly proposes with a cycle-varying tag still gets two proposals,
// because the tags are part of the identity hash. That property is pinned
// by internal/core/m5_trust_characterization_test.go
// (TestM5Characterization_CycleTagBreaksProposalIdempotency) and its
// contract twin TestM5Contract_CycleVaryingTagDoesNotCreateSecondProposal
// still fails by design. Closing it would mean changing the identity hash
// itself, which touches shared dedup semantics and needs Session A's
// arbitration.
//
// MINIMAL PROPOSED CORRECTION (applied): keep the cycle marker out of the
// proposal's identity-bearing fields. It now lives in the finding's Reason,
// which is logged and never persisted. Note that the audit's own suggestion
// of carrying it in the validation_criteria would NOT have worked:
// validation_criteria is part of the proposal's metadata and therefore part
// of the identity hash. internal/core/p3_identity_test.go pins that.
func TestM5Characterization_CycleMarkerDefeatsProposalIdentity(t *testing.T) {
	a, dm, _ := m5NewAudit(t)

	// Seed a high-confidence memory for the pill to attack. The pill
	// targets the highest-confidence row in collection 'memories' with
	// confidence >= 0.8; a fresh memory is created at exactly 0.8.
	if _, err := dm.SaveMemoryNodeForInvocation(
		"memories",
		"A high-confidence memory the poison pill will target.",
		"", []string{"m5", "target"}, nil, nil,
		false, 1.0, "", "", "", "",
		mpmcore.ActiveContext{},
	); err != nil {
		t.Fatalf("SaveMemoryNodeForInvocation: %v", err)
	}

	first, err := (&PoisonPillHunt{Cycle: 5}).Run(context.Background(), a)
	if err != nil {
		t.Fatalf("PoisonPillHunt cycle 5: %v", err)
	}
	if len(first) == 0 {
		t.Skip("no eligible poison-pill target in this workspace; " +
			"the characterization is conditional on a high-confidence memory")
	}

	second, err := (&PoisonPillHunt{Cycle: 10}).Run(context.Background(), a)
	if err != nil {
		t.Fatalf("PoisonPillHunt cycle 10: %v", err)
	}
	if len(second) == 0 {
		t.Skip("no eligible poison-pill target in this workspace")
	}

	firstTags, _ := first[0].Payload["tags"].([]string)
	secondTags, _ := second[0].Payload["tags"].([]string)

	// CURRENT BEHAVIOUR (after P3): the cycle marker is GONE, so the two
	// cycles produce the SAME proposal for the same target.
	for _, tag := range firstTags {
		if m5TagsContain([]string{tag}, "cycle_5") {
			t.Errorf("expected the cycle-5 finding NOT to carry a cycle marker, got %v",
				firstTags)
		}
	}
	if m5TagsContain(secondTags, "cycle_10") {
		t.Errorf("expected the cycle-10 finding NOT to carry a cycle marker, got %v",
			secondTags)
	}
	if m5TagsWithout(firstTags, "cycle_5") != m5TagsWithout(secondTags, "cycle_10") {
		t.Errorf("expected the two findings to be identical once the cycle "+
			"marker is gone, got %v vs %v", firstTags, secondTags)
	}
	firstHyp, _ := first[0].Payload["hypothesis"].(string)
	secondHyp, _ := second[0].Payload["hypothesis"].(string)
	if firstHyp != secondHyp {
		t.Errorf("expected the two findings to carry the same hypothesis once "+
			"the cycle marker is gone, got %q vs %q", firstHyp, secondHyp)
	}
}

// m5TagsContain reports whether the tag list holds the given tag.
func m5TagsContain(tags []string, want string) bool {
	for _, t := range tags {
		if t == want {
			return true
		}
	}
	return false
}

// m5TagsWithout returns the tag list with the given tag removed.
func m5TagsWithout(tags []string, drop string) string {
	out := ""
	for i, t := range tags {
		if t == drop {
			continue
		}
		if i > 0 {
			out += ","
		}
		out += t
	}
	return out
}
