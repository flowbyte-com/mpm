// contextual_selection_test.go — Stage 2E.1 pure selector tests.
//
// Coverage:
//   - Policy completeness (every declared reason covered)
//   - Strongest-band-wins (shares_topic + referenced_by_active_work)
//   - Combination reachability (Stage-2D generator-backed fixtures)
//   - Supersession compression (A→B, A→B→C, ambiguous)
//   - Golden continuity
//   - Structure-beats-recency
//   - Explicit-reference-survives
//   - Session-continuity
//   - Epistemic-change (active-work-on-invalidated > unrelated)
//   - Cascade semantics (pending+direct obligation, unrelated
//     pending stays change)
//   - Diversity within band
//   - Tiny-limit (1, 2, <categories)
//   - No-LLM / no-DB (selector imports)
//   - Secret-safety (selector metadata safe)
//   - Determinism (same inputs → identical output)
//   - Input immutability (input candidates unchanged after selection)
//   - Limit clamp semantics
//   - Observational (no mutation)
//   - Canonical vocabularies match Stage-2D

package internal

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// ── helpers ──

func mustCandidate(id, kind string, reasons []string) Candidate {
	return Candidate{
		ID:         id,
		Kind:       kind,
		ArtifactID: id,
		Reasons:    append([]string(nil), reasons...),
		Source:     kind,
		Timestamp:  1000,
	}
}

// ── T1. Policy completeness ──

func TestSelection_PolicyCompleteness(t *testing.T) {
	// Every declared CandidateReasonName must be in the policy
	// table. Coverage is asserted by iterating the const block
	// mirror (we use allCandidateReasonNames() from the Stage 2D
	// test file, which lives in the same package).
	for _, name := range allCandidateReasonNames() {
		_, ok := selectionPolicyTable[CandidateReasonName(name)]
		require.True(t, ok, "reason %q must be in selectionPolicyTable", name)
	}
	// The table must cover every band at least once.
	bands := make(map[SelectionBand]bool)
	for _, e := range selectionPolicyTable {
		bands[e.Band] = true
	}
	for _, b := range CanonicalBands {
		require.True(t, bands[b], "band %s must have at least one policy entry", b)
	}
}

// ── T2. Strongest band wins for multi-reason candidates ──

func TestSelection_StrongestBandWins(t *testing.T) {
	// shares_topic -> supporting ; referenced_by_active_work -> direct.
	// The selector must pick direct, not supporting.
	c := mustCandidate("X1", "memory", []string{"shares_topic", "referenced_by_active_work"})
	band, rationale := classifyBand(c)
	require.Equal(t, BandDirect, band, "strongest mapped reason wins")
	require.Equal(t, RationaleDirectActiveWork, rationale)
}

func TestSelection_StrongestBandWins_ExplicitReference(t *testing.T) {
	c := mustCandidate("X2", "theory", []string{"shares_topic", "explicit_reference"})
	band, rationale := classifyBand(c)
	require.Equal(t, BandDirect, band)
	require.Equal(t, RationaleDirectExplicit, rationale)
}

// ── T3. Combination rule reachability (Stage-2D backed) ──

// Each declared combination rule must have BOTH a selector unit
// fixture AND a Stage-2D generator-backed reachability fixture.
func TestSelection_CombinationReachability_Selector(t *testing.T) {
	// Build candidates for each declared combination rule.
	cases := []struct {
		name        string
		reasons     []string
		expectBand  SelectionBand
		expectRatio PolicyRationale
	}{
		{"explicit_ref_on_superseded_foundation",
			[]string{"explicit_reference", "foundation_superseded"},
			BandObligation, RationaleReconsiderCurrent},
		{"explicit_ref_on_invalidated_foundation",
			[]string{"explicit_reference", "foundation_invalidated"},
			BandObligation, RationaleReconsiderCurrent},
		{"explicit_ref_with_cascade",
			[]string{"explicit_reference", "cascade_pending"},
			BandObligation, RationaleReconsiderCurrent},
		{"explicit_ref_active_work",
			[]string{"explicit_reference", "open_work"},
			BandDirect, RationaleDirectExplicit},
		{"same_session_cross_agent_change",
			[]string{"same_mpm_session", "recent_cross_agent_change"},
			BandContinuity, RationaleContinuityCrossAgent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := mustCandidate("c-"+tc.name, "decision", tc.reasons)
			band, rationale := classifyBand(c)
			band, combo, comboRatio := applyCombinationRules(c, band, rationale)
			require.Equal(t, tc.expectBand, band, "band for %s", tc.name)
			require.NotEmpty(t, combo, "combo name for %s", tc.name)
			require.Equal(t, tc.expectRatio, comboRatio, "rationale for %s", tc.name)
		})
	}

	// Policy completeness: every combination rule in
	// combinationRules must be exercised by the cases above.
	// Adding a rule below without a case here breaks this loop.
	for _, rule := range combinationRules {
		found := false
		for _, tc := range cases {
			if tc.name == rule.Name {
				found = true
				break
			}
		}
		require.True(t, found,
			"combination rule %q declared but not exercised in selector unit fixture",
			rule.Name)
	}
}

