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

// DecisionInitialConfidence is the per-type baseline confidence value a
// decision falls back to when its evidence is neutralized (F5-2). It
// matches the per-type initial value used by SaveMemoryNode (0.5 for
// decisions), and is identical in spirit to ChallengedMemoryConfidenceFloor
// for memories: the artifact is at "no evidence" until fresh evidence
// arrives, rather than retaining its pre-correction value.
//
// Exported so the regression test in internal/core can reference it
// without duplicating the constant.
const DecisionInitialConfidence = 0.5

// ChallengeMemoryWithTheory weakens a memory and creates a pending theory.
// Mirrors callChallengeMemory.
func (dm *DatabaseManager) ChallengeMemoryWithTheory(memoryID, evidence string) (map[string]interface{}, error) {
	mem, err := dm.GetMemory(memoryID)
	if err != nil {
		// GetMemory already returns the canonical "memory not found: <id>"
		// error (F7 fix). Wrapping again would produce a doubled prefix
		// visible to the agent caller: "memory not found: memory not found: <id>".
		return nil, err
	}
	// slashAmount is a POSITIVE weight reduction (see ChallengeMemory). The
	// historic caller passed -2 here; ChallengeMemory computes
	// `weight = MAX(1, weight - slashAmount)`, so a negative amount silently
	// INCREASED the disputed memory's weight (5 → 7) — the exact opposite of
	// "weaken". Challenging knowledge must never raise its rank.
	if err := dm.ChallengeMemory(memoryID, 2, evidence); err != nil {
		return nil, fmt.Errorf("weaken memory: %w", err)
	}
	// A challenge is an EVENT: repeating it with identical evidence still
	// produces a distinct theory row (the CLI path behaves the same way).
	// Baking the wall-clock into the content keeps each challenge's
	// identity unique under the F19 idempotency rule without special-
	// casing theories out of dedup.
	theoryContent := fmt.Sprintf("CHALLENGED_MEMORY_ID: %s\nEVIDENCE: %s\nCHALLENGED_AT_NANO: %d\nORIGINAL_CONTENT: %s",
		memoryID, evidence, time.Now().UnixNano(), mem["content"])
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

	// Point the memory's forward link at the REAL theory row. ChallengeMemory
	// stashes the raw evidence text into challenged_theory_id (the arbitration
	// paths have no theory row); now that a theory exists, replace it so
	// `mpm challenge restore` resolves the actual theory instead of no-oping
	// against prose. The evidence text moves to challenged_evidence.
	linkPatch, _ := json.Marshal(map[string]interface{}{
		"challenged_theory_id": theory.ID,
		"challenged_evidence":  evidence,
	})
	if _, err := dm.SQLDB().Exec(
		`UPDATE memories SET metadata = json_patch(COALESCE(metadata,'{}'), ?) WHERE id = ? AND deleted_at IS NULL`,
		string(linkPatch), memoryID,
	); err != nil {
		return nil, fmt.Errorf("link challenge theory: %w", err)
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
	return dm.ProposeTheoryWithExtras(hypothesis, validationCriteria, dependencies, sourceIDs, tags, nil)
}

// ProposeTheoryWithExtras extends ProposeTheory with an optional map of
// additional top-level metadata fields. Used by the cascade materializer
// to store cascade metadata (cascade=true, cascade_version, dead_artifact_id,
// etc.) at the top level of the row's metadata JSON so queries can
// filter and read cascade fields directly without parsing nested blobs.
// cascadeFields is only appended; it never overwrites the standard fields
// (status, validation_criteria, dependencies).
func (dm *DatabaseManager) ProposeTheoryWithExtras(hypothesis, validationCriteria string, dependencies []string, sourceIDs []string, tags []string, cascadeFields map[string]interface{}) (map[string]interface{}, error) {
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
	// Append cascade fields at top level (e.g. cascade=true, cascade_version,
	// dead_artifact_id). These are the design-spec fields the materializer
	// writes so check_wakes and query tools can read cascade metadata directly.
	for k, v := range cascadeFields {
		meta[k] = v
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
//
// Cascade hook (Task 4 of the 2026-08-04 epistemic cascade plan): a
// transition to status="disproven" enqueues one cascade intent per
// downstream decision/theory that cited this theory (via explicit
// `dependencies` JSON or typed `epistemic_provenance` citations).
// The status update, the +1 reinforcement, and the cascade intents
// commit inside a single WithTx transaction so a partial failure
// cannot leave the theory in an inconsistent state.
//
// Trigger policy (pinned by the brief and the design spec):
//
//   - status="proven"             → NO cascade (a proven theory is
//                                   load-bearing, not invalidated).
//   - status="disproven" with
//     prior status="pending"      → cascade (the invalidation event).
//   - status="disproven" with
//     prior status="disproven"    → NO cascade (idempotent; the
//                                   downstream intents already exist
//                                   or were never created).
//
// The "transition" check is enforced by an in-tx UPDATE that
// filters on status='pending'; a second disprove call updates zero
// rows, the cascade hook is skipped, and the caller sees a
// successful response (consistent with the legacy pre-cascade
// behavior). The downstream intent dedup key
// (dead, downstream, event) collapses any race-condition duplicates
// at the storage layer regardless.
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

	// Atomicity boundary: status update + reinforcement + cascade
	// enqueue all live in one tx. Proven and disproven both enter the
	// tx; only the disprove branch enqueues cascade intents, so the
	// brief's "only a transition to disproven" policy is structurally
	// enforced.
	var transitioned bool
	err = dm.WithTx(func(node DBNode) error {
		patch := map[string]interface{}{
			"status":      newStatus,
			"conclusion":  conclusion,
			"resolved_at": now,
			"resolved_by": "call:resolve_theory",
			"resolved_by_via": "manual",
		}
		patchJSON, err := json.Marshal(patch)
		if err != nil {
			return fmt.Errorf("marshal patch: %w", err)
		}

		// Atomic UPDATE: the WHERE filter on status='pending' is the
		// transition detector. A second disprove call (status is
		// already 'disproven') updates zero rows and transitioned
		// stays false → cascade hook skipped.
		//
		// The +1 reinforcement is folded into the same UPDATE so it
		// participates in the transition filter (no reinforcement
		// on a no-op resolve).
		res, err := node.ExecTracked(`
			UPDATE memories
			SET metadata = json_patch(COALESCE(metadata, '{}'), ?),
			    weight = MIN(weight + 1, 100),
			    last_accessed_at = CAST(strftime('%s','now') AS INTEGER), runtime_seconds_since_access = 0, runtime_last_accrued_at = CAST(strftime('%s','now') AS INTEGER)
			WHERE id = ? AND collection = 'theories' AND deleted_at IS NULL
			  AND json_extract(metadata, '$.status') = 'pending'
		`, 0, string(patchJSON), theoryID)
		if err != nil {
			return fmt.Errorf("resolve theory: %w", err)
		}
		rows, _ := res.RowsAffected()
		transitioned = rows > 0

		// Cascade hook: only enqueue on a real transition to
		// disproven. A proven transition (or a repeated disprove on
		// an already-disproven theory) updates zero rows above and
		// skipped the cascade.
		if transitioned && newStatus == "disproven" {
			if _, err := dm.EnqueueCascadeInvalidation(
				node.Tx(), // see DBNode extension below
				theoryID, "theory",
				"theory_disproven", "", 0,
			); err != nil {
				return fmt.Errorf("cascade enqueue: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

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

// SupersedeDecision implements decision invalidation (audit finding F9).
//
// A corrected or superseding decision must be distinguishable from stale
// knowledge WITHOUT deleting history. Mechanism:
//
//   - A new decision row is recorded (full RecordDecision semantics:
//     provenance, active-context metadata).
//   - The ORIGINAL is marked in metadata (superseded=true,
//     superseded_by=<new id>, superseded_at=<rfc3339>) and tagged
//     "superseded" + "superseded-by:<new id>" — the exact tag contract
//     HybridSearch's Phase 5b correction-chain discount already honours
//     (combined score × 0.25), so stale decisions no longer co-rank with
//     current knowledge while remaining fully inspectable.
//   - epistemic_provenance rows link old → new in both directions
//     (source_ids on the new row; superseded_by pointer on the old row).
//
// Multiple corrections chain naturally: each supersede marks its own
// predecessor, and every intermediate stays discoverable by following
// superseded_by pointers. Competing replacements each carry their own
// supersedes:<id> tag for audit.
//
// originalID must be an existing, live decision. An already-superseded
// original is rejected — supersede the CURRENT decision instead, otherwise
// two "current" readings could coexist.
func (dm *DatabaseManager) SupersedeDecision(originalID, contextText, choice, rationale, outcome string, tags []string, sourceIDs []string, ac ActiveContext) (map[string]interface{}, error) {
	mem, err := dm.GetMemory(originalID)
	if err != nil || mem == nil {
		return nil, fmt.Errorf("original decision not found: %s", originalID)
	}
	if coll, _ := mem["collection"].(string); coll != "decisions" {
		return nil, fmt.Errorf("memory %s is not a decision (collection: %s)", originalID, coll)
	}
	metaStr, _ := mem["metadata"].(string)
	var existing map[string]interface{}
	_ = json.Unmarshal([]byte(metaStr), &existing)
	if v, _ := existing["superseded_by"].(string); v != "" {
		return nil, fmt.Errorf("decision %s is already superseded by %s — supersede the current decision instead", originalID, v)
	}

	// Record the replacement first so we have its ID for the back-link.
	allSourceIDs := append([]string{originalID}, sourceIDs...)
	res, err := dm.RecordDecision(contextText, choice, rationale, outcome, tags, allSourceIDs, ac)
	if err != nil {
		return nil, err
	}
	newID, _ := res["id"].(string)

	now := time.Now().UTC().Format(time.RFC3339)
	patchJSON, _ := json.Marshal(map[string]interface{}{
		"superseded":    true,
		"superseded_by": newID,
		"superseded_at": now,
	})

	// Mark the original inside one transaction: metadata patch + tags.
	err = dm.WithTx(func(node DBNode) error {
		if _, err := node.ExecTracked(
			`UPDATE memories SET metadata = json_patch(COALESCE(metadata,'{}'), ?) WHERE id = ? AND deleted_at IS NULL`,
			0, string(patchJSON), originalID); err != nil {
			return fmt.Errorf("mark superseded: %w", err)
		}
		if _, err := node.ExecTracked(
			`UPDATE memories SET tags = CASE
				WHEN COALESCE(tags,'') = '' THEN ?
				ELSE tags || ',' || ?
			END WHERE id = ? AND deleted_at IS NULL`,
			0, "superseded,superseded-by:"+newID, "superseded,superseded-by:"+newID, originalID); err != nil {
			return fmt.Errorf("tag superseded: %w", err)
		}
		// F5-2 (alpha-final): drop the original's confidence to the per-type
		// initial baseline and append a confidence_history row tagged
		// "supersede". Without this, the live confidence column continues
		// to advertise the pre-correction value while HybridSearch's
		// ×0.25 retrieval discount only affects ranking — the two views
		// silently disagree. The recompute path can re-elevate the value
		// if fresh corroborating evidence arrives (just like the F7.1
		// challenged-memory path).
		if _, err := node.ExecTracked(
			`UPDATE memories SET confidence = ? WHERE id = ? AND deleted_at IS NULL`,
			0, DecisionInitialConfidence, originalID); err != nil {
			return fmt.Errorf("drop superseded confidence: %w", err)
		}
		if _, err := node.ExecTracked(`
			INSERT INTO confidence_history (id, artifact_id, artifact_type, confidence, computed_at, evidence_count, trigger)
			VALUES (?, ?, 'decision', ?, ?, 0, 'supersede')
		`, 0, GenerateID(), originalID, DecisionInitialConfidence, time.Now().Unix()); err != nil {
			return fmt.Errorf("supersede confidence history: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	dm.LogAudit(AuditInfo, "epistemology",
		fmt.Sprintf("supersede_decision %s -> %s", originalID, newID), "",
		AuditContext{"original_id": originalID, "new_id": newID})

	return map[string]interface{}{
		"success":          true,
		"original_id":      originalID,
		"id":               newID,
		"choice":           choice,
		"superseded_at":    now,
	}, nil
}

// InvalidateDecision retires a decision that is no longer valid WITHOUT a
// replacement. History is preserved and inspectable; the tag drives the
// HybridSearch supersession discount so it stops outranking current
// knowledge. Idempotent: re-invalidating an already-invalidated decision
// updates the reason but does not fail.
func (dm *DatabaseManager) InvalidateDecision(decisionID, reason string) (map[string]interface{}, error) {
	mem, err := dm.GetMemory(decisionID)
	if err != nil || mem == nil {
		return nil, fmt.Errorf("decision not found: %s", decisionID)
	}
	if coll, _ := mem["collection"].(string); coll != "decisions" {
		return nil, fmt.Errorf("memory %s is not a decision (collection: %s)", decisionID, coll)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	patchJSON, _ := json.Marshal(map[string]interface{}{
		"invalidated":     true,
		"invalidated_at":  now,
		"invalid_reason":  reason,
	})
	err = dm.WithTx(func(node DBNode) error {
		if _, err := node.ExecTracked(
			`UPDATE memories SET metadata = json_patch(COALESCE(metadata,'{}'), ?) WHERE id = ? AND deleted_at IS NULL`,
			0, string(patchJSON), decisionID); err != nil {
			return fmt.Errorf("mark invalidated: %w", err)
		}
		if _, err := node.ExecTracked(
			`UPDATE memories SET tags = CASE
				WHEN COALESCE(tags,'') = '' THEN 'superseded'
				ELSE tags || ',superseded'
			END WHERE id = ? AND deleted_at IS NULL AND tags NOT LIKE '%superseded%'`,
			0, decisionID); err != nil {
			return fmt.Errorf("tag invalidated: %w", err)
		}
		// F5-2 (alpha-final): mirror SupersedeDecision's confidence drop.
		// An invalidated decision is "no evidence"; the live confidence
		// column must reflect that, not the pre-invalidation value.
		if _, err := node.ExecTracked(
			`UPDATE memories SET confidence = ? WHERE id = ? AND deleted_at IS NULL`,
			0, DecisionInitialConfidence, decisionID); err != nil {
			return fmt.Errorf("drop invalidated confidence: %w", err)
		}
		if _, err := node.ExecTracked(`
			INSERT INTO confidence_history (id, artifact_id, artifact_type, confidence, computed_at, evidence_count, trigger)
			VALUES (?, ?, 'decision', ?, ?, 0, 'invalidate')
		`, 0, GenerateID(), decisionID, DecisionInitialConfidence, time.Now().Unix()); err != nil {
			return fmt.Errorf("invalidate confidence history: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"success":       true,
		"id":            decisionID,
		"invalidated":   true,
		"invalidated_at": now,
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

// ─── Decision read symmetry (alpha-4 D-005) ────────────────────────────────
//
// The decision WRITE surface (record/supersede/invalidate) has been
// available since alpha-2, but the READ surface was CLI-only — agents
// had to either drop to SQL or rely on the metadata.provenance
// inclusion in wake context to recover their own decisions. These
// three methods expose show/list/query as first-class CoreDB
// operations so the tool surface (mpm_decisions show/list/query) and
// the CLI subcommands (`mpm decisions show/list/query`) can share a
// single round-trip path.
//
// The return shape is a generic map so callers can read metadata_json
// (status, superseded_by, invalidated_at) directly without coupling
// to a struct. Memory writes go through SaveMemoryNode so the
// decision rows land in the standard `memories` table with
// collection='decisions'.

// GetDecision returns the row for a single decision ID with metadata
// parsed into a `metadata` sub-map. Returns a NotFound-shaped error
// (containing "no rows") if the id is unknown — callers translate to
// their envelope shape.
func (dm *DatabaseManager) GetDecision(id string) (map[string]interface{}, error) {
	if id == "" {
		return nil, fmt.Errorf("get decision: id is required")
	}
	row := dm.db.QueryRow(`
		SELECT id, content, tags, metadata, created_at, updated_at, weight,
		       COALESCE(reinforcement_count, 0)
		FROM memories
		WHERE id = ? AND collection = 'decisions' AND deleted_at IS NULL`, id)

	var (
		gotID, content, tags, metadata string
		createdAt, updatedAt           int64
		weight, reinforcement          int64
	)
	if err := row.Scan(&gotID, &content, &tags, &metadata, &createdAt, &updatedAt, &weight, &reinforcement); err != nil {
		return nil, fmt.Errorf("get decision %s: %w", id, err)
	}

	// Parse metadata_json so callers see a `metadata` sub-map instead of
	// a string blob. Defensive: an unparseable metadata becomes {}.
	parsedMeta := map[string]interface{}{}
	if metadata != "" {
		if err := json.Unmarshal([]byte(metadata), &parsedMeta); err != nil {
			parsedMeta = map[string]interface{}{"_unparsed": metadata}
		}
	}

	return map[string]interface{}{
		"id":                   gotID,
		"content":              content,
		"tags":                 parseDecisionTags(tags),
		"metadata":             parsedMeta,
		"created_at":           createdAt,
		"updated_at":           updatedAt,
		"weight":               weight,
		"reinforcement_count":  reinforcement,
		"status":               extractDecisionStatus(parsedMeta),
	}, nil
}

// ListDecisions returns decision rows matching the filter. The zero
// value (Status="" + Tags=nil + Limit=0) defaults to active decisions
// only with the default page size (50).
func (dm *DatabaseManager) ListDecisions(filter DecisionFilter) ([]map[string]interface{}, error) {
	limit := filter.Limit
	if limit <= 0 {
		limit = 50
	}
	status := filter.Status
	if status == "" {
		status = "active"
	}

	// Build the WHERE clause based on status filter. `active` is the
	// null-superseded and null-invalidated rows; `superseded`/`invalidated`
	// surface the corresponding metadata flag.
	var whereExtra string
	switch status {
	case "active":
		whereExtra = `AND json_extract(metadata, '$.superseded') IS NULL
		              AND json_extract(metadata, '$.invalidated') IS NULL`
	case "superseded":
		whereExtra = `AND json_extract(metadata, '$.superseded') IS NOT NULL`
	case "invalidated":
		whereExtra = `AND json_extract(metadata, '$.invalidated') IS NOT NULL`
	case "all":
		// No extra filter.
	default:
		return nil, fmt.Errorf("list decisions: unknown status filter %q (use active|all|superseded|invalidated)", status)
	}

	// COALESCE wraps the limit so NULL from the params binding becomes 50.
	// Defensive against a caller passing 0 explicitly (handled above, but
	// the SQL is the load-bearing boundary).
	query := fmt.Sprintf(`
		SELECT id, content, tags, metadata, created_at, updated_at, weight,
		       COALESCE(reinforcement_count, 0)
		FROM memories
		WHERE collection = 'decisions' AND deleted_at IS NULL
		%s
		ORDER BY created_at DESC
		LIMIT COALESCE(?, 50)`, whereExtra)

	rows, err := dm.db.Query(query, limit)
	if err != nil {
		return nil, fmt.Errorf("list decisions: %w", err)
	}
	defer rows.Close()

	var out []map[string]interface{}
	for rows.Next() {
		var (
			id, content, tags, metadata string
			createdAt, updatedAt        int64
			weight, reinforcement       int64
		)
		if err := rows.Scan(&id, &content, &tags, &metadata, &createdAt, &updatedAt, &weight, &reinforcement); err != nil {
			return nil, fmt.Errorf("scan decision: %w", err)
		}
		parsedMeta := map[string]interface{}{}
		if metadata != "" {
			if err := json.Unmarshal([]byte(metadata), &parsedMeta); err != nil {
				parsedMeta = map[string]interface{}{"_unparsed": metadata}
			}
		}
		out = append(out, map[string]interface{}{
			"id":                  id,
			"content":             content,
			"tags":                parseDecisionTags(tags),
			"metadata":            parsedMeta,
			"created_at":          createdAt,
			"updated_at":          updatedAt,
			"weight":              weight,
			"reinforcement_count": reinforcement,
			"status":              extractDecisionStatus(parsedMeta),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate decisions: %w", err)
	}
	return out, nil
}

// QueryDecisions reuses SearchMemories against the decisions collection
// so we get FTS5 + BM25 + reinforcement scoring for free. Limit defaults
// to 50 when 0 is passed.
func (dm *DatabaseManager) QueryDecisions(query string, limit int) ([]map[string]interface{}, error) {
	if query == "" {
		return nil, fmt.Errorf("query decisions: query is required")
	}
	if limit <= 0 {
		limit = 50
	}
	rows, err := dm.SearchMemories(query, "decisions", false, limit, 0)
	if err != nil {
		return nil, fmt.Errorf("query decisions: %w", err)
	}
	return rows, nil
}

// extractDecisionStatus returns the decision's lifecycle status from its
// metadata block. Order: invalidated > superseded > active.
func extractDecisionStatus(meta map[string]interface{}) string {
	if meta == nil {
		return "active"
	}
	if _, ok := meta["invalidated"]; ok {
		return "invalidated"
	}
	if _, ok := meta["superseded"]; ok {
		return "superseded"
	}
	return "active"
}

// parseDecisionTags wraps a string-typed tags column in []string. If the
// column is empty or unparseable, returns an empty slice.
func parseDecisionTags(tags string) []string {
	if tags == "" {
		return []string{}
	}
	var out []string
	if err := json.Unmarshal([]byte(tags), &out); err != nil {
		return []string{}
	}
	return out
}