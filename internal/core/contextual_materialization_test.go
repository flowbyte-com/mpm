// contextual_materialization_test.go — Stage 2E.2 bounded contextual
// materialization tests.
//
// Coverage:
//   - Golden materialization fixture (representative kinds)
//   - Tiny-budget fixture (degradation is deterministic)
//   - Huge-artifact fixture (cannot monopolize budget)
//   - Missing-pointer fixture (no candidate substitution)
//   - Handoff read-only fixture (read markers unchanged)
//   - Wake read-only fixture (fired unchanged)
//   - Activity repetition fixture (typed equality grouping)
//   - Supersession fixture (current canonical detail; trigger preserved)
//   - Secret-safety fixture (sentinels never appear in detail)
//   - UTF-8 truncation fixture (valid UTF-8 at boundary)
//   - Determinism fixture (byte-equivalent output)
//   - Diagnostics invariants (counts sum; pre-zero maps)
//   - Pointer retention contract (metadata envelope always populated)
//   - Status vocabulary (closed; item coverage)

package internal

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

// ── T1. Pointer retention + status vocabulary ───────────────

func TestMaterialization_PointerRetention(t *testing.T) {
	dm := NewTestDM(t)
	now := time.Now().Unix()
	reset := pinTimeNowUnix(t, now)
	defer reset()

	workID, _, _, _, _, _, _, _, _, _ := stage2DSeedContext(t, dm)
	stage2DSeedActivity(t, dm)
	_ = workID

	cands, err := dm.GenerateContextualCandidates(ContextQuery{
		MPMSessionID:       "mpm-stage2d-session",
		FrameworkSessionID: "fw-stage2d-session",
		FrameworkName:      "openclaw",
		WorkIDs:            []string{workID},
	})
	require.NoError(t, err)
	sel := SelectContextualCandidates(toCandidates(cands.Candidates), DefaultSelectionPolicy(), now)
	require.NotEmpty(t, sel.Items)

	res := MaterializeContextualSelection(sel, dm, DefaultMaterializationLimits(), now)
	require.NotEmpty(t, res.Items)
	require.Equal(t, len(sel.Items), len(res.Items),
		"output item count must equal selected input count")

	// Every output item must retain the Stage-2E.1 envelope.
	for _, mi := range res.Items {
		require.NotEmpty(t, mi.CandidateID, "candidate_id must be set")
		require.NotEmpty(t, mi.Kind, "kind must be set")
		require.NotEmpty(t, mi.Band, "band must be set (selection metadata)")
		require.NotEmpty(t, mi.Rationale, "rationale must be set (selection metadata)")
		require.NotEmpty(t, mi.Status, "status must be one of the closed vocabulary")
		// Pointer may be empty for activity candidates (no canonical
		// pointer), but the envelope itself is always present.
	}
}

// ── T2. Golden materialization fixture ────────────────────────

func TestMaterialization_GoldenFixture(t *testing.T) {
	dm := NewTestDM(t)
	now := time.Now().Unix()
	reset := pinTimeNowUnix(t, now)
	defer reset()

	workID, _, _, _, _, _, _, _, _, _ := stage2DSeedContext(t, dm)
	stage2DSeedActivity(t, dm)
	_ = workID

	cands, err := dm.GenerateContextualCandidates(ContextQuery{
		MPMSessionID:       "mpm-stage2d-session",
		FrameworkSessionID: "fw-stage2d-session",
		FrameworkName:      "openclaw",
		WorkIDs:            []string{workID},
	})
	require.NoError(t, err)
	sel := SelectContextualCandidates(toCandidates(cands.Candidates), DefaultSelectionPolicy(), now)
	require.NotEmpty(t, sel.Items)

	res := MaterializeContextualSelection(sel, dm, DefaultMaterializationLimits(), now)
	require.NotEmpty(t, res.Items)

	// Selected order preserved.
	for i := 0; i < len(sel.Items); i++ {
		require.Equal(t, sel.Items[i].Candidate.ID, res.Items[i].CandidateID,
			"output order must match selection order at index %d", i)
	}

	// Every output must be within detail budget.
	require.LessOrEqual(t, res.Diagnostics.DetailBytesUsed, res.Diagnostics.DetailBudgetBytes,
		"detail chars used must not exceed budget")
	require.Equal(t, len(sel.Items), res.Diagnostics.SelectedInputCount,
		"selected input count must match selection.Items length")
	require.Equal(t, len(sel.Items), res.Diagnostics.OutputItemCount,
		"output item count must equal selected input count")

	// No item should have arbitrary full content; detail is bounded.
	for _, mi := range res.Items {
		require.LessOrEqual(t, mi.DetailBytes, HardMaterializationPerItemCap,
			"item detail must fit within hard per-item cap")
	}
}

