// contextual_candidates_supersession_test.go — Stage 2D supersession
// representation tests for the supersession-chain repair (the
// cross-agent continuity defect closed at this commit).
//
// Verifies that for every authoritative supersession path the
// generator emits both the historical predecessor AND the
// canonical successor with correct lifecycle states and chain
// reasons, so Stage 2E.1's compressSupersessionChain can fire
// without Stage 2D having to invent canonical state.

package internal

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// supersessionHasReason returns true if the candidate carries `r`
// in Reasons. Renamed to avoid collision with the existing
// hasReason helper in contextual_candidates_test.go.
func supersessionHasReason(c Candidate, r string) bool {
	for _, cr := range c.Reasons {
		if cr == r {
			return true
		}
	}
	return false
}

// supersessionHasRelated returns true if the candidate's
// RelatedIDs include id.
func supersessionHasRelated(c Candidate, id string) bool {
	for _, rid := range c.RelatedIDs {
		if rid == id {
			return true
		}
	}
	return false
}

// seedTheorySupersession inserts: theoryA (active), theoryB
// (superseded, points to A via metadata.superseded_by), and a
// confidence_history row marking B as superseded by A.
func seedTheorySupersession(t *testing.T, dm *DatabaseManager, a, b string) {
	t.Helper()
	now := time.Now().Unix()
	_, err := dm.SQLDB().Exec(`
		INSERT INTO memories (id, collection, content, created_at, updated_at, tags, metadata, source_id, source_db)
		VALUES (?, 'theories', ?, ?, ?, '["pending"]', '{}', '', 'openclaw')
	`, a, "active successor: "+a, now, now)
	require.NoError(t, err)
	_, err = dm.SQLDB().Exec(`
		INSERT INTO memories (id, collection, content, created_at, updated_at, tags, metadata, source_id, source_db)
		VALUES (?, 'theories', ?, ?, ?, '["superseded","superseded-by:`+a+`"]',
		        json_object('superseded_by', ?), '', 'openclaw')
	`, b, "historical predecessor: "+b, now, now, a)
	require.NoError(t, err)
	_, err = dm.SQLDB().Exec(`
		INSERT INTO confidence_history (id, artifact_id, artifact_type, confidence, computed_at, evidence_count, trigger)
		VALUES ('ch-test-`+b+`', ?, 'theory', 0.1, ?, 0, 'supersede')
	`, b, now)
	require.NoError(t, err)
}

// seedTheoryChain inserts a 3-link chain A -> B -> C where C is the
// current canonical successor.
func seedTheoryChain(t *testing.T, dm *DatabaseManager, a, b, c string) {
	t.Helper()
	now := time.Now().Unix()
	// C is active.
	_, err := dm.SQLDB().Exec(`
		INSERT INTO memories (id, collection, content, created_at, updated_at, tags, metadata, source_id, source_db)
		VALUES (?, 'theories', ?, ?, ?, '["pending"]', '{}', '', 'openclaw')
	`, c, "current canonical: "+c, now, now)
	require.NoError(t, err)
	// B is superseded by C.
	_, err = dm.SQLDB().Exec(`
		INSERT INTO memories (id, collection, content, created_at, updated_at, tags, metadata, source_id, source_db)
		VALUES (?, 'theories', ?, ?, ?, '["superseded","superseded-by:`+c+`"]',
		        json_object('superseded_by', ?), '', 'openclaw')
	`, b, "intermediate: "+b, now, now, c)
	require.NoError(t, err)
	// A is superseded by B.
	_, err = dm.SQLDB().Exec(`
		INSERT INTO memories (id, collection, content, created_at, updated_at, tags, metadata, source_id, source_db)
		VALUES (?, 'theories', ?, ?, ?, '["superseded","superseded-by:`+b+`"]',
		        json_object('superseded_by', ?), '', 'openclaw')
	`, a, "obsolete: "+a, now, now, b)
	require.NoError(t, err)
	// Confidence history for both supersede events.
	_, err = dm.SQLDB().Exec(`
		INSERT INTO confidence_history (id, artifact_id, artifact_type, confidence, computed_at, evidence_count, trigger)
		VALUES ('ch-a-`+a+`', ?, 'theory', 0.1, ?, 0, 'supersede')
	`, a, now)
	require.NoError(t, err)
	_, err = dm.SQLDB().Exec(`
		INSERT INTO confidence_history (id, artifact_id, artifact_type, confidence, computed_at, evidence_count, trigger)
		VALUES ('ch-b-`+b+`', ?, 'theory', 0.1, ?, 0, 'supersede')
	`, b, now)
	require.NoError(t, err)
}

