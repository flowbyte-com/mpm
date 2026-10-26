//go:build m5contract

package internal

// m5_trust_contract_test.go — CONTRACT tests for the five M5 trust defects.
//
// THESE TESTS FAIL AGAINST TODAY'S PRODUCTION BEHAVIOUR. THAT IS THE POINT.
//
// They assert the behaviour mpm-event/0.1 §3.7 (M5) requires and that the
// substrate does not yet provide:
//
//   - trust is always observation;
//   - capturing an observation creates no memory, lesson or decision row;
//   - a proposal must cite RESOLVABLE evidence, and an unresolvable citation
//     is REJECTED; and
//   - neither the adapter nor the critic promotes.
//
// Each test's doc comment names the defect it covers, the production code
// responsible, the minimal proposed correction, and whether the correction
// needs Session A's approval.
//
// ── WHY THESE ARE NOT IN A GREEN CI GATE ──────────────────────────────────
//
// The `m5contract` build tag excludes this file from every default build:
// `make test`, `make test-race`, `make test-core-precommit`, the CI "Core
// tests" step and the CI full-suite sweep all invoke `go test -tags fts5`
// with no additional tags, so none of them compiles this file.
//
// That exclusion is MECHANICALLY GUARDED, not merely intended:
//
//	internal/core/m5_trust_gate_test.go (always compiled, always green)
//	  proves the contract files are absent from the default build, present
//	  under the tag, and that neither the Makefile nor any CI workflow
//	  enables the tag.
//
// So a contract test cannot be silently absorbed into a required green gate,
// and a green gate cannot be silently emptied by deleting a contract test.
//
// ── HOW TO RUN THEM ───────────────────────────────────────────────────────
//
//	cd internal/core
//	CGO_CFLAGS=-DSQLITE_ENABLE_FTS5=1 CGO_LDFLAGS=-lm \
//	  go test -tags 'fts5 m5contract' -count=1 -run 'TestM5Contract' -v ./
//
// Expect FAILURES. Each failure is the defect, made visible and measurable.
//
// ── HOW THEY ARE RETIRED ──────────────────────────────────────────────────
//
// When Session A adjudicates a defect and the correction lands, the
// matching characterization test in m5_trust_characterization_test.go is
// INVERTED (or deleted, where the defect is accepted as-is) in the SAME PR,
// and the corresponding test here is deleted. The two files are a pair: the
// characterization pins the behaviour before the change, the contract pins
// the behaviour after it.
//
// ── ISOLATION ─────────────────────────────────────────────────────────────
//
// Same contract as the characterization suite: HOME, MPM_WORKSPACE and
// MPM_DB_PATH are pinned to disposable locations and the database is a
// disposable file-backed database at the canonical workspace path.

import (
	"testing"
)

// ── D1: duplicate evidence must not inflate confidence ─────────────────────

// TestM5Contract_DuplicateEvidenceDoesNotInflateConfidence covers defect D1.
//
// REPRODUCTION (today, green in the characterization suite):
// two byte-identical mpm_evidence submissions against one artifact create
// two evidence rows and raise the artifact's confidence twice — measured at
// 0.5 → 0.6224593183800414 → 0.7310585651678245.
//
// EXPECTED CORRECT BEHAVIOUR:
// submitting the same observation twice must leave the artifact's confidence
// where one submission left it, and must leave exactly one evidence row.
//
// EXISTING IMPLEMENTATION RESPONSIBLE:
// internal/core/evidence_store.go:292 mints a fresh id per insert; the
// evidence table has no unique index beyond its primary key and no
// content-hash column; the confidence recompute at
// internal/core/evidence_store.go:511-554 sums every non-expired,
// non-auto_capture row.
//
// MINIMAL PROPOSED CORRECTION:
// give the evidence row an observation identity and refuse a second row that
// carries the same identity for the same artifact. Whether that identity is
// a new column, a stored hash, or a unique index over existing columns is
// Session A's decision — the contract test is written against the BEHAVIOUR,
// not the mechanism, so A's choice does not invalidate it.
//
// NEEDS SESSION A'S APPROVAL: YES — the mechanism is a schema change
// (internal/core/schema.go), which is the sprint's highest-collision file.
func TestM5Contract_DuplicateEvidenceDoesNotInflateConfidence(t *testing.T) {
	dm := m5NewDM(t)
	id := m5ProposePendingTheory(t, dm,
		"Scheduler heartbeat rename is not atomic across filesystems.",
		[]string{"m5", "contract"})

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
	if _, err := dm.AddEvidence(in); err != nil {
		t.Fatalf("first AddEvidence: %v", err)
	}
	afterFirst := m5Confidence(t, dm, id)

	if _, err := dm.AddEvidence(in); err != nil {
		t.Fatalf("second AddEvidence (identical): %v", err)
	}
	afterSecond := m5Confidence(t, dm, id)

	rows := m5ScalarInt(t, dm.SQLDB(),
		`SELECT COUNT(*) FROM evidence WHERE artifact_id = ?`, id)
	if rows != 1 {
		t.Errorf("expected exactly 1 evidence row after two identical submissions "+
			"(defect D1), got %d", rows)
	}
	if diff := afterSecond - afterFirst; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("expected the duplicate submission not to change confidence "+
			"(defect D1): %.9f -> %.9f", afterFirst, afterSecond)
	}
	if diff := afterFirst - before; diff <= 0 {
		t.Errorf("expected the FIRST submission to raise confidence: %.9f -> %.9f",
			before, afterFirst)
	}
}

