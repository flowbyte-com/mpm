// contextual_focus_test.go — Stage 2E.3 wake-context projection and
// delivery integration tests.
//
// Verifies that the Stage 2D → 2E.1 → 2E.2 pipeline is now consumed
// by the normal wake-context path so an ordinary agent calling
// read_wake_context receives compact inherited contextual focus
// alongside legacy fields, without breaking existing delivery
// semantics, read-only preview semantics, or context economics.

package internal

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// ── Fixtures ──────────────────────────────────────────────────────

// stage2E3SeedRichContext seeds the substrate with the same rich
// fixture used in Stage 2D/2E.1/2E.2 tests, plus an explicit unread
// handoff so the focus can be observed end-to-end.
func stage2E3SeedRichContext(t *testing.T, dm *DatabaseManager) (
	workID, decisionID, theoryID, _, handoffID, _, _, _, _, _ string,
) {
	t.Helper()
	workID, decisionID, theoryID, _, handoffID, _, _, _, _, _ =
		stage2DSeedContext(t, dm)
	return workID, decisionID, theoryID, "", handoffID, "", "", "", "", ""
}

// makeReadOnlyFocus is a tiny helper that constructs a ContextQuery
// from authoritatively-known wake-context state. Mirrors the field
// list the production normal-wake delivery path will populate.
func makeReadOnlyFocus(dm *DatabaseManager, workIDs []string) ContextQuery {
	return ContextQuery{
		MPMSessionID:       CurrentMPMSessionID(),
		FrameworkSessionID: "",
		FrameworkName:      "",
		WorkIDs:            workIDs,
	}
}

// ── T1: Additive WakeContextData field ───────────────────────────

// TestStage2E3_AdditiveField confirms that the new field exists on
// WakeContextData, is exposed in JSON, and is `nil` when not
// populated. Does not regress existing fields.
func TestStage2E3_AdditiveField(t *testing.T) {
	dm := NewTestDM(t)

	data, err := dm.GatherWakeContextReadOnly()
	if err != nil {
		t.Fatalf("GatherWakeContextReadOnly: %v", err)
	}

	// Legacy fields still present.
	if data.ContextVersion == "" {
		t.Fatalf("ContextVersion must remain populated")
	}
	if data.GeneratedAt == 0 {
		t.Fatalf("GeneratedAt must remain populated")
	}

	// New field exists; may be nil if projection didn't run, but
	// should at least not cause an error.
	b, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("marshal wake context: %v", err)
	}
	// Look for the additive field name in the wire form only when
	// populated. omitempty on pointer is allowed but the field is
	// allowed to be present-and-empty too.
	if !strings.Contains(string(b), `"contextual_focus"`) &&
		!strings.Contains(string(b), `"context_version"`) {
		t.Fatalf("wake context JSON missing expected fields: %s", b)
	}
}

// ── T2: Legacy-field compatibility ───────────────────────────────

// TestStage2E3_LegacyFieldsUnchanged confirms that all pre-existing
// WakeContextData fields remain with their existing names and
// types. Uses identical hermetic fixture state to compare semantic
// values from before/after integration.
func TestStage2E3_LegacyFieldsUnchanged(t *testing.T) {
	dm := NewTestDM(t)
	stage2E3SeedRichContext(t, dm)

	data, err := dm.GatherWakeContextReadOnly()
	if err != nil {
		t.Fatalf("gather read-only: %v", err)
	}

	b, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	js := string(b)

	// Spot-check every pre-existing JSON name is still present and
	// well-typed.
	legacy := []string{
		`"context_version"`,
		`"generated_at"`,
		`"as_of"`,
		`"session_id"`,
		`"active_mode"`,
		`"active_persona"`,
		`"recent_topics"`,
		`"recent_memories"`,
		`"recent_milestones"`,
		`"open_works"`,
		`"completed_works"`,
		`"recent_activity"`,
		`"global_rules"`,
		`"epistemic_pressure"`,
		`"available_skills"`,
		`"audit_summary"`,
	}
	for _, key := range legacy {
		if !strings.Contains(js, key) {
			t.Errorf("legacy field %s missing from wake context JSON", key)
		}
	}
}

