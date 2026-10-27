package internal

// m5_trust_characterization_test.go — CHARACTERIZATION tests for the five
// trust defects demonstrated in docs/dev/session-b-audit.md.
//
// WHAT THIS FILE IS.
//
// These tests assert the CURRENT behaviour of the substrate, including the
// defective behaviour. They PASS today. They are the standard
// characterization-test contract: before a defect is fixed, the current
// behaviour is pinned so the fix is a visible, deliberate change rather than
// a silent behaviour shift.
//
// When Session A adjudicates a defect and the fix lands, the corresponding
// characterization test is INVERTED (or deleted where the defect is accepted
// as-is). The inversion is part of the fix's PR, not an afterthought.
//
// RETIRED. D1 (duplicate evidence inflates confidence) and D5 (repeated
// delivery counts as independent corroboration) were REPAIRED on the
// feat/session-b-d1-evidence-identity branch: the observation-identity
// mechanism in internal/core/evidence_identity.go gives an evidence row a
// durable observation identity, and the evidence table's PRIMARY KEY enforces
// "at most one stored row per observation per artifact". Their M5-COMPLIANT
// counterparts are ordinary regression tests now, in
// internal/core/evidence_identity_test.go — they run in every green gate
// instead of behind the `m5contract` tag. The D2, D3 and D4
// characterizations below still pin defects that are open.
//
// THE M5-COMPLIANT COUNTERPARTS live in m5_trust_contract_test.go, which is
// gated behind the `m5contract` build tag and therefore FAILS today. They are
// deliberately NOT part of any green CI gate — see m5_trust_gate_test.go, which
// mechanically proves the gate cannot silently absorb them.
//
// ISOLATION.
//
// Every test pins HOME, MPM_WORKSPACE and MPM_DB_PATH to disposable
// locations (docs/dev/dogfooding-runbook.md §5) and opens a disposable
// file-backed database at the canonical <workspace>/src/db/mpm.db path, so
// the production path resolution is exercised rather than bypassed. Nothing
// here can reach the operator's live database.
//
// WHY THESE DEFECTS MATTER.
//
// mpm-event/0.1 §3.7 (M5) requires: trust is always observation; capturing an
// observation creates no memory/lesson/decision row; a proposal must cite
// resolvable evidence, and an unresolvable citation is rejected; and neither
// the adapter nor the critic promotes. The substrate does not yet honour
// those clauses. These tests make the gap measurable.

import (
	"database/sql"
	"testing"
)

// ── D1: RETIRED — duplicate evidence no longer inflates confidence ────────
//
// The characterization that pinned the DEFECTIVE behaviour is retired. The
// correction landed with the observation-identity mechanism (see
// internal/core/evidence_identity.go): the evidence row's id is derived from
// the observation's identity and the table's PRIMARY KEY enforces "at most
// one stored row per observation per artifact".
//
// The M5-COMPLIANT counterpart of this characterization is now an ordinary
// regression test in internal/core/evidence_identity_test.go
// (TestEvidenceIdentity_RepeatedDeliveryDoesNotInflateConfidence), which
// runs in every green gate.
//
// WHAT WAS MEASURED (Session B, 2026-10-26, disposable workspace):
//
//	confidence: 0.5 → 0.6224593183800414 → 0.7310585651678245
//	confidence_history: (0.622459…, evidence_count=1, evidence_added)
//	                   (0.731058…, evidence_count=2, evidence_added)
//
// and is now:
//
//	confidence: 0.5 → 0.6224593183800414 → 0.6224593183800414
//	confidence_history: one row (evidence_count=1, evidence_added)

// ── D2: the trusted lesson surface has no pending state ───────────────────