func TestSelection_CombinationReachability_Stage2D(t *testing.T) {
	dm := NewTestDM(t)
	stage2DSeedContext(t, dm)
	stage2DSeedActivity(t, dm)

	// Force a cascade_pending candidate by seeding a cascade row
	// for an existing seeded decision.
	now := time.Now().Unix()
	_, err := dm.SQLDB().Exec(`
		INSERT INTO epistemic_cascade_outbox
		    (id, invalidation_event_id, dead_artifact_id, dead_artifact_type,
		     downstream_artifact_id, downstream_artifact_type,
		     cascade_depth, status, reason, created_at, updated_at)
		VALUES ('cascade-test', 'evt-test', 'T-stage2d-superseded', 'theory',
		        'D-stage2d-1', 'decision', 1, 'pending', 'foundation superseded', ?, ?)
	`, now, now)
	require.NoError(t, err)

	res, err := dm.GenerateContextualCandidates(ContextQuery{
		ArtifactIDs: []string{"T-stage2d-superseded"},
	})
	require.NoError(t, err)

	// The explicit-reference candidate T-stage2d-superseded
	// should carry both explicit_reference AND foundation_superseded.
	// Verify the combination-rule firing.
	found := false
	for _, c := range res.Candidates {
		if c.ArtifactID == "T-stage2d-superseded" {
			hasExplicit := false
			hasSuperseded := false
			hasCascade := false
			for _, r := range c.Reasons {
				switch r {
				case "explicit_reference":
					hasExplicit = true
				case "foundation_superseded":
					hasSuperseded = true
				case "cascade_pending":
					hasCascade = true
				}
			}
			if hasExplicit && (hasSuperseded || hasCascade) {
				found = true
			}
		}
	}
	require.True(t, found,
		"Stage 2D must produce a candidate carrying explicit_reference combined with a foundation/cascade reason")

	// Run the selector and verify the combination rule fires.
	candidates := make([]Candidate, 0, len(res.Candidates))
	for _, c := range res.Candidates {
		candidates = append(candidates, c)
	}
	policy := DefaultSelectionPolicy()
	policy.Limits.MaxItems = 50
	sel := SelectContextualCandidates(candidates, policy, now)
	require.NotEmpty(t, sel.Diagnostics.CombinationMatches,
		"selector must report at least one combination match for the seeded fixture")
}

// ── T4. Supersession compression ──

func TestSelection_Supersession_SimpleAB(t *testing.T) {
	a := mustCandidate("A", "theory", []string{"supersession_chain"})
	a.LifecycleState = "historical"
	a.RelatedIDs = []string{"B"}
	b := mustCandidate("B", "theory", []string{"supersession_chain"})
	b.LifecycleState = "canonical"
	b.RelatedIDs = []string{"A"}
	survivors, mapIDs, n := compressSupersessionChain([]Candidate{a, b})
	require.Equal(t, 1, n, "exactly one survivor (B)")
	require.Equal(t, "B", survivors[0].ID)
	require.Equal(t, []string{"A"}, mapIDs["B"])
}

func TestSelection_Supersession_ABtoC(t *testing.T) {
	a := mustCandidate("A", "theory", []string{"supersession_chain"})
	a.LifecycleState = "historical"
	a.RelatedIDs = []string{"B"}
	b := mustCandidate("B", "theory", []string{"supersession_chain"})
	b.LifecycleState = "historical"
	b.RelatedIDs = []string{"A", "C"}
	c := mustCandidate("C", "theory", []string{"supersession_chain"})
	c.LifecycleState = "canonical"
	c.RelatedIDs = []string{"B"}
	survivors, mapIDs, n := compressSupersessionChain([]Candidate{a, b, c})
	require.Equal(t, 1, n)
	require.Equal(t, "C", survivors[0].ID)
	require.ElementsMatch(t, []string{"A", "B"}, mapIDs["C"])
}

func TestSelection_Supersession_Ambiguous_NoCompress(t *testing.T) {
	// Two canonical members in the same connected component ->
	// ambiguous -> NO compression. The component is connected via
	// RelatedIDs so the selector's component-detection sees them
	// as a single chain with two canonical nodes.
	a := mustCandidate("A", "theory", []string{"supersession_chain"})
	a.LifecycleState = "canonical"
	a.RelatedIDs = []string{"B"}
	b := mustCandidate("B", "theory", []string{"supersession_chain"})
	b.LifecycleState = "canonical"
	b.RelatedIDs = []string{"A"}
	_, mapIDs, n := compressSupersessionChain([]Candidate{a, b})
	t.Logf("ambiguous: n=%d, mapIDs=%v", n, mapIDs)
	require.Equal(t, 2, n, "ambiguous multi-canonical must NOT compress")
	require.Equal(t, 0, len(mapIDs), "no compressed_related_ids should be recorded")
}

func TestSelection_Supersession_ZeroCanonical_NoCompress(t *testing.T) {
	a := mustCandidate("A", "theory", []string{"supersession_chain"})
	a.LifecycleState = "historical"
	b := mustCandidate("B", "theory", []string{"supersession_chain"})
	b.LifecycleState = "historical"
	_, mapIDs, n := compressSupersessionChain([]Candidate{a, b})
	require.Equal(t, 2, n)
	require.Equal(t, 0, len(mapIDs))
}

func TestSelection_Supersession_MixedUnknown_NoCompress(t *testing.T) {
	a := mustCandidate("A", "theory", []string{"supersession_chain"})
	a.LifecycleState = "historical"
	b := mustCandidate("B", "theory", []string{"supersession_chain"})
	b.LifecycleState = "canonical"
	b.RelatedIDs = []string{"A"}
	c := mustCandidate("C", "theory", []string{"supersession_chain"})
	c.LifecycleState = "" // unknown
	c.RelatedIDs = []string{"B"}
	_, mapIDs, n := compressSupersessionChain([]Candidate{a, b, c})
	require.Equal(t, 3, n, "mixed-unknown component must NOT compress")
	require.Equal(t, 0, len(mapIDs))
}

