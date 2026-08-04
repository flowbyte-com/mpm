// epistemology_tools.go — DM methods for the cognitive immune system
// and decision ledger.
//
// Five methods covering the full theory lifecycle:
//   - ChallengeMemoryWithTheory: weaken a memory + create a pending theory
//   - ProposeTheory: log a hypothesis with validation criteria
//   - ResolveTheory: close the loop (proven / disproven)
//   - RecordDecision: persist a decision with context + rationale
//   - ReviewMemories: spaced reinforcement review (Tier 2)
package internal

import (
	"encoding/json"
	"fmt"
	"time"
)

// ChallengeMemoryWithTheory weakens a memory and creates a pending theory.
// Mirrors callChallengeMemory.
func (dm *DatabaseManager) ChallengeMemoryWithTheory(memoryID, evidence string) (map[string]interface{}, error) {
	mem, err := dm.GetMemory(memoryID)
	if err != nil {
		return nil, fmt.Errorf("memory not found: %w", err)
	}
	if err := dm.ChallengeMemory(memoryID, -2, evidence); err != nil {
		return nil, fmt.Errorf("weaken memory: %w", err)
	}
	theoryContent := fmt.Sprintf("CHALLENGED_MEMORY_ID: %s\nEVIDENCE: %s\nORIGINAL_CONTENT: %s",
		memoryID, evidence, mem["content"])
	theoryMeta := map[string]interface{}{
		"status":               "pending",
		"challenged_memory_id": memoryID,
		"evidence":             evidence,
	}
	store, err := dm.getSharedStore()
	if err != nil {
		return nil, fmt.Errorf("get memory store: %w", err)
	}
	theory, err := store.AddMemory(theoryContent, "theories", []string{"challenge"}, theoryMeta, "", "call")
	if err != nil {
		return nil, fmt.Errorf("create theory: %w", err)
	}
	return map[string]interface{}{
		"success":       true,
		"memory_id":     memoryID,
		"action":        "weakened",
		"theory_id":     theory.ID,
		"theory_status": "pending",
	}, nil
}