// ── D3: a proposal must cite resolvable evidence ──────────────────────────

// TestM5Contract_UnresolvableCitationIsRejected covers defect D3.
//
// REPRODUCTION (today, green in the characterization suite):
// ProposeTheory with a source_id that resolves to nothing succeeds, and the
// citation is stored with source_type='memory' — the ResolveArtifactType
// fallback at internal/core/cascade_provenance.go:168-178.
//
// EXPECTED CORRECT BEHAVIOUR:
// a proposal whose cited source id does not resolve is REJECTED, and no
// theory row is created. The rejection must leave no partial state.
//
// EXISTING IMPLEMENTATION RESPONSIBLE:
// internal/core/epistemology_tools.go:676-698 recordSourceCitationsNode
// calls recordProvenanceNode with source_type="" and event_id hardcoded to
// the downstream id, and never validates that the cited id exists.
//
// MINIMAL PROPOSED CORRECTION:
// validate each cited source id against its declared source type before the
// insert, and roll the artifact insert back with it. The whole proposal is
// already one WithTx, so the rollback is free.
//
// NEEDS SESSION A'S APPROVAL: YES — the citation must carry a source type,
// which is a tool-registry change to mpm_theories propose
// (internal/core/tools/registry_list.go). The validation itself is core
// logic and needs no schema change.
func TestM5Contract_UnresolvableCitationIsRejected(t *testing.T) {
	dm := m5NewDM(t)

	const phantom = "no-such-artifact-0000000000000000"

	_, err := dm.ProposeTheory(
		"A session consulted MPM but recorded nothing.",
		"Show the session's invocation ids.",
		nil,
		[]string{phantom},
		[]string{"m5", "contract"},
	)
	if err == nil {
		t.Errorf("expected the proposal to be REJECTED because its cited source id %q "+
			"does not resolve (defect D3)", phantom)
	}

	rows := m5ScalarInt(t, dm.SQLDB(),
		`SELECT COUNT(*) FROM memories WHERE collection = 'theories'`)
	if rows != 0 {
		t.Errorf("expected a rejected proposal to leave NO theory row (defect D3), "+
			"got %d", rows)
	}
}

// TestM5Contract_CitationPreservesItsType covers the second half of defect D3.
//
// REPRODUCTION (today, green in the characterization suite):
// citing a tool_invocations.id as a proposal source stores source_type
// 'memory', because the source type is never carried and the fallback
// mislabels it.
//
// EXPECTED CORRECT BEHAVIOUR:
// the citation preserves the type of what it cites, so a reader can tell a
// citation of a tool invocation from a citation of a memory.
//
// EXISTING IMPLEMENTATION RESPONSIBLE:
// internal/core/cascade_provenance.go:168-178 — source_type is written empty
// and then resolved (or defaulted to "memory") at write time.
//
// MINIMAL PROPOSED CORRECTION:
// let the caller declare the source type and write it through verbatim.
// The column already exists and is free-text, so this needs NO schema
// change — only the registry parameter and the write path.
//
// NEEDS SESSION A'S APPROVAL: YES — the tool-registry parameter on
// mpm_theories propose.
func TestM5Contract_CitationPreservesItsType(t *testing.T) {
	dm := m5NewDM(t)

	const invocationID = "m5-tool-invocation-000000000001"
	m5SeedToolInvocation(t, dm, invocationID)

	// The proposal must succeed — the citation resolves.
	res, err := dm.ProposeTheory(
		"A session consulted MPM but recorded nothing.",
		"Show the session's invocation ids.",
		nil,
		[]string{invocationID},
		[]string{"m5", "contract"},
	)
	if err != nil {
		t.Fatalf("ProposeTheory citing a resolvable tool invocation: %v", err)
	}
	proposalID, _ := res["id"].(string)

	var sourceType string
	if err := dm.SQLDB().QueryRow(
		`SELECT source_type FROM epistemic_provenance WHERE downstream_id = ?`,
		proposalID).Scan(&sourceType); err != nil {
		t.Fatalf("query epistemic_provenance: %v", err)
	}
	if sourceType != "tool_invocation" {
		t.Errorf("expected the citation to preserve source_type 'tool_invocation' "+
			"(defect D3), got %q", sourceType)
	}
}

// ── D4: a repeated finding must not create a second proposal ───────────────