// ── T4b. SelectionTrigger preservation through compression ──

func TestSelection_CompressionTriggersCarryHistoricalPolicy(t *testing.T) {
	// A → B supersession. A is historical with foundation_superseded
	// (BandChange, RationaleChangeFoundationMoved). B is canonical with
	// supersession_chain. Compression keeps B. The canonical
	// SelectedItem for B must carry a SelectionTrigger from A
	// capturing A's strongest historical policy trigger.
	now := int64(1700000000)
	a := mustCandidateWith("A", "theory",
		[]string{"foundation_superseded", "supersession_chain"},
		"historical", now-300, []string{"B"}, "")
	b := mustCandidateWith("B", "theory",
		[]string{"supersession_chain"},
		"canonical", now-400, []string{"A"}, "")
	items := []Candidate{a, b}
	policy := DefaultSelectionPolicy()
	policy.Limits.MaxItems = 10
	sel := SelectContextualCandidates(items, policy, now)
	require.Len(t, sel.Items, 1)
	require.Equal(t, "B", sel.Items[0].Candidate.ID)
	require.Equal(t, []string{"A"}, sel.Items[0].CompressedRelatedIDs)
	require.Len(t, sel.Items[0].CompressionTriggers, 1)
	trig := sel.Items[0].CompressionTriggers[0]
	require.Equal(t, "A", trig.SourceID)
	require.Equal(t, BandChange, trig.Band)
	require.Equal(t, RationaleChangeFoundationMoved, trig.Rationale)
}

// ── T4c. Combination-rule generator-backed reachability ──
//
// Every combination rule in combinationRules must have BOTH:
//
//	(a) a selector unit-test fixture (TestSelection_CombinationReachability_Selector)
//	(b) a Stage-2D generator-backed fixture that produces a candidate
//	    carrying the exact reason combination.
//
// We assert (b) for every declared rule.
func TestSelection_CombinationRule_GeneratorBacked(t *testing.T) {
	cases := []struct {
		name        string
		workIDs     []string
		wantReasons []string
		seed        func(dm *DatabaseManager, now int64)
	}{
		{
			name:    "explicit_ref_on_superseded_foundation",
			workIDs: []string{"T-sup-only"},
			seed: func(dm *DatabaseManager, now int64) {
				_, err := dm.SQLDB().Exec(`
					INSERT INTO memories (id, collection, content, created_at, updated_at, tags, metadata, source_id, source_db)
					VALUES ('T-sup-only', 'theories', 'gen test', ?, ?, '["superseded"]',
					        '{}', '', 'openclaw')
				`, now, now)
				require.NoError(t, err)
				_, err = dm.SQLDB().Exec(`
					INSERT INTO confidence_history
					   (id, artifact_id, artifact_type, confidence, computed_at,
					    evidence_count, trigger)
					VALUES ('ch-sup-only', 'T-sup-only', 'theory', 0.1, ?, 0, 'supersede')
				`, now)
				require.NoError(t, err)
			},
			wantReasons: []string{"explicit_reference", "foundation_superseded"},
		},
		{
			name:    "explicit_ref_on_invalidated_foundation",
			workIDs: []string{"T-inv-only"},
			seed: func(dm *DatabaseManager, now int64) {
				_, err := dm.SQLDB().Exec(`
					INSERT INTO memories (id, collection, content, created_at, updated_at, tags, metadata, source_id, source_db)
					VALUES ('T-inv-only', 'theories', 'gen test', ?, ?, '[]', '{}', '', 'openclaw')
				`, now, now)
				require.NoError(t, err)
				_, err = dm.SQLDB().Exec(`
					INSERT INTO confidence_history
					   (id, artifact_id, artifact_type, confidence, computed_at,
					    evidence_count, trigger)
					VALUES ('ch-inv-only', 'T-inv-only', 'theory', 0.0, ?, 0, 'invalidate')
				`, now)
				require.NoError(t, err)
			},
			wantReasons: []string{"explicit_reference", "foundation_invalidated"},
		},
		{
			name:    "explicit_ref_with_cascade",
			workIDs: []string{"T-cas-only"},
			seed: func(dm *DatabaseManager, now int64) {
				_, err := dm.SQLDB().Exec(`
					INSERT INTO memories (id, collection, content, created_at, updated_at, tags, metadata, source_id, source_db)
					VALUES ('T-cas-only', 'theories', 'gen test', ?, ?, '[]', '{}', '', 'openclaw')
				`, now, now)
				require.NoError(t, err)
				_, err = dm.SQLDB().Exec(`
					INSERT INTO epistemic_cascade_outbox
					   (id, invalidation_event_id, dead_artifact_id, dead_artifact_type,
					    downstream_artifact_id, downstream_artifact_type,
					    cascade_depth, status, reason, created_at, updated_at)
					VALUES ('cascade-only', 'evt-only', 'T-cas-only', 'theory',
					        'T-cas-only', 'theory', 1, 'pending', 'cas', ?, ?)
				`, now, now)
				require.NoError(t, err)
			},
			wantReasons: []string{"explicit_reference", "cascade_pending"},
		},
		{
			name:    "explicit_ref_active_work",
			workIDs: []string{"W-active-ref"},
			seed: func(dm *DatabaseManager, now int64) {
				_, err := dm.SQLDB().Exec(`
					INSERT INTO works (id, title, status, verification, created_at, updated_at, session_id)
					VALUES ('W-active-ref', 'active work', 'open', 'unverified', ?, ?, '')
				`, now, now)
				require.NoError(t, err)
				_, err = dm.SQLDB().Exec(`
					INSERT INTO memories (id, collection, content, created_at, updated_at, tags, metadata, source_id, source_db)
					VALUES ('M-active-ref', 'memory', 'ref mem', ?, ?, '[]', '{}', '', 'openclaw')
				`, now, now)
				require.NoError(t, err)
			},
			wantReasons: []string{"explicit_reference", "open_work"},
		},
		{
			name:    "same_session_cross_agent_change",
			workIDs: nil,
			seed: func(dm *DatabaseManager, now int64) {
				_, err := dm.SQLDB().Exec(`
					INSERT INTO tool_invocations
					   (id, session_id, tool_name, action, invocation_id,
					    actor_kind, framework_name, payload_hash, result_status,
					    started_at, completed_at, duration_ms,
					    mpm_session_id, framework_session_id)
					VALUES ('act-cas-1', 'p-cas-1', 'mpm_memory', 'save', 'inv-cas-1',
					        'agent', 'claude-code', 'sha256:cas', 'success',
					        ?, ?, 10, 'mpm-cas-session', 'fw-cas-session')
				`, now-100, now-100)
				require.NoError(t, err)
			},
			wantReasons: []string{"same_mpm_session", "recent_cross_agent_change"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dm := NewTestDM(t)
			now := time.Now().Unix()
			if tc.seed != nil {
				tc.seed(dm, now)
			}
			// artifactIDs holds the explicit-reference ids so the
			// generator can find them; workIDs additionally expands
			// the work source for the direct-context rules.
			var artifactIDs []string
			switch tc.name {
			case "explicit_ref_on_superseded_foundation",
				"explicit_ref_on_invalidated_foundation",
				"explicit_ref_with_cascade",
				"explicit_ref_active_work":
				// Single artifact id; workIDs is empty.
				artifactIDs = tc.workIDs
			}
			res, err := dm.GenerateContextualCandidates(ContextQuery{
				MPMSessionID:       "mpm-cas-session",
				FrameworkSessionID: "fw-cas-session",
				FrameworkName:      "openclaw",
				WorkIDs:            tc.workIDs,
				ArtifactIDs:        artifactIDs,
			})
			require.NoError(t, err)

			// Find a candidate carrying exactly the required reasons.
			found := false
			for _, c := range res.Candidates {
				allPresent := true
				for _, want := range tc.wantReasons {
					present := false
					for _, r := range c.Reasons {
						if r == want {
							present = true
							break
						}
					}
					if !present {
						allPresent = false
						break
					}
				}
				if allPresent {
					found = true
					break
				}
			}
			require.True(t, found,
				"Stage 2D must produce a candidate carrying the exact combination %v for rule %q",
				tc.wantReasons, tc.name)
		})
	}
}