// ── A. SIMPLE chain A → B ────────────────────────────────────────

func TestStage2D_Supersession_SimpleChain_AB(t *testing.T) {
	dm := NewTestDM(t)
	seedTheorySupersession(t, dm, "T-A", "T-B")

	res, err := dm.GenerateContextualCandidates(ContextQuery{})
	require.NoError(t, err)

	byID := map[string]Candidate{}
	for _, c := range res.Candidates {
		byID[c.ID] = c
	}

	// Predecessor (B) must surface with lifecycle_state="historical",
	// foundation_superseded, supersession_chain, and RelatedIDs={A}.
	predecessor, ok := byID["theory:T-B"]
	require.True(t, ok, "predecessor T-B must surface as a candidate")
	require.Equal(t, "historical", predecessor.LifecycleState,
		"predecessor T-B must have lifecycle_state=historical")
	require.True(t, supersessionHasReason(predecessor, "foundation_superseded"),
		"predecessor T-B carries foundation_superseded")
	require.True(t, supersessionHasReason(predecessor, "supersession_chain"),
		"predecessor T-B carries supersession_chain")
	require.True(t, supersessionHasRelated(predecessor, "T-A"),
		"predecessor T-B RelatedIDs includes T-A")

	// Successor (A) must surface with lifecycle_state="canonical"
	// and supersession_chain.
	successor, ok := byID["theory:T-A"]
	require.True(t, ok, "successor T-A must surface as its own candidate")
	require.Equal(t, "canonical", successor.LifecycleState,
		"successor T-A must have lifecycle_state=canonical")
	require.True(t, supersessionHasReason(successor, "supersession_chain"),
		"successor T-A carries supersession_chain")
	require.True(t, supersessionHasRelated(successor, "T-B"),
		"successor T-A RelatedIDs includes T-B (predecessor)")
}

// ── B. MULTI-HOP chain A → B → C ─────────────────────────────────

func TestStage2D_Supersession_MultiHop_ABC(t *testing.T) {
	dm := NewTestDM(t)
	seedTheoryChain(t, dm, "T-A", "T-B", "T-C")

	res, err := dm.GenerateContextualCandidates(ContextQuery{})
	require.NoError(t, err)

	byID := map[string]Candidate{}
	for _, c := range res.Candidates {
		byID[c.ID] = c
	}

	// All three must surface.
	for _, id := range []string{"theory:T-A", "theory:T-B", "theory:T-C"} {
		_, ok := byID[id]
		require.True(t, ok, "%s must surface as a candidate", id)
	}

	// Lifecycle states.
	require.Equal(t, "historical", byID["theory:T-A"].LifecycleState,
		"T-A (deepest obsolete) must be historical")
	require.Equal(t, "historical", byID["theory:T-B"].LifecycleState,
		"T-B (intermediate superseded) must be historical")
	require.Equal(t, "canonical", byID["theory:T-C"].LifecycleState,
		"T-C (current canonical successor) must be canonical")

	// T-C must carry supersession_chain (it is the chain's
	// canonical endpoint).
	require.True(t, supersessionHasReason(byID["theory:T-C"], "supersession_chain"),
		"T-C must carry supersession_chain")
}