// TestM5Contract_CycleVaryingTagDoesNotCreateSecondProposal covers defect D4.
//
// REPRODUCTION (today, green in the characterization suite):
// the same hypothesis proposed with tags [... "cycle_5"] and [... "cycle_6"]
// produces two distinct proposals, because the identity hash includes
// json(tags) (internal/core/db.go:6269-6287). The critic's PoisonPillHunt
// emits exactly this pattern (internal/critic/hunts.go:373-378).
//
// EXPECTED CORRECT BEHAVIOUR:
// a periodic reviewer re-finding the same thing must not accumulate a new
// pending proposal every cycle. The cycle marker must not be part of the
// proposal's identity.
//
// EXISTING IMPLEMENTATION RESPONSIBLE:
// internal/core/db.go:6269-6287 MemoryIdentityHash folds json(tags) into the
// identity; the partial unique index idx_memories_identity_live
// (internal/core/schema.go:992) then treats a cycle-varying tag as a
// genuinely different artifact.
//
// MINIMAL PROPOSED CORRECTION:
// keep the cycle marker out of the identity — e.g. carry it in the
// validation_criteria or the proposal's metadata rather than in its tags —
// OR exclude a designated critic-marker tag from the identity hash.
//
// The first option needs NO schema change and NO shared-interface change: it
// is a change to how the critic emits findings, which is Session B's own
// area once A has ruled on the trust policy.
//
// NEEDS SESSION A'S APPROVAL: PARTIAL. The emission-side fix is Session B's.
// If A instead mandates a change to the identity hash, that touches
// internal/core/db.go and the shared dedup semantics, which needs A's
// arbitration.
func TestM5Contract_CycleVaryingTagDoesNotCreateSecondProposal(t *testing.T) {
	dm := m5NewDM(t)

	const hypothesis = "Survival-rate asymmetry: direct vs call ratio exceeds threshold."

	first, err := dm.ProposeTheory(hypothesis, "Investigate the decay path.",
		nil, nil, []string{"critic", "survival-asymmetry", "cycle_5"})
	if err != nil {
		t.Fatalf("first ProposeTheory: %v", err)
	}
	second, err := dm.ProposeTheory(hypothesis, "Investigate the decay path.",
		nil, nil, []string{"critic", "survival-asymmetry", "cycle_6"})
	if err != nil {
		t.Fatalf("second ProposeTheory (cycle_6): %v", err)
	}
	firstID, _ := first["id"].(string)
	secondID, _ := second["id"].(string)
	if firstID != secondID {
		t.Errorf("expected the cycle-varying tag NOT to create a second proposal "+
			"(defect D4): got ids %q and %q", firstID, secondID)
	}

	rows := m5ScalarInt(t, dm.SQLDB(),
		`SELECT COUNT(*) FROM memories WHERE collection = 'theories'`)
	if rows != 1 {
		t.Errorf("expected exactly 1 theory row after the cycle-varying re-proposal "+
			"(defect D4), got %d", rows)
	}
}

// ── D5: repeated delivery is not independent corroboration ─────────────────

// TestM5Contract_RepeatedDeliveryDoesNotCountAsIndependentCorroboration
// covers defect D5.
//
// REPRODUCTION (today, green in the characterization suite):
// two submissions describing the SAME observation and two submissions
// describing DIFFERENT observations produce the SAME confidence, because the
// evidence table carries no identity for the underlying observation and the
// confidence formula sums every row in full.
//
// EXPECTED CORRECT BEHAVIOUR:
// the same observation delivered again must contribute what ONE observation
// contributes, while two distinct observations must continue to contribute
// twice. The distinction must be preserved, not merely asserted.
//
// EXISTING IMPLEMENTATION RESPONSIBLE:
// internal/core/confidence.go:119-138 sums strength × independence over
// every scoring evidence row; internal/core/evidence_store.go:511-554 loads
// them. independence_factor is the intended dependence signal but it is
// caller-asserted and unenforced, and there is no observation identity to
// enforce it against.
//
// MINIMAL PROPOSED CORRECTION:
// the same observation identity that closes defect D1 also closes D5 —
// once the substrate can tell two submissions of one observation from two
// submissions of two observations, the dependence is representable and
// enforceable. The mechanism is A's choice; this test pins the BEHAVIOUR.
//
// NEEDS SESSION A'S APPROVAL: YES — the mechanism is a schema change
// (internal/core/schema.go), which is the sprint's highest-collision file.
func TestM5Contract_RepeatedDeliveryDoesNotCountAsIndependentCorroboration(t *testing.T) {
	dm := m5NewDM(t)

	// The SAME observation, submitted twice.
	repeatedID := m5ProposePendingTheory(t, dm,
		"A repeated observation must not read as independent corroboration.",
		[]string{"m5", "contract", "repeated"})
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

	// TWO DIFFERENT observations.
	distinctID := m5ProposePendingTheory(t, dm,
		"Two distinct observations must read as independent corroboration.",
		[]string{"m5", "contract", "distinct"})
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

	if !(distinctConf > repeatedConf) {
		t.Errorf("expected two DISTINCT observations to score higher than one "+
			"observation delivered twice (defect D5): distinct=%.9f repeated=%.9f",
			distinctConf, repeatedConf)
	}
}