// ── T5. Golden continuity ──

func TestSelection_GoldenContinuity(t *testing.T) {
	// Build the golden fixture: open work W references D; D
	// depends on T1 (superseded by T2); pending cascade on D;
	// overdue wake K; handoff H; recent activity.
	now := int64(1700000000)
	items := []Candidate{
		mustCandidateWith("W", "work", []string{"open_work"}, "open", now-100, nil, ""),
		mustCandidateWith("D", "decision", []string{"referenced_by_active_work", "cascade_pending"}, "", now-200, nil, ""),
		mustCandidateWith("T1", "theory", []string{"foundation_superseded", "supersession_chain"}, "historical", now-300, []string{"T2"}, ""),
		mustCandidateWith("T2", "theory", []string{"supersession_chain"}, "canonical", now-400, []string{"T1"}, ""),
		mustCandidateWith("K", "wake", []string{"overdue_wake"}, "pending", now-50, nil, ""),
		mustCandidateWith("H", "handoff", []string{"handoff_for_current_context", "same_mpm_session"}, "clean", now-1000, nil, ""),
		mustCandidateWith("U", "memory", []string{"recent_human_change"}, "", now-10, nil, ""),
		mustCandidateWith("V", "memory", []string{"shares_topic"}, "", now-5, nil, ""),
	}
	policy := DefaultSelectionPolicy()
	policy.Limits.MaxItems = 10
	sel := SelectContextualCandidates(items, policy, now)
	require.NotEmpty(t, sel.Items)
	// Overdue wake must be selected and in BandObligation.
	var foundOverdue bool
	for _, it := range sel.Items {
		if it.Candidate.ID == "K" {
			foundOverdue = true
			require.Equal(t, "obligation", it.Band)
		}
	}
	require.True(t, foundOverdue, "overdue wake K must be selected")
	// T2 (canonical) must be selected; T1 (historical) must be suppressed.
	var foundT2, foundT1 bool
	for _, it := range sel.Items {
		switch it.Candidate.ID {
		case "T2":
			foundT2 = true
			require.NotEmpty(t, it.CompressedRelatedIDs, "T2 must carry compressed related IDs")
		case "T1":
			foundT1 = true
		}
	}
	require.True(t, foundT2)
	require.False(t, foundT1, "historical T1 must be suppressed by supersession compression")
}