// ── T3. Tiny-budget fixture ──────────────────────────────────

func TestMaterialization_TinyBudget(t *testing.T) {
	dm := NewTestDM(t)
	now := time.Now().Unix()
	reset := pinTimeNowUnix(t, now)
	defer reset()

	workID, _, _, _, _, _, _, _, _, _ := stage2DSeedContext(t, dm)
	cands, err := dm.GenerateContextualCandidates(ContextQuery{
		MPMSessionID: "mpm-stage2d-session",
		WorkIDs:      []string{workID},
	})
	require.NoError(t, err)
	sel := SelectContextualCandidates(toCandidates(cands.Candidates), DefaultSelectionPolicy(), now)
	require.NotEmpty(t, sel.Items)

	// Deliberately tiny budget: 50 bytes total, 20 bytes per item.
	res := MaterializeContextualSelection(sel, dm, MaterializationLimits{
		DetailBudgetBytes: 50,
		PerItemByteCap:    20,
	}, now)
	require.NotEmpty(t, res.Items)
	require.Equal(t, len(sel.Items), len(res.Items),
		"tiny budget must NOT reduce item count — selected items remain pointer-accountable")

	// Detail chars used must be <= budget.
	require.LessOrEqual(t, res.Diagnostics.DetailBytesUsed, 50)
	// Some items must be pointer_only due to budget exhaustion.
	hasPointerOnly := false
	for _, mi := range res.Items {
		if mi.Status == StatusPointerOnly {
			hasPointerOnly = true
		}
	}
	require.True(t, hasPointerOnly,
		"tiny budget should push at least one item to pointer_only")
}

// ── T4. Huge-artifact fixture ────────────────────────────────

func TestMaterialization_HugeArtifactCannotMonopolize(t *testing.T) {
	dm := NewTestDM(t)
	now := time.Now().Unix()
	reset := pinTimeNowUnix(t, now)
	defer reset()

	// Seed a huge memory content; force it to be the only candidate
	// by using ArtifactIDs so the explicit_reference path surfaces it.
	huge := strings.Repeat("X", 50000)
	_, err := dm.SQLDB().Exec(`
		INSERT INTO memories (id, collection, content, created_at, updated_at, tags, metadata, source_id, source_db)
		VALUES ('M-huge', 'memory', ?, ?, ?, '[]', '{}', '', 'openclaw')`,
		huge, now, now)
	require.NoError(t, err)

	cands, err := dm.GenerateContextualCandidates(ContextQuery{
		ArtifactIDs: []string{"M-huge"},
	})
	require.NoError(t, err)
	require.NotEmpty(t, cands.Candidates)

	sel := SelectContextualCandidates(toCandidates(cands.Candidates), DefaultSelectionPolicy(), now)
	require.NotEmpty(t, sel.Items)

	res := MaterializeContextualSelection(sel, dm, DefaultMaterializationLimits(), now)
	require.NotEmpty(t, res.Items)

	// Per-item cap must apply even on huge content.
	for _, mi := range res.Items {
		require.LessOrEqual(t, mi.DetailBytes, HardMaterializationPerItemCap)
		if mi.Kind == "memory" {
			// The huge content (50_000 chars) MUST be bounded by the
			// kind materializer (truncate(content, 400)) and MUST NOT
			// monopolize the global budget.
			require.LessOrEqual(t, mi.DetailBytes, 800,
				"huge memory detail must be bounded by the kind materializer")
			// Bounded content size must be a tiny fraction of the
			// original 50_000 chars.
			require.Less(t, mi.DetailBytes, 1000)
		}
	}
}

