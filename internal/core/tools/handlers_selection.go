// handlers_selection.go — Stage 2E.1 public action handler.
//
// `mpm_context contextual_selection` is a debugging / inspection
// surface. It runs the Stage 2D candidate generator, then runs
// the pure selector, and returns the bounded SelectionResult.
//
// Not a new MCP tool — it is an action of the existing
// `mpm_context` tool. The 21-tool count is preserved.
//
// Selection is observational: no mutation of handoff read state,
// wake firing, scratchpad, cascade state, audit (the selector
// itself), or any other persistent substrate state. Ordinary
// tool-invocation telemetry is normal MPM call infrastructure
// and is not considered selector mutation.

package tools

import (
	"fmt"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// handleContextualSelection runs Stage 2D candidate generation
// + Stage 2E.1 deterministic selection and returns a bounded
// SelectionResult envelope.
func handleContextualSelection(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	if dm == nil {
		return nil, fmt.Errorf("contextual_selection: dm is nil")
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
	// Build a CandidateGenerationResult → slice of Candidate
	// for the selector.
	res, err := dm.GenerateContextualCandidates(q)
	if err != nil {
		return nil, fmt.Errorf("contextual_selection: %w", err)
	}
	candidates := make([]mpminternal.Candidate, 0, len(res.Candidates))
	for _, c := range res.Candidates {
		candidates = append(candidates, c)
	}

	// Build policy from caller params (selection_limits.max_items).
	policy := mpminternal.DefaultSelectionPolicy()
	if limits, ok := p["selection_limits"].(map[string]interface{}); ok {
		if v, ok := limits["max_items"].(float64); ok {
			policy.Limits.MaxItems = int(v)
		}
	}

	// now: caller may supply a pin (unix seconds). If absent,
	// use the generation's generated_at for determinism.
	now := int64(0)
	if v, ok := p["now"].(float64); ok {
		now = int64(v)
	} else {
		now = res.Diagnostics.GeneratedAt
	}

	sel := mpminternal.SelectContextualCandidates(candidates, policy, now)
	return map[string]interface{}{
		"success":     true,
		"items":       sel.Items,
		"count":       sel.Diagnostics.SelectedCount,
		"diagnostics": sel.Diagnostics,
	}, nil
}