// TestM5Characterization_LessonSurfaceHasNoPendingState pins the core half
// of defect D2.
//
// A lesson is trusted knowledge in MPM: the wake-context payload, the FTS
// index and the retrieval path all serve it directly. The lessons_base table
// carries NO status column, so a lesson that has been written is
// indistinguishable from a lesson that has been reviewed and accepted.
//
// The critic writes lessons directly (SurvivalAsymmetryHunt and
// WeakTheoryHunt both emit `mpm_lessons save` on every cycle, with no
// evidence and no review — see internal/critic/hunts.go:106-118 and
// :297-309). The critic-side characterization of that emission lives in
// internal/critic/m5_trust_characterization_test.go.
//
// WHY IT MATTERS. M5 requires that neither the adapter nor the critic
// promotes. Today the critic writes into a surface where every row is
// trusted on arrival.
//
// MEASURED (Session B, 2026-10-26): the pragma of lessons_base lists
// rowid, id, type, content, tags, reinforcement_count, source_session_id,
// created, content_hash, retrieval_priority, importance, confidence — and no
// status, state or review column of any kind.
func TestM5Characterization_LessonSurfaceHasNoPendingState(t *testing.T) {
	dm := m5NewDM(t)

	// The lessons view is an INSTEAD OF view over lessons_base; the base
	// table is where the rows actually live.
	cols := map[string]bool{}
	rows, err := dm.SQLDB().Query(`PRAGMA table_info(lessons_base)`)
	if err != nil {
		t.Fatalf("PRAGMA table_info(lessons_base): %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid       int
			name      string
			ctype     string
			notNull   int
			dfltValue sql.NullString
			pk        int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dfltValue, &pk); err != nil {
			t.Fatalf("scan pragma: %v", err)
		}
		cols[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate pragma: %v", err)
	}

	// CURRENT BEHAVIOUR: there is nowhere to record "proposed but not
	// reviewed" on a lesson.
	for _, forbidden := range []string{"status", "state", "review_status", "pending"} {
		if cols[forbidden] {
			t.Errorf("lessons_base unexpectedly has a %q column — the characterization "+
				"of defect D2 is stale; re-audit the trust boundary", forbidden)
		}
	}

	// A written lesson is immediately trusted: it lands in the FTS index
	// that backs retrieval, with a full confidence value.
	//
	// The marker token is a single alphanumeric word on purpose. lessons_fts
	// is an external-content FTS5 table over the `lessons` view with columns
	// (content, tags); an unquoted multi-word MATCH term is parsed as a
	// column filter and fails with "no such column", so the probe token
	// must be one token.
	const marker = "m5unreviewedlessonmarker"
	const lessonText = "A critic-authored observation that was never reviewed " + marker + "."

	if _, err := dm.AddLesson(
		lessonText,
		LessonTypeWarning, []string{"m5", "probe"}, ""); err != nil {
		t.Fatalf("AddLesson: %v", err)
	}

	var ftsHits int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM lessons_fts WHERE lessons_fts MATCH ?`,
		marker).Scan(&ftsHits); err != nil {
		t.Fatalf("query lessons_fts: %v", err)
	}
	if ftsHits != 1 {
		t.Errorf("expected the unreviewed lesson to be immediately retrievable "+
			"through lessons_fts (1 hit), got %d — the characterization of defect "+
			"D2 is stale", ftsHits)
	}

	var conf sql.NullFloat64
	if err := dm.SQLDB().QueryRow(
		`SELECT confidence FROM lessons_base WHERE content = ?`,
		lessonText).Scan(&conf); err != nil {
		t.Fatalf("query lessons_base confidence: %v", err)
	}
	if !conf.Valid || conf.Float64 <= 0 {
		t.Errorf("expected the unreviewed lesson to carry a trusted confidence value, got %v", conf)
	}
}

// ── D3: unresolvable citations are accepted and mislabelled ───────────────

// TestM5Characterization_UnresolvableCitationIsAcceptedAndMislabelled pins
// defect D3.
//
// A proposal's source_ids are written as epistemic_provenance citations, but:
//
//   - the cited id is NEVER validated to exist; and
//   - source_type is written EMPTY, which makes recordProvenanceNode resolve
//     the id against the artifact tables and fall back to "memory"
//     (internal/core/cascade_provenance.go:168-178).
//
// So a citation of a tool invocation is stored as a citation of a memory, and
// a citation of nothing at all is stored just the same.
//
// WHY IT MATTERS. M5 requires that a proposal cite RESOLVABLE evidence and
// that an unresolvable citation be rejected. Today neither holds: the
// substrate cannot distinguish a citation of a memory from a citation of a
// tool invocation, and it cannot tell a valid citation from a typo.
//
// MEASURED (Session B, 2026-10-26): proposing a theory whose source_ids
// contains a tool_invocations.id succeeded with "success":true, and the
// citation landed as source_id=<the invocation id> source_type='memory'.
func TestM5Characterization_UnresolvableCitationIsAcceptedAndMislabelled(t *testing.T) {
	dm := m5NewDM(t)

	// A citation that resolves to nothing at all.
	const phantom = "no-such-artifact-0000000000000000"

	res, err := dm.ProposeTheory(
		"A session consulted MPM but recorded nothing.",
		"Show the session's invocation ids.",
		nil,
		[]string{phantom},
		[]string{"m5", "probe"},
	)
	if err != nil {
		t.Fatalf("ProposeTheory with an unresolvable source_id: %v", err)
	}
	proposalID, _ := res["id"].(string)
	if proposalID == "" {
		t.Fatalf("ProposeTheory returned no id: %v", res)
	}

	// CURRENT BEHAVIOUR: the proposal was ACCEPTED.
	proposals := m5ScalarInt(t, dm.SQLDB(),
		`SELECT COUNT(*) FROM memories WHERE id = ? AND collection = 'theories'`,
		proposalID)
	if proposals != 1 {
		t.Fatalf("expected the proposal to be persisted despite the unresolvable "+
			"citation, got %d rows — the characterization of defect D3 is stale", proposals)
	}

	// CURRENT BEHAVIOUR: the citation was stored, and it was mislabelled.
	var (
		sourceID   string
		sourceType string
		eventID    string
		downstream string
	)
	if err := dm.SQLDB().QueryRow(
		`SELECT source_id, source_type, event_id, downstream_type
		   FROM epistemic_provenance WHERE downstream_id = ?`, proposalID).
		Scan(&sourceID, &sourceType, &eventID, &downstream); err != nil {
		t.Fatalf("query epistemic_provenance: %v", err)
	}
	if sourceID != phantom {
		t.Errorf("expected source_id %q, got %q", phantom, sourceID)
	}
	if sourceType != "memory" {
		t.Errorf("expected the mislabelled source_type 'memory' (the "+
			"ResolveArtifactType fallback), got %q — the characterization of "+
			"defect D3 is stale", sourceType)
	}
	// event_id carries the DOWNSTREAM id, not the cited id — so the cited
	// observation is not identifiable from the citation row.
	if eventID != proposalID {
		t.Errorf("expected event_id to be the proposal id %q (the current "+
			"overloading), got %q — the characterization of defect D3 is stale",
			proposalID, eventID)
	}
	if downstream != "theory" {
		t.Errorf("expected downstream_type 'theory', got %q", downstream)
	}
}

// ── D4: cycle-specific tags defeat proposal idempotency ───────────────────

// TestM5Characterization_CycleTagBreaksProposalIdempotency pins defect D4.
//
// mpm_theories propose is idempotent when hypothesis, tags and collection
// all match. The identity hash includes json(tags)
// (internal/core/db.go:6269-6287), so a tag that varies per cycle defeats
// the dedup entirely and produces a second, distinct proposal.
//
// The critic emits exactly that pattern: PoisonPillHunt tags its theory
// with cycle_N (internal/critic/hunts.go:373-378). A new retrospective
// hunt that follows the same pattern would accumulate an unbounded stream of
// duplicate proposals, one per cycle, each needing its own review.
//
// WHY IT MATTERS. A periodic reviewer that cannot tell "the same finding
// again" from "a new finding" cannot scale, and the proposal table fills
// with near-identical pending rows.
//
// MEASURED (Session B, 2026-10-26): the same hypothesis with tags
// cycle_5 and cycle_6 produced two distinct theory ids.
func TestM5Characterization_CycleTagBreaksProposalIdempotency(t *testing.T) {
	dm := m5NewDM(t)

	const hypothesis = "Survival-rate asymmetry: direct vs call ratio exceeds threshold."

	// CONTROL: identical hypothesis AND identical tags → one proposal.
	first, err := dm.ProposeTheory(hypothesis, "Investigate the decay path.",
		nil, nil, []string{"critic", "survival-asymmetry", "cycle_5"})
	if err != nil {
		t.Fatalf("first ProposeTheory: %v", err)
	}
	second, err := dm.ProposeTheory(hypothesis, "Investigate the decay path.",
		nil, nil, []string{"critic", "survival-asymmetry", "cycle_5"})
	if err != nil {
		t.Fatalf("second ProposeTheory (identical tags): %v", err)
	}
	firstID, _ := first["id"].(string)
	secondID, _ := second["id"].(string)
	if firstID != secondID {
		t.Errorf("expected identical hypothesis+tags to be idempotent, got ids %q and %q "+
			"— the characterization of defect D4 is stale", firstID, secondID)
	}
	control := m5ScalarInt(t, dm.SQLDB(),
		`SELECT COUNT(*) FROM memories WHERE collection = 'theories'`)
	if control != 1 {
		t.Fatalf("expected 1 theory row after the identical pair, got %d", control)
	}

	// DEFECT: the same hypothesis with a cycle-varying tag → a SECOND,
	// DISTINCT proposal.
	third, err := dm.ProposeTheory(hypothesis, "Investigate the decay path.",
		nil, nil, []string{"critic", "survival-asymmetry", "cycle_6"})
	if err != nil {
		t.Fatalf("third ProposeTheory (cycle_6): %v", err)
	}
	thirdID, _ := third["id"].(string)
	if thirdID == firstID {
		t.Fatalf("expected the cycle_6 tag to produce a DISTINCT proposal id, got %q "+
			"— the characterization of defect D4 is stale", thirdID)
	}

	after := m5ScalarInt(t, dm.SQLDB(),
		`SELECT COUNT(*) FROM memories WHERE collection = 'theories'`)
	if after != 2 {
		t.Fatalf("expected 2 theory rows after the cycle-tagged proposal, got %d "+
			"— the characterization of defect D4 is stale", after)
	}
}

// ── D5: RETIRED — repeated delivery is no longer independent corroboration ─
//
// The characterization that pinned the DEFECTIVE behaviour is retired. The
// same correction that closed D1 closes D5: an observation's identity is
// durable, so a delivery of the same observation is a REPLAY (no row, no
// recompute, no confidence change) while two distinct observations continue
// to contribute twice.
//
// The M5-COMPLIANT counterpart of this characterization is now an ordinary
// regression test in internal/core/evidence_identity_test.go
// (TestEvidenceIdentity_RepeatedCriticCyclesDoNotManufactureCorroboration),
// which runs in every green gate.
//
// WHAT WAS MEASURED (Session B, 2026-10-26): two byte-identical evidence
// rows and two genuinely different rows produced the same confidence. Now:
// the repeated delivery lands on one row and the distinct observations land
// on two.
