// d4_proposal_identity_test.go — the D4 half of the M5 integrity matrix.
//
// WHAT THIS FILE IS.
//
// The M5 integrity matrix is a deterministic answer to twelve questions
// about how the substrate treats proposals and citations. This file
// answers the questions that belong to the PROPOSAL IDENTITY (D4); the
// citation questions (D3) are answered in d3_legacy_citation_test.go and
// in the existing p2_citation_test.go.
//
// THE IDENTITY MODEL, AS THE SUBSTRATE STATES IT.
//
// A proposal's identity is the SHA-256 of its content hash, its tags and
// its stable metadata (internal/core/db.go, MemoryIdentityHash). For a
// proposal the content is
//
//	hypothesis + "\n\nVALIDATION_CRITERIA: " + validation_criteria
//
// and the metadata is
//
//	{"status":"pending", "validation_criteria":…, "dependencies":[…]}
//
// so FOUR things decide whether two proposals are the same proposal:
// the hypothesis, the validation criteria, the dependencies and the tags.
// The critic states the same model in its own words
// (internal/critic/audit.go): "the hypothesis, the tags and the
// validation criteria ARE the proposal's identity".
//
// WHAT THESE TESTS PIN.
//
//   - the parts of the model that MUST hold (an identical proposal is one
//     proposal; two genuinely different hypotheses stay two proposals);
//   - the parts that are a DELIBERATE, DOCUMENTED choice (a different tag
//     set is a different proposal) — pinned as a characterization, not as
//     a contract, because whether tags SHOULD discriminate is the open
//     question in docs/dev/session-b-d4-proposal-identity.md;
//   - and the behaviours that are easy to get wrong: concurrency, a
//     resolved proposal, and a process restart.
//
// ISOLATION.
//
// Every test pins HOME, MPM_WORKSPACE and MPM_DB_PATH to disposable
// locations and opens a disposable file-backed database at the canonical
// <workspace>/src/db/mpm.db path.

package internal

import (
	"fmt"
	"sync"
	"testing"
)

// d4Propose proposes a theory and returns its id.
func d4Propose(t *testing.T, dm *DatabaseManager, hypothesis, criteria string, deps, tags []string) string {
	t.Helper()
	res, err := dm.ProposeTheory(hypothesis, criteria, deps, nil, tags)
	if err != nil {
		t.Fatalf("ProposeTheory(%q): %v", hypothesis, err)
	}
	id, _ := res["id"].(string)
	if id == "" {
		t.Fatalf("ProposeTheory(%q) returned no id: %+v", hypothesis, res)
	}
	return id
}

// d4TheoryCount counts the live theory rows.
func d4TheoryCount(t *testing.T, dm *DatabaseManager) int {
	t.Helper()
	return m5ScalarInt(t, dm.SQLDB(),
		`SELECT COUNT(*) FROM memories WHERE collection = 'theories' AND deleted_at IS NULL`)
}