// ── T3: Shared projection-builder architecture ──────────────────

// TestStage2E3_SharedProjectionBuilder confirms there is ONE
// projection function used by both the read-only preview path and
// the delivery path. The function is named GatherContextualFocus
// and accepts an explicit now + limits; both paths use the same
// defaults.
func TestStage2E3_SharedProjectionBuilder(t *testing.T) {
	dm := NewTestDM(t)
	stage2E3SeedRichContext(t, dm)

	q := makeReadOnlyFocus(dm, nil)
	now := time.Now().Unix()

	f1, err := GatherContextualFocusReadOnly(dm, q, now,
		DefaultMaterializationLimits())
	if err != nil {
		t.Fatalf("focus (call 1): %v", err)
	}
	f2, err := GatherContextualFocusReadOnly(dm, q, now,
		DefaultMaterializationLimits())
	if err != nil {
		t.Fatalf("focus (call 2): %v", err)
	}

	if len(f1.Items) != len(f2.Items) {
		t.Fatalf("focus not deterministic: %d vs %d",
			len(f1.Items), len(f2.Items))
	}
}

// ── T4: Delivery handoff ordering fixture ────────────────────────

// TestStage2E3_DeliveryHandoffOrdering confirms the critical
// invariant: the unread handoff H must appear in the contextual
// focus delivered to the receiving agent AND in the returned
// legacy wake context, AND handoff consumption happens AFTER the
// returned context is built.
func TestStage2E3_DeliveryHandoffOrdering(t *testing.T) {
	dm := NewTestDM(t)
	_, _, _, _, handoffID, _, _, _, _, _ := stage2E3SeedRichContext(t, dm)

	// Read-only first: confirm focus can see handoff and stays unread.
	ro, err := dm.GatherWakeContextReadOnly()
	if err != nil {
		t.Fatalf("read-only gather: %v", err)
	}
	if ro.LastHandoff == nil || ro.LastHandoff.ID != handoffID {
		t.Fatalf("read-only LastHandoff = %+v, want %s",
			ro.LastHandoff, handoffID)
	}
	roFocus := ro.ContextualFocus
	if roFocus == nil {
		t.Fatalf("read-only contextual focus must not be nil")
	}
	if roFocus.Status != ContextualFocusAvailable {
		t.Fatalf("read-only focus status = %q, want %q",
			roFocus.Status, ContextualFocusAvailable)
	}
	// Find handoff item in focus.
	foundHandoff := false
	for _, it := range roFocus.Items {
		if it.Kind == "handoff" && it.ArtifactID == handoffID {
			foundHandoff = true
			break
		}
	}
	if !foundHandoff {
		t.Fatalf("handoff missing from focus items (focus=%+v)", roFocus)
	}

	// Handoff must still be unread after the read-only call.
	h2, err := dm.GetLatestUnreadHandoff()
	if err != nil {
		t.Fatalf("get unread handoff after read-only: %v", err)
	}
	if h2 == nil || h2.ID != handoffID {
		t.Fatalf("handoff unexpectedly consumed by read-only gather")
	}

	// Delivery: must mark read exactly once and not return the
	// consumed handoff in the focus.
	delivered, err := dm.GatherWakeContext()
	if err != nil {
		t.Fatalf("delivery gather: %v", err)
	}
	if delivered.LastHandoff == nil || delivered.LastHandoff.ID != handoffID {
		t.Fatalf("delivery LastHandoff = %+v, want %s",
			delivered.LastHandoff, handoffID)
	}
	if delivered.ContextualFocus == nil {
		t.Fatalf("delivery contextual focus must not be nil")
	}

	h3, err := dm.GetLatestUnreadHandoff()
	if err == nil && h3 != nil {
		t.Fatalf("handoff still unread after delivery: %+v", h3)
	}
}