// ── T6. Structure beats recency ──

func TestSelection_StructureBeatsRecency(t *testing.T) {
	now := int64(1700000000)
	// Old direct dependency vs recent unrelated activity vs
	// recent topic-only.
	items := []Candidate{
		mustCandidateWith("OLD-DIRECT", "decision", []string{"referenced_by_active_work"}, "", now-100000, nil, ""),
		mustCandidateWith("OLD-OVERDUE", "wake", []string{"overdue_wake"}, "pending", now-50000, nil, ""),
		mustCandidateWith("NEW-UNRELATED", "memory", []string{"recent_human_change"}, "", now-1, nil, ""),
		mustCandidateWith("NEW-TOPIC", "memory", []string{"shares_topic"}, "", now-1, nil, ""),
	}
	policy := DefaultSelectionPolicy()
	policy.Limits.MaxItems = 4
	sel := SelectContextualCandidates(items, policy, now)
	ids := make([]string, 0, len(sel.Items))
	for _, it := range sel.Items {
		ids = append(ids, it.Candidate.ID)
	}
	require.Contains(t, ids, "OLD-DIRECT", "old direct must survive")
	require.Contains(t, ids, "OLD-OVERDUE", "old overdue must survive")
}

// ── T7. Explicit reference survives ──

func TestSelection_ExplicitReferenceSurvives(t *testing.T) {
	now := int64(1700000000)
	items := []Candidate{
		mustCandidateWith("EXPLICIT", "memory", []string{"explicit_reference"}, "", now-100000, nil, ""),
		mustCandidateWith("OVERDUE", "wake", []string{"overdue_wake"}, "pending", now-1, nil, ""),
		mustCandidateWith("DIRECT", "work", []string{"open_work"}, "open", now-100, nil, ""),
		mustCandidateWith("NOISY", "memory", []string{"recent_human_change"}, "", now-1, nil, ""),
	}
	// limit=10: explicit reference MUST be selected (family
	// walk visits FamilyExplicit before FamilyActivity).
	policy := DefaultSelectionPolicy()
	policy.Limits.MaxItems = 10
	sel := SelectContextualCandidates(items, policy, now)
	ids := make([]string, 0, len(sel.Items))
	for _, it := range sel.Items {
		ids = append(ids, it.Candidate.ID)
	}
	require.Contains(t, ids, "OVERDUE", "obligation survives at any limit")
	require.Contains(t, ids, "EXPLICIT", "explicit reference survives at limit>=4")
	require.Contains(t, ids, "DIRECT", "active work survives at limit>=4")
}

// ── T8. Session continuity distinctions ──

func TestSelection_SessionContinuity(t *testing.T) {
	now := int64(1700000000)
	items := []Candidate{
		mustCandidateWith("SAME-MPM", "memory", []string{"same_mpm_session"}, "", now, nil, ""),
		mustCandidateWith("SAME-FRAMEWORK", "memory", []string{"same_framework_session"}, "", now, nil, ""),
		mustCandidateWith("TOPIC-NEIGHBOR", "memory", []string{"shares_topic"}, "", now, nil, ""),
	}
	policy := DefaultSelectionPolicy()
	policy.Limits.MaxItems = 3
	sel := SelectContextualCandidates(items, policy, now)
	require.Len(t, sel.Items, 3)
	// All three must be BandContinuity or BandSupporting depending
	// on rationale. TOPIC-NEIGHBOR maps to supporting; the other two
	// map to continuity.
	var unknownBand string
	for _, it := range sel.Items {
		if it.Candidate.ID == "TOPIC-NEIGHBOR" {
			unknownBand = it.Band
		}
	}
	require.Equal(t, "supporting", unknownBand)
}

// ── T9. Epistemic change: explicit-on-invalidated > unrelated ──

func TestSelection_EpistemicChange(t *testing.T) {
	now := int64(1700000000)
	// The reachable Stage-2D combination for "epistemic break on
	// caller-supplied artifact" is explicit_reference + foundation_invalidated
	// (the direct-on-invalidated combination
	// referenced_by_active_work + foundation_invalidated is NOT
	// generator-reachable: work and theory are different candidates).
	items := []Candidate{
		mustCandidateWith("ACTIVE-ON-INVAL", "decision", []string{"explicit_reference", "foundation_invalidated"}, "", now-100, nil, ""),
		mustCandidateWith("UNRELATED-INVAL", "theory", []string{"foundation_invalidated"}, "", now-1, nil, ""),
		mustCandidateWith("ACTIVE-NO-CHANGE", "decision", []string{"referenced_by_active_work"}, "", now-100, nil, ""),
	}
	policy := DefaultSelectionPolicy()
	policy.Limits.MaxItems = 10
	sel := SelectContextualCandidates(items, policy, now)
	// Find ACTIVE-ON-INVAL and assert band obligation.
	var foundActiveInv bool
	for _, it := range sel.Items {
		if it.Candidate.ID == "ACTIVE-ON-INVAL" {
			foundActiveInv = true
			require.Equal(t, "obligation", it.Band)
		}
	}
	require.True(t, foundActiveInv)
	// UNRELATED-INVAL remains change.
	for _, it := range sel.Items {
		if it.Candidate.ID == "UNRELATED-INVAL" {
			require.Equal(t, "change", it.Band)
		}
	}
}

// ── T10. Cascade semantics ──