// ── C. Successor already surfaced by another source ─────────────

func TestStage2D_Supersession_SuccessorAlreadyPresent_NoDuplicate(t *testing.T) {
	dm := NewTestDM(t)
	now := time.Now().Unix()

	// Insert a work item that explicitly references the
	// successor T-A so T-A is already a candidate from the
	// work-source path.
	_, err := dm.SQLDB().Exec(`
		INSERT INTO works (id, title, status, verification, created_at, updated_at, session_id)
		VALUES ('W-A-refs', 'work that references T-A', 'open', 'unverified', ?, ?, '')
	`, now, now)
	require.NoError(t, err)

	// Seed the supersession chain A (canonical) -> B (historical).
	seedTheorySupersession(t, dm, "T-A", "T-B")

	res, err := dm.GenerateContextualCandidates(
		ContextQuery{WorkIDs: []string{"W-A-refs"}})
	require.NoError(t, err)

	// Count candidates by id (deduped).
	countByID := map[string]int{}
	for _, c := range res.Candidates {
		countByID[c.ID]++
	}
	require.Equal(t, 1, countByID["theory:T-A"],
		"T-A must not be duplicated even when surfaced through work+supersession")
	require.Equal(t, 1, countByID["theory:T-B"],
		"T-B must surface exactly once")

	// Lifecycle state on the deduplicated T-A: canonical.
	tA := Candidate{}
	for _, c := range res.Candidates {
		if c.ID == "theory:T-A" {
			tA = c
			break
		}
	}
	require.Equal(t, "canonical", tA.LifecycleState,
		"T-A lifecycle must remain canonical after dedup")
	require.True(t, supersessionHasReason(tA, "supersession_chain"),
		"T-A must carry supersession_chain after dedup")
}

// ── D. Missing successor (broken chain) ────────────────────────

func TestStage2D_Supersession_MissingSuccessor(t *testing.T) {
	dm := NewTestDM(t)

	// Insert a theory that claims superseded_by but does not
	// actually point at any existing theory.
	now := time.Now().Unix()
	_, err := dm.SQLDB().Exec(`
		INSERT INTO memories (id, collection, content, created_at, updated_at, tags, metadata, source_id, source_db)
		VALUES ('T-orphan', 'theories', 'orphan theory with missing successor', ?, ?,
		        '["superseded","superseded-by:T-DOES-NOT-EXIST"]',
		        json_object('superseded_by', 'T-DOES-NOT-EXIST'), '', 'openclaw')
	`, now, now)
	require.NoError(t, err)
	_, err = dm.SQLDB().Exec(`
		INSERT INTO confidence_history (id, artifact_id, artifact_type, confidence, computed_at, evidence_count, trigger)
		VALUES ('ch-orphan', 'T-orphan', 'theory', 0.1, ?, 0, 'supersede')
	`, now)
	require.NoError(t, err)

	// Generator must NOT panic, NOT invent a canonical candidate.
	res, err := dm.GenerateContextualCandidates(ContextQuery{})
	require.NoError(t, err)

	for _, c := range res.Candidates {
		if c.Kind == "theory" {
			require.NotContains(t, c.ID, "T-DOES-NOT-EXIST",
				"generator must not fabricate a canonical successor")
		}
	}

	// Predecessor itself must still surface (as historical).
	for _, c := range res.Candidates {
		if c.ID == "theory:T-orphan" {
			require.Equal(t, "historical", c.LifecycleState)
			require.True(t, supersessionHasReason(c, "foundation_superseded"))
		}
	}
}

// ── E. Cycle ─────────────────────────────────────────────────────

