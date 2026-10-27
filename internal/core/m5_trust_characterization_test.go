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

// ── D1: duplicate evidence inflates confidence ─────────────────────────────

// TestM5Characterization_DuplicateEvidenceInflatesConfidence pins defect D1.
//
// Two byte-identical evidence submissions against the same artifact produce
// two evidence rows, and the artifact's confidence rises with each one. The
// confidence_history ledger records both, with evidence_count 1 then 2.
//
// WHY IT MATTERS. A periodic critic re-submitting the same observation would
// accumulate duplicate rows and monotonically inflate the proposal's
// confidence on every cycle — repetition mistaken for corroboration, which is
// the exact inverse of M5's trust boundary.
//
// MEASURED (Session B, 2026-10-26, disposable workspace):
//
//	confidence: 0.5 → 0.6224593183800414 → 0.7310585651678245
//	confidence_history: (0.622459…, evidence_count=1, evidence_added)
//	                   (0.731058…, evidence_count=2, evidence_added)
//
// ROOT CAUSE. The evidence table has no unique index beyond its primary key
// and no content-hash column, and AddEvidence mints a fresh id on every
// insert (internal/core/evidence_store.go:292). The confidence recompute
// sums every non-expired, non-auto_capture row
// (internal/core/evidence_store.go:511-554), so each duplicate counts in
// full.
//
// CONTRACT COUNTERPART (m5contract, currently failing):
// TestM5Contract_DuplicateEvidenceDoesNotInflateConfidence.
func TestM5Characterization_DuplicateEvidenceInflatesConfidence(t *testing.T) {
	dm := m5NewDM(t)
	id := m5ProposePendingTheory(t, dm,
		"Scheduler heartbeat rename is not atomic across filesystems.",
		[]string{"m5", "probe"})

	before := m5Confidence(t, dm, id)

	in := EvidenceInput{
		ArtifactID:         id,
		ArtifactType:       "theory",
		Type:               "observation",
		SourceGroup:        "tool_invocation",
		Strength:           0.5,
		IndependenceFactor: 1.0,
		CreatedBy:          "critic:probe",
		Notes:              "source observation O-1",
	}

	first, err := dm.AddEvidence(in)
	if err != nil {
		t.Fatalf("first AddEvidence: %v", err)
	}
	afterFirst := m5Confidence(t, dm, id)

	// The second submission is byte-identical to the first.
	second, err := dm.AddEvidence(in)
	if err != nil {
		t.Fatalf("second AddEvidence: %v", err)
	}
	afterSecond := m5Confidence(t, dm, id)

	// CURRENT BEHAVIOUR: two rows.
	rows := m5ScalarInt(t, dm.SQLDB(),
		`SELECT COUNT(*) FROM evidence WHERE artifact_id = ?`, id)
	if rows != 2 {
		t.Fatalf("expected 2 evidence rows after two identical submissions, got %d", rows)
	}

	// CURRENT BEHAVIOUR: confidence rises twice, and each AddEvidence
	// reports the new (higher) value.
	if !(afterFirst > before) {
		t.Errorf("expected confidence to rise after the first submission: %.6f -> %.6f",
			before, afterFirst)
	}
	if !(afterSecond > afterFirst) {
		t.Errorf("expected confidence to rise again after the duplicate submission: %.6f -> %.6f",
			afterFirst, afterSecond)
	}

	firstConf, _ := first["confidence"].(float64)
	secondConf, _ := second["confidence"].(float64)
	if !(firstConf > 0 && secondConf > firstConf) {
		t.Errorf("expected AddEvidence to report a rising confidence: first=%v second=%v",
			firstConf, secondConf)
	}

	// CURRENT BEHAVIOUR: the ledger records both, with a growing evidence
	// count — the audit trail shows the duplication explicitly.
	var historyRows int
	var counts []int
	rowsQ, err := dm.SQLDB().Query(
		`SELECT evidence_count FROM confidence_history
		  WHERE artifact_id = ? AND trigger = 'evidence_added'
		  ORDER BY rowid ASC`, id)
	if err != nil {
		t.Fatalf("query confidence_history: %v", err)
	}
	defer rowsQ.Close()
	for rowsQ.Next() {
		var n int
		if err := rowsQ.Scan(&n); err != nil {
			t.Fatalf("scan confidence_history: %v", err)
		}
		counts = append(counts, n)
		historyRows++
	}
	if err := rowsQ.Err(); err != nil {
		t.Fatalf("iterate confidence_history: %v", err)
	}
	if historyRows != 2 {
		t.Fatalf("expected 2 confidence_history rows for the duplicate pair, got %d (%v)",
			historyRows, counts)
	}
	if counts[0] != 1 || counts[1] != 2 {
		t.Errorf("expected evidence_count 1 then 2 in confidence_history, got %v", counts)
	}
}

// ── D2: the trusted lesson surface has no pending state ───────────────────