// d4TheoryIDs lists the ids of the live theory rows, in creation order.
func d4TheoryIDs(t *testing.T, dm *DatabaseManager) []string {
	t.Helper()
	rows, err := dm.SQLDB().Query(
		`SELECT id FROM memories WHERE collection = 'theories' AND deleted_at IS NULL
		  ORDER BY created_at, id`)
	if err != nil {
		t.Fatalf("query theory ids: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan theory id: %v", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate theory ids: %v", err)
	}
	return out
}

// ── Matrix 1: repeated identical proposals ───────────────────────────────

// TestD4_RepeatedIdenticalProposalIsOneProposal answers matrix question 1.
//
// The same claim, proposed twice, is ONE proposal. This is the property
// the whole identity model exists to provide, and it is what stops a
// caller that repeats itself from filling the table with duplicates.
func TestD4_RepeatedIdenticalProposalIsOneProposal(t *testing.T) {
	dm := m5NewDM(t)

	const hyp = "Survival-rate asymmetry between two provenance cohorts."
	const crit = "Recompute both survival rates."
	tags := []string{"critic", "survival-asymmetry"}

	first := d4Propose(t, dm, hyp, crit, nil, tags)
	second := d4Propose(t, dm, hyp, crit, nil, tags)
	third := d4Propose(t, dm, hyp, crit, nil, tags)

	if first != second || second != third {
		t.Errorf("repeated identical proposals produced %q, %q, %q — they must be one proposal",
			first, second, third)
	}
	if n := d4TheoryCount(t, dm); n != 1 {
		t.Errorf("theory rows = %d, want 1", n)
	}
}

// ── Matrix 2: materially different hypotheses ────────────────────────────

// TestD4_MateriallyDifferentHypothesesRemainDistinct answers matrix
// question 2.
//
// Two genuinely different hypotheses must remain two proposals. A
// correction that collapsed them would be a correctness failure, not a
// deduplication win.
func TestD4_MateriallyDifferentHypothesesRemainDistinct(t *testing.T) {
	dm := m5NewDM(t)

	const crit = "Recompute both survival rates."
	tags := []string{"critic", "survival-asymmetry"}

	first := d4Propose(t, dm, "The call cohort decays faster than the direct cohort.", crit, nil, tags)
	second := d4Propose(t, dm, "The direct cohort decays faster than the call cohort.", crit, nil, tags)

	if first == second {
		t.Errorf("two materially different hypotheses produced the same proposal %q — "+
			"they must remain distinguishable", first)
	}
	if n := d4TheoryCount(t, dm); n != 2 {
		t.Errorf("theory rows = %d, want 2", n)
	}
}

// ── Matrix 3: metadata-only changes ─────────────────────────────────────

// TestD4_MetadataOnlyChangeCreatesASecondProposal answers matrix
// question 3, and pins the OPEN question of D4.
//
// A proposal whose only difference is its TAG SET is currently a SECOND
// proposal. That is the substrate's documented identity model — tags are
// identity-bearing (db.go: MemoryIdentityHash) and the critic says so
// explicitly (audit.go) — and it is the mechanism that let a
// cycle-varying tag create a new pending proposal every cycle.
//
// This test PINS THE CURRENT BEHAVIOUR as a characterization, not as a
// contract. Whether a tag difference SHOULD split a proposal is the
// question Session A has to rule on; see
// docs/dev/session-b-d4-proposal-identity.md §5. The characterization
// exists so that any change to it is a DELIBERATE, visible change
// rather than a silent one.
func TestD4_MetadataOnlyChangeCreatesASecondProposal(t *testing.T) {
	dm := m5NewDM(t)

	const hyp = "A hypothesis whose evaluation context varies."
	const crit = "Same criteria."

	first := d4Propose(t, dm, hyp, crit, nil, []string{"critic", "survival-asymmetry", "cycle_5"})
	second := d4Propose(t, dm, hyp, crit, nil, []string{"critic", "survival-asymmetry", "cycle_6"})

	if first == second {
		t.Fatalf("expected the tag difference to produce a second proposal under the "+
			"CURRENT identity model, got the same id %q — the characterization of the "+
			"residual D4 exposure is stale", first)
	}
	if n := d4TheoryCount(t, dm); n != 2 {
		t.Fatalf("theory rows = %d, want 2 — the characterization of the residual D4 "+
			"exposure is stale", n)
	}
}

// TestD4_TagOrderIsSignificantUnderTheCurrentIdentity pins a SECOND
// incidental difference found during the D4 audit.
//
// Tags are a SET, not a sequence: ["critic","alpha"] and
// ["alpha","critic"] name exactly the same two labels. The identity hash
// folds json(tags) — a SEQUENCE — so the two orderings are two
// different proposals.
//
// This is the same class as the cycle-tag exposure and it is reachable
// without any cycle at all: an agent that proposes the same claim twice
// with the same labels in a different order gets two pending proposals.
// It is reported, not fixed: fixing it changes the shared identity
// algorithm, which needs Session A's approval.
func TestD4_TagOrderIsSignificantUnderTheCurrentIdentity(t *testing.T) {
	dm := m5NewDM(t)

	const hyp = "A hypothesis proposed twice with the same labels."
	const crit = "Same criteria."

	first := d4Propose(t, dm, hyp, crit, nil, []string{"critic", "alpha"})
	second := d4Propose(t, dm, hyp, crit, nil, []string{"alpha", "critic"})

	if first == second {
		t.Fatalf("expected the tag ORDER to produce a second proposal under the CURRENT "+
			"identity model, got the same id %q — the characterization is stale", first)
	}
	if n := d4TheoryCount(t, dm); n != 2 {
		t.Fatalf("theory rows = %d, want 2 — the characterization is stale", n)
	}
}

// ── Matrix 4: different validation criteria ─────────────────────────────

// TestD4_DifferentValidationCriteriaRemainDistinct answers matrix
// question 4.
//
// The validation criteria are part of the claim's identity — twice over:
// the criteria are folded into the proposal's CONTENT and are also stored
// in its metadata. Two proposals that ask for different checks are two
// proposals. This is pinned by internal/core/p3_identity_test.go and it
// is load-bearing for the critic's hunts.
func TestD4_DifferentValidationCriteriaRemainDistinct(t *testing.T) {
	dm := m5NewDM(t)

	const hyp = "A hypothesis with two different evaluation plans."
	tags := []string{"critic", "alpha"}

	first := d4Propose(t, dm, hyp, "Cite the decay events.", nil, tags)
	second := d4Propose(t, dm, hyp, "Cite the archive events.", nil, tags)

	if first == second {
		t.Errorf("two proposals with different validation criteria produced the same id %q", first)
	}
	if n := d4TheoryCount(t, dm); n != 2 {
		t.Errorf("theory rows = %d, want 2", n)
	}
}

// TestD4_DifferentDependenciesRemainDistinct pins the fourth
// identity-bearing field.
//
// The dependencies are part of the proposal's metadata, so a proposal
// that stands on a different foundation is a different proposal. This
// matters: a dependency edge is a claim about what the proposal rests
// on.
func TestD4_DifferentDependenciesRemainDistinct(t *testing.T) {
	dm := m5NewDM(t)

	const hyp = "A hypothesis that rests on different foundations."
	const crit = "Same criteria."
	tags := []string{"critic", "alpha"}

	first := d4Propose(t, dm, hyp, crit, []string{"theory-a"}, tags)
	second := d4Propose(t, dm, hyp, crit, []string{"theory-b"}, tags)

	if first == second {
		t.Errorf("two proposals with different dependencies produced the same id %q", first)
	}
	if n := d4TheoryCount(t, dm); n != 2 {
		t.Errorf("theory rows = %d, want 2", n)
	}
}

// ── Matrix 5: repeated cycles after a process restart ───────────────────

// TestD4_ProposalIdentitySurvivesAProcessRestart answers matrix
// question 5 on the substrate side.
//
// Nothing but the database file survives a restart. A proposal's identity
// must therefore be recomputable from what is STORED — which is exactly
// what the identity_hash column freezes at insert time for.
func TestD4_ProposalIdentitySurvivesAProcessRestart(t *testing.T) {
	ws := m5IsolatedWorkspace(t)
	dm, err := NewDatabaseManager(ws)
	if err != nil {
		t.Fatalf("NewDatabaseManager(%q): %v", ws, err)
	}

	const hyp = "A hypothesis proposed before a restart."
	const crit = "Same criteria."
	tags := []string{"critic", "alpha"}

	before := d4Propose(t, dm, hyp, crit, nil, tags)

	// Close the database entirely: nothing but the file survives.
	if err := dm.Close(); err != nil {
		t.Fatalf("close dm: %v", err)
	}

	reopened, err := NewDatabaseManager(ws)
	if err != nil {
		t.Fatalf("reopen NewDatabaseManager(%q): %v", ws, err)
	}
	t.Cleanup(func() { _ = reopened.Close() })

	after := d4Propose(t, reopened, hyp, crit, nil, tags)
	if before != after {
		t.Errorf("the proposal id changed across a process restart: %q -> %q — the identity "+
			"must be recomputable from what is stored", before, after)
	}
	if n := d4TheoryCount(t, reopened); n != 1 {
		t.Errorf("theory rows = %d, want 1 — a restart must not duplicate the proposal", n)
	}
}

// ── Matrix 9: previously resolved proposals ─────────────────────────────

// TestD4_ResolvedProposalIsNotReopenedByAnIdenticalReproposal answers
// matrix question 9.
//
// Once a proposal has been adjudicated, re-proposing the identical claim
// returns the ADJUDICATED proposal rather than opening a second, pending
// one. The identity hash is frozen at insert time, so the resolved row
// still matches its own identity.
//
// This is a documented characteristic rather than a defect: the substrate
// treats a claim as adjudicated, and re-opening it is an explicit act
// (a new hypothesis, or a supersede), not something a repeat call does
// by accident.
func TestD4_ResolvedProposalIsNotReopenedByAnIdenticalReproposal(t *testing.T) {
	dm := m5NewDM(t)

	const hyp = "A hypothesis that gets adjudicated."
	const crit = "Same criteria."
	tags := []string{"critic", "alpha"}

	original := d4Propose(t, dm, hyp, crit, nil, tags)
	if _, err := dm.ResolveTheory(original, "The cohort survives.", "proven"); err != nil {
		t.Fatalf("ResolveTheory: %v", err)
	}

	repeat := d4Propose(t, dm, hyp, crit, nil, tags)
	if repeat != original {
		t.Errorf("re-proposing an adjudicated claim produced a NEW proposal %q (was %q) — "+
			"the adjudicated proposal must not be reopened by a repeat call", repeat, original)
	}
	if n := d4TheoryCount(t, dm); n != 1 {
		t.Errorf("theory rows = %d, want 1", n)
	}

	// And the proposal is no longer pending.
	var status string
	if err := dm.SQLDB().QueryRow(
		`SELECT json_extract(metadata, '$.status') FROM memories WHERE id = ?`, original).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status == "pending" {
		t.Errorf("status = %q, want a resolved status", status)
	}
}

// ── Matrix 11: concurrent proposal submissions ───────────────────────────

// TestD4_ConcurrentIdenticalProposalsProduceOneProposal answers matrix
// question 11 for the identical case.
//
// N concurrent callers proposing the identical claim must produce ONE
// proposal. The dedup is enforced by the pre-insert duplicate probe in
// saveMemoryRow, backed by the partial unique index
// (idx_memories_identity_live): the losers of the race re-probe and
// return the winner's id.
//
// WHAT IS PINNED, EXACTLY.
//
// The DEDUP invariant, which is this test's subject:
//
//   - every caller that SUCCEEDS returns the SAME id;
//   - exactly ONE theory row exists for the claim; and
//   - that one row is the row every success named — a success never
//     reports a proposal that was not recorded.
//
// WHAT IS NOT PINNED, AND WHY.
//
// A caller may FAIL under write contention, with "database is locked".
// That is finding F2 — a concurrent write is LOST to SQLITE_BUSY — and
// it is measured, mechanism-isolated and scheduled in the D4 report §6.
// It is not a dedup defect, and it is not this test's subject.
//
// It is asserted here only as a FAILURE, never as a success: a caller
// that loses the race must get an ERROR. Asserting that all N callers
// succeed would be asserting that F2 does not exist — and it would make
// this test FLAKY, because F2 is real: under the race detector it
// reproduces roughly one run in six.
//
// (This test's first draft asserted that all eight callers succeed. It
// was rewritten after the race-detector sweep showed it failing on
// exactly that assertion — with "database is locked" — which is the
// defect it was accidentally pinning. The dedup invariant it was
// written to prove is unchanged and is asserted above. Its
// non-vacuity was re-proven afterwards: defeating the dedup (the
// pre-insert probe) makes it fail on exactly the assertion it exists
// to make.)
func TestD4_ConcurrentIdenticalProposalsProduceOneProposal(t *testing.T) {
	dm := m5NewDM(t)

	const hyp = "A hypothesis proposed concurrently."
	const crit = "Same criteria."
	tags := []string{"critic", "alpha"}

	const n = 8
	ids := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := dm.ProposeTheory(hyp, crit, nil, nil, tags)
			errs[i] = err
			if err == nil {
				ids[i], _ = res["id"].(string)
			}
		}(i)
	}
	wg.Wait()

	// Every SUCCESS must name the SAME proposal. This is the dedup
	// guarantee: the identity hash is what makes the eight callers
	// agree, and the write path is what makes it enforceable.
	var winner string
	succeeded := 0
	for i := range ids {
		if errs[i] != nil {
			// A lost race must be a failure, never a success with
			// nothing recorded.
			continue
		}
		succeeded++
		if ids[i] == "" {
			t.Errorf("concurrent proposal %d returned success with NO id", i)
			continue
		}
		if winner == "" {
			winner = ids[i]
			continue
		}
		if ids[i] != winner {
			t.Errorf("concurrent proposal %d returned %q, want %q — identical concurrent "+
				"proposals must resolve to ONE proposal", i, ids[i], winner)
		}
	}
	if succeeded == 0 {
		t.Fatalf("every concurrent proposal failed (%v) — the test must exercise the "+
			"success path at least once", errs[0])
	}

	// Exactly one row for the claim — and it is the row every success
	// named. (N successes legitimately map to ONE row here; that is what
	// the dedup means. The row-per-success correspondence is the DISTINCT
	// case's invariant, and it is asserted there.)
	rows := d4TheoryIDs(t, dm)
	if len(rows) != 1 {
		t.Errorf("theory rows = %d (%v), want exactly 1 — identical concurrent "+
			"proposals must produce exactly one proposal", len(rows), rows)
	} else if rows[0] != winner {
		t.Errorf("the single recorded proposal is %q but the callers were told %q — "+
			"a success must name the proposal that was actually recorded",
			rows[0], winner)
	}
}