func TestStage2D_Supersession_Cycle(t *testing.T) {
	dm := NewTestDM(t)
	now := time.Now().Unix()

	// A and B each claim the other as superseded_by — a cycle.
	for _, id := range []string{"T-cycle-A", "T-cycle-B"} {
		other := "T-cycle-B"
		if id == "T-cycle-B" {
			other = "T-cycle-A"
		}
		_, err := dm.SQLDB().Exec(`
			INSERT INTO memories (id, collection, content, created_at, updated_at, tags, metadata, source_id, source_db)
			VALUES (?, 'theories', ?, ?, ?, '["superseded","superseded-by:`+other+`"]',
			        json_object('superseded_by', ?), '', 'openclaw')
		`, id, "cycle "+id, now, now, other)
		require.NoError(t, err)
	}
	_, err := dm.SQLDB().Exec(`
		INSERT INTO confidence_history (id, artifact_id, artifact_type, confidence, computed_at, evidence_count, trigger)
		VALUES ('ch-cycle-A', 'T-cycle-A', 'theory', 0.1, ?, 0, 'supersede')
	`, now)
	require.NoError(t, err)
	_, err = dm.SQLDB().Exec(`
		INSERT INTO confidence_history (id, artifact_id, artifact_type, confidence, computed_at, evidence_count, trigger)
		VALUES ('ch-cycle-B', 'T-cycle-B', 'theory', 0.1, ?, 0, 'supersede')
	`, now)
	require.NoError(t, err)

	// Generator must terminate (bounded) and not panic.
	res, err := dm.GenerateContextualCandidates(ContextQuery{})
	require.NoError(t, err)
	require.LessOrEqual(t, len(res.Candidates), 32,
		"generator must remain bounded under cycles (max 32 from default caps)")

	// Neither cycle member may be declared canonical by the
	// generator (no fabricated canonical state).
	for _, c := range res.Candidates {
		if c.Kind == "theory" {
			require.NotEqual(t, "canonical", c.LifecycleState,
				"cycle must not produce canonical lifecycle state")
		}
	}
}

// ── F. Decision supersession ────────────────────────────────────

func TestStage2D_Supersession_Decision(t *testing.T) {
	dm := NewTestDM(t)
	now := time.Now().Unix()

	// Decisions also use memories table with collection='decisions'.
	// The current epistemic source only treats theories specially;
	// decisions still surface via confidence_history supersede.
	_, err := dm.SQLDB().Exec(`
		INSERT INTO memories (id, collection, content, created_at, updated_at, tags, metadata, source_id, source_db)
		VALUES ('D-successor', 'decisions', 'current canonical decision', ?, ?, '[]', '{}', '', 'openclaw')
	`, now, now)
	require.NoError(t, err)
	_, err = dm.SQLDB().Exec(`
		INSERT INTO memories (id, collection, content, created_at, updated_at, tags, metadata, source_id, source_db)
		VALUES ('D-predecessor', 'decisions', 'historical decision',
		        ?, ?, '["superseded","superseded-by:D-successor"]',
		        json_object('superseded_by', 'D-successor'), '', 'openclaw')
	`, now, now)
	require.NoError(t, err)
	_, err = dm.SQLDB().Exec(`
		INSERT INTO confidence_history (id, artifact_id, artifact_type, confidence, computed_at, evidence_count, trigger)
		VALUES ('ch-d-pred', 'D-predecessor', 'decision', 0.1, ?, 0, 'supersede')
	`, now)
	require.NoError(t, err)

	res, err := dm.GenerateContextualCandidates(ContextQuery{})
	require.NoError(t, err)

	byID := map[string]Candidate{}
	for _, c := range res.Candidates {
		byID[c.ID] = c
	}

	predecessor, ok := byID["decision:D-predecessor"]
	require.True(t, ok, "decision predecessor must surface")
	require.Equal(t, "historical", predecessor.LifecycleState,
		"decision predecessor lifecycle_state=historical")

	successor, ok := byID["decision:D-successor"]
	require.True(t, ok, "decision successor must surface as its own candidate")
	require.Equal(t, "canonical", successor.LifecycleState,
		"decision successor lifecycle_state=canonical")
}