// ── T5. Missing-pointer fixture ─────────────────────────────

func TestMaterialization_MissingPointerNoSubstitution(t *testing.T) {
	dm := NewTestDM(t)
	now := time.Now().Unix()
	reset := pinTimeNowUnix(t, now)
	defer reset()

	workID, _, _, _, _, _, _, _, _, _ := stage2DSeedContext(t, dm)
	_ = workID
	cands, err := dm.GenerateContextualCandidates(ContextQuery{
		MPMSessionID: "mpm-stage2d-session",
	})
	require.NoError(t, err)
	require.NotEmpty(t, cands.Candidates)

	// Select, then DELETE the underlying work row before materializing.
	sel := SelectContextualCandidates(toCandidates(cands.Candidates), DefaultSelectionPolicy(), now)
	require.NotEmpty(t, sel.Items)

	for i := range sel.Items {
		if sel.Items[i].Candidate.Kind == "work" {
			_, err := dm.SQLDB().Exec(`DELETE FROM works WHERE id = ?`,
				sel.Items[i].Candidate.ArtifactID)
			require.NoError(t, err)
		}
	}

	res := MaterializeContextualSelection(sel, dm, DefaultMaterializationLimits(), now)
	require.NotEmpty(t, res.Items)
	require.Equal(t, len(sel.Items), len(res.Items),
		"missing pointer must NOT cause candidate substitution")

	// At least one item must be StatusMissing or StatusPointerOnly.
	hasMissing := false
	for _, mi := range res.Items {
		if mi.Status == StatusMissing || mi.Status == StatusPointerOnly {
			hasMissing = true
		}
	}
	require.True(t, hasMissing,
		"some selected items must report missing/pointer_only after row deletion")
}

// ── T6. Handoff read-only fixture ────────────────────────────

func TestMaterialization_HandoffReadOnly(t *testing.T) {
	dm := NewTestDM(t)
	now := time.Now().Unix()

	cands, err := dm.GenerateContextualCandidates(ContextQuery{
		MPMSessionID: "mpm-stage2d-session",
	})
	require.NoError(t, err)

	sel := SelectContextualCandidates(toCandidates(cands.Candidates), DefaultSelectionPolicy(), now)

	// Snapshot handoff read state.
	var beforeUnread int
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM session_handoffs WHERE read_at IS NULL`).Scan(&beforeUnread))

	res := MaterializeContextualSelection(sel, dm, DefaultMaterializationLimits(), now)
	_ = res

	var afterUnread int
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM session_handoffs WHERE read_at IS NULL`).Scan(&afterUnread))
	require.Equal(t, beforeUnread, afterUnread,
		"materialization must NOT advance handoff read markers")
}

// ── T7. Wake read-only fixture ──────────────────────────────