// TestM5Characterization_LessonSurfaceHasNoPendingState pins the core half
// of defect D2.
//
// A lesson is trusted knowledge in MPM: the wake-context payload, the FTS
// index and the retrieval path all serve it directly. The lessons_base table
// carries NO status column, so a lesson that has been written is
// indistinguishable from a lesson that has been reviewed and accepted.
//
// The critic no longer writes lessons directly. Session B's P3b
// (2026-10-27) moved SurvivalAsymmetryHunt and WeakTheoryHunt to
// `mpm_theories propose` — the only existing untrusted proposal surface —
// so the critic stopped writing into the trusted lesson surface at all.
//
// What this test pins is therefore the SUBSTRATE half of D2, which is NOT
// closed: the lesson surface itself still has no way to record "proposed
// but not reviewed". Any other writer — an agent, an adapter, a future
// tool — that writes a lesson still writes TRUSTED knowledge on arrival.
// The critic-side characterization of the old behaviour lived in
// internal/critic/m5_trust_characterization_test.go and is retired; its
// M5-compliant counterpart is now ordinary CI coverage as
// TestP3b_CriticFindingIsNotTrustedWithoutReview in
// internal/critic/p3b_trust_boundary_test.go.
//
// WHY IT MATTERS. M5 requires that neither the adapter nor the critic
// promotes. The critic no longer does; the SURFACE still cannot express
// the difference.
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

// ── D5: repeated delivery is indistinguishable from corroboration ─────────

// TestM5Characterization_RepeatedDeliveryCountsAsIndependentCorroboration
// pins defect D5.
//
// The confidence formula sums strength × independence over every
// non-expired, non-auto_capture evidence row
// (internal/core/confidence.go:119-138). The evidence table carries no
// identity for the underlying observation, so two rows describing the SAME
// observation are counted exactly like two rows describing DIFFERENT
// observations.
//
// The independence_factor field is the intended mechanism for expressing
// dependence, but it is caller-asserted and nothing enforces it against the
// observation's identity — and there is no identity to enforce it against.
//
// WHY IT MATTERS. M5's evidence-backed-proposal rule presumes that attached
// evidence is a set of DISTINCT observations. Without an observation
// identity, a retried delivery, a duplicated ingestion and a replayed
// collector all masquerade as independent corroboration.
//
// MEASURED (Session B, 2026-10-26): two byte-identical evidence rows and
// two genuinely different rows produce the same confidence.
func TestM5Characterization_RepeatedDeliveryCountsAsIndependentCorroboration(t *testing.T) {
	dm := m5NewDM(t)

	// Case A: the SAME observation, delivered twice.
	repeatedID := m5ProposePendingTheory(t, dm,
		"A repeated observation must not read as independent corroboration.",
		[]string{"m5", "probe", "repeated"})
	repeated := EvidenceInput{
		ArtifactID:         repeatedID,
		ArtifactType:       "theory",
		Type:               "observation",
		SourceGroup:        "tool_invocation",
		Strength:           0.5,
		IndependenceFactor: 1.0,
		CreatedBy:          "critic:probe",
		Notes:              "observation O-1",
	}
	if _, err := dm.AddEvidence(repeated); err != nil {
		t.Fatalf("AddEvidence (repeated, first): %v", err)
	}
	if _, err := dm.AddEvidence(repeated); err != nil {
		t.Fatalf("AddEvidence (repeated, second): %v", err)
	}

	// Case B: TWO DIFFERENT observations, one source group.
	distinctID := m5ProposePendingTheory(t, dm,
		"Two distinct observations must read as independent corroboration.",
		[]string{"m5", "probe", "distinct"})
	distinct := repeated
	distinct.ArtifactID = distinctID
	distinct.Notes = "observation O-2"
	if _, err := dm.AddEvidence(distinct); err != nil {
		t.Fatalf("AddEvidence (distinct, first): %v", err)
	}
	distinct.Notes = "observation O-3"
	if _, err := dm.AddEvidence(distinct); err != nil {
		t.Fatalf("AddEvidence (distinct, second): %v", err)
	}

	repeatedConf := m5Confidence(t, dm, repeatedID)
	distinctConf := m5Confidence(t, dm, distinctID)

	// CURRENT BEHAVIOUR: identical. The substrate cannot tell repeated
	// delivery from independent corroboration, because it has no way to
	// identify the underlying observation.
	if diff := repeatedConf - distinctConf; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("expected repeated delivery and independent corroboration to be "+
			"indistinguishable today (both %.9f vs %.9f) — the characterization of "+
			"defect D5 is stale", repeatedConf, distinctConf)
	}

	// The dependence signal EXISTS but is purely caller-asserted: a single
	// row at independence 0.5 contributes the same as two rows at 0.5, so
	// nothing forces a caller to declare the dependence.
	halfID := m5ProposePendingTheory(t, dm,
		"A caller may declare dependence through independence_factor.",
		[]string{"m5", "probe", "half"})
	half := repeated
	half.ArtifactID = halfID
	half.IndependenceFactor = 0.5
	half.Notes = "observation O-4"
	if _, err := dm.AddEvidence(half); err != nil {
		t.Fatalf("AddEvidence (half): %v", err)
	}
	halfConf := m5Confidence(t, dm, halfID)

	// 0.5 strength at independence 0.5 == 0.25 effective, which is half of
	// the 0.5 contributed by one full-strength row. The mechanism works
	// when a caller uses it; nothing enforces that it IS used.
	if halfConf >= repeatedConf {
		t.Errorf("expected a declared-dependent single row (%.9f) to score below the "+
			"repeated-delivery case (%.9f) — the characterization of defect D5 is stale",
			halfConf, repeatedConf)
	}
}