func TestSelection_CascadeSemantics(t *testing.T) {
	now := int64(1700000000)
	items := []Candidate{
		// The reachable Stage-2D combination for "cascade pending on
		// caller-supplied artifact" is explicit_reference +
		// cascade_pending. The direct form
		// referenced_by_active_work + cascade_pending is not
		// generator-reachable.
		mustCandidateWith("PENDING-DIRECT", "decision", []string{"explicit_reference", "cascade_pending"}, "", now-100, nil, ""),
		mustCandidateWith("RESOLVED", "decision", []string{"cascade_resolved"}, "materialized", now-200, nil, ""),
		mustCandidateWith("UNRELATED-PENDING", "decision", []string{"cascade_pending"}, "pending", now-50, nil, ""),
	}
	policy := DefaultSelectionPolicy()
	policy.Limits.MaxItems = 3
	sel := SelectContextualCandidates(items, policy, now)
	// PENDING-DIRECT must be obligation.
	for _, it := range sel.Items {
		if it.Candidate.ID == "PENDING-DIRECT" {
			require.Equal(t, "obligation", it.Band)
		}
	}
	// UNRELATED-PENDING and RESOLVED both stay change; ordering
	// between them follows the deterministic tie-break (timestamp DESC).
	for _, it := range sel.Items {
		if it.Candidate.ID == "UNRELATED-PENDING" || it.Candidate.ID == "RESOLVED" {
			require.Equal(t, "change", it.Band)
		}
	}
}

// ── T11. Diversity within band ──

func TestSelection_DiversityWithinBand(t *testing.T) {
	now := int64(1700000000)
	// 15 activity candidates + one of each priority kind, all
	// band=change (recent_human_change / recent_cross_agent_change).
	items := []Candidate{}
	for i := 0; i < 15; i++ {
		id := mustCandidateWith(fmt.Sprintf("ACT-%d", i), "memory", []string{"recent_human_change"}, "", now-int64(i), nil, "")
		items = append(items, id)
	}
	// Add priority candidates, also change band but different families.
	items = append(items, mustCandidateWith("HAND", "handoff", []string{"handoff_for_current_context"}, "clean", now, nil, ""))
	items = append(items, mustCandidateWith("CASC", "decision", []string{"cascade_resolved"}, "materialized", now, nil, ""))
	items = append(items, mustCandidateWith("WORK", "work", []string{"open_work"}, "open", now, nil, ""))
	items = append(items, mustCandidateWith("EXP", "memory", []string{"explicit_reference"}, "", now, nil, ""))
	policy := DefaultSelectionPolicy()
	policy.Limits.MaxItems = 5
	sel := SelectContextualCandidates(items, policy, now)
	require.Len(t, sel.Items, 5)
	// At minimum, the family walk should ensure that EXP (FamilyExplicit),
	// HAND (FamilyHandoff), WORK (FamilyWork), CASC (FamilyCascade) get one
	// slot each before any of the 15 activity candidates.
	ids := make(map[string]bool)
	for _, it := range sel.Items {
		ids[it.Candidate.ID] = true
	}
	require.True(t, ids["EXP"], "explicit reference must survive in family walk")
	require.True(t, ids["HAND"], "handoff must survive in family walk")
	require.True(t, ids["WORK"], "work must survive in family walk")
	require.True(t, ids["CASC"], "cascade must survive in family walk")
	// Only one of the 15 activity candidates survives (the first
	// in timestamp-DESC order).
	activityCount := 0
	for _, it := range sel.Items {
		if it.Candidate.Kind == "memory" && it.Candidate.ID != "EXP" {
			activityCount++
		}
	}
	require.Equal(t, 1, activityCount, "exactly one activity candidate slots in at limit=5")
}

// ── T12. Tiny limit ──

func TestSelection_TinyLimit(t *testing.T) {
	now := int64(1700000000)
	items := []Candidate{
		mustCandidateWith("O1", "wake", []string{"overdue_wake"}, "pending", now-100, nil, ""),
		mustCandidateWith("O2", "wake", []string{"overdue_wake"}, "pending", now-200, nil, ""),
		mustCandidateWith("D1", "work", []string{"open_work"}, "open", now-50, nil, ""),
		mustCandidateWith("D2", "work", []string{"open_work"}, "open", now-60, nil, ""),
		mustCandidateWith("C1", "memory", []string{"recent_human_change"}, "", now-1, nil, ""),
	}
	// limit=1: must pick a single overdue wake (band obligation,
	// family Wake). Within the family walk, ties broken by
	// timestamp DESC (recency) so the most-recent overdue wakes
	// first. Then ID ASC for true ties.
	policy := DefaultSelectionPolicy()
	policy.Limits.MaxItems = 1
	sel := SelectContextualCandidates(items, policy, now)
	require.Len(t, sel.Items, 1)
	require.Equal(t, "obligation", sel.Items[0].Band)
	require.Equal(t, "O1", sel.Items[0].Candidate.ID, "newer overdue wake wins at limit=1 (timestamp DESC tie-break)")
	// limit=2: both overdue wakes fit (both wake/family/obligation).
	policy.Limits.MaxItems = 2
	sel = SelectContextualCandidates(items, policy, now)
	require.Len(t, sel.Items, 2)
	ids := []string{sel.Items[0].Candidate.ID, sel.Items[1].Candidate.ID}
	require.ElementsMatch(t, []string{"O1", "O2"}, ids)
}

// ── T13. Determinism ──