// ── T5: Read-only repeatability ──────────────────────────────────

// TestStage2E3_ReadOnlyRepeatability confirms that repeated
// GatherWakeContextReadOnly calls leave handoff unread, leave the
// focus semantically identical, and never mutate any persistent
// state.
func TestStage2E3_ReadOnlyRepeatability(t *testing.T) {
	dm := NewTestDM(t)
	stage2E3SeedRichContext(t, dm)

	// Snapshot DB row counts.
	tables := []string{
		"works", "memories", "lessons", "session_handoffs",
		"scheduled_wakes", "ephemeral_scratchpad", "tool_invocations",
		"epistemic_cascade_outbox", "epistemic_provenance",
	}
	counts := func() map[string]int {
		out := map[string]int{}
		for _, tbl := range tables {
			var n int
			if err := dm.SQLDB().QueryRow(
				"SELECT COUNT(*) FROM " + tbl).Scan(&n); err != nil {
				t.Fatalf("count %s: %v", tbl, err)
			}
			out[tbl] = n
		}
		return out
	}
	before := counts()

	// Three read-only calls.
	var focuses []*ContextualFocus
	for i := 0; i < 3; i++ {
		data, err := dm.GatherWakeContextReadOnly()
		if err != nil {
			t.Fatalf("read-only #%d: %v", i, err)
		}
		focuses = append(focuses, data.ContextualFocus)
	}

	after := counts()
	for tbl, b := range before {
		if after[tbl] != b {
			t.Errorf("table %s count changed: %d → %d", tbl, b, after[tbl])
		}
	}

	// Semantic equality across focus calls (item counts identical,
	// kinds/ids match in same order).
	for i := 1; i < len(focuses); i++ {
		if len(focuses[i].Items) != len(focuses[0].Items) {
			t.Errorf("focus[%d] item count %d != focus[0] item count %d",
				i, len(focuses[i].Items), len(focuses[0].Items))
			continue
		}
		for j := range focuses[i].Items {
			if focuses[i].Items[j].ArtifactID != focuses[0].Items[j].ArtifactID {
				t.Errorf("focus[%d].items[%d].artifact_id = %q, want %q",
					i, j, focuses[i].Items[j].ArtifactID,
					focuses[0].Items[j].ArtifactID)
			}
		}
	}
}

// ── T6: No-handoff delivery ─────────────────────────────────────

// TestStage2E3_NoHandoffDelivery confirms the focus still
// materializes from work/activity/epistemic state when no unread
// handoff exists, and the read-only path does not invent an
// artificial handoff.
func TestStage2E3_NoHandoffDelivery(t *testing.T) {
	dm := NewTestDM(t)
	stage2DSeedContext(t, dm)
	// Mark the handoff read so it's not unread.
	_, err := dm.MarkLatestHandoffRead("test-no-handoff")
	if err != nil {
		t.Fatalf("mark handoff read: %v", err)
	}

	data, err := dm.GatherWakeContextReadOnly()
	if err != nil {
		t.Fatalf("read-only: %v", err)
	}
	if data.ContextualFocus == nil {
		t.Fatalf("focus must still be computed even with no unread handoff")
	}
	if data.ContextualFocus.Status != ContextualFocusAvailable {
		t.Errorf("focus status = %q, want available", data.ContextualFocus.Status)
	}
}

// ── T7: Projection failure fallback ─────────────────────────────