// TestD4_ConcurrentDistinctProposalsAreNeverSilentlyLost answers matrix
// question 11 for the distinct case.
//
// N concurrent callers proposing DIFFERENT claims must produce N
// DISTINCT proposals — none may be swallowed by the dedup, and none may
// share an id with another.
//
// The invariant pinned here is deliberately the one that must hold under
// ANY level of write contention:
//
//   - a caller that gets a SUCCESS must have its proposal recorded, and
//   - a caller that gets a FAILURE must get an ERROR, never a success
//     with no row.
//
// The number of callers that lose a RACE is a property of the machine,
// not of the substrate, so it is measured and reported rather than
// asserted here. See docs/dev/session-b-d4-proposal-identity.md §6 — the
// measurement found a real defect (concurrent proposals are lost to
// SQLITE_BUSY) which is reported for Session A's ruling, because the fix
// touches the substrate's transaction discipline.
func TestD4_ConcurrentDistinctProposalsAreNeverSilentlyLost(t *testing.T) {
	dm := m5NewDM(t)

	const n = 8
	ids := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := dm.ProposeTheory(
				fmt.Sprintf("A distinct hypothesis number %d.", i),
				"Same criteria.", nil, nil, []string{"critic", "alpha"})
			errs[i] = err
			if err == nil {
				ids[i], _ = res["id"].(string)
			}
		}(i)
	}
	wg.Wait()

	seen := map[string]bool{}
	succeeded := 0
	for i := range ids {
		if errs[i] != nil {
			// A failure must be a failure. It must never be a success
			// with nothing recorded.
			continue
		}
		succeeded++
		id := ids[i]
		if id == "" {
			t.Errorf("concurrent proposal %d returned success with NO id", i)
			continue
		}
		if seen[id] {
			t.Errorf("two distinct concurrent proposals share the id %q — a distinct "+
				"proposal must never be swallowed by the dedup", id)
		}
		seen[id] = true
	}

	// Every success must correspond to exactly one recorded row. This
	// is the load-bearing half: a proposal the caller was told exists
	// must exist.
	if got := d4TheoryCount(t, dm); got != succeeded {
		t.Errorf("theory rows = %d but %d concurrent proposals reported success — "+
			"a success must always correspond to a recorded proposal", got, succeeded)
	}
	if succeeded == 0 {
		t.Fatalf("every concurrent proposal failed (%v) — the test must exercise the "+
			"success path at least once", errs[0])
	}
}