// ProposeTheory logs a hypothesis with validation criteria and auto-links
// to the "theories" topic. Mirrors callProposeTheory.
//
// dependencies: optional list of artifact IDs (memories, lessons, other
// theories) that this theory's reasoning depends on. Stored as a JSON
// array in the `dependencies` column. When any of these are deleted
// (soft or hard), FireStaleFoundationWakes emits a wake per dependent
// theory so the agent can re-evaluate. Empty/nil is fine — that's the
// default for theories with no forward dependencies.
//
// sourceIDs: optional list of artifact IDs this theory cites in its
// provenance (retrieval-time citations the agent recorded as evidence
// for the hypothesis). Unlike dependencies, source_ids do not fire
// deletion wakes — they are retrospective citations that the cascade
// materializer uses to discover dependency edges when an upstream
// artifact is invalidated. Each entry becomes one row in
// epistemic_provenance with downstream_type='theory'.
//
// The two surfaces coexist on purpose: `dependencies` is the
// forward-looking "I will be stale if X goes away" edge,
// `source_ids` is the retrospective "I drew on X to reason about
// this" edge. The cascade materializer unions both at lookup time.
//
// Atomicity: the artifact insert + dependencies column update +
// citation loop run inside a single WithTx transaction. A failure
// in any of those steps rolls back the entire theory row, so a
// half-written theory with no citations cannot exist. The
// topic-link step (AddMemoryToTopic) runs outside the transaction
// because topic memberships are observability metadata, not
// load-bearing for the cascade materializer.
func (dm *DatabaseManager) ProposeTheory(hypothesis, validationCriteria string, dependencies []string, sourceIDs []string, tags []string) (map[string]interface{}, error) {
	if tags == nil {
		tags = []string{}
	}
	content := hypothesis
	if validationCriteria != "" {
		content += "\n\nVALIDATION_CRITERIA: " + validationCriteria
	}
	depsJSON, err := encodeDependencyList(dependencies)
	if err != nil {
		return nil, fmt.Errorf("encode dependencies: %w", err)
	}
	meta := map[string]interface{}{
		"status":              "pending",
		"validation_criteria": validationCriteria,
		"dependencies":        dependencies,
	}

	var memID string
	err = dm.WithTx(func(node DBNode) error {
		id, err := dm.SaveMemoryNode(node, "theories", content, "", tags, meta, nil, false, 1, "", "0.5", "0.5", "")
		if err != nil {
			return fmt.Errorf("propose theory: %w", err)
		}
		// Persist dependencies in the dedicated column. SaveMemoryNode
		// doesn't accept a column-list, so we patch via the SQL
		// interface directly inside the same transaction.
		if depsJSON != "" {
			if _, err := node.ExecTracked(
				`UPDATE memories SET dependencies = ? WHERE id = ? AND collection = 'theories'`,
				0, depsJSON, id,
			); err != nil {
				return fmt.Errorf("persist dependencies: %w", err)
			}
		}
		// Persist source_ids as typed provenance citations so the
		// cascade materializer can discover dependency edges during
		// invalidation. Event id is the theory's own id — this is the
		// "explicit decision/time citation" case in the design spec
		// (vs. retrieval-time citations whose event_id is the wake id).
		if err := dm.recordSourceCitationsNode(node, sourceIDs, id, "theory"); err != nil {
			return fmt.Errorf("persist theory source_ids: %w", err)
		}
		memID = id
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Topic link is outside the tx — topic_memberships is
	// observability, not load-bearing for the cascade materializer.
	// A failure here is non-fatal and the user still gets a
	// successful theory id.
	topicID, _ := dm.GetOrCreateTopic("theories")
	_ = dm.AddMemoryToTopic(memID, topicID, "primary")
	return map[string]interface{}{
		"success":      true,
		"id":           memID,
		"status":       "pending",
		"hypothesis":   hypothesis,
		"dependencies": dependencies,
	}, nil
}

// encodeDependencyList converts a dependency list to the JSON storage
// format used in the `dependencies` column. Returns "" for empty/nil —
// the empty string is the column's sentinel for "no dependencies",
// distinct from NULL which means "column not yet populated" for
// pre-migration rows. SQLite's json_each over an empty string returns
// zero rows, so the wake-on-delete scan treats both cases as "no
// forward dependencies".
func encodeDependencyList(deps []string) (string, error) {
	if len(deps) == 0 {
		return "", nil
	}
	out, err := json.Marshal(deps)
	if err != nil {
		return "", fmt.Errorf("marshal deps: %w", err)
	}
	return string(out), nil
}

// ResolveTheory marks a theory as proven or disproven. Mirrors callResolveTheory.
func (dm *DatabaseManager) ResolveTheory(theoryID, conclusion, newStatus string) (map[string]interface{}, error) {
	if newStatus != "proven" && newStatus != "disproven" {
		return nil, fmt.Errorf("newStatus must be 'proven' or 'disproven'")
	}
	mem, err := dm.GetMemory(theoryID)
	if err != nil {
		return nil, fmt.Errorf("theory not found: %w", err)
	}
	if coll, _ := mem["collection"].(string); coll != "theories" {
		return nil, fmt.Errorf("memory %s is not a theory (collection: %s)", theoryID, coll)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	patch := map[string]interface{}{
		"status":      newStatus,
		"conclusion":  conclusion,
		"resolved_at": now,
	}
	patchJSON, _ := json.Marshal(patch)
	if err := dm.UpdateMemoryMetadata(theoryID, string(patchJSON)); err != nil {
		return nil, fmt.Errorf("resolve theory: %w", err)
	}
	dm.ReinforceMemory(theoryID, 1)
	return map[string]interface{}{
		"success":     true,
		"id":          theoryID,
		"status":      newStatus,
		"conclusion":  conclusion,
		"resolved_at": now,
	}, nil
}

// RecordDecision logs an architectural decision. Mirrors callRecordDecision.
// ac injects provenance + active mode/persona into meta so downstream
// consumers can attribute the decision to the agent's runtime context.
//
// sourceIDs: optional list of artifact IDs the agent cited as evidence
// for this decision. Each entry becomes one row in epistemic_provenance
// with downstream_type='decision', so the cascade materializer can walk
// the dependency graph when an upstream artifact is invalidated. Empty
// or nil is fine — most decisions are made without named citations.
//
// Atomicity: the artifact insert + citation loop run inside a single
// WithTx transaction. A failure in the citation loop rolls back the
// decision row, so a half-written decision with no citations cannot
// exist. This is the fix for the original "artifact-first, citations
// second" ordering — see the Task 2 review note for the failure mode.
func (dm *DatabaseManager) RecordDecision(contextText, choice, rationale, outcome string, tags []string, sourceIDs []string, ac ActiveContext) (map[string]interface{}, error) {
	if tags == nil {
		tags = []string{}
	}
	content := "CHOICE: " + choice
	if contextText != "" {
		content += "\nCONTEXT: " + contextText
	}
	if rationale != "" {
		content += "\nRATIONALE: " + rationale
	}
	if outcome != "" {
		content += "\nOUTCOME: " + outcome
	}
	meta := ac.withActiveContextMeta(nil)
	if contextText != "" {
		meta["context"] = contextText
	}
	if rationale != "" {
		meta["rationale"] = rationale
	}

	var memID string
	err := dm.WithTx(func(node DBNode) error {
		id, err := dm.SaveMemoryNode(node, "decisions", content, "", tags, meta, nil, false, 1, "", "0.5", "0.5", "")
		if err != nil {
			return fmt.Errorf("record decision: %w", err)
		}
		// Persist source_ids as typed provenance citations. Event id
		// is the decision's own id — same convention as ProposeTheory.
		if err := dm.recordSourceCitationsNode(node, sourceIDs, id, "decision"); err != nil {
			return fmt.Errorf("persist decision source_ids: %w", err)
		}
		memID = id
		return nil
	})
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"success": true,
		"id":      memID,
		"choice":  choice,
	}, nil
}

// recordSourceCitationsNode writes one epistemic_provenance row per
// non-empty source id, using the supplied downstream id and type.
// Shared helper so ProposeTheory and RecordDecision share one code
// path — the cascade materializer relies on the citation shape being
// uniform across both artifact kinds.
//
// The DBNode parameter lets the caller run the citation writes inside
// an active transaction (e.g. ProposeTheory wraps the artifact insert
// + citation loop in one WithTx so a mid-loop failure rolls back the
// artifact itself).
//
// Empty / nil sourceIDs is a clean no-op (no rows to write).
func (dm *DatabaseManager) recordSourceCitationsNode(node DBNode, sourceIDs []string, downstreamID, downstreamType string) error {
	if len(sourceIDs) == 0 {
		return nil
	}
	if downstreamID == "" {
		return fmt.Errorf("recordSourceCitations: downstream_id is empty")
	}
	if downstreamType == "" {
		return fmt.Errorf("recordSourceCitations: downstream_type is empty")
	}
	for _, src := range sourceIDs {
		if src == "" {
			// Skip empties rather than fail — the source_ids JSON
			// sometimes carries a trailing empty from upstream
			// parsers and the user's intent is "ignore blanks".
			continue
		}
		if err := dm.recordProvenanceNode(node, src, "", downstreamID, downstreamType, downstreamID); err != nil {
			return err
		}
	}
	return nil
}

// ReviewMemories returns memories due for spaced reinforcement review:
// LTM or high-weight memories not accessed in `days`+ days. This is the
// read-only counterpart to handleReview's CLI flag parsing — the JSON
// result is suitable for both the agent's next-action selection and
// the web UI's review panel.
func (dm *DatabaseManager) ReviewMemories(daysSinceAccess, limit int) (map[string]interface{}, error) {
	if daysSinceAccess <= 0 {
		daysSinceAccess = 30
	}
	if limit <= 0 {
		limit = 20
	}
	items, err := dm.GetSpacedReinforcementReview(daysSinceAccess, limit)
	if err != nil {
		return nil, fmt.Errorf("review: %w", err)
	}
	return map[string]interface{}{
		"success": true,
		"items":   items,
		"count":   len(items),
		"days":    daysSinceAccess,
		"limit":   limit,
	}, nil
}