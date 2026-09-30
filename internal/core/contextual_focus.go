// contextual_focus.go — Stage 2E.3 wake-context projection and
// delivery integration.
//
// Architecture placement
// ────────────────────────
//
//	discovery         (Stage 2D  — GenerateContextualCandidates)
//	selection         (Stage 2E.1 — SelectContextualCandidates)
//	materialization   (Stage 2E.2 — MaterializeContextualSelection)
//	wake projection   (Stage 2E.3 — THIS FILE)
//	wake delivery     (existing — GatherWakeContext)
//
// Stage 2E.3 packages the bounded materialization result into a
// compact delivery type and integrates it as an additive
// `contextual_focus` field on WakeContextData. It does NOT rerank,
// does NOT re-select, does NOT persist routing state, does NOT
// call LLM, does NOT consume handoffs.
//
// Observational guarantee
// ────────────────────────
//   - inherits every Stage 2D / 2E.1 / 2E.2 guarantee (no table
//     mutation, no read markers advanced, no wake firing, no
//     scratchpad writes, no audit writes)
//   - never re-invokes the public contextual_materialization
//     action via the dispatcher
//   - only call into the substrate is the same Stage 2D / 2E.1 /
//     2E.2 functions the public action already calls
//
// Deterministic
// ─────────────
//   identical (ContextQuery, now, limits) → byte-equivalent
//   semantic output. Output order matches selection order
//   (Stage 2E.1's deterministic policy order is preserved).

package internal

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
)

// ContextualFocusStatus is the closed vocabulary for the top-level
// projection outcome. It is intentionally distinct from the per-item
// MaterializationStatus (which is `materialized` / `pointer_only` /
// `missing` / `unsupported` / `error`).
type ContextualFocusStatus string

const (
	// ContextualFocusAvailable — the projection pipeline ran to
	// completion. The Items slice may still be empty (no
	// candidates selected) but the projection itself is healthy.
	ContextualFocusAvailable ContextualFocusStatus = "available"
	// ContextualFocusDegraded — the projection pipeline failed
	// unexpectedly. Items may be empty or partial; legacy wake
	// context still returns; the raw Go error is NOT exposed to
	// the agent — only a safe internal diagnostic.
	ContextualFocusDegraded ContextualFocusStatus = "degraded"
)

// ContextualFocusTrigger is the bounded wire form of
// SelectionTrigger for the delivery envelope. Compact enough to
// ship to agents without leaking Stage 2E.1 internal
// SelectionTrigger structure.
type ContextualFocusTrigger struct {
	SourceID        string `json:"source_id,omitempty"`
	Band            string `json:"band,omitempty"`
	Rationale       string `json:"rationale,omitempty"`
	CombinationName string `json:"combination_name,omitempty"`
}

// ContextualFocusItem is one delivered focus item. Designed to
// answer WHY/WHAT/WHERE for the receiving agent with bounded
// detail and full pointer provenance. Detail is the Stage 2E.2
// safe-detail payload (already bounded); truncated is orthogonal.
type ContextualFocusItem struct {
	// ── Identity envelope ──
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	ArtifactID string `json:"artifact_id"`
	Pointer    string `json:"pointer,omitempty"`

	// ── Why — band / why_now ──
	Band      string `json:"band"`
	Rationale string `json:"rationale"`
	WhyNow    string `json:"why_now"`

	// ── What — bounded detail ──
	Status    string `json:"status"`
	Detail    string `json:"detail,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`

	// ── Where / supplementary ──
	LifecycleState       string                   `json:"lifecycle_state,omitempty"`
	SelectionTriggers    []ContextualFocusTrigger `json:"selection_triggers,omitempty"`
	CompressedRelatedIDs []string                 `json:"compressed_related_ids,omitempty"`
}

// ContextualFocus is the compact delivery projection of the
// contextual routing pipeline. It deliberately omits:
//   - the full Stage 2D Candidate envelope
//   - selection diagnostics
//   - materialization diagnostics
//   - source diagnostic maps
//   - combination debug metadata beyond the compact trigger form
//
// Agents needing the debug envelope call the public
// contextual_materialization action.
type ContextualFocus struct {
	Status             ContextualFocusStatus `json:"status"`
	ErrorMessage       string                `json:"error_message,omitempty"`
	SelectedInputCount int                   `json:"selected_input_count"`
	Items              []ContextualFocusItem `json:"items"`
}

// ContextualFocusFocusBuildConfig collects the parameters of one
// wake-context focus projection. Both read-only and delivery paths
// populate it identically — they differ only in the OUTSIDE
// delivery mutation (handoff read marker).
type ContextualFocusBuildConfig struct {
	Now    int64
	Limits MaterializationLimits
	Query  ContextQuery
}