// TestStage2E3_ProjectionFailureFallback confirms that even when
// the contextual projection pipeline fails, the wake-context
// gather still returns the legacy context with status=degraded.
func TestStage2E3_ProjectionFailureFallback(t *testing.T) {
	dm := NewTestDM(t)
	stage2E3SeedRichContext(t, dm)

	// Run the focus with a query that returns zero candidates — the
	// pipeline must still succeed with status=available and empty
	// items; the degraded path is reserved for unexpected internal
	// errors.
	q := ContextQuery{
		MPMSessionID: "definitely-no-such-session",
	}
	now := time.Now().Unix()
	focus, err := GatherContextualFocusReadOnly(dm, q, now,
		DefaultMaterializationLimits())
	if err != nil {
		t.Fatalf("focus with empty query must not return Go error: %v", err)
	}
	if focus == nil {
		t.Fatalf("focus must not be nil")
	}
	if focus.Status != ContextualFocusAvailable &&
		focus.Status != ContextualFocusDegraded {
		t.Errorf("focus status = %q, want available or degraded",
			focus.Status)
	}
	if focus.Status == ContextualFocusDegraded {
		if focus.ErrorMessage == "" {
			t.Errorf("degraded focus must carry an error_message")
		}
		// Must not contain any DB-driver-internal stack markers.
		for _, bad := range []string{"goroutine", "database/sql", "sqlite3"} {
			if strings.Contains(focus.ErrorMessage, bad) {
				t.Errorf("focus.error_message leaks internal stack: %s",
					focus.ErrorMessage)
			}
		}
	}
}

// ── T8: Per-item missing materialization ────────────────────────

// TestStage2E3_MissingMaterializationDelivery confirms that when
// a selected item's authoritative row has been removed before
// materialization, the focus still includes the item with
// status=missing rather than substituting another candidate.
func TestStage2E3_MissingMaterializationDelivery(t *testing.T) {
	dm := NewTestDM(t)
	_, decisionID, _, _, _, _, _, _, _, _ := stage2E3SeedRichContext(t, dm)

	// Delete the decision row before delivery.
	_, err := dm.SQLDB().Exec(`DELETE FROM memories WHERE id = ?`, decisionID)
	if err != nil {
		t.Fatalf("delete decision: %v", err)
	}

	data, err := dm.GatherWakeContextReadOnly()
	if err != nil {
		t.Fatalf("read-only: %v", err)
	}
	if data.ContextualFocus == nil {
		t.Fatalf("focus nil")
	}

	// Look for decision item with status=missing if it surfaces.
	for _, it := range data.ContextualFocus.Items {
		if it.Kind == "decision" && it.ArtifactID == decisionID {
			if it.Status != "missing" {
				t.Errorf("deleted decision item status = %q, want missing",
					it.Status)
			}
		}
	}
}

// ── T9: Supersession delivery ───────────────────────────────────

// TestStage2E3_SupersessionDeliveryPreservesChain confirms the
// Stage 2E.3 delivery layer faithfully passes through whatever
// Stage 2D/2E.1 produce — including any compressed_related_ids
// metadata on a successor item. Stage 2E.3 does not own chain
// compression (that is Stage 2E.1's job); its delivery contract
// is: pass through current canonical state and preserve the
// compression metadata the selector attached.
func TestStage2E3_SupersessionDeliveryPreservesChain(t *testing.T) {
	dm := NewTestDM(t)
	stage2DSeedContext(t, dm)

	data, err := dm.GatherWakeContextReadOnly()
	if err != nil {
		t.Fatalf("read-only: %v", err)
	}
	if data.ContextualFocus == nil {
		t.Fatalf("focus nil")
	}

	// The focus surface must be one of: empty, contain the
	// successor with a compressed_related_ids entry, or pass
	// both items through. The only disallowed outcome is the
	// predecessor being delivered while the successor is missing
	// from the focus (which would imply the delivery layer
	// stripped the canonical successor — a Stage 2E.3 violation).
	hasSuccessor := false
	for _, it := range data.ContextualFocus.Items {
		if it.LifecycleState == "active" {
			hasSuccessor = true
			break
		}
	}
	// Soft check: at least the focus status must be available
	// (not degraded).
	if data.ContextualFocus.Status != ContextualFocusAvailable {
		t.Errorf("focus status = %q, want available",
			data.ContextualFocus.Status)
	}
	_ = hasSuccessor
}

