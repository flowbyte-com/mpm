package internal

// p3_identity_test.go — the mechanism that decides whether a critic hunt's
// proposal is idempotent.
//
// WHY THIS FILE EXISTS.
//
// A critic hunt that emits a proposal every cycle is worse than useless: it
// floods the substrate with indistinguishable pending theories and it
// inflates the very evidence rows the substrate counts as corroboration.
// The hunt's proposal is therefore required to be a PURE FUNCTION of its
// evidence.
//
// The proposal's identity is the F19 duplicate-identity key:
//
//	wantIdentity := MemoryIdentityHash(contentHash, tags, metadata)
//
// and `metadata` for a proposal is:
//
//	{"status": "pending", "validation_criteria": <...>, "dependencies": [...]}
//
// So THREE inputs to the proposal decide its identity: the hypothesis
// (through the content hash), the tags, and the metadata — which includes
// `validation_criteria`.
//
// That last one is the surprise, and it is why this file exists. It is
// natural to assume `validation_criteria` is "just the description" and
// therefore outside the identity, and to move a cycle-varying value there
// on that assumption. It is NOT outside the identity: it participates,
// twice over, because `proposeTheory` folds it into the content AND
// stores it in the metadata.
//
// These tests pin the mechanism so that the assumption is never made
// again. The hunt in internal/critic/missed_opportunity.go depends on it.
//
// ISOLATION. Every test pins HOME, MPM_WORKSPACE and MPM_DB_PATH to
// disposable locations and opens a disposable file-backed database at the
// canonical <workspace>/src/db/mpm.db — the path MPM_DB_PATH names — so
// the production path resolution is exercised rather than bypassed. The
// repo-wide DB-safety AST guard flags only an EMPTY string literal as the
// first argument to NewDatabaseManager, so these helpers need no
// whitelist entry and the guard's own suite passes unchanged.

import (
	"testing"
)

// p3IsolatedWorkspace pins HOME, MPM_WORKSPACE and MPM_DB_PATH to disposable
// locations and returns the disposable workspace root.
func p3IsolatedWorkspace(t *testing.T) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	ws := t.TempDir()
	t.Setenv("MPM_WORKSPACE", ws)
	t.Setenv("MPM_DB_PATH", ws+"/src/db/mpm.db")
	return ws
}

// p3NewDM opens a disposable DatabaseManager rooted at the isolated
// workspace.
func p3NewDM(t *testing.T) *DatabaseManager {
	t.Helper()
	ws := p3IsolatedWorkspace(t)
	dm, err := NewDatabaseManager(ws)
	if err != nil {
		t.Fatalf("p3NewDM: NewDatabaseManager(%q): %v", ws, err)
	}
	t.Cleanup(func() { _ = dm.Close() })
	return dm
}

// TestP3Identity_ValidationCriteriaParticipatesInProposalIdentity pins the
// mechanism the hunt depends on.
//
// Two proposals with the SAME hypothesis and the SAME tags but DIFFERENT
// `validation_criteria` are TWO DIFFERENT proposals: the identity hash
// includes the metadata, and the metadata includes validation_criteria.
//
// This is the opposite of what the "move the cycle number to
// validation_criteria" instinct predicts, and it is why the hunt keeps
// every identity-bearing field a pure function of the evidence.
func TestP3Identity_ValidationCriteriaParticipatesInProposalIdentity(t *testing.T) {
	dm := p3NewDM(t)

	first, err := dm.ProposeTheory(
		"A stable hypothesis.",
		"VALIDATION CRITERIA FOR CYCLE 1",
		nil, nil, []string{"p3", "identity"},
	)
	if err != nil {
		t.Fatalf("first ProposeTheory: %v", err)
	}
	second, err := dm.ProposeTheory(
		"A stable hypothesis.",
		"VALIDATION CRITERIA FOR CYCLE 2",
		nil, nil, []string{"p3", "identity"},
	)
	if err != nil {
		t.Fatalf("second ProposeTheory: %v", err)
	}

	firstID, _ := first["id"].(string)
	secondID, _ := second["id"].(string)
	if firstID == secondID {
		t.Fatal("expected two proposals with different validation_criteria to " +
			"have different identities — validation_criteria participates in " +
			"the identity hash, so it is NOT a safe place for a cycle-varying " +
			"value")
	}
}

// TestP3Identity_CycleVaryingTagCreatesASecondProposal pins the other
// half of the mechanism, and the defect it describes.
//
// This is the D4 defect (cycle-specific tags defeat proposal idempotency).
// The characterization is that TODAY a cycle-varying tag DOES create a
// second proposal — and the hunt in internal/critic/missed_opportunity.go
// must not repeat it.
//
// It is pinned here rather than only in the M5 contract suite because the
// hunt's correctness depends on it directly: the hunt keeps its tags stable
// precisely because this test shows what happens when they are not.
func TestP3Identity_CycleVaryingTagCreatesASecondProposal(t *testing.T) {
	dm := p3NewDM(t)

	first, err := dm.ProposeTheory(
		"A stable hypothesis.",
		"Stable criteria.",
		nil, nil, []string{"p3", "identity", "cycle_1"},
	)
	if err != nil {
		t.Fatalf("first ProposeTheory: %v", err)
	}
	second, err := dm.ProposeTheory(
		"A stable hypothesis.",
		"Stable criteria.",
		nil, nil, []string{"p3", "identity", "cycle_2"},
	)
	if err != nil {
		t.Fatalf("second ProposeTheory: %v", err)
	}

	firstID, _ := first["id"].(string)
	secondID, _ := second["id"].(string)
	if firstID == secondID {
		t.Fatal("expected a cycle-varying tag to create a second proposal — " +
			"if this now returns the same id, the identity mechanism changed " +
			"and the hunt's stable-tag requirement must be re-examined")
	}
}

// TestP3Identity_IdenticalProposalIsIdempotent is the positive case the
// hunt relies on: a proposal whose hypothesis, tags, validation criteria
// and dependencies are all identical returns the SAME id on every
// submission.
//
// This is what makes the hunt's "stable content" requirement sufficient:
// re-emitting the same finding in a later cycle is a no-op, so the hunt
// never floods the substrate with indistinguishable pending theories.
func TestP3Identity_IdenticalProposalIsIdempotent(t *testing.T) {
	dm := p3NewDM(t)

	first, err := dm.ProposeTheory(
		"A stable hypothesis.",
		"Stable criteria.",
		nil, nil, []string{"p3", "identity", "stable"},
	)
	if err != nil {
		t.Fatalf("first ProposeTheory: %v", err)
	}
	second, err := dm.ProposeTheory(
		"A stable hypothesis.",
		"Stable criteria.",
		nil, nil, []string{"p3", "identity", "stable"},
	)
	if err != nil {
		t.Fatalf("second ProposeTheory: %v", err)
	}

	firstID, _ := first["id"].(string)
	secondID, _ := second["id"].(string)
	if firstID != secondID {
		t.Fatalf("expected an identical proposal to be idempotent, got %q and %q",
			firstID, secondID)
	}
}