// DefaultContextualFocusConfig returns the canonical normal-wake
// delivery configuration: default materialization limits, current
// epoch-second timestamp, and a query populated from authoritative
// wake-context state (MPM session, framework session, framework
// name, active work IDs).
func DefaultContextualFocusConfig(workIDs []string) ContextualFocusBuildConfig {
	return ContextualFocusBuildConfig{
		Now:    nowUnix(),
		Limits: DefaultMaterializationLimits(),
		Query: ContextQuery{
			MPMSessionID:       CurrentMPMSessionID(),
			FrameworkSessionID: os.Getenv("MPM_PROVENANCE_FRAMEWORK_SESSION_ID"),
			FrameworkName:      os.Getenv("MPM_PROVENANCE_FRAMEWORK"),
			WorkIDs:            workIDs,
		},
	}
}

// nowUnix is the canonical wall-clock source for projection
// timestamps. Pulled into a single helper so test seams can pin
// it without leaking time.Now calls into the projection builder.
var nowUnix = func() int64 { return timeNowUnix() }

// GatherContextualFocusReadOnly is the shared Stage 2D → 2E.1 →
// 2E.2 → 2E.3 projection builder. Both the read-only preview path
// and the delivery path invoke this same function. The ONLY
// delivery mutation (handoff read marker) is performed outside
// this builder in gatherWakeContext.
//
// Inputs:
//
//	dm   — the substrate (must be non-nil)
//	q    — bounded ContextQuery
//	now  — single authoritative epoch-second timestamp
//	lims — bounded materialization limits (zero values fall back
//	       to DefaultMaterializationLimits)
//
// Output:
//
//	*ContextualFocus — never nil; always safe to dereference
//
// Guarantees:
//   - no persistent mutation
//   - no handoff read marker advancement
//   - no wake firing
//   - no scratchpad writes
//   - no audit writes
//   - never returns a Go error for missing data; degrades
//     gracefully to status=degraded with a safe internal
//     diagnostic if the projection pipeline itself fails.
func GatherContextualFocusReadOnly(
	dm *DatabaseManager,
	q ContextQuery,
	now int64,
	lims MaterializationLimits,
) (*ContextualFocus, error) {
	if dm == nil {
		return &ContextualFocus{
			Status:             ContextualFocusDegraded,
			ErrorMessage:       "substrate_unavailable",
			SelectedInputCount: 0,
			Items:              []ContextualFocusItem{},
		}, nil
	}

	// Step 1 — candidates (Stage 2D).
	candRes, err := dm.GenerateContextualCandidates(q)
	if err != nil {
		slog.Warn("contextual_focus: candidates failed",
			"err", err.Error())
		return &ContextualFocus{
			Status:             ContextualFocusDegraded,
			ErrorMessage:       "candidate_generation_failed",
			SelectedInputCount: 0,
			Items:              []ContextualFocusItem{},
		}, nil
	}

	// Step 2 — selection (Stage 2E.1).
	selNow := now
	if selNow == 0 {
		selNow = candRes.Diagnostics.GeneratedAt
	}
	cands := make([]Candidate, 0, len(candRes.Candidates))
	cands = append(cands, candRes.Candidates...)
	sel := SelectContextualCandidates(cands, DefaultSelectionPolicy(), selNow)

	// Step 3 — materialization (Stage 2E.2).
	mat := MaterializeContextualSelection(sel, dm, lims, selNow)

	// Step 4 — compact delivery projection (Stage 2E.3).
	focus := &ContextualFocus{
		Status:             ContextualFocusAvailable,
		SelectedInputCount: len(mat.Items),
		Items:              projectToDeliveryItems(mat.Items),
	}
	return focus, nil
}

// projectToDeliveryItems converts the Stage 2E.2 materialization
// envelope into the compact delivery type. Drops the Candidate
// envelope, selection diagnostics, materialization diagnostics,
// and debug metadata beyond the compact trigger form.
//
// Order is preserved exactly: 1 selected == 1 output.
func projectToDeliveryItems(items []MaterializedContextItem) []ContextualFocusItem {
	out := make([]ContextualFocusItem, 0, len(items))
	for _, it := range items {
		triggers := make([]ContextualFocusTrigger, 0, len(it.CompressionTriggers))
		for _, tr := range it.CompressionTriggers {
			triggers = append(triggers, ContextualFocusTrigger{
				SourceID:        tr.SourceID,
				Band:            tr.Band,
				Rationale:       tr.Rationale,
				CombinationName: tr.CombinationName,
			})
		}
		out = append(out, ContextualFocusItem{
			ID:                   it.CandidateID,
			Kind:                 it.Kind,
			ArtifactID:           it.ArtifactID,
			Pointer:              it.Pointer,
			Band:                 it.Band,
			Rationale:            string(it.Rationale),
			WhyNow:               it.WhyNow,
			Status:               string(it.Status),
			Detail:               it.Detail,
			Truncated:            it.Truncated,
			LifecycleState:       it.LifecycleState,
			SelectionTriggers:    triggers,
			CompressedRelatedIDs: it.CompressedRelatedIDs,
		})
	}
	return out
}