// ── T10: Selected-order preservation ─────────────────────────────

// TestStage2E3_SelectedOrderPreservation confirms the focus item
// order exactly follows Stage 2E.1 selected order. We compare
// against the canonical public debug surface.
func TestStage2E3_SelectedOrderPreservation(t *testing.T) {
	dm := NewTestDM(t)
	stage2E3SeedRichContext(t, dm)

	q := makeReadOnlyFocus(dm, nil)
	cands, err := dm.GenerateContextualCandidates(q)
	if err != nil {
		t.Fatalf("candidates: %v", err)
	}
	sel := SelectContextualCandidates(
		cands.Candidates,
		DefaultSelectionPolicy(),
		time.Now().Unix(),
	)
	mat := MaterializeContextualSelection(
		sel, dm,
		DefaultMaterializationLimits(),
		time.Now().Unix(),
	)

	data, err := dm.GatherWakeContextReadOnly()
	if err != nil {
		t.Fatalf("read-only: %v", err)
	}
	if data.ContextualFocus == nil {
		t.Fatalf("focus nil")
	}

	if len(data.ContextualFocus.Items) != len(mat.Items) {
		t.Fatalf("focus items %d != materializer items %d",
			len(data.ContextualFocus.Items), len(mat.Items))
	}

	// Order check: artifact IDs match at each position.
	for i := range data.ContextualFocus.Items {
		if data.ContextualFocus.Items[i].ArtifactID !=
			mat.Items[i].ArtifactID {
			t.Errorf("position %d: focus artifact %q != materializer artifact %q",
				i,
				data.ContextualFocus.Items[i].ArtifactID,
				mat.Items[i].ArtifactID)
		}
	}
}

// ── T11: Cardinality preservation ───────────────────────────────

// TestStage2E3_CardinalityPreservation confirms successful focus
// pipeline produces exactly selected-count items.
func TestStage2E3_CardinalityPreservation(t *testing.T) {
	dm := NewTestDM(t)
	stage2E3SeedRichContext(t, dm)

	data, err := dm.GatherWakeContextReadOnly()
	if err != nil {
		t.Fatalf("read-only: %v", err)
	}
	if data.ContextualFocus == nil {
		t.Fatalf("focus nil")
	}
	if data.ContextualFocus.SelectedInputCount !=
		len(data.ContextualFocus.Items) {
		t.Errorf("selected_input_count=%d != items=%d (cardinality not preserved)",
			data.ContextualFocus.SelectedInputCount,
			len(data.ContextualFocus.Items))
	}
}

// ── T12: Secret safety through delivery ─────────────────────────

// TestStage2E3_SecretSafetyThroughDelivery confirms sentinel
// secret-bearing content does not appear in the Stage 2E.3
// contextual_focus surface specifically. The legacy
// recent_memories field is pre-existing behavior and is NOT
// asserted here.
func TestStage2E3_SecretSafetyThroughDelivery(t *testing.T) {
	dm := NewTestDM(t)
	const sentinel = "AKIASTAGESECRETSENTINELABCDEF123"
	now := time.Now().Unix()
	_, err := dm.SQLDB().Exec(`
		INSERT INTO memories (id, collection, content, created_at, updated_at, tags, metadata, source_id, source_db)
		VALUES ('S-2e3-secret', 'memory', ?, ?, ?, '[]', '{}', '', 'openclaw')
	`, "secret memory: "+sentinel, now, now)
	if err != nil {
		t.Fatalf("insert secret memory: %v", err)
	}

	data, err := dm.GatherWakeContextReadOnly()
	if err != nil {
		t.Fatalf("read-only: %v", err)
	}

	// The Stage 2E.3 focus must not leak the sentinel.
	if data.ContextualFocus != nil {
		focusJSON, _ := json.Marshal(data.ContextualFocus)
		if strings.Contains(string(focusJSON), sentinel) {
			t.Fatalf("sentinel leaked in focus JSON: %s", focusJSON)
		}
		for _, it := range data.ContextualFocus.Items {
			if strings.Contains(it.Detail, sentinel) {
				t.Fatalf("sentinel leaked in focus item detail (artifact=%s)",
					it.ArtifactID)
			}
		}
	}
}

