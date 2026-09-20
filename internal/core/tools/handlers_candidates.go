// handlers_candidates.go — mpm_context action=contextual_candidates.
// Stage 2D: deterministic bounded candidate generation surface for
// debugging + future Stage 2E ranking. Read-only, no LLM, no
// embeddings.

package tools

import (
	"fmt"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// handleContextualCandidates implements mpm_context
// action=contextual_candidates. It composes the canonical
// ContextQuery from ActiveContext identity fields plus caller-
// supplied params, then delegates to dm.GenerateContextualCandidates.
//
// Identity plumbing (Stage 2C/2C.1/2C.2) means we read
//   - ac.MPMSessionID        → ContextQuery.MPMSessionID
//   - ac.FrameworkSessionID  → ContextQuery.FrameworkSessionID
//   - ac.FrameworkName       → ContextQuery.FrameworkName
//   - ac.SessionID           → carried separately for legacy dispatch
//     grouping; not collapsed into MPM_SESSION_ID semantics.
// Caller-supplied params override defaults but never collapse one
// identity axis into another.
func handleContextualCandidates(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, p map[string]interface{}) (interface{}, error) {
	if dm == nil {
		return nil, fmt.Errorf("contextual_candidates: dm is nil")
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
		for _, id := range v {
			if s, ok := id.(string); ok && s != "" {
				q.WorkIDs = append(q.WorkIDs, s)
			}
		}
	}
	if v, ok := p["topic_ids"].([]interface{}); ok {
		for _, id := range v {
			if s, ok := id.(string); ok && s != "" {
				q.TopicIDs = append(q.TopicIDs, s)
			}
		}
	}
	if v, ok := p["artifact_ids"].([]interface{}); ok {
		for _, id := range v {
			if s, ok := id.(string); ok && s != "" {
				q.ArtifactIDs = append(q.ArtifactIDs, s)
			}
		}
	}
	if v, ok := p["query_text"].(string); ok {
		q.QueryText = v
	}
	// Optional caller overrides for the per-source / global caps.
	if v, ok := p["limits"].(map[string]interface{}); ok {
		if x, ok := v["work"].(float64); ok {
			q.Limits.Work = int(x)
		}
		if x, ok := v["handoff"].(float64); ok {
			q.Limits.Handoff = int(x)
		}
		if x, ok := v["activity"].(float64); ok {
			q.Limits.Activity = int(x)
		}
		if x, ok := v["epistemic"].(float64); ok {
			q.Limits.Epistemic = int(x)
		}
		if x, ok := v["cascade"].(float64); ok {
			q.Limits.Cascade = int(x)
		}
		if x, ok := v["wake"].(float64); ok {
			q.Limits.Wake = int(x)
		}
		if x, ok := v["scratchpad"].(float64); ok {
			q.Limits.Scratchpad = int(x)
		}
		if x, ok := v["topic"].(float64); ok {
			q.Limits.Topic = int(x)
		}
		if x, ok := v["global"].(float64); ok {
			q.Limits.Global = int(x)
		}
	}

	res, err := dm.GenerateContextualCandidates(q)
	if err != nil {
		return nil, fmt.Errorf("contextual_candidates: %w", err)
	}
	return map[string]interface{}{
		"success":     true,
		"candidates":  res.Candidates,
		"count":       len(res.Candidates),
		"sources":     res.Sources,
		"diagnostics": res.Diagnostics,
	}, nil
}