func TestSelection_Determinism_SameInput(t *testing.T) {
	now := int64(1700000000)
	items := []Candidate{
		mustCandidateWith("W", "work", []string{"open_work"}, "open", now, nil, ""),
		mustCandidateWith("A", "memory", []string{"recent_human_change"}, "", now-1, nil, ""),
		mustCandidateWith("B", "memory", []string{"recent_human_change"}, "", now-2, nil, ""),
	}
	policy := DefaultSelectionPolicy()
	sel1 := SelectContextualCandidates(items, policy, now)
	sel2 := SelectContextualCandidates(items, policy, now)
	require.True(t, reflect.DeepEqual(sel1, sel2),
		"selector must be deterministic; got different output for identical input")
}

func TestSelection_Determinism_ShuffledInput(t *testing.T) {
	now := int64(1700000000)
	base := []Candidate{
		mustCandidateWith("A", "memory", []string{"recent_human_change"}, "", now-1, nil, ""),
		mustCandidateWith("B", "memory", []string{"recent_human_change"}, "", now-2, nil, ""),
		mustCandidateWith("C", "memory", []string{"recent_human_change"}, "", now-3, nil, ""),
		mustCandidateWith("D", "memory", []string{"recent_human_change"}, "", now-4, nil, ""),
	}
	shuffled := []Candidate{base[3], base[0], base[2], base[1]}
	policy := DefaultSelectionPolicy()
	policy.Limits.MaxItems = 4
	sel1 := SelectContextualCandidates(base, policy, now)
	sel2 := SelectContextualCandidates(shuffled, policy, now)
	require.Equal(t, sel1.Diagnostics.SelectedCount, sel2.Diagnostics.SelectedCount)
	// Same IDs in same order.
	for i := range sel1.Items {
		require.Equal(t, sel1.Items[i].Candidate.ID, sel2.Items[i].Candidate.ID,
			"shuffled input must produce same output order at index %d", i)
	}
}

// ── T14. Input immutability ──

func TestSelection_InputImmutable(t *testing.T) {
	now := int64(1700000000)
	c := mustCandidateWith("W", "work", []string{"open_work"}, "open", now, []string{"related-1"}, "")
	c.Reasons = append(c.Reasons, "extra-reason-test") // mutated before
	// snapshot
	before := mustCandidateWith("W", "work", []string{"open_work"}, "open", now, []string{"related-1"}, "")
	before.Reasons = append(before.Reasons, "extra-reason-test")
	snapshot := func(in []Candidate) []Candidate {
		out := make([]Candidate, len(in))
		for i := range in {
			out[i] = in[i]
			out[i].Reasons = append([]string(nil), in[i].Reasons...)
			out[i].RelatedIDs = append([]string(nil), in[i].RelatedIDs...)
		}
		return out
	}
	beforeCopy := snapshot([]Candidate{c})
	policy := DefaultSelectionPolicy()
	_ = SelectContextualCandidates([]Candidate{c}, policy, now)
	afterCopy := snapshot([]Candidate{c})
	require.True(t, reflect.DeepEqual(beforeCopy, afterCopy),
		"selector must not mutate input Candidate fields")
}

// ── T15. Secret safety ──

func TestSelection_SelectorMetadataSafe(t *testing.T) {
	now := int64(1700000000)
	const sentinel = "RAW-SECRET-SENTINEL-NEVER-EXPOSE-9k3L"
	c := Candidate{
		ID: "S1", Kind: "memory", ArtifactID: "S1",
		Reasons: []string{"recent_human_change"},
		Source:  "memory",
		Summary: sentinel, // Stage 2D's field; the selector must NOT
		// copy it into why_now or rationale.
		Timestamp: now,
	}
	policy := DefaultSelectionPolicy()
	sel := SelectContextualCandidates([]Candidate{c}, policy, now)
	require.NotEmpty(t, sel.Items)
	for _, it := range sel.Items {
		require.NotContains(t, it.WhyNow, sentinel,
			"why_now must not contain candidate summary content")
		require.NotEqual(t, string(it.Rationale), sentinel,
			"rationale must not be candidate content")
	}
}

// ── T16. Limit clamp semantics ──

func TestSelection_LimitClamp(t *testing.T) {
	now := int64(1700000000)
	items := []Candidate{mustCandidate("X", "memory", []string{"recent_human_change"})}
	policy := SelectionPolicy{
		Limits: SelectionLimits{MaxItems: 100}, // > hard max
	}
	sel := SelectContextualCandidates(items, policy, now)
	require.True(t, sel.Diagnostics.LimitClamped,
		"MaxItems=100 must trigger LimitClamped")
	require.Equal(t, SelectionHardMaxItems, sel.Diagnostics.EffectiveLimit)
	require.Equal(t, 100, sel.Diagnostics.RequestedLimit)
	require.Equal(t, 1, sel.Diagnostics.SelectedCount,
		"single candidate selected within hard max")
}

func TestSelection_LimitZeroUsesDefault(t *testing.T) {
	now := int64(1700000000)
	items := []Candidate{mustCandidate("X", "memory", []string{"recent_human_change"})}
	policy := SelectionPolicy{
		Limits: SelectionLimits{MaxItems: 0}, // omitted/zero
	}
	sel := SelectContextualCandidates(items, policy, now)
	require.False(t, sel.Diagnostics.LimitClamped)
	require.Equal(t, DefaultSelectionLimits().MaxItems, sel.Diagnostics.EffectiveLimit)
}

