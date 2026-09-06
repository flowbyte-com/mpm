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
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
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

// ChallengeMemoryWithTheory weakens a memory and creates a pending
// theory in a single transaction. The pre-D.2 implementation
// composed three separate transactions:
//
//   1. dm.ChallengeMemory     (status flip, weight demotion, evidence
//                              neutralization, confidence reset)
//   2. MemoryStore.AddMemory  (theory row INSERT — its own TX)
//   3. dm.SQLDB().Exec        (challenged_theory_id link UPDATE)
//
// A failure in step 2 or 3 left the memory in the challenged state
// without the theory row or the forward link, an audit-trail-destroying
// partial state. The D.2 fix wraps all three operations in a single
// transaction so either the entire challenge transition commits or
// none of it does.
//
// F7.1 invariants (status flip, prior-weight capture, weight demotion,
// evidence neutralization, confidence reset, theory row creation,
// forward-link update) are preserved exactly.
func (dm *DatabaseManager) ChallengeMemoryWithTheory(memoryID, evidence string) (map[string]interface{}, error) {
	// Pre-flight memory existence check (matches the legacy
	// non-tx wrapper's behaviour: a missing id fails fast before
	// any writes, with the canonical "memory not found: <id>"
	// message). The check is also re-asserted inside the tx by
	// challengeMemoryInTx so a concurrent delete between the
	// pre-check and BEGIN still aborts cleanly.
	mem, err := dm.GetMemory(memoryID)
	if err != nil {
		return nil, err
	}

	tx, err := dm.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("challenge with theory: begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	// Step 1: F7.1 work inside the shared tx.
	if err := challengeMemoryInTx(tx, memoryID, 2, evidence); err != nil {
		return nil, fmt.Errorf("challenge with theory: weaken: %w", err)
	}

	// Step 2: theory row INSERT inside the same tx. Pre-fix this
	// used MemoryStore.AddMemory which opened its own transaction;
	// the direct INSERT keeps everything atomic.
	theoryID := GenerateID()
	theoryContent := fmt.Sprintf("CHALLENGED_MEMORY_ID: %s\nEVIDENCE: %s\nCHALLENGED_AT_NANO: %d\nORIGINAL_CONTENT: %s",
		memoryID, evidence, time.Now().UnixNano(), mem["content"])
	theoryMeta := map[string]interface{}{
		"status":               "pending",
		"challenged_memory_id": memoryID,
		"evidence":             evidence,
	}
	theoryMetaJSON, _ := json.Marshal(theoryMeta)
	if _, err := tx.Exec(
		`INSERT INTO memories (id, collection, content, tags, metadata, created_at, weight)
		 VALUES (?, 'theories', ?, '["challenge"]', ?, ?, 1)`,
		theoryID, theoryContent, string(theoryMetaJSON), time.Now().Unix(),
	); err != nil {
		return nil, fmt.Errorf("challenge with theory: create theory: %w", err)
	}

	// Step 3: forward-link UPDATE inside the same tx. Pre-fix this
	// was a separate dm.SQLDB().Exec; the post-D.2 path uses the
	// shared tx so the link can never be left dangling.
	linkPatch, _ := json.Marshal(map[string]interface{}{
		"challenged_theory_id": theoryID,
		"challenged_evidence":  evidence,
	})
	if _, err := tx.Exec(
		`UPDATE memories SET metadata = json_patch(COALESCE(metadata,'{}'), ?) WHERE id = ? AND deleted_at IS NULL`,
		string(linkPatch), memoryID,
	); err != nil {
		return nil, fmt.Errorf("challenge with theory: link theory: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("challenge with theory: commit: %w", err)
	}
	committed = true

	return map[string]interface{}{
		"success":       true,
		"memory_id":     memoryID,
		"action":        "weakened",
		"theory_id":     theoryID,
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

	// D-002: capture the current status so a no-op resolve can report
	// the persisted state. Without this, a retry/duplicate resolve
	// would return success with the *requested* status while the
	// substrate still holds the prior terminal status — a silent
	// response lie.
	//
	// GetMemory returns metadata as a raw JSON string (see web_db.go),
	// so the live status has to be parsed out of that envelope rather
	// than via a top-level type assertion.
	currentStatus := extractTheoryStatusFromMemory(mem)

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
		// stays false → cascade hook skipped, and the post-tx
		// D-002 guard surfaces the persisted terminal status.
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

	// D-002: a no-op resolve (RowsAffected==0) means the theory was
	// already in a terminal state. Returning success here would let
	// the caller believe the new status was applied; the persisted
	// status remains the prior terminal value. Surface the lie as an
	// explicit error so retry wrappers and double-clicks stop
	// reporting false state transitions.
	if !transitioned {
		persisted := currentStatus
		if persisted == "" {
			persisted = "non-pending"
		}
		return nil, fmt.Errorf("theory %s is already resolved (status=%s); refusing to overwrite with newStatus=%s", theoryID, persisted, newStatus)
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
	// D-007 (alpha-4.1.1): content is the canonical textual body of the
	// decision. The pre-fix shape prefixed each structured field with
	// a label ("CHOICE: ", "CONTEXT: ", "RATIONALE: ", "OUTCOME: ") and
	// reconstructed the original text from the same structured fields
	// that were ALSO stamped into metadata. That made show/list output
	// duplicated: every decision surfaced its context/rationale twice —
	// once in content (with label), once in metadata.context /
	// metadata.rationale. The fix removes the labels. content keeps the
	// FTS-searchable body; metadata stays the structured parsed fields.
	// show/list (GetDecision / ListDecisions / QueryDecisions) already
	// return the row as stored and never reconstructed — that half of
	// the spec was already correct.
	var contentParts []string
	if choice != "" {
		contentParts = append(contentParts, choice)
	}
	if contextText != "" {
		contentParts = append(contentParts, contextText)
	}
	if rationale != "" {
		contentParts = append(contentParts, rationale)
	}
	if outcome != "" {
		contentParts = append(contentParts, outcome)
	}
	content := strings.Join(contentParts, "\n\n")
	meta := ac.withActiveContextMeta(nil)
	if contextText != "" {
		meta["context"] = contextText
	}
	if rationale != "" {
		meta["rationale"] = rationale
	}
	if outcome != "" {
		meta["outcome"] = outcome
	}
	if choice != "" {
		meta["choice"] = choice
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
// if the id is unknown — callers translate to their envelope shape.
//
// 2026-09-05 audit residual pass §I-C.14: the previous shape
// propagated the raw sql driver error (`sql: no rows in result
// set`) for unknown ids, leaking implementation detail into the
// public tool response. Now translates sql.ErrNoRows into the
// canonical "decision not found" envelope, matching GetMemory's
// behavior at internal/core/web_db.go:281.
func (dm *DatabaseManager) GetDecision(id string) (map[string]interface{}, error) {
	if id == "" {
		return nil, fmt.Errorf("get decision: id is required")
	}
	row := dm.db.QueryRow(`
		SELECT id, content, tags, metadata, created_at, updated_at, weight,
		       COALESCE(reinforcement_count, 0)
		FROM memories
		WHERE id = ? AND collection = 'decisions' AND deleted_at IS NULL`, id)

	// Substrate Defense Triad #2: NULL safety on legacy tags/metadata
	// columns. The sibling GetTheory in this file uses sql.NullString for
	// these columns (the exact same NULL-panic class that caused the
	// 2026-09-02 OpenClaw `theories_pending` discrepancy). Decision rows
	// written before tags/metadata were mandatory could have SQL NULL,
	// and scanning NULL into a concrete string panics with
	// "converting NULL to string is unsupported".
	var (
		gotID, content      string
		tags, metadata      sql.NullString
		createdAt, updatedAt int64
		weight, reinforcement int64
	)
	if err := row.Scan(&gotID, &content, &tags, &metadata, &createdAt, &updatedAt, &weight, &reinforcement); err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("decision not found: %s", id)
		}
		return nil, fmt.Errorf("get decision %s: %w", id, err)
	}

	// Parse metadata_json so callers see a `metadata` sub-map instead of
	// a string blob. Defensive: an unparseable metadata becomes {}.
	parsedMeta := map[string]interface{}{}
	if metadata.Valid && metadata.String != "" {
		if err := json.Unmarshal([]byte(metadata.String), &parsedMeta); err != nil {
			parsedMeta = map[string]interface{}{"_unparsed": metadata.String}
		}
	}

	return map[string]interface{}{
		"id":                   gotID,
		"content":              content,
		"tags":                 parseDecisionTagsAny(tags),
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
	// 2026-09-05 audit remediation pass 2: the previous shape
	// coerced `limit <= 0` to 50, which silently overrode the
	// caller-supplied limit (including limit=0 from the public
	// surface). The handler now validates limit via parseLimitStrict,
	// so any value reaching here is intentional. CLI callers that
	// want the historical default-50 behaviour pass 50 explicitly
	// (see cmd/mpm/handlers_epistemology.go).
	limit := filter.Limit
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

	// Tag filter — alpha-4 ledger audit SEV-1 fix. The DecisionFilter
	// contract has always promised a Tags filter, but ListDecisions
	// never applied it (the field was dead code). Tags are stored as a
	// JSON array in the `tags` column; an OR-of-exists predicate per
	// requested tag filters to rows whose tags array contains any of
	// the requested tags. A NULL tags column cannot match a tag query,
	// which is the documented behavior.
	if len(filter.Tags) > 0 {
		tagClauses := make([]string, 0, len(filter.Tags))
		for _, t := range filter.Tags {
			if t == "" {
				continue
			}
			tagClauses = append(tagClauses, "EXISTS (SELECT 1 FROM json_each(tags) WHERE value = ?)")
		}
		if len(tagClauses) > 0 {
			whereExtra += " AND (" + strings.Join(tagClauses, " OR ") + ")"
		}
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

	// Build args: tag placeholders + limit placeholder. Order matches
	// the WHERE clause construction (tags first, then limit).
	args := make([]interface{}, 0, len(filter.Tags)+1)
	for _, t := range filter.Tags {
		if t == "" {
			continue
		}
		args = append(args, t)
	}
	args = append(args, limit)

	rows, err := dm.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("list decisions: %w", err)
	}
	defer rows.Close()

	var out []map[string]interface{}
	for rows.Next() {
		var (
			id, content         string
			tags, metadata      sql.NullString
			createdAt, updatedAt int64
			weight, reinforcement int64
		)
		if err := rows.Scan(&id, &content, &tags, &metadata, &createdAt, &updatedAt, &weight, &reinforcement); err != nil {
			return nil, fmt.Errorf("scan decision: %w", err)
		}
		parsedMeta := map[string]interface{}{}
		if metadata.Valid && metadata.String != "" {
			if err := json.Unmarshal([]byte(metadata.String), &parsedMeta); err != nil {
				parsedMeta = map[string]interface{}{"_unparsed": metadata.String}
			}
		}
		out = append(out, map[string]interface{}{
			"id":                  id,
			"content":             content,
			"tags":                parseDecisionTagsAny(tags),
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
	// 2026-09-05 audit remediation pass 2: see ListDecisions.
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

// =============================================================================
// Theory read surface (alpha-4 audit D-006).
//
// Theories are stored as memories with collection='theories'. Status lives in
// the metadata JSON. GetTheory / ListTheories / QueryTheories mirror the
// GetDecision / ListDecisions / QueryDecisions shape so the agent runtime
// has a single mental model for "read an epistemic artifact by collection".
//
// Status vocabulary:
//   - "pending"   — newly proposed, not yet resolved (default filter)
//   - "proven"    — manually validated as true
//   - "disproven" — manually invalidated as false
//   - "resolved"  — synthetic family containing proven + disproven
//   - "all"       — no filter
// =============================================================================

// GetTheory returns the row for a single theory ID with metadata parsed
// into a `metadata` sub-map. Returns a NotFound-shaped error if the id is
// unknown or the row belongs to a different collection.
func (dm *DatabaseManager) GetTheory(id string) (map[string]interface{}, error) {
	if id == "" {
		return nil, fmt.Errorf("get theory: id is required")
	}
	row := dm.db.QueryRow(`
		SELECT id, content, tags, metadata, created_at, updated_at, weight,
		       COALESCE(reinforcement_count, 0)
		FROM memories
		WHERE id = ? AND collection = 'theories' AND deleted_at IS NULL`, id)

	var (
		gotID, content           string
		tags, metadata           sql.NullString
		createdAt, updatedAt     int64
		weight, reinforcement    int64
	)
	if err := row.Scan(&gotID, &content, &tags, &metadata, &createdAt, &updatedAt, &weight, &reinforcement); err != nil {
		return nil, fmt.Errorf("get theory %s: %w", id, err)
	}

	// Defense-in-depth: tags/metadata are JSON columns written through
	// distinct paths (legacy callers omit them entirely → SQL NULL,
	// current callers write '[]' / '{}'). Scan to NullString so a
	// NULL row cannot panic the read path. The 2026-08-17 substrate
	// defense triad mandates this on every nullable scalar column.
	parsedMeta := map[string]interface{}{}
	if metadata.Valid && metadata.String != "" {
		if err := json.Unmarshal([]byte(metadata.String), &parsedMeta); err != nil {
			parsedMeta = map[string]interface{}{"_unparsed": metadata.String}
		}
	}
	tagsStr := ""
	if tags.Valid {
		tagsStr = tags.String
	}

	// Theory content is stored as "<hypothesis>\n\nVALIDATION_CRITERIA: ..."
	// by ProposeTheory. Split it so callers see the two fields directly
	// rather than having to re-parse on every read.
	hypothesis, validationCriteria := splitHypothesisAndCriteria(content)

	return map[string]interface{}{
		"id":                  gotID,
		"hypothesis":          hypothesis,
		"validation_criteria": validationCriteria,
		"tags":                parseDecisionTags(tagsStr),
		"metadata":            parsedMeta,
		"created_at":          createdAt,
		"updated_at":          updatedAt,
		"weight":              weight,
		"reinforcement_count": reinforcement,
		"status":              extractTheoryStatus(parsedMeta),
	}, nil
}

// ListTheories returns theory rows matching the filter. The zero value
// (Status="" + Tags=nil + Limit=0) defaults to pending theories only with
// the default page size (50).
//
// The status filter dispatches to a JSON predicate on metadata.status:
//   - pending  → status='pending'
//   - proven   → status='proven'
//   - disproven → status='disproven'
//   - resolved → status IN ('proven','disproven') (matches CLI "resolved"
//                semantics: anything no longer pending)
//   - all      → no extra predicate
func (dm *DatabaseManager) ListTheories(filter TheoryFilter) ([]map[string]interface{}, error) {
	// 2026-09-05 audit remediation pass 2: see ListDecisions.
	limit := filter.Limit
	status := filter.Status
	if status == "" {
		status = "pending"
	}

	var whereExtra string
	switch status {
	case "pending":
		// The `pending` predicate MUST match health_check.theories_pending
		// exactly — including the expires_at filter — so the OpenClaw
		// gateway's pending-theory count agrees with what
		// `mpm_theories list status=pending` returns.
		whereExtra = `AND json_extract(metadata, '$.status') = 'pending' AND (expires_at IS NULL OR expires_at > strftime('%s','now'))`
	case "proven":
		whereExtra = `AND json_extract(metadata, '$.status') = 'proven' AND (expires_at IS NULL OR expires_at > strftime('%s','now'))`
	case "disproven":
		whereExtra = `AND json_extract(metadata, '$.status') = 'disproven' AND (expires_at IS NULL OR expires_at > strftime('%s','now'))`
	case "resolved":
		// M3 audit D-010: include the legacy literal `resolved` value
		// alongside the canonical `proven`/`disproven` so pre-fix rows
		// remain queryable through the same filter. New resolutions
		// (post-fix) write only `proven` or `disproven`.
		whereExtra = `AND json_extract(metadata, '$.status') IN ('proven','disproven','resolved') AND (expires_at IS NULL OR expires_at > strftime('%s','now'))`
	case "all":
		// No extra filter. `all` is for forensics / historical review —
		// operators may want to surface expired rows to triage stale work.
		// The strict status filters above match the health_check metric
		// exactly; `all` does not, by design.
	default:
		return nil, fmt.Errorf("list theories: unknown status filter %q (use pending|all|proven|disproven|resolved)", status)
	}

	query := `
		SELECT id, content, tags, metadata, created_at, updated_at, weight,
		       COALESCE(reinforcement_count, 0)
		FROM memories
		WHERE collection = 'theories' AND deleted_at IS NULL
	` + whereExtra + `
		ORDER BY created_at DESC
		LIMIT ?
	`
	rows, err := dm.db.Query(query, limit)
	if err != nil {
		return nil, fmt.Errorf("list theories: %w", err)
	}
	defer rows.Close()

	var out []map[string]interface{}
	for rows.Next() {
		var (
			gotID, content         string
			tags, metadata         sql.NullString
			createdAt, updatedAt   int64
			weight, reinforcement  int64
		)
		if err := rows.Scan(&gotID, &content, &tags, &metadata, &createdAt, &updatedAt, &weight, &reinforcement); err != nil {
			return nil, fmt.Errorf("scan theory row: %w", err)
		}
		// Defense-in-depth (Substrate Defense Triad #2): rows from
		// pre-D-005 paths may have NULL `tags` / `metadata` columns.
		// Scanning NULL into a concrete `string` panics with
		// "converting NULL to string is unsupported"; this read surface
		// must stay reachable even for legacy rows so the agent can
		// triage them via mpm_resolve.
		parsedMeta := map[string]interface{}{}
		if metadata.Valid && metadata.String != "" {
			if err := json.Unmarshal([]byte(metadata.String), &parsedMeta); err != nil {
				parsedMeta = map[string]interface{}{"_unparsed": metadata.String}
			}
		}
		tagsStr := ""
		if tags.Valid {
			tagsStr = tags.String
		}
		hypothesis, validationCriteria := splitHypothesisAndCriteria(content)
		out = append(out, map[string]interface{}{
			"id":                  gotID,
			"hypothesis":          hypothesis,
			"validation_criteria": validationCriteria,
			"tags":                parseDecisionTags(tagsStr),
			"metadata":            parsedMeta,
			"created_at":          createdAt,
			"updated_at":          updatedAt,
			"weight":              weight,
			"reinforcement_count": reinforcement,
			"status":              extractTheoryStatus(parsedMeta),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate theory rows: %w", err)
	}
	return out, nil
}

// QueryTheories reuses SearchMemories against the theories collection so
// the BM25 + reinforcement weighting the runtime already implements is
// available to the theory read path without re-implementing ranking.
func (dm *DatabaseManager) QueryTheories(query string, limit int) ([]map[string]interface{}, error) {
	if query == "" {
		return nil, fmt.Errorf("query theories: query is required")
	}
	// 2026-09-05 audit remediation pass 2: see ListDecisions.
	rows, err := dm.SearchMemories(query, "theories", false, limit, 0)
	if err != nil {
		return nil, fmt.Errorf("query theories: %w", err)
	}
	return rows, nil
}

// extractTheoryStatus returns the theory's lifecycle status from its
// metadata block. Defaults to "pending" when no status key is present
// (matches the ProposeTheory default).
func extractTheoryStatus(meta map[string]interface{}) string {
	if meta == nil {
		return "pending"
	}
	if s, ok := meta["status"].(string); ok && s != "" {
		return s
	}
	return "pending"
}

// splitHypothesisAndCriteria reverses the "\n\nVALIDATION_CRITERIA: ..."
// concatenation done by ProposeTheory so GetTheory / ListTheories can
// surface the two fields separately. If the content has no separator,
// the entire content is the hypothesis and the criteria is empty.
func splitHypothesisAndCriteria(content string) (string, string) {
	const sep = "\n\nVALIDATION_CRITERIA: "
	if i := strings.Index(content, sep); i >= 0 {
		return content[:i], content[i+len(sep):]
	}
	return content, ""
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

// parseDecisionTagsAny is the NULL-safe variant for the read paths that
// scan the tags column into sql.NullString. It mirrors parseDecisionTags
// on the Valid=true branch and returns an empty slice on NULL — the same
// default the string-typed variant returns on the empty-string branch.
// Substrate Defense Triad #2 (NULL safety).
func parseDecisionTagsAny(tags sql.NullString) []string {
	if !tags.Valid || tags.String == "" {
		return []string{}
	}
	return parseDecisionTags(tags.String)
}

// extractTheoryStatusFromMemory pulls metadata.status from a GetMemory
// map projection. The metadata column is returned as a raw JSON string
// (see web_db.go:GetMemory), so the live status has to be parsed out
// of that envelope. Returns "" when the field is missing or unparseable
// — callers must treat "" as "unknown" rather than "pending".
func extractTheoryStatusFromMemory(mem map[string]interface{}) string {
	if mem == nil {
		return ""
	}
	// Some call sites pass an already-parsed metadata map.
	if meta, ok := mem["metadata"].(map[string]interface{}); ok {
		if s, ok := meta["status"].(string); ok {
			return s
		}
	}
	// Canonical shape: metadata is a raw JSON string.
	if raw, ok := mem["metadata"].(string); ok && raw != "" {
		var meta map[string]interface{}
		if err := json.Unmarshal([]byte(raw), &meta); err == nil {
			if s, ok := meta["status"].(string); ok {
				return s
			}
		}
	}
	return ""
}