// handlers_materialization.go — Stage 2E.2 public action handler.
//
// `mpm_context contextual_materialization` is a debugging / inspection
// surface. It runs the Stage 2D candidate generator, then the Stage
// 2E.1 selector, then the Stage 2E.2 bounded materializer, and
// returns the bounded MaterializationResult envelope.
//
// Not a new MCP tool — it is an action of the existing
// `mpm_context` tool. The 21-tool count is preserved.
//
// Materialization is observational: no mutation of handoff read state,
// wake firing, scratchpad, cascade state, audit, or any other
// persistent substrate state. Ordinary tool-invocation telemetry is
// normal MPM call infrastructure and is not considered selector or
// materialization mutation.

package tools

import (
	"fmt"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// handleContextualMaterialization runs Stage 2D candidate generation,
// Stage 2E.1 deterministic selection, and Stage 2E.2 bounded
// materialization, returning a bounded MaterializationResult envelope.
func handleContextualMaterialization(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	if dm == nil {
		return nil, fmt.Errorf("contextual_materialization: dm is nil")
	}
	q := mpminternal.ContextQuery{
		MPMSessionID:       ac.MPMSessionID,
		FrameworkSessionID: ac.FrameworkSessionID,
		FrameworkName:      ac.FrameworkName,
	}
	if v, ok := p["mpm_session_id"].(string); ok && v != "" {
		q.MPMSessionID = v
	}
	if v, ok := p["framework_session_id"].(string); ok && v != "" {
		q.FrameworkSessionID = v
	}
	if v, ok := p["framework_name"].(string); ok && v != "" {
		q.FrameworkName = v
	}
	if v, ok := p["work_ids"].([]interface{}); ok {
		for _, x := range v {
			if s, ok := x.(string); ok {
				q.WorkIDs = append(q.WorkIDs, s)
			}
		}
	}
	if v, ok := p["topic_ids"].([]interface{}); ok {
		for _, x := range v {
			if s, ok := x.(string); ok {
				q.TopicIDs = append(q.TopicIDs, s)
			}
		}
	}
	if v, ok := p["artifact_ids"].([]interface{}); ok {
		for _, x := range v {
			if s, ok := x.(string); ok {
				q.ArtifactIDs = append(q.ArtifactIDs, s)
			}
		}
	}

	// Build limits from caller params (selection_limits + materialization_limits).
	limits := mpminternal.DefaultMaterializationLimits()
	if ml, ok := p["materialization_limits"].(map[string]interface{}); ok {
		if v, ok := ml["detail_budget_bytes"].(float64); ok && v > 0 {
			limits.DetailBudgetBytes = int(v)
		}
		if v, ok := ml["per_item_byte_cap"].(float64); ok && v > 0 {
			limits.PerItemByteCap = int(v)
		}
	}
	policy := mpminternal.DefaultSelectionPolicy()
	if sl, ok := p["selection_limits"].(map[string]interface{}); ok {
		if v, ok := sl["max_items"].(float64); ok && v > 0 {
			policy.Limits.MaxItems = int(v)
		}
	}

	// Step 1: candidates.
	res, err := dm.GenerateContextualCandidates(q)
	if err != nil {
		return nil, fmt.Errorf("contextual_materialization: candidates: %w", err)
	}

	// Step 2: selection.
	now := int64(0)
	if v, ok := p["now"].(float64); ok {
		now = int64(v)
	} else {
		now = res.Diagnostics.GeneratedAt
	}
	candidates := make([]mpminternal.Candidate, 0, len(res.Candidates))
	for _, c := range res.Candidates {
		candidates = append(candidates, c)
	}
	sel := mpminternal.SelectContextualCandidates(candidates, policy, now)

	// Step 3: materialization. MaterializeContextualSelection needs
	// the concrete *DatabaseManager (kind-specific getters use
	// internal fields); type-assert the CoreDB interface.
	dmConcrete, ok := dm.(*mpminternal.DatabaseManager)
	if !ok {
		return nil, fmt.Errorf("contextual_materialization: dm is not *DatabaseManager")
	}
	mat := mpminternal.MaterializeContextualSelection(sel, dmConcrete, limits, now)

	return map[string]interface{}{
		"success":     true,
		"items":       mat.Items,
		"count":       mat.Diagnostics.OutputItemCount,
		"diagnostics": mat.Diagnostics,
	}, nil
}