func TestMaterialization_WakeReadOnly(t *testing.T) {
	dm := NewTestDM(t)
	now := time.Now().Unix()

	// Seed an unfired wake so we can observe fired=0 invariant.
	_, err := dm.SQLDB().Exec(`
		INSERT INTO scheduled_wakes (id, target_time, reason, fired, theory_id, created_by, created_at)
		VALUES ('w-mat', ?, 'test', 0, '', 'mpm-cli', ?)`,
		now+7200, now)
	require.NoError(t, err)

	cands, err := dm.GenerateContextualCandidates(ContextQuery{})
	require.NoError(t, err)

	sel := SelectContextualCandidates(toCandidates(cands.Candidates), DefaultSelectionPolicy(), now)
	res := MaterializeContextualSelection(sel, dm, DefaultMaterializationLimits(), now)
	_ = res

	// Wake must still be fired=0; no fired_at stamp.
	var fired int
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT fired FROM scheduled_wakes WHERE id = 'w-mat'`).Scan(&fired))
	require.Equal(t, 0, fired, "materialization must NOT fire the wake")
}

// ── T8. Activity repetition fixture ─────────────────────────

func TestMaterialization_ActivityRepetition(t *testing.T) {
	dm := NewTestDM(t)
	now := time.Now().Unix()
	stage2DSeedActivity(t, dm)

	cands, err := dm.GenerateContextualCandidates(ContextQuery{
		MPMSessionID:       "mpm-stage2d-session",
		FrameworkSessionID: "fw-stage2d-session",
		FrameworkName:      "openclaw",
	})
	require.NoError(t, err)

	sel := SelectContextualCandidates(toCandidates(cands.Candidates), DefaultSelectionPolicy(), now)

	// Activity items that share (tool, action, framework_name)
	// must remain individually addressable. The current Stage 2D
	// emits per-event distinct artifact_ids, so they survive the
	// materialization as separate items.
	res := MaterializeContextualSelection(sel, dm, DefaultMaterializationLimits(), now)
	activityCount := 0
	for _, mi := range res.Items {
		if mi.Kind == "activity" {
			activityCount++
			require.NotEmpty(t, mi.ArtifactID)
		}
	}
	require.Equal(t, activityCount, len(res.Items),
		"every activity item must remain individually addressable (no silent collapse)")
}

// ── T9. Supersession fixture ───────────────────────────────

func TestMaterialization_SupersessionCurrentCanonical(t *testing.T) {
	dm := NewTestDM(t)
	now := time.Now().Unix()
	reset := pinTimeNowUnix(t, now)
	defer reset()

	workID, _, _, _, _, _, _, _, _, _ := stage2DSeedContext(t, dm)
	cands, err := dm.GenerateContextualCandidates(ContextQuery{
		MPMSessionID: "mpm-stage2d-session",
		WorkIDs:      []string{workID},
	})
	require.NoError(t, err)
	sel := SelectContextualCandidates(toCandidates(cands.Candidates), DefaultSelectionPolicy(), now)

	res := MaterializeContextualSelection(sel, dm, DefaultMaterializationLimits(), now)

	// Canonical survivors with triggers must preserve them.
	for _, mi := range res.Items {
		if len(mi.CompressionTriggers) > 0 {
			// Triggers must carry source_id + band + rationale.
			for _, tr := range mi.CompressionTriggers {
				require.NotEmpty(t, tr.SourceID)
				require.NotEmpty(t, tr.Band)
				require.NotEmpty(t, tr.Rationale)
			}
			// Detail (if any) must reference the canonical artifact,
			// not the suppressed predecessor body.
			require.NotContains(t, mi.Detail, "predecessor body",
				"detail must reflect canonical successor, not historical predecessor body")
		}
	}
}

// ── T10. Secret-safety fixture ──────────────────────────────

func TestMaterialization_SecretSafety(t *testing.T) {
	dm := NewTestDM(t)
	now := time.Now().Unix()
	const sentinel = "RAW-SECRET-SENTINEL-MAT-9k3L"

	// Insert a memory with sentinel ONLY in metadata JSON, not in
	// content. Materializer exposes content (user-authored) but never
	// metadata. This is the canonical "metadata leak" test.
	_, err := dm.SQLDB().Exec(`
		INSERT INTO memories (id, collection, content, created_at, updated_at, tags, metadata, source_id, source_db)
		VALUES ('M-secret-mat', 'memory', ?, ?, ?, '[]', ?, '', 'openclaw')`,
		"clean content with no secret", now, now,
		`{"sentinel":"`+sentinel+`"}`)
	require.NoError(t, err)

	cands, err := dm.GenerateContextualCandidates(ContextQuery{
		ArtifactIDs: []string{"M-secret-mat"},
	})
	require.NoError(t, err)
	sel := SelectContextualCandidates(toCandidates(cands.Candidates), DefaultSelectionPolicy(), now)
	res := MaterializeContextualSelection(sel, dm, DefaultMaterializationLimits(), now)

	for _, mi := range res.Items {
		// Metadata JSON must NOT appear in detail (we deliberately
		// exclude metadata from materialization; this is the test).
		require.NotContains(t, mi.Detail, sentinel,
			"detail must not leak secret from metadata field")
		// But clean content must still appear.
		require.Contains(t, mi.Detail, "clean content with no secret",
			"safe content must still appear in detail")
	}

	// Serialize the whole result and confirm the sentinel still does
	// not appear anywhere in the public envelope.
	b, _ := json.Marshal(res)
	require.NotContains(t, string(b), sentinel,
		"serialized materialization must not leak secret in any field")
}

// ── T11. UTF-8 truncation fixture ───────────────────────────

func TestMaterialization_UTF8Truncation(t *testing.T) {
	dm := NewTestDM(t)
	now := time.Now().Unix()

	// Seed a memory with multi-byte characters at the truncation
	// boundary.
	multiByte := "αβγδεζηθικλμ" // 12 Greek letters
	_, err := dm.SQLDB().Exec(`
		INSERT INTO memories (id, collection, content, created_at, updated_at, tags, metadata, source_id, source_db)
		VALUES ('M-utf8', 'memory', ?, ?, ?, '[]', '{}', '', 'openclaw')`,
		multiByte, now, now)
	require.NoError(t, err)

	cands, err := dm.GenerateContextualCandidates(ContextQuery{
		ArtifactIDs: []string{"M-utf8"},
	})
	require.NoError(t, err)
	sel := SelectContextualCandidates(toCandidates(cands.Candidates), DefaultSelectionPolicy(), now)

	// Per-item cap of 5 runes; should truncate to a valid 5-rune prefix.
	res := MaterializeContextualSelection(sel, dm, MaterializationLimits{
		DetailBudgetBytes: 5000,
		PerItemByteCap:    5,
	}, now)

	for _, mi := range res.Items {
		if mi.Kind == "memory" && mi.Detail != "" {
			require.True(t, utf8.ValidString(mi.Detail),
				"truncated detail must be valid UTF-8")
			require.LessOrEqual(t, utf8.RuneCountInString(mi.Detail), 5)
			require.True(t, mi.Truncated, "truncated=true when per-item cap applies")
		}
	}
}

// ── T12. Determinism fixture ───────────────────────────────

func TestMaterialization_Determinism(t *testing.T) {
	dm := NewTestDM(t)
	now := time.Now().Unix()
	workID, _, _, _, _, _, _, _, _, _ := stage2DSeedContext(t, dm)
	_ = workID
	reset := pinTimeNowUnix(t, now)
	defer reset()

	cands, err := dm.GenerateContextualCandidates(ContextQuery{
		MPMSessionID: "mpm-stage2d-session",
	})
	require.NoError(t, err)
	sel := SelectContextualCandidates(toCandidates(cands.Candidates), DefaultSelectionPolicy(), now)

	// Materialize twice; byte-equivalent semantic output.
	r1 := MaterializeContextualSelection(sel, dm, DefaultMaterializationLimits(), now)
	r2 := MaterializeContextualSelection(sel, dm, DefaultMaterializationLimits(), now)

	b1, _ := json.Marshal(r1)
	b2, _ := json.Marshal(r2)
	require.Equal(t, string(b1), string(b2),
		"identical input must produce byte-equivalent materialization output")
}

// ── T13. Diagnostic invariants ──────────────────────────────

func TestMaterialization_DiagnosticInvariants(t *testing.T) {
	dm := NewTestDM(t)
	now := time.Now().Unix()
	workID, _, _, _, _, _, _, _, _, _ := stage2DSeedContext(t, dm)
	_ = workID
	reset := pinTimeNowUnix(t, now)
	defer reset()

	cands, err := dm.GenerateContextualCandidates(ContextQuery{
		MPMSessionID: "mpm-stage2d-session",
	})
	require.NoError(t, err)
	sel := SelectContextualCandidates(toCandidates(cands.Candidates), DefaultSelectionPolicy(), now)

	res := MaterializeContextualSelection(sel, dm, DefaultMaterializationLimits(), now)

	// selected_input_count == output_item_count.
	require.Equal(t, res.Diagnostics.SelectedInputCount, res.Diagnostics.OutputItemCount)
	// detail_chars_used <= detail_budget.
	require.LessOrEqual(t, res.Diagnostics.DetailBytesUsed, res.Diagnostics.DetailBudgetBytes)

	// ByStatus pre-zero for every canonical status.
	for _, s := range []MaterializationStatus{
		StatusMaterialized, StatusPointerOnly, StatusMissing,
		StatusUnsupported, StatusError,
	} {
		_, ok := res.Diagnostics.ByStatus[string(s)]
		require.True(t, ok, "by_status must contain %s", s)
	}
	// ByKind pre-zero for every canonical kind.
	for _, k := range CanonicalKinds {
		_, ok := res.Diagnostics.ByKind[k]
		require.True(t, ok, "by_kind must contain %s", k)
	}

	// Sum of ByStatus >= output item count (one per item, statuses
	// are mutually exclusive except truncated which is orthogonal).
	statusSum := 0
	for _, v := range res.Diagnostics.ByStatus {
		statusSum += v
	}
	require.GreaterOrEqual(t, statusSum, res.Diagnostics.OutputItemCount)
}

// ── T14. Default budget reasoning ───────────────────────────

func TestMaterialization_DefaultsReasonableForProduction(t *testing.T) {
	d := DefaultMaterializationLimits()
	// Default total budget <= HardMaterializationDetailBudget.
	require.LessOrEqual(t, d.DetailBudgetBytes, HardMaterializationDetailBudget)
	require.LessOrEqual(t, d.PerItemByteCap, HardMaterializationPerItemCap)
	// 10 selected items × 800-char per-item cap = 8000, well above
	// the 6000 default budget (so the budget IS the gating constraint,
	// not the per-item cap).
	require.Equal(t, 6000, d.DetailBudgetBytes)
	require.Equal(t, 800, d.PerItemByteCap)
}

// ── T15. Pointer-only on missing read ──────────────────────

func TestMaterialization_PointerOnlyOnError(t *testing.T) {
	dm := NewTestDM(t)
	now := time.Now().Unix()

	// Build a synthetic selected item with a kind that exists but
	// a pointer that won't resolve. We simulate this by constructing
	// the SelectedItem directly.
	c := Candidate{
		ID:             "work:does-not-exist",
		Kind:           "work",
		ArtifactID:     "does-not-exist",
		Pointer:        "mpm://work/does-not-exist",
		LifecycleState: "open",
	}
	sel := SelectionResult{
		Items: []SelectedItem{
			{
				Candidate: c,
				Band:      "direct",
				Rationale: "direct_active_work",
				WhyNow:    "Open work item.",
			},
		},
	}
	res := MaterializeContextualSelection(sel, dm, DefaultMaterializationLimits(), now)
	require.Len(t, res.Items, 1)
	require.Equal(t, StatusMissing, res.Items[0].Status,
		"missing work should be StatusMissing, not pointer_only")
	require.Empty(t, res.Items[0].Detail)
	require.Equal(t, "does-not-exist", res.Items[0].ArtifactID,
		"pointer envelope must remain even when detail fails")
}

// ── T15b. Wake-context read-only size comparison (Section 43) ──
//
// Per the final clarifications: Stage 2E.2 size comparison MUST NOT
// invoke the mutating wake-context path. We use
// GatherWakeContextReadOnly (read-only) and prove before/after that
// handoff read markers are unchanged.
func TestMaterialization_WakeContextReadOnlySizeComparison(t *testing.T) {
	dm := NewTestDM(t)
	now := time.Now().Unix()
	reset := pinTimeNowUnix(t, now)
	defer reset()

	// Seed enough substrate that a real wake-context payload
	// produces a non-trivial measurement.
	_, _, _, _, handoffID, _, _, _, _, _ := stage2DSeedContext(t, dm)
	stage2DSeedActivity(t, dm)

	// Snapshot handoff read state BEFORE.
	var beforeBM int
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM session_handoffs WHERE read_at IS NOT NULL`).Scan(&beforeBM))

	// Measure read-only wake-context size (does NOT mark handoff read).
	wcData, err := dm.GatherWakeContextReadOnly()
	require.NoError(t, err)
	wcBytes := len([]byte(formatWakeContext(wcData)))

	// Snapshot handoff read state AFTER wake-context measure.
	var afterWCBM int
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM session_handoffs WHERE read_at IS NOT NULL`).Scan(&afterWCBM))
	require.Equal(t, beforeBM, afterWCBM,
		"GatherWakeContextReadOnly must not mutate handoff read markers")

	// Generate materialization size for the same production state.
	cands, err := dm.GenerateContextualCandidates(ContextQuery{
		MPMSessionID:       "mpm-stage2d-session",
		FrameworkSessionID: "fw-stage2d-session",
		FrameworkName:      "openclaw",
		WorkIDs:            []string{handoffID},
	})
	require.NoError(t, err)
	sel := SelectContextualCandidates(toCandidates(cands.Candidates), DefaultSelectionPolicy(), now)
	mat := MaterializeContextualSelection(sel, dm, DefaultMaterializationLimits(), now)
	matBytes, _ := json.MarshalIndent(mat, "", "  ")
	_ = matBytes

	// Snapshot handoff read state AFTER materialization.
	var afterMatBM int
	require.NoError(t, dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM session_handoffs WHERE read_at IS NOT NULL`).Scan(&afterMatBM))
	require.Equal(t, beforeBM, afterMatBM,
		"materialization must not mutate handoff read markers")

	// Reportable comparison (printed, not asserted — Stage 2E.3
	// decides delivery-envelope economics).
	t.Logf("read-only wake context (formatWakeContext): %d bytes", wcBytes)
	t.Logf("materialization (MaterializeContextualSelection): %d bytes serialized", len(matBytes))
	t.Logf("materialization detail budget used: %d/%d bytes",
		mat.Diagnostics.DetailBytesUsed, mat.Diagnostics.DetailBudgetBytes)
}