// formatContextualFocus renders the compact focus as a human-
// readable block. One line per item; respects selection order.
func formatContextualFocus(f *ContextualFocus) string {
	if f == nil {
		return ""
	}
	if f.Status != ContextualFocusAvailable {
		return "**Contextual focus:** <degraded> (projection pipeline could not run; legacy wake context still valid)"
	}
	if len(f.Items) == 0 {
		return "**Contextual focus:** (no items selected)"
	}
	lines := []string{
		fmt.Sprintf("**Contextual focus** (%d items):", len(f.Items)),
	}
	for _, it := range f.Items {
		head := fmt.Sprintf("  - [%s] %s:%s", it.Band, it.Kind, it.ArtifactID)
		if it.Status != "materialized" {
			head += fmt.Sprintf(" [%s]", it.Status)
		}
		if it.Detail != "" {
			// Reuse wake-context truncation contract: rune-safe
			// cap at 240 chars to keep prose readable.
			detail := it.Detail
			if len(detail) > 240 {
				detail = truncateUTF8(detail, 240)
			}
			head += " — " + detail
			if it.Truncated {
				head += "…"
			}
		}
		if it.Pointer != "" {
			head += "  (pointer: " + it.Pointer + ")"
		}
		lines = append(lines, head)
	}
	return joinLines(lines)
}

// joinLines is a tiny helper that joins lines with the canonical
// newline separator. Wrapped so future formatting changes do not
// ripple.
func joinLines(lines []string) string {
	out := ""
	for i, l := range lines {
		if i > 0 {
			out += "\n"
		}
		out += l
	}
	return out
}

// serializeContextualFocus produces a deterministic JSON envelope
// of the focus for diagnostics / wire logging. Output is
// sorted-stable by Go's encoding/json rules (struct field order).
func serializeContextualFocus(f *ContextualFocus) string {
	if f == nil {
		return "{}"
	}
	b, err := json.Marshal(f)
	if err != nil {
		return fmt.Sprintf(`{"status":"%s","serialization_error":true}`, f.Status)
	}
	return string(b)
}

// gatherActiveWorkIDs returns the authoritative list of open
// work IDs from the substrate, ordered most-recently-touched
// first. Mirrors the gatherOpenWorks ordering so the focus
// builder gets the same structural relationship data. Capped
// at the canonical 5 entries to match the legacy wake-context
// envelope.
func (dm *DatabaseManager) gatherActiveWorkIDs() []string {
	if dm == nil || dm.db == nil {
		return nil
	}
	rows, err := dm.db.Query(`
		SELECT id FROM works
		WHERE status = 'open' AND archived_at IS NULL
		ORDER BY updated_at DESC, created_at DESC
		LIMIT 5
	`)
	if err != nil {
		dm.LogAudit(AuditWarn, "wake_context", "gatherActiveWorkIDs: "+err.Error(), "", AuditContext{})
		return nil
	}
	defer rows.Close()
	out := make([]string, 0, 5)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			dm.LogAudit(AuditWarn, "wake_context", "scan active work id: "+err.Error(), "", AuditContext{})
			continue
		}
		out = append(out, id)
	}
	return out
}

// buildWakeContextFocusQuery builds the canonical ContextQuery
// for the normal wake delivery path. Populates only fields
// already known authoritatively from wake-context state:
//
//   - MPMSessionID:        CurrentMPMSessionID()
//   - FrameworkSessionID:  MPM_PROVENANCE_FRAMEWORK_SESSION_ID env
//   - FrameworkName:       MPM_PROVENANCE_FRAMEWORK env (canonical
//     provenance framework name; with fallback
//     MPM_FRAMEWORK for legacy adapter support)
//   - WorkIDs:             active open work IDs (gatherActiveWorkIDs)
//
// It does NOT synthesize QueryText, TopicIDs, or ArtifactIDs
// from handoff summary / work title / directives / prose. Stage
// 2E.3 remains structurally deterministic — no LLM, no
// adaptive routing, no inferred context.
func buildWakeContextFocusQuery(workIDs []string) ContextQuery {
	fw := os.Getenv("MPM_PROVENANCE_FRAMEWORK")
	if fw == "" {
		fw = os.Getenv("MPM_FRAMEWORK")
	}
	return ContextQuery{
		MPMSessionID:       CurrentMPMSessionID(),
		FrameworkSessionID: os.Getenv("MPM_PROVENANCE_FRAMEWORK_SESSION_ID"),
		FrameworkName:      fw,
		WorkIDs:            workIDs,
	}
}