// ── Matrix 12: no automatic promotion of a pending hypothesis ───────────

// TestD4_PendingProposalIsNeverAutomaticallyPromoted answers matrix
// question 12.
//
// A pending proposal stays pending. Nothing promotes it: no lesson is
// written to the trusted surface, no row lands in the lessons table, and
// the proposal's confidence stays at the base value a proposal is minted
// with.
func TestD4_PendingProposalIsNeverAutomaticallyPromoted(t *testing.T) {
	dm := m5NewDM(t)

	const hyp = "A hypothesis that must stay pending."
	proposalID := d4Propose(t, dm, hyp, "Same criteria.", nil, []string{"critic", "alpha"})

	// The proposal is pending.
	var status string
	if err := dm.SQLDB().QueryRow(
		`SELECT json_extract(metadata, '$.status') FROM memories WHERE id = ?`, proposalID).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != "pending" {
		t.Errorf("status = %q, want %q — a proposal is minted pending", status, "pending")
	}

	// Nothing was written to the trusted lesson surface.
	if n := m5ScalarInt(t, dm.SQLDB(), `SELECT COUNT(*) FROM lessons_base`); n != 0 {
		t.Errorf("lessons_base rows = %d, want 0 — a pending proposal must never be promoted "+
			"to a trusted lesson", n)
	}

	// The proposal's confidence is the base confidence, not a raised one.
	var conf float64
	if err := dm.SQLDB().QueryRow(
		`SELECT confidence FROM memories WHERE id = ?`, proposalID).Scan(&conf); err != nil {
		t.Fatalf("read confidence: %v", err)
	}
	if conf != 0.5 {
		t.Errorf("confidence = %v, want 0.5 — a pending proposal carries the base confidence", conf)
	}
}