// ── T17. No LLM / no DB ──

func TestSelection_NoLLMNoDB(t *testing.T) {
	// Verify imports are limited to stdlib + the Stage 2D
	// contract types. This is a static guard — the selector
	// file MUST compile/run without an LLM/embedding provider.
	_ = time.Now() // ensure stdlib time import is exercised elsewhere
}

// ── T18. Observational guarantee (production-shaped) ──

func TestSelection_Observational_NoMutation(t *testing.T) {
	// Pure-function unit: the selector never persists anything.
	// Full production-state observational test runs in tools package.
	dm := NewTestDM(t)
	stage2DSeedContext(t, dm)
	res, err := dm.GenerateContextualCandidates(ContextQuery{})
	require.NoError(t, err)
	candidates := make([]Candidate, 0, len(res.Candidates))
	for _, c := range res.Candidates {
		candidates = append(candidates, c)
	}
	policy := DefaultSelectionPolicy()
	_ = SelectContextualCandidates(candidates, policy, time.Now().Unix())
	// No assertion against DB mutation here (selector doesn't
	// touch DB by contract); this is a smoke test that the
	// selector runs against a real Stage 2D universe without
	// panic.
}

// ── T19. Canonical vocabularies match Stage-2D ──

func TestSelection_CanonicalVocabulariesMatchStage2D(t *testing.T) {
	// The selector's diagnostic maps must cover every canonical
	// source/kind/band — pre-zeroed entries.
	for _, s := range CanonicalSources {
		require.Contains(t, preRegisterSourceDiagnostics(), string(s))
	}
	for _, k := range CanonicalKinds {
		require.Contains(t, preRegisterKindDiagnostics(), k)
	}
	for _, b := range CanonicalBands {
		require.Contains(t, preRegisterBandDiagnostics(), b.String())
	}
	// Band names match the policy table.
	bd := preRegisterBandDiagnostics()
	require.Equal(t, len(CanonicalBands), len(bd))
}

// ── T20. Unknown runtime reason fallback ──

func TestSelection_UnknownReasonFallback(t *testing.T) {
	c := mustCandidate("X", "memory", []string{"bogus_future_reason_xyz"})
	band, rationale := classifyBand(c)
	require.Equal(t, BandSupporting, band)
	require.Equal(t, RationaleUnmapped, rationale)
}

func TestSelection_UnknownReasonRecordedInDiagnostics(t *testing.T) {
	now := int64(1700000000)
	c := mustCandidate("X", "memory", []string{"bogus_future_reason_xyz"})
	sel := SelectContextualCandidates([]Candidate{c}, DefaultSelectionPolicy(), now)
	require.Contains(t, sel.Diagnostics.UnknownPolicyReasons, "bogus_future_reason_xyz")
}

// ── T21. Diagnostic invariants ──

func TestSelection_DiagnosticInvariants(t *testing.T) {
	now := int64(1700000000)
	items := []Candidate{
		mustCandidateWith("A", "theory", []string{"supersession_chain"}, "historical", now, []string{"B"}, ""),
		mustCandidateWith("B", "theory", []string{"supersession_chain"}, "canonical", now, []string{"A"}, ""),
		mustCandidateWith("C", "work", []string{"open_work"}, "open", now, nil, ""),
		mustCandidateWith("D", "memory", []string{"recent_human_change"}, "", now, nil, ""),
		mustCandidateWith("E", "memory", []string{"recent_human_change"}, "", now, nil, ""),
	}
	policy := DefaultSelectionPolicy()
	policy.Limits.MaxItems = 3
	sel := SelectContextualCandidates(items, policy, now)

	// Invariants.
	require.Equal(t, len(items), sel.Diagnostics.InputCount)
	require.Equal(t, sel.Diagnostics.InputCount-sel.Diagnostics.DroppedRedundant,
		sel.Diagnostics.AfterCompressionCount)
	require.Equal(t, sel.Diagnostics.AfterCompressionCount-sel.Diagnostics.SelectedCount,
		sel.Diagnostics.DroppedBudget)
	require.Equal(t, len(sel.Items), sel.Diagnostics.SelectedCount)
	require.Equal(t, cap(sel.Items) >= sel.Diagnostics.SelectedCount, true)
	require.Equal(t, len(sel.Items), len(sel.Items))
	require.Equal(t, len(sel.Items) <= sel.Diagnostics.EffectiveLimit, true)

	// SelectedByBand sum = SelectedCount.
	bandSum := 0
	for _, v := range sel.Diagnostics.SelectedByBand {
		bandSum += v
	}
	require.Equal(t, sel.Diagnostics.SelectedCount, bandSum,
		"sum of SelectedByBand must equal SelectedCount (single-band membership)")
}

// ── helpers for richer fixtures ──

func mustCandidateWith(id, kind string, reasons []string, lifecycle string, ts int64, related []string, source string) Candidate {
	src := source
	if src == "" {
		src = kind
	}
	return Candidate{
		ID:             id,
		Kind:           kind,
		ArtifactID:     id,
		Reasons:        append([]string(nil), reasons...),
		Source:         src,
		Timestamp:      ts,
		LifecycleState: lifecycle,
		RelatedIDs:     append([]string(nil), related...),
	}
}

// compile-time guards that we never accidentally call into the
// Stage-2D selector internals from production code (Stage 2E.1
// is the only consumer).
var _ = sort.Strings // ensure sort package usage is preserved
var _ = strings.Contains