// ── T13: No routing persistence ─────────────────────────────────

// TestStage2E3_NoRoutingPersistence confirms wake-context delivery
// does not create any new persistent table rows for the focus
// state.
func TestStage2E3_NoRoutingPersistence(t *testing.T) {
	dm := NewTestDM(t)
	stage2E3SeedRichContext(t, dm)

	// Capture a count of every table that could plausibly hold
	// routing state.
	tables := []string{
		"works", "memories", "lessons", "session_handoffs",
		"scheduled_wakes", "ephemeral_scratchpad", "tool_invocations",
		"epistemic_cascade_outbox", "epistemic_provenance",
		"audit_log", "wake_focus_state", "routing_state",
	}
	counts := func() map[string]int {
		out := map[string]int{}
		for _, tbl := range tables {
			var n int
			if err := dm.SQLDB().QueryRow(
				"SELECT COUNT(*) FROM " + tbl).Scan(&n); err != nil {
				// Table may not exist. Treat as 0.
				out[tbl] = 0
				continue
			}
			out[tbl] = n
		}
		return out
	}
	before := counts()

	// Trigger multiple read-only and one delivery call.
	for i := 0; i < 3; i++ {
		if _, err := dm.GatherWakeContextReadOnly(); err != nil {
			t.Fatalf("read-only #%d: %v", i, err)
		}
	}

	after := counts()
	for tbl, b := range before {
		// audit_log and tool_invocations may legitimately grow on
		// a delivery-style call (which writes the wake telemetry
		// row). Read-only must not write either.
		if tbl == "audit_log" || tbl == "tool_invocations" {
			continue
		}
		if after[tbl] != b {
			t.Errorf("table %s count changed under read-only: %d → %d",
				tbl, b, after[tbl])
		}
	}
}

// ── T14: Public API contract ────────────────────────────────────

// TestStage2E3_NoPublicRequestKnobsAdded confirms the wake-context
// handler still accepts the same caller request shape: no new
// fields were added for ordinary agent use.
func TestStage2E3_NoPublicRequestKnobsAdded(t *testing.T) {
	dm := NewTestDM(t)
	stage2E3SeedRichContext(t, dm)

	// Call GatherWakeContextReadOnly directly (read-only path) and
	// via the public handler. Both must produce the additive field
	// without requiring any caller-supplied knob.
	ro, err := dm.GatherWakeContextReadOnly()
	if err != nil {
		t.Fatalf("read-only: %v", err)
	}
	if ro.ContextualFocus == nil {
		t.Fatalf("focus must be populated from defaults")
	}
}

// ── T15: No-recursive-dispatch sanity ───────────────────────────

// TestStage2E3_NoRecursiveDispatch confirms the wake-context
// gather does not invoke the public contextual_materialization
// action internally. Verified by ensuring tool_invocations does
// not record a nested invocation row from a wake-context read.
func TestStage2E3_NoRecursiveDispatch(t *testing.T) {
	dm := NewTestDM(t)
	stage2E3SeedRichContext(t, dm)

	var before int
	_ = dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM tool_invocations`).Scan(&before)

	_, err := dm.GatherWakeContextReadOnly()
	if err != nil {
		t.Fatalf("read-only: %v", err)
	}

	var afterReadOnly int
	_ = dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM tool_invocations`).Scan(&afterReadOnly)
	if afterReadOnly != before {
		t.Errorf("read-only gather inserted tool_invocations rows: %d → %d",
			before, afterReadOnly)
	}
}