func TestMaterialization_OnePerSelectedActivity(t *testing.T) {
	dm := NewTestDM(t)
	now := time.Now().Unix()
	reset := pinTimeNowUnix(t, now)
	defer reset()

	// Build four selected activity items with IDENTICAL summary
	// (would have triggered grouping under the old contract).
	// Stage 2E.2 final clarifications require 1 selected == 1 output.
	mk := func(id string) SelectedItem {
		return SelectedItem{
			Candidate: Candidate{
				ID:         "activity:" + id,
				Kind:       "activity",
				ArtifactID: id,
				Source:     "activity",
				Timestamp:  now - 60,
				ActorKind:  "agent",
				Summary:    "mpm_memory.save",
			},
			Band:      "change",
			Rationale: "change_recent_activity",
			WhyNow:    "Recent change recorded.",
		}
	}
	sel := SelectionResult{
		Items: []SelectedItem{mk("ev-1"), mk("ev-1"), mk("ev-1"), mk("ev-2")},
	}
	res := MaterializeContextualSelection(sel, dm, DefaultMaterializationLimits(), now)
	require.Equal(t, len(sel.Items), len(res.Items),
		"1 selected activity == 1 materialized output (no grouping)")
	// Each output retains its own ArtifactID.
	seen := map[string]bool{}
	for _, mi := range res.Items {
		seen[mi.ArtifactID] = true
	}
	for _, id := range []string{"ev-1", "ev-1", "ev-1", "ev-2"} {
		require.Contains(t, idsFor(t, seen), id)
	}
}

func idsFor(t *testing.T, m map[string]bool) []string {
	t.Helper()
	out := []string{}
	for k := range m {
		out = append(out, k)
	}
	return out
}

// ── Helpers ──────────────────────────────────────────────────

func toCandidates(in []Candidate) []Candidate {
	out := make([]Candidate, len(in))
	copy(out, in)
	return out
}
